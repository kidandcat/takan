package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// tzdata is embedded so Europe/Madrid resolves on a host without it, which
	// is what the scheduler interprets every reminder in.
	_ "time/tzdata"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/api"
	"github.com/kidandcat/takan/internal/assistant"
	"github.com/kidandcat/takan/internal/config"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/oauth"
	"github.com/kidandcat/takan/internal/ratelimit"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/internal/web"
	"github.com/kidandcat/takan/modules"
	assistantmod "github.com/kidandcat/takan/modules/assistant"
	"github.com/kidandcat/takan/modules/display"
	"github.com/kidandcat/takan/modules/email"
	"github.com/kidandcat/takan/modules/health"
	"github.com/kidandcat/takan/modules/machine"
	"github.com/kidandcat/takan/modules/mercadona"
	"github.com/kidandcat/takan/modules/people"
	"github.com/kidandcat/takan/modules/tv"
	"github.com/kidandcat/takan/modules/vault"
)

func main() {
	// atlas-send / atlas-sched / atlas-task are the helper binaries the CLI
	// agent calls. They are symlinks to this binary, so dispatch on the invoked
	// name and also accept the equivalent subcommands.
	switch filepath.Base(os.Args[0]) {
	case "atlas-send":
		runSend(os.Args[1:])
		return
	case "atlas-sched":
		runSched(os.Args[1:])
		return
	case "atlas-task":
		runTask(os.Args[1:])
		return
	}
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "send":
			runSend(os.Args[2:])
			return
		case "sched":
			runSched(os.Args[2:])
			return
		case "task":
			runTask(os.Args[2:])
			return
		default:
			log.Fatalf("unknown command %q (known: send, sched, task)", os.Args[1])
		}
	}

	cfg := config.Load()
	if cfg.SessionKey == "dev-insecure-change-me" {
		log.Printf("WARNING: ATLAS_SESSION_KEY is the insecure default — set a random key before storing secrets")
	}
	if err := cfg.CheckLocalAddr(); err != nil {
		log.Fatalf("config: %v", err)
	}
	var backup *store.BackupOpts
	if cfg.BackupBucket != "" {
		backup = &store.BackupOpts{
			Endpoint:  cfg.BackupEndpoint,
			Region:    cfg.BackupRegion,
			Bucket:    cfg.BackupBucket,
			Prefix:    cfg.BackupPrefix,
			AccessKey: cfg.BackupAccessKey,
			SecretKey: cfg.BackupSecretKey,
		}
	}
	st, err := store.Open(cfg.DataDir, backup)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// One-time import from standalone mercadona.db (if still present).
	legacyMerc := filepath.Join(cfg.DataDir, "mercadona.db")
	if err := st.ImportLegacyMercadonaDB(legacyMerc); err != nil {
		log.Printf("mercadona legacy import: %v", err)
	}

	box, err := cryptox.NewBox(cfg.SessionKey)
	if err != nil {
		log.Fatalf("crypto: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub := agenthub.New(
		func(ctx context.Context, token string) (machineID, userID, name string, err error) {
			m, err := st.MachineByAgentToken(ctx, token)
			if err != nil {
				return "", "", "", err
			}
			return m.ID, m.UserID, m.Name, nil
		},
		func(ctx context.Context, machineID string) {
			_ = st.TouchMachine(ctx, machineID)
		},
	)

	// Mercadona shares the main Takan/Colmena DB (rows keyed by owner user id).
	mbox, err := mercadona.NewBox(cfg.SessionKey)
	if err != nil {
		log.Fatalf("mercadona crypto: %v", err)
	}
	mercMod := mercadona.NewModule(st, st.DB(), mbox, cfg.PublicURL)

	rl := ratelimit.New()
	bashLimit := machine.BashLimiter(nil)
	if cfg.MachineBashPerMin > 0 {
		max := cfg.MachineBashPerMin
		bashLimit = func(userID string) bool {
			return rl.Allow("bash:"+userID, max, time.Minute)
		}
	}
	authLimit := func(key string) bool {
		max := cfg.AuthPerMin
		if max <= 0 {
			return true
		}
		return rl.Allow(key, max, time.Minute)
	}
	// "Send code" is far cheaper to abuse than a guess, so it gets its own
	// window (default 3 per 15 min) applied per IP and globally.
	loginCodeLimit := func(key string) bool {
		window := time.Duration(cfg.LoginCodeWindowMin) * time.Minute
		if window <= 0 {
			window = 15 * time.Minute
		}
		return rl.Allow(key, cfg.LoginCodePerWindow, window)
	}
	// The account that receives login codes is the operator, whatever the users
	// table creation order says.
	st.SetOwnerHint(cfg.OwnerEmail)
	sendLoginCode := email.LoginCodeFactory(st, box, cfg.ResendAPIKey, cfg.AuthEmailFrom)

	// The assistant needs an owner row to hang its data off. On a fresh instance
	// there is none yet, so it starts on the next boot, after the first sign-in.
	asst := startAssistant(ctx, st, box, hub, cfg)

	prov := &modules.Provider{
		Store: st,
		Hub:   hub,
		MercadonaLinked: func(ctx context.Context, userID string) bool {
			return mercadona.HasLinkedSession(ctx, st.DB(), userID)
		},
		Machine:   machine.Factory(st, hub, bashLimit),
		Mercadona: mercMod.Factory(),
		Email:     email.Factory(st, box),
		People:    people.Factory(st),
		Health:    health.Factory(st),
		Vault:     vault.Factory(st, box),
		Display:   display.Factory(st, hub),
		TV:        tv.Factory(st, hub),
	}
	if asst != nil {
		prov.Assistant = assistantmod.Factory(asst)
		prov.AssistantStatus = func(ctx context.Context) (bool, string) {
			return assistantReadiness(asst.Status(ctx))
		}
	}

	webSrv, err := web.New(st, hub, box, cfg.PublicURL, cfg.DataDir)
	if err != nil {
		log.Fatalf("web: %v", err)
	}
	webSrv.AuthRateLimit = authLimit
	webSrv.SendLoginCode = sendLoginCode
	webSrv.OwnerEmail = cfg.OwnerEmail
	webSrv.LoginCodeRateLimit = loginCodeLimit
	if asst != nil {
		webSrv.Assistant = asst
	}
	webSrv.OnMercadonaSave = func(ctx context.Context, userID, emailAddr, password, postal string) error {
		return mercadona.LinkAccount(ctx, st.DB(), mbox, userID, emailAddr, password, postal)
	}
	webSrv.OnMercadonaClear = func(ctx context.Context, userID string) error {
		return mercadona.UnlinkAccount(ctx, st.DB(), userID)
	}

	mcpSrv := &mcp.Server{
		Name:      "takan",
		PublicURL: cfg.PublicURL,
		Sessions:  mcp.NewSessionHub(),
		Resolve: func(ctx context.Context, bearer string) (string, error) {
			u, err := st.UserByAccessToken(ctx, bearer)
			if err != nil {
				return "", err
			}
			return u.ID, nil
		},
		ToolsFor: prov.ToolsFor,
	}
	// A finished machine_ai_run wakes other agents over MCP SSE and lands in the
	// Telegram chat that asked for it.
	hub.OnJobEvent = func(userID, machineName string, job agenthub.AIJob) {
		mcpSrv.NotifyUser(userID, "notifications/takan/machine_ai_job", machine.NotificationFromJob(machineName, job))
		if asst != nil {
			asst.OnJobEvent(userID, machineName, job)
		}
	}
	webSrv.OnToolsChanged = mcpSrv.NotifyToolsChanged

	apiSrv := &api.Server{
		Store: st, Box: box, PublicURL: cfg.PublicURL,
		OnToolsChanged: mcpSrv.NotifyToolsChanged,
		AuthRateLimit:  authLimit,
		StatusJSON:     prov.StatusJSON,

		SendLoginCode:      sendLoginCode,
		OwnerEmail:         cfg.OwnerEmail,
		LoginCodeRateLimit: loginCodeLimit,
	}

	oauthSrv := &oauth.Server{
		Store:            st,
		PublicURL:        cfg.PublicURL,
		RateLimit:        authLimit,
		UserFromSession:  webSrv.CurrentUser,
		CreateSession:    webSrv.CreateWebSession,
		SetSessionCookie: webSrv.SetSessionCookie,
	}

	// Periodic GC for expired tokens/sessions and delivered job routing rows.
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			if n, err := st.DeleteExpiredOAuthTokens(context.Background()); err != nil {
				log.Printf("token gc: %v", err)
			} else if n > 0 {
				log.Printf("token gc: removed %d expired rows", n)
			}
			if _, err := st.PurgeJobChats(context.Background(), 30*24*time.Hour); err != nil {
				log.Printf("job chat gc: %v", err)
			}
			if _, err := st.PurgeLoginCodes(context.Background(), time.Hour); err != nil {
				log.Printf("login code gc: %v", err)
			}
			rl.Cleanup(2 * time.Hour)
		}
	}()

	mux := http.NewServeMux()
	webSrv.Routes(mux)
	oauthSrv.Routes(mux)
	apiSrv.Routes(mux)
	if asst != nil {
		// The phone app talks to the app host, which proxies only /v1/*.
		asst.AppRoutes(mux)
	}
	mux.HandleFunc("POST /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("GET /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("DELETE /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("OPTIONS /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("GET /agent/ws", hub.HandleWS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /install.sh", serveInstallSh(cfg.PublicURL))
	mux.Handle("GET /download/", http.StripPrefix("/download/", http.FileServer(http.Dir(agentBinDir()))))

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}

	// The loopback server carries /health, /jobs, /tasks and /internal/*: the
	// surface the assistant CLIs and Gatus use, never reverse-proxied.
	var localSrv *http.Server
	if asst != nil {
		localMux := http.NewServeMux()
		asst.LocalRoutes(localMux)
		localSrv = &http.Server{
			Addr:              cfg.LocalAddr,
			Handler:           localMux,
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			log.Printf("assistant local API on %s", cfg.LocalAddr)
			if err := localSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("local API error: %v", err)
			}
		}()
		go func() {
			if err := asst.Run(ctx); err != nil {
				log.Printf("assistant stopped: %v", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
		if localSrv != nil {
			_ = localSrv.Shutdown(shutdown)
		}
	}()

	if cfg.OwnerEmail == "" {
		log.Printf("warning: ATLAS_OWNER_EMAIL is not set — panel login falls back to the owner row address")
	}
	log.Printf("takan listening on %s public=%s app=%s data=%s (single operator)",
		cfg.Listen, cfg.PublicURL, cfg.AppURL, cfg.DataDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// startAssistant builds the in-process assistant, or returns nil with a clear
// log line. A missing credential must not take the panel down: the hub is still
// useful without the Telegram side, and the panel is where it gets configured.
func startAssistant(ctx context.Context, st *store.Store, box *cryptox.Box,
	hub *agenthub.Hub, cfg config.Config) *assistant.Assistant {
	owner, err := st.Owner(ctx)
	if err != nil || owner == nil {
		log.Printf("assistant: not started — this instance has no owner yet; sign in to the panel first")
		return nil
	}
	a, err := assistant.New(ctx, st, box, hub, assistant.Config{
		OwnerID:                owner.ID,
		OwnerTelegram:          cfg.OwnerTelegramID,
		DataDir:                cfg.DataDir,
		AgentHome:              cfg.AgentHome,
		TelegramBotToken:       cfg.TelegramBotToken,
		GroqAPIKey:             cfg.GroqAPIKey,
		AppToken:               cfg.AppToken,
		FirebaseServiceAccount: cfg.FirebaseServiceAccount,
		LegacyDir:              cfg.LegacyDir,
	})
	if err != nil {
		log.Printf("assistant: not started — %v", err)
		return nil
	}
	return a
}

// assistantReadiness renders the module status row for takan_status.
func assistantReadiness(s assistant.Status) (bool, string) {
	bot := s.BotUsername
	if bot == "" {
		bot = "connecting"
	} else {
		bot = "@" + bot
	}
	detail := fmt.Sprintf("%s · owner %d · %d chat(s) · %d task(s) running · %d job(s) scheduled",
		bot, s.OwnerTelegram, s.KnownChats, s.RunningTasks, s.ScheduledJobs)
	if !s.Enabled {
		return false, detail + " · disabled in the panel"
	}
	if !s.PollHealthy {
		reason := s.LastPollError
		if reason == "" {
			reason = "no successful poll yet"
		}
		return false, detail + " · not receiving updates: " + reason
	}
	return true, detail
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/v1/health" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func agentBinDir() string {
	if d := os.Getenv("ATLAS_AGENT_BIN_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("TAKAN_AGENT_BIN_DIR"); d != "" {
		return d
	}
	return "/opt/takan/agents"
}
