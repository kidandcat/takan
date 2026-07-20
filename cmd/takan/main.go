package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kidandcat/mercadona-mcp/sdk"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/config"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/modules"
	"github.com/kidandcat/takan/internal/modules/machine"
	"github.com/kidandcat/takan/internal/modules/mercadona"
	"github.com/kidandcat/takan/internal/oauth"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/internal/web"
)

func main() {
	cfg := config.Load()
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

	box, err := cryptox.NewBox(cfg.SessionKey)
	if err != nil {
		log.Fatalf("crypto: %v", err)
	}

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

	// Mercadona multi-tenant DB (aliases, cart prefs, encrypted sessions).
	mdb, err := sdk.OpenDB(filepath.Join(cfg.DataDir, "mercadona.db"))
	if err != nil {
		log.Fatalf("mercadona store: %v", err)
	}
	defer mdb.Close()
	mbox, err := sdk.NewBox(cfg.SessionKey)
	if err != nil {
		log.Fatalf("mercadona crypto: %v", err)
	}
	mercMod := mercadona.NewModule(st, mdb, mbox, cfg.PublicURL)

	prov := &modules.Provider{
		Store:     st,
		Machine:   machine.Factory(st, hub),
		Mercadona: mercMod.Factory(),
	}

	webSrv, err := web.New(st, hub, box, cfg.PublicURL)
	if err != nil {
		log.Fatalf("web: %v", err)
	}
	webSrv.OnMercadonaSave = func(ctx context.Context, userID, email, password, postal string) error {
		return mercadona.LinkAccount(ctx, mdb, mbox, userID, email, password, postal)
	}
	webSrv.OnMercadonaClear = func(ctx context.Context, userID string) error {
		return mercadona.UnlinkAccount(ctx, mdb, userID)
	}

	mcpSrv := &mcp.Server{
		Name:      "takan",
		PublicURL: cfg.PublicURL,
		Resolve: func(ctx context.Context, bearer string) (string, error) {
			// OAuth access tokens only (no long-lived static API keys).
			u, err := st.UserByAccessToken(ctx, bearer)
			if err != nil {
				return "", err
			}
			return u.ID, nil
		},
		ToolsFor: prov.ToolsFor,
	}

	oauthSrv := &oauth.Server{
		Store:            st,
		PublicURL:        cfg.PublicURL,
		UserFromSession:  webSrv.CurrentUser,
		CreateSession:    webSrv.CreateWebSession,
		SetSessionCookie: webSrv.SetSessionCookie,
	}

	mux := http.NewServeMux()
	webSrv.Routes(mux)
	oauthSrv.Routes(mux)
	mux.HandleFunc("POST /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("GET /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("DELETE /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("OPTIONS /mcp", mcpSrv.HandleHTTP)
	mux.HandleFunc("GET /agent/ws", hub.HandleWS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /install.sh", serveInstallSh(cfg.PublicURL))
	// Prebuilt agents (placed next to binary at deploy time)
	mux.Handle("GET /download/", http.StripPrefix("/download/", http.FileServer(http.Dir(agentBinDir()))))

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	log.Printf("takan listening on %s public=%s", cfg.Listen, cfg.PublicURL)
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

func serveInstallSh(publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		script := `#!/usr/bin/env bash
set -euo pipefail
# Takan agent installer — only the agent token is required.
#   curl -fsSL ` + publicURL + `/install.sh | bash -s -- <token>
TOKEN="${TAKAN_AGENT_TOKEN:-}"
NAME="${TAKAN_AGENT_NAME:-}"
URL="${TAKAN_URL:-` + publicURL + `}"
# Positional: bash -s -- <token> [--name mac]
if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
  TOKEN="$1"
  shift
fi
while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --url) URL="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -z "$NAME" ]; then
  NAME="$(hostname -s 2>/dev/null || echo machine)"
fi
if [ -z "$TOKEN" ]; then
  echo "usage: curl -fsSL $URL/install.sh | bash -s -- <agent-token>" >&2
  exit 1
fi

BIN_DIR="${HOME}/.local/bin"
mkdir -p "$BIN_DIR"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac
TMP=$(mktemp)
curl -fsSL "$URL/download/takan-agent-${OS}-${ARCH}" -o "$TMP" || {
  echo "download failed — place takan-agent in $BIN_DIR manually" >&2
  exit 1
}
chmod +x "$TMP"
mv "$TMP" "$BIN_DIR/takan-agent"
# launchd (mac) or systemd user
if [ "$(uname -s)" = "Darwin" ]; then
  PLIST="$HOME/Library/LaunchAgents/com.takan.agent.plist"
  mkdir -p "$(dirname "$PLIST")"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.takan.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN_DIR/takan-agent</string>
    <string>--url</string><string>$URL</string>
    <string>--token</string><string>$TOKEN</string>
    <string>--name</string><string>$NAME</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/.takan/agent.log</string>
  <key>StandardErrorPath</key><string>$HOME/.takan/agent.log</string>
</dict></plist>
EOF
  mkdir -p "$HOME/.takan"
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "takan-agent loaded (launchd). log: ~/.takan/agent.log"
else
  mkdir -p "$HOME/.config/takan" "$HOME/.config/systemd/user"
  cat > "$HOME/.config/takan/agent.env" <<EOF
TAKAN_URL=$URL
TAKAN_AGENT_TOKEN=$TOKEN
TAKAN_AGENT_NAME=$NAME
EOF
  cat > "$HOME/.config/systemd/user/takan-agent.service" <<EOF
[Unit]
Description=Takan machine agent
After=network-online.target
[Service]
EnvironmentFile=%h/.config/takan/agent.env
ExecStart=%h/.local/bin/takan-agent --url \${TAKAN_URL} --token \${TAKAN_AGENT_TOKEN} --name \${TAKAN_AGENT_NAME}
Restart=always
RestartSec=5
[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now takan-agent
  echo "takan-agent started (systemd user)"
fi
`
		// strip windows line endings if any
		_, _ = w.Write([]byte(strings.ReplaceAll(script, "\r\n", "\n")))
	}
}
