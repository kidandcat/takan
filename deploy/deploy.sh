#!/usr/bin/env bash
#
# Deploy the hub.
#
# Cross-compiles on the Mac (the VPS has no Go toolchain), ships the binary,
# installs the unit and the atlas-send / atlas-sched / atlas-task symlinks, then
# restarts and health-checks both listeners.
#
# It will NOT restart while a conversation is running: a restart mid-turn kills
# a live agent and loses the answer.
#
# Usage: deploy/deploy.sh [ssh-host]   (default host: vps2)

set -euo pipefail

HOST="${1:-vps2}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$REPO_DIR"

echo "==> Testing"
go build ./... && go vet ./... && go test ./... >/dev/null

echo "==> Building for linux/amd64"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/takan-linux-amd64 ./cmd/takan
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/takan-agent-linux-amd64 ./cmd/takan-agent

echo "==> Uploading to $HOST"
scp -q /tmp/takan-linux-amd64 "$HOST:/tmp/takan"
scp -q /tmp/takan-agent-linux-amd64 "$HOST:/tmp/takan-agent-linux-amd64"
scp -q deploy/takan.service "$HOST:/tmp/takan.service"

# The in-flight check has to recognise the agent process, and the command is a
# panel setting rather than a constant. Read it from the hub's database; fall
# back to the default when the assistant has never been configured.
AGENT_QUERY="SELECT COALESCE(json_extract(config_json, '\$.agent.command'), '') FROM user_modules WHERE module_id = 'assistant' LIMIT 1;"
AGENT_CMD=$(ssh "$HOST" "sqlite3 /opt/takan/data/default.db \"$AGENT_QUERY\" 2>/dev/null" || true)
AGENT_CMD=${AGENT_CMD:-grok}
echo "==> Agent command: $AGENT_CMD"

# Likewise the hub's port: it is an env-file setting, not a constant (vps2 runs
# on 8096), so a hardcoded health check fails a deploy that actually worked.
# Read it from the target's env file with the same precedence as the binary —
# ATLAS_LISTEN wins over the deprecated TAKAN_LISTEN.
LISTEN_LINES=$(ssh "$HOST" "sudo grep -hE '^[[:space:]]*(ATLAS|TAKAN)_LISTEN=' /etc/takan/takan.env 2>/dev/null" || true)
HUB_LISTEN=$(printf '%s\n' "$LISTEN_LINES" | grep -E '^[[:space:]]*ATLAS_LISTEN=' | tail -1 | cut -d= -f2- || true)
if [ -z "$HUB_LISTEN" ]; then
  HUB_LISTEN=$(printf '%s\n' "$LISTEN_LINES" | grep -E '^[[:space:]]*TAKAN_LISTEN=' | tail -1 | cut -d= -f2- || true)
fi
# Strip the host and any quoting; what is left must be a bare port number.
HUB_PORT=${HUB_LISTEN##*:}
HUB_PORT=${HUB_PORT//[^0-9]/}
HUB_PORT=${HUB_PORT:-8090}
echo "==> Hub port: $HUB_PORT"

echo "==> Installing on $HOST"
ssh "$HOST" AGENT_CMD="$AGENT_CMD" bash -s <<'EOF'
set -euo pipefail
AGENT_CMD=${AGENT_CMD:-grok}

BIN_PATH=/opt/takan/takan
AGENT_DIR=/opt/takan/agents

sudo mkdir -p /opt/takan/data "$AGENT_DIR"

# Keep the previous binary so a rollback is a copy, not a rebuild.
if [ -f "$BIN_PATH" ]; then
  sudo cp -a "$BIN_PATH" "$BIN_PATH.bak.$(date +%s)"
fi
sudo install -o root -g root -m 0755 /tmp/takan "$BIN_PATH"
sudo install -o root -g root -m 0755 /tmp/takan-agent-linux-amd64 "$AGENT_DIR/takan-agent-linux-amd64"

# The helper CLIs are the same binary; it dispatches on argv[0].
sudo ln -sf "$BIN_PATH" /usr/local/bin/atlas-send
sudo ln -sf "$BIN_PATH" /usr/local/bin/atlas-sched
sudo ln -sf "$BIN_PATH" /usr/local/bin/atlas-task

sudo install -o root -g root -m 0644 /tmp/takan.service /etc/systemd/system/takan.service
# The old 512M drop-in would OOM-kill every agent run now that they share this
# cgroup. The unit sets 2G; remove the override that would win over it.
sudo rm -f /etc/systemd/system/takan.service.d/memory.conf
sudo systemctl daemon-reload
sudo systemctl enable takan.service

# Do not kill an in-flight conversation. Walk the descendants of the hub looking
# for an agent whose cwd is the live workspace (not tasks/ or routines/, which
# are detached and survive a restart as orphans).
descendants() {
  local pid=$1 child
  for child in $(pgrep -P "$pid" || true); do
    printf '%s\n' "$child"
    descendants "$child"
  done
}

conversation_agent_running() {
  local hub_pid comm cwd
  hub_pid=$(systemctl show -p MainPID --value takan.service)
  if [[ -z "$hub_pid" || "$hub_pid" == "0" ]]; then
    return 1
  fi
  while read -r pid; do
    [[ -z "$pid" ]] && continue
    comm=$(ps -o comm= -p "$pid" 2>/dev/null | tr -d ' ')
    cwd=$(readlink -f "/proc/$pid/cwd" 2>/dev/null || true)
    if [[ "$comm" == "$AGENT_CMD"* && "$cwd" != *"/tasks/"* && "$cwd" != *"/routines/"* ]]; then
      return 0
    fi
  done < <(descendants "$hub_pid")
  return 1
}

if conversation_agent_running; then
  echo "waiting for the in-flight conversation to finish before restarting"
  deadline=$((SECONDS + 360))
  while conversation_agent_running; do
    if (( SECONDS >= deadline )); then
      echo "refusing to restart: a conversation agent is still running" >&2
      exit 1
    fi
    sleep 5
  done
fi

sudo systemctl restart takan.service
rm -f /tmp/takan /tmp/takan.service /tmp/takan-agent-linux-amd64
EOF

echo "==> Waiting for the service to come up"
sleep 4
ssh "$HOST" HUB_PORT="$HUB_PORT" bash -s <<'EOF'
set -euo pipefail
HUB_PORT=${HUB_PORT:-8090}
systemctl is-active takan.service
echo "--- panel ---"
curl -fsS "localhost:$HUB_PORT/healthz"
echo "--- assistant (loopback) ---"
curl -fsS localhost:8099/health && echo
echo "--- app channel ---"
curl -fsS "localhost:$HUB_PORT/v1/health" && echo
echo "--- memory cap (must be >= 2G) ---"
systemctl show -p MemoryMax --value takan.service
EOF

echo "==> Deployed"
