package bots

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
)

// ProvisionTimeout bounds one provision run on the target machine.
const ProvisionTimeout = 4 * time.Minute

// DefaultBotBinDir is where the hub keeps bot daemon binaries, named
// <name>-<os>-<arch> (e.g. atlas-linux-amd64). Override with TAKAN_BOT_BIN_DIR.
const DefaultBotBinDir = "/opt/takan/bot-binaries"

// BinaryName is the daemon shipped by provisioning. v1 installs Atlas; the
// binary directory is keyed by this name so other daemons can be added later.
const BinaryName = "atlas"

// UnitMarker is stamped into every unit provisioning writes. Its absence in an
// existing unit of the same name means a human installed that service, so
// provisioning refuses rather than overwriting someone's working daemon.
const UnitMarker = "# managed-by: takan-bots"

// TokenResolver hands back the clear Telegram credential of a channel.
// Implemented by the telegram module, which owns the sealing key.
type TokenResolver func(ctx context.Context, c *store.TelegramChannel) (string, error)

// Provisioner installs bot daemons on machines through their takan-agent.
//
// Transport: the agent's existing `bash` command. That is deliberate — every
// agent already in the field supports it, so provisioning needs no agent
// update, and the script is a fixed server-side template (the panel never
// supplies shell). Secrets are NOT passed on the command line: the script
// fetches them from the hub with a short-lived ticket, so the Telegram token
// never appears in the target machine's process list.
type Provisioner struct {
	Store     *store.Store
	Hub       *agenthub.Hub
	PublicURL string
	// Token unseals a channel credential at dispatch time.
	Token TokenResolver
	// Notify optional: operator notification on success/failure.
	Notify Notifier
	// BinDir overrides DefaultBotBinDir.
	BinDir string
	// Box unseals the runtime bundle (grok CLI credentials + daemon config).
	// Without it the bundle endpoint 404s and provisioning still installs a
	// daemon, it just has no brain.
	Box *cryptox.Box
}

func (p *Provisioner) binDir() string {
	if d := strings.TrimSpace(p.BinDir); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("TAKAN_BOT_BIN_DIR")); d != "" {
		return d
	}
	return DefaultBotBinDir
}

// Start kicks off a provision run in the background and returns immediately.
// Progress is visible in the panel through the bot's provision status.
func (p *Provisioner) Start(userID, botID string) {
	if p == nil || p.Store == nil {
		return
	}
	ctx := context.Background()
	if err := p.Store.SetBotProvisionState(ctx, botID, store.ProvisionQueued, ""); err != nil {
		log.Printf("bots: queue provision %s: %v", botID, err)
		return
	}
	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), ProvisionTimeout+time.Minute)
		defer cancel()
		if err := p.run(runCtx, userID, botID); err != nil {
			log.Printf("bots: provision %s failed: %v", botID, err)
		}
	}()
}

func (p *Provisioner) run(ctx context.Context, userID, botID string) error {
	bot, err := p.Store.BotByID(ctx, userID, botID)
	if err != nil {
		return err
	}
	fail := func(msg string) error {
		_ = p.Store.SetBotProvisionState(ctx, botID, store.ProvisionFailed, msg)
		_ = p.Store.RevokeProvisionTickets(ctx, botID)
		p.notify(ctx, bot, false, msg)
		return fmt.Errorf("%s", msg)
	}

	if bot.MachineName == "" {
		return fail("no target machine set for this bot")
	}
	channel, chatID, err := p.Store.ChannelForConsumer(ctx, userID, store.ConsumerBot, botID, store.DirectionReceive)
	if err != nil || channel == nil {
		return fail("bot is not attached to a telegram channel")
	}
	if chatID == "" {
		return fail(fmt.Sprintf("channel %q has no chats — add the owner or group chat first", channel.Name))
	}
	if p.Token == nil {
		return fail("no credential resolver configured on the hub")
	}
	// Unsealed only to be handed to the target machine over the ticketed fetch.
	if _, err := p.Token(ctx, channel); err != nil {
		return fail("channel credential unavailable: " + err.Error())
	}

	// The daemon authenticates to the hub with its own bot token; minting it here
	// means the operator never copies a token by hand.
	if _, err := p.Store.IssueBotToken(ctx, userID, botID); err != nil {
		return fail("could not mint the bot hub token: " + err.Error())
	}
	ticket, err := p.Store.IssueProvisionTicket(ctx, botID, userID, bot.MachineName)
	if err != nil {
		return fail("could not mint a provision ticket: " + err.Error())
	}

	if err := p.Store.SetBotProvisionState(ctx, botID, store.ProvisionRunning, ""); err != nil {
		return err
	}
	script := p.script(bot.Instance, ticket)
	res, err := p.Hub.RunBash(ctx, userID, bot.MachineName, script, ProvisionTimeout)
	_ = p.Store.RevokeProvisionTickets(ctx, botID)
	if err != nil {
		return fail("agent: " + err.Error())
	}
	if res.Error != "" {
		return fail("agent: " + res.Error)
	}
	if res.ExitCode != 0 {
		return fail(fmt.Sprintf("exit %d: %s", res.ExitCode, tailLine(res.Stderr, res.Stdout)))
	}
	out := tailLine(res.Stdout, "")
	state := store.ProvisionOK
	if strings.Contains(res.Stdout, "adopted existing unit") {
		state = store.ProvisionAdopted
	}
	if err := p.Store.SetBotProvisionState(ctx, botID, state, ""); err != nil {
		return err
	}
	p.notify(ctx, bot, true, out)
	return nil
}

