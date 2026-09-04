#!/usr/bin/env bash
#
# Deploy the hub.
#
# Cross-compiles on the Mac (the VPS has no Go toolchain), ships the binary and
# the unit, then restarts and health-checks the panel.
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

# The hub's port is an env-file setting, not a constant (vps2 runs on 8096), so
# a hardcoded health check would fail a deploy that actually worked.
HUB_LISTEN=$(ssh "$HOST" "sudo grep -hE '^[[:space:]]*TAKAN_LISTEN=' /etc/takan/takan.env 2>/dev/null | tail -1 | cut -d= -f2-" || true)
# Strip the host and any quoting; what is left must be a bare port number.
HUB_PORT=${HUB_LISTEN##*:}
HUB_PORT=${HUB_PORT//[^0-9]/}
HUB_PORT=${HUB_PORT:-8090}
echo "==> Hub port: $HUB_PORT"

echo "==> Installing on $HOST"
ssh "$HOST" bash -s <<'EOF'
set -euo pipefail

BIN_PATH=/opt/takan/takan
AGENT_DIR=/opt/takan/agents

sudo mkdir -p /opt/takan/data "$AGENT_DIR"

# Keep the previous binary so a rollback is a copy, not a rebuild.
if [ -f "$BIN_PATH" ]; then
  sudo cp -a "$BIN_PATH" "$BIN_PATH.bak.$(date +%s)"
fi
sudo install -o root -g root -m 0755 /tmp/takan "$BIN_PATH"
sudo install -o root -g root -m 0755 /tmp/takan-agent-linux-amd64 "$AGENT_DIR/takan-agent-linux-amd64"

sudo install -o root -g root -m 0644 /tmp/takan.service /etc/systemd/system/takan.service
sudo systemctl daemon-reload
sudo systemctl enable takan.service
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
echo "--- memory cap ---"
systemctl show -p MemoryMax --value takan.service
EOF

echo "==> Deployed"
