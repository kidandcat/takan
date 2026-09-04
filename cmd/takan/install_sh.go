package main

import (
	"net/http"
	"strings"
)

// serveInstallSh renders the one-liner installer for takan-agent. The public URL
// is a config value, so nothing here hardcodes a domain.
func serveInstallSh(publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		script := `#!/usr/bin/env bash
set -euo pipefail
# Takan agent installer — only the agent token is required.
#   curl -fsSL ` + publicURL + `/install.sh | bash -s -- <token>
# Prefers a system (root) service when root/sudo is available; falls back to user.
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

# Root helper: identity, passwordless sudo, or interactive sudo when a TTY is available.
AS_ROOT=()
if [ "$(id -u)" -eq 0 ]; then
  AS_ROOT=()
  USE_SYSTEM=1
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  AS_ROOT=(sudo)
  USE_SYSTEM=1
elif [ -t 0 ] && command -v sudo >/dev/null 2>&1 && sudo -v 2>/dev/null; then
  AS_ROOT=(sudo)
  USE_SYSTEM=1
else
  USE_SYSTEM=0
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac
TMP=$(mktemp)
curl -fsSL "$URL/download/takan-agent-${OS}-${ARCH}" -o "$TMP" || {
  echo "download failed — cannot fetch takan-agent-${OS}-${ARCH}" >&2
  exit 1
}
chmod +x "$TMP"

if [ "$(uname -s)" = "Darwin" ]; then
  if [ "$USE_SYSTEM" = "1" ]; then
    BIN_DIR=/usr/local/bin
    PLIST=/Library/LaunchDaemons/com.takan.agent.plist
    LOG_DIR=/var/log/takan
    "${AS_ROOT[@]}" mkdir -p "$BIN_DIR" "$LOG_DIR"
    "${AS_ROOT[@]}" mv "$TMP" "$BIN_DIR/takan-agent"
    "${AS_ROOT[@]}" chmod 755 "$BIN_DIR/takan-agent"
    launchctl bootout "gui/$(id -u)/com.takan.agent" 2>/dev/null || true
    launchctl unload "$HOME/Library/LaunchAgents/com.takan.agent.plist" 2>/dev/null || true
    rm -f "$HOME/Library/LaunchAgents/com.takan.agent.plist" 2>/dev/null || true
    "${AS_ROOT[@]}" tee "$PLIST" >/dev/null <<EOF
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
  <key>StandardOutPath</key><string>$LOG_DIR/agent.log</string>
  <key>StandardErrorPath</key><string>$LOG_DIR/agent.log</string>
</dict></plist>
EOF
    "${AS_ROOT[@]}" launchctl bootout system/com.takan.agent 2>/dev/null || true
    "${AS_ROOT[@]}" launchctl unload "$PLIST" 2>/dev/null || true
    "${AS_ROOT[@]}" launchctl load "$PLIST" 2>/dev/null || "${AS_ROOT[@]}" launchctl bootstrap system "$PLIST"
    echo "takan-agent loaded (launchd system). log: $LOG_DIR/agent.log"
  else
    BIN_DIR="${HOME}/.local/bin"
    mkdir -p "$BIN_DIR" "$HOME/.takan"
    mv "$TMP" "$BIN_DIR/takan-agent"
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
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load "$PLIST"
    echo "takan-agent loaded (launchd user). log: ~/.takan/agent.log"
  fi
else
  if [ "$USE_SYSTEM" = "1" ]; then
    BIN_DIR=/usr/local/bin
    ENV_DIR=/etc/takan
    UNIT=/etc/systemd/system/takan-agent.service
    "${AS_ROOT[@]}" mkdir -p "$BIN_DIR" "$ENV_DIR"
    "${AS_ROOT[@]}" mv "$TMP" "$BIN_DIR/takan-agent"
    "${AS_ROOT[@]}" chmod 755 "$BIN_DIR/takan-agent"
    "${AS_ROOT[@]}" tee "$ENV_DIR/agent.env" >/dev/null <<EOF
TAKAN_URL=$URL
TAKAN_AGENT_TOKEN=$TOKEN
TAKAN_AGENT_NAME=$NAME
EOF
    "${AS_ROOT[@]}" chmod 600 "$ENV_DIR/agent.env"
    "${AS_ROOT[@]}" tee "$UNIT" >/dev/null <<'EOF'
[Unit]
Description=Takan machine agent
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
EnvironmentFile=/etc/takan/agent.env
ExecStart=/usr/local/bin/takan-agent --url ${TAKAN_URL} --token ${TAKAN_AGENT_TOKEN} --name ${TAKAN_AGENT_NAME}
Restart=always
RestartSec=5
TimeoutStopSec=10
KillMode=mixed
[Install]
WantedBy=multi-user.target
EOF
    if systemctl --user is-active takan-agent >/dev/null 2>&1; then
      systemctl --user kill -s SIGKILL takan-agent 2>/dev/null || true
      timeout 3 systemctl --user stop takan-agent 2>/dev/null || true
    fi
    systemctl --user disable takan-agent 2>/dev/null || true
    rm -f "$HOME/.config/systemd/user/takan-agent.service" 2>/dev/null || true
    systemctl --user daemon-reload 2>/dev/null || true
    "${AS_ROOT[@]}" systemctl stop takan-agent 2>/dev/null || true
    pkill -9 -x takan-agent 2>/dev/null || true
    "${AS_ROOT[@]}" systemctl daemon-reload
    "${AS_ROOT[@]}" systemctl enable --now takan-agent
    echo "takan-agent started (systemd system)"
  else
    BIN_DIR="${HOME}/.local/bin"
    mkdir -p "$BIN_DIR" "$HOME/.config/takan" "$HOME/.config/systemd/user"
    mv "$TMP" "$BIN_DIR/takan-agent"
    cat > "$HOME/.config/takan/agent.env" <<EOF
TAKAN_URL=$URL
TAKAN_AGENT_TOKEN=$TOKEN
TAKAN_AGENT_NAME=$NAME
EOF
    chmod 600 "$HOME/.config/takan/agent.env"
    cat > "$HOME/.config/systemd/user/takan-agent.service" <<EOF
[Unit]
Description=Takan machine agent
After=network-online.target
[Service]
EnvironmentFile=%h/.config/takan/agent.env
ExecStart=%h/.local/bin/takan-agent --url \${TAKAN_URL} --token \${TAKAN_AGENT_TOKEN} --name \${TAKAN_AGENT_NAME}
Restart=always
RestartSec=5
TimeoutStopSec=10
KillMode=mixed
[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload
    systemctl --user enable --now takan-agent
    if command -v loginctl >/dev/null 2>&1; then
      if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)" != "yes" ]; then
        if command -v sudo >/dev/null 2>&1 && sudo -n loginctl enable-linger "$(id -un)" 2>/dev/null; then
          echo "enabled systemd linger for $(id -un)"
        else
          echo "note: enable linger so the agent survives logout: sudo loginctl enable-linger $(id -un)" >&2
        fi
      fi
    fi
    echo "takan-agent started (systemd user)"
  fi
fi
`
		_, _ = w.Write([]byte(strings.ReplaceAll(script, "\r\n", "\n")))
	}
}