func (p *Provisioner) notify(ctx context.Context, bot *store.Bot, ok bool, detail string) {
	if p.Notify == nil || bot == nil {
		return
	}
	head := fmt.Sprintf("Takan · bot %s", bot.Name)
	if ok {
		head += fmt.Sprintf("\nProvisioned on %s (%s.service running)", bot.MachineName, bot.Instance)
	} else {
		head += fmt.Sprintf("\nProvisioning FAILED on %s", bot.MachineName)
	}
	if d := strings.TrimSpace(detail); d != "" {
		head += "\n\n" + truncate(d, 400)
	}
	_ = p.Notify(ctx, bot.UserID, head) // safe-ignore: operator notification is best-effort and must not fail the caller
}

// tailLine returns the last meaningful line of the preferred stream.
func tailLine(primary, fallback string) string {
	pick := strings.TrimSpace(primary)
	if pick == "" {
		pick = strings.TrimSpace(fallback)
	}
	lines := strings.Split(pick, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return truncate(l, 400)
		}
	}
	return ""
}

// safeInstance guards the values interpolated into the script template.
var safeInstance = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// script renders the fixed provisioning template.
//
// Only non-secret values are interpolated (instance name, hub URL) plus the
// short-lived ticket. Everything sensitive is fetched over HTTPS and piped
// straight to disk, so it never reaches argv or the agent's logs.
func (p *Provisioner) script(instance, ticket string) string {
	hub := strings.TrimSuffix(p.PublicURL, "/")
	return `set -euo pipefail
INSTANCE=` + shellQuote(instance) + `
HUB=` + shellQuote(hub) + `
TICKET=` + shellQuote(ticket) + `

if ! command -v systemctl >/dev/null 2>&1; then
  echo "takan-provision: this machine has no systemd; bot provisioning supports Linux/systemd hosts only" >&2
  exit 78
fi
for tool in curl tar; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "takan-provision: $tool is required on the target machine" >&2
    exit 78
  fi
done

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  SUDO="sudo -n"
else
  echo "takan-provision: takan-agent runs as $(id -un) without passwordless sudo; cannot install a system unit" >&2
  exit 77
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "takan-provision: unsupported architecture $(uname -m)" >&2; exit 78 ;;
esac

UNIT=/etc/systemd/system/$INSTANCE.service
ENVDIR=/etc/$INSTANCE
ENVFILE=$ENVDIR/$INSTANCE.env
BIN=/usr/local/bin/$INSTANCE
SVCHOME=` + shellQuote(DefaultServiceHome) + `
GROKHOME=$SVCHOME/.grok
DATADIR=/var/lib/$INSTANCE
TMPBIN="$(mktemp)"
BUNDLEDIR="$(mktemp -d)"
trap 'rm -f "$TMPBIN"; rm -rf "$BUNDLEDIR"' EXIT

# A unit we did not write belongs to the operator: adopt it instead of
# replacing it. Adoption only adds an environment drop-in, so the original unit,
# its ExecStart, its user and its binary are all left exactly as they are.
#
# This guard runs BEFORE anything is written, because the runtime bundle below
# writes into the service user's home and data directory: on an adopted unit
# those belong to the operator, and re-owning them is exactly the incident
# TAKAN_BOTS.md section 9 exists to prevent.
MODE=install
if [ -e "$UNIT" ] && ! grep -qF ` + shellQuote(UnitMarker) + ` "$UNIT" 2>/dev/null; then
  MODE=adopt
fi

umask 077
$SUDO mkdir -p "$ENVDIR"

# Secrets travel in the response body, never on a command line.
curl -fsS --max-time 60 -H "Authorization: Bearer $TICKET" \
  "$HUB/api/bots/provision/env?mode=$MODE" | $SUDO tee "$ENVFILE" >/dev/null
$SUDO chmod 600 "$ENVFILE"

if [ "$MODE" = install ]; then
  curl -fsS --max-time 180 -H "Authorization: Bearer $TICKET" \
    "$HUB/api/bots/binary?os=linux&arch=$ARCH" -o "$TMPBIN"
  if [ ! -s "$TMPBIN" ]; then
    echo "takan-provision: hub returned an empty binary" >&2
    exit 1
  fi
  $SUDO install -m 0755 "$TMPBIN" "$BIN"
  # The daemon dispatches on argv[0] for its helper CLIs, and the workspace
  # guide tells the agent to call them by name.
  for helper in send sched task; do
    $SUDO ln -sf "$BIN" "/usr/local/bin/$INSTANCE-$helper"
  done
  $SUDO install -d -m 0700 "$DATADIR"
  $SUDO install -d -m 0755 "$DATADIR/workspace"

  # --- runtime bundle: the daemon's brain ---
  # grok CLI + its subscription credentials + the base daemon config, fetched
  # over the same run-scoped ticket. Absent bundle is not fatal: the daemon
  # installs and runs, it just cannot answer until one is imported.
  if curl -fsS --max-time 120 -H "Authorization: Bearer $TICKET" \
      "$HUB/api/bots/provision/bundle" -o "$BUNDLEDIR/bundle.tar"; then
    tar -xf "$BUNDLEDIR/bundle.tar" -C "$BUNDLEDIR"
    $SUDO install -d -m 0700 "$GROKHOME"

    # The CLI is installed into the SERVICE USER's home, even when the machine
    # already has one elsewhere: a host wrapper typically pins HOME at a human's
    # account, so a shared grok would read that human's credentials and, run as
    # root, re-own them (section 9). A self-contained brain avoids both.
    if [ ! -x "$GROKHOME/bin/grok" ]; then
      GROKVER=""
      if [ -f "$BUNDLEDIR/grok-version" ]; then
        GROKVER="$(tr -d '[:space:]' < "$BUNDLEDIR/grok-version")"
      fi
      INSTALLER="$BUNDLEDIR/grok-install.sh"
      if curl -fsSL --max-time 120 https://x.ai/cli/install.sh -o "$INSTALLER"; then
        # Pinned version first (the same build the bundle came from), latest as
        # the fallback when that version is no longer published.
        if [ -n "$GROKVER" ] && $SUDO env HOME="$SVCHOME" bash "$INSTALLER" "$GROKVER" >/dev/null 2>&1; then
          :
        elif $SUDO env HOME="$SVCHOME" bash "$INSTALLER" >/dev/null 2>&1; then
          :
        else
          echo "takan-provision: grok CLI install failed; $INSTANCE will run but cannot answer" >&2
        fi
      else
        echo "takan-provision: could not download the grok installer" >&2
      fi
    fi
    if [ ! -e /usr/local/bin/grok ] && [ -x "$GROKHOME/bin/grok" ]; then
      $SUDO tee /usr/local/bin/grok >/dev/null <<GROKEOF
#!/bin/bash
` + UnitMarker + `
# grok wrapper for the $INSTANCE service user. That user is root here (the
# generated unit sets no User=), so there is nobody to drop privileges to; on a
# host where grok belongs to a human, this file is never written.
export HOME=$SVCHOME
export XDG_CONFIG_HOME=$SVCHOME/.config
export PATH="$GROKHOME/bin:\$PATH"
exec $GROKHOME/bin/grok "\$@"
GROKEOF
      $SUDO chmod 0755 /usr/local/bin/grok
    fi

    # Credentials are refreshed on every run; the operator-tunable files are
    # only seeded, so a local edit survives a re-provision.
    if [ -f "$BUNDLEDIR/grok/auth.json" ]; then
      $SUDO install -m 0600 "$BUNDLEDIR/grok/auth.json" "$GROKHOME/auth.json"
    fi
    if [ -f "$BUNDLEDIR/grok/config.toml" ]; then
      $SUDO install -m 0600 "$BUNDLEDIR/grok/config.toml" "$GROKHOME/config.toml"
    fi
    if [ -f "$BUNDLEDIR/data/config.toml" ] && [ ! -f "$DATADIR/config.toml" ]; then
      $SUDO install -m 0600 "$BUNDLEDIR/data/config.toml" "$DATADIR/config.toml"
    fi
    if [ -f "$BUNDLEDIR/data/workspace/AGENTS.md" ] && [ ! -f "$DATADIR/workspace/AGENTS.md" ]; then
      $SUDO install -m 0644 "$BUNDLEDIR/data/workspace/AGENTS.md" "$DATADIR/workspace/AGENTS.md"
    fi
  else
    echo "takan-provision: no runtime bundle on the hub; $INSTANCE will run without grok credentials" >&2
  fi

  $SUDO tee "$UNIT" >/dev/null <<UNITEOF
` + UnitMarker + `
[Unit]
Description=Takan bot instance $INSTANCE
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENVFILE
Environment=HOME=$SVCHOME
Environment=PATH=$GROKHOME/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
WorkingDirectory=$DATADIR
ExecStart=$BIN
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
MemoryMax=1G
MemorySwapMax=0
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNITEOF
  $SUDO systemctl enable "$INSTANCE.service" >/dev/null 2>&1 || true
else
  # Adoption: the operator owns the unit. Add only an environment drop-in, so
  # ExecStart, User and the installed binary stay exactly as they were. The
  # leading "-" makes the file optional, so removing it cannot brick the unit.
  $SUDO mkdir -p "$UNIT.d"
  $SUDO tee "$UNIT.d/takan.conf" >/dev/null <<DROPEOF
` + UnitMarker + `
[Service]
EnvironmentFile=-$ENVFILE
DROPEOF
fi

$SUDO systemctl daemon-reload
$SUDO systemctl restart "$INSTANCE.service"
sleep 2
$SUDO systemctl is-active "$INSTANCE.service" >/dev/null 2>&1 || {
  # Dump context first, then the summary line: the hub surfaces the LAST stderr
  # line in the panel, so it must be the explanation rather than status noise.
  $SUDO journalctl -u "$INSTANCE.service" -n 20 --no-pager >&2 2>/dev/null || true
  LASTLOG="$($SUDO journalctl -u "$INSTANCE.service" -n 20 --no-pager 2>/dev/null \
    | grep -v '^-- ' | grep -oE '[a-z]+: (fatal|error):.*' | tail -1 || true)"
  if [ -n "$LASTLOG" ]; then
    echo "$INSTANCE.service installed but exited: $LASTLOG" >&2
  else
    echo "$INSTANCE.service installed but did not stay active" >&2
  fi
  exit 1
}
if [ "$MODE" = adopt ]; then
  echo "takan-provision: adopted existing unit $INSTANCE.service on $(hostname -s 2>/dev/null || echo machine); env drop-in installed, unit file untouched"
else
  echo "takan-provision: $INSTANCE.service installed and active on $(hostname -s 2>/dev/null || echo machine)"
fi
`
}

