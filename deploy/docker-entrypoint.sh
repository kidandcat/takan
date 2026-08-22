#!/bin/sh
# Persist a session key on the data volume when the operator did not set one.
# The key encrypts vault / Mercadona / email / telegram / SIP — do not lose the volume.
set -eu
DATA="${TAKAN_DATA_DIR:-/data}"
mkdir -p "$DATA"
KEY_FILE="$DATA/.session_key"

if [ -z "${TAKAN_SESSION_KEY:-}" ]; then
	if [ -f "$KEY_FILE" ]; then
		TAKAN_SESSION_KEY=$(tr -d '\n' < "$KEY_FILE")
	else
		TAKAN_SESSION_KEY=$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
		umask 077
		printf '%s\n' "$TAKAN_SESSION_KEY" > "$KEY_FILE"
		echo "takan: generated TAKAN_SESSION_KEY → $KEY_FILE (keep this volume)" >&2
	fi
	export TAKAN_SESSION_KEY
fi

export TAKAN_LISTEN="${TAKAN_LISTEN:-0.0.0.0:8090}"
export TAKAN_DATA_DIR="$DATA"
export TAKAN_AGENT_BIN_DIR="${TAKAN_AGENT_BIN_DIR:-/opt/takan/agents}"
export TAKAN_PUBLIC_URL="${TAKAN_PUBLIC_URL:-http://localhost:8090}"

exec /usr/local/bin/takan "$@"
