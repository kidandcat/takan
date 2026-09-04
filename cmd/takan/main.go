package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// tzdata is embedded so Europe/Madrid resolves on a host without it, which
	// is the zone the health diary resolves "today" in.
	_ "time/tzdata"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/api"
	"github.com/kidandcat/takan/internal/config"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/oauth"
	"github.com/kidandcat/takan/internal/ratelimit"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/internal/web"
	"github.com/kidandcat/takan/modules"
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
	cfg := config.Load()
	if cfg.SessionKey == "dev-insecure-change-me" {
		log.Printf("WARNING: TAKAN_SESSION_KEY is the insecure default — set a random key before storing secrets")
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

	webSrv, err := web.New(st, hub, box, cfg.PublicURL, cfg.DataDir)
	if err != nil {
		log.Fatalf("web: %v", err)
	}
	webSrv.AuthRateLimit = authLimit
	webSrv.SendLoginCode = sendLoginCode
	webSrv.OwnerEmail = cfg.OwnerEmail
	webSrv.LoginCodeRateLimit = loginCodeLimit
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
	// A finished machine_ai_run wakes the agents watching it over MCP SSE.
	hub.OnJobEvent = func(userID, machineName string, job agenthub.AIJob) {
		mcpSrv.NotifyUser(userID, "notifications/takan/machine_ai_job", machine.NotificationFromJob(machineName, job))
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

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	if cfg.OwnerEmail == "" {
		log.Printf("warning: TAKAN_OWNER_EMAIL is not set — panel login falls back to the owner row address")
	}
	log.Printf("takan listening on %s public=%s data=%s (single operator)",
		cfg.Listen, cfg.PublicURL, cfg.DataDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func agentBinDir() string {
	if d := os.Getenv("TAKAN_AGENT_BIN_DIR"); d != "" {
		return d
	}
	return "/opt/takan/agents"
}