// shellQuote renders a value as a single-quoted shell literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- provisioning HTTP endpoints ---

// authTicket resolves the short-lived provision ticket on the Authorization header.
func (s *Server) authTicket(r *http.Request) (*store.Bot, bool) {
	raw := bearer(r)
	if raw == "" {
		return nil, false
	}
	b, err := s.Store.BotByProvisionTicket(r.Context(), raw)
	if err != nil || b == nil {
		return nil, false
	}
	return b, true
}

func bearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// provisionEnv returns the daemon env file for a provision run in progress.
// Ticket-authenticated; the body is the only place the Telegram token appears.
func (s *Server) provisionEnv(w http.ResponseWriter, r *http.Request) {
	bot, ok := s.authTicket(r)
	if !ok {
		s.writeErr(w, http.StatusUnauthorized, "invalid or expired provision ticket")
		return
	}
	if s.Provision == nil || s.Provision.Token == nil {
		s.writeErr(w, http.StatusInternalServerError, "provisioning is not configured on this hub")
		return
	}
	channel, chatID, err := s.Store.ChannelForConsumer(r.Context(), bot.UserID,
		store.ConsumerBot, bot.ID, store.DirectionReceive)
	if err != nil || channel == nil {
		s.writeErr(w, http.StatusConflict, "bot is not attached to a telegram channel")
		return
	}
	token, err := s.Provision.Token(r.Context(), channel)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "channel credential unavailable")
		return
	}
	hubToken, err := s.Store.IssueBotToken(r.Context(), bot.UserID, bot.ID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "could not mint the hub token")
		return
	}
	hub := strings.TrimSuffix(s.PublicURL, "/")

	// The four variables the daemon has always read (see TAKAN_BOTS.md §5).
	var b strings.Builder
	b.WriteString("# Generated by Takan. Do not edit; re-provision from the panel instead.\n")
	writeEnv(&b, "TELEGRAM_BOT_TOKEN", token)
	writeEnv(&b, "ALLOWED_CHAT_ID", channel.PrimaryChat(chatID))
	writeEnv(&b, "TAKAN_HUB_URL", hub)
	writeEnv(&b, "TAKAN_BOT_TOKEN", hubToken)

	// The data directory is only ours to set when Takan wrote the unit:
	// repointing an adopted daemon's data dir would orphan its state.
	if r.URL.Query().Get("mode") != "adopt" {
		writeEnv(&b, "ATLAS_DATA_DIR", BundleDataDir(bot.Instance))
	}
	// GROQ_API_KEY (voice transcription) rides the runtime bundle and is safe
	// to hand an adopted daemon too — it adds a capability, it moves nothing.
	if bundle, err := s.bundleFor(r.Context(), bot.UserID); err == nil && bundle != nil && bundle.GroqAPIKey != "" {
		writeEnv(&b, "GROQ_API_KEY", bundle.GroqAPIKey)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String())) // safe-ignore: response already committed; the client is gone
}

