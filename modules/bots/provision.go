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
if ! command -v curl >/dev/null 2>&1; then
  echo "takan-provision: curl is required on the target machine" >&2
  exit 78
fi

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
TMPBIN="$(mktemp)"
trap 'rm -f "$TMPBIN"' EXIT

# A unit we did not write belongs to the operator: adopt it instead of
# replacing it. Adoption only adds an environment drop-in, so the original unit,
# its ExecStart, its user and its binary are all left exactly as they are.
MODE=install
if [ -e "$UNIT" ] && ! grep -qF ` + shellQuote(UnitMarker) + ` "$UNIT" 2>/dev/null; then
  MODE=adopt
fi

umask 077
$SUDO mkdir -p "$ENVDIR"

# Secrets travel in the response body, never on a command line.
curl -fsS --max-time 60 -H "Authorization: Bearer $TICKET" \
  "$HUB/api/bots/provision/env" | $SUDO tee "$ENVFILE" >/dev/null
$SUDO chmod 600 "$ENVFILE"

if [ "$MODE" = install ]; then
  curl -fsS --max-time 180 -H "Authorization: Bearer $TICKET" \
    "$HUB/api/bots/binary?os=linux&arch=$ARCH" -o "$TMPBIN"
  if [ ! -s "$TMPBIN" ]; then
    echo "takan-provision: hub returned an empty binary" >&2
    exit 1
  fi
  $SUDO install -m 0755 "$TMPBIN" "$BIN"

  $SUDO tee "$UNIT" >/dev/null <<UNITEOF
` + UnitMarker + `
[Unit]
Description=Takan bot instance $INSTANCE
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENVFILE
ExecStart=$BIN
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
MemoryMax=512M
MemorySwapMax=0

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

	// Exactly the four variables the daemon reads (see TAKAN_BOTS.md).
	var b strings.Builder
	b.WriteString("# Generated by Takan. Do not edit; re-provision from the panel instead.\n")
	writeEnv(&b, "TELEGRAM_BOT_TOKEN", token)
	writeEnv(&b, "ALLOWED_CHAT_ID", channel.PrimaryChat(chatID))
	writeEnv(&b, "TAKAN_HUB_URL", hub)
	writeEnv(&b, "TAKAN_BOT_TOKEN", hubToken)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String())) // safe-ignore: response already committed; the client is gone
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