// bundleFor unseals the account's runtime bundle, or returns (nil, nil) when
// there is none (or no key configured) — provisioning still works without it.
func (s *Server) bundleFor(ctx context.Context, userID string) (*Bundle, error) {
	if s.Provision == nil || s.Provision.Box == nil {
		return nil, nil
	}
	row, err := s.Store.RuntimeBundle(ctx, userID)
	if err != nil || row == nil {
		return nil, err
	}
	return OpenBundle(s.Provision.Box, row.PayloadEnc)
}

// provisionBundle serves the runtime bundle as a tar for one provision run.
// Ticket-authenticated like the env endpoint; the body is the only place the
// grok credentials appear, so they never reach argv on the target machine.
func (s *Server) provisionBundle(w http.ResponseWriter, r *http.Request) {
	bot, ok := s.authTicket(r)
	if !ok {
		s.writeErr(w, http.StatusUnauthorized, "invalid or expired provision ticket")
		return
	}
	bundle, err := s.bundleFor(r.Context(), bot.UserID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "runtime bundle could not be unsealed")
		return
	}
	if bundle == nil {
		s.writeErr(w, http.StatusNotFound, "no runtime bundle imported on this hub")
		return
	}
	body, err := bundle.Tar(bot.Name, bot.Instance, BundleDataDir(bot.Instance))
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "could not render the runtime bundle")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body) // safe-ignore: response already committed; the client is gone
}

// writeEnv emits a systemd EnvironmentFile line with a quoted value.
func writeEnv(b *strings.Builder, key, value string) {
	value = strings.NewReplacer("\n", "", "\r", "", `"`, "").Replace(value)
	fmt.Fprintf(b, "%s=\"%s\"\n", key, value)
}

// serveBinary hands the bot daemon binary to a provision run or an agent.
//
// Auth accepts a machine agent token or a provision ticket, so the same
// endpoint works for zero-touch provisioning and for manual agent-side use.
func (s *Server) serveBinary(w http.ResponseWriter, r *http.Request) {
	raw := bearer(r)
	if raw == "" {
		s.writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	authorized := false
	if _, err := s.Store.BotByProvisionTicket(r.Context(), raw); err == nil {
		authorized = true
	} else if _, err := s.Store.MachineByAgentToken(r.Context(), raw); err == nil {
		authorized = true
	}
	if !authorized {
		s.writeErr(w, http.StatusUnauthorized, "invalid agent token or provision ticket")
		return
	}
	if s.Provision == nil {
		s.writeErr(w, http.StatusInternalServerError, "provisioning is not configured on this hub")
		return
	}
	goos := sanitizeSlug(r.URL.Query().Get("os"), "linux")
	arch := sanitizeSlug(r.URL.Query().Get("arch"), "amd64")
	name := sanitizeSlug(r.URL.Query().Get("name"), BinaryName)
	path := filepath.Join(s.Provision.binDir(), fmt.Sprintf("%s-%s-%s", name, goos, arch))
	f, err := os.Open(path)
	if err != nil {
		s.writeErr(w, http.StatusNotFound,
			fmt.Sprintf("no %s binary for %s/%s on this hub — upload it to %s", name, goos, arch, s.Provision.binDir()))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "cannot stat binary")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

// sanitizeSlug keeps path components to a safe alphabet.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func sanitizeSlug(v, def string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || !slugRe.MatchString(v) {
		return def
	}
	return v
}
