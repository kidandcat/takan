package bots

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// provFixture wires a bots.Server with provisioning against a temp binary dir.
func provFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	dir := t.TempDir()
	f.srv.PublicURL = "https://takan.test"
	f.srv.Provision = &Provisioner{
		Store:     f.st,
		PublicURL: "https://takan.test",
		BinDir:    dir,
		Token: func(_ context.Context, c *store.TelegramChannel) (string, error) {
			return "telegram-secret-" + c.Name, nil
		},
	}
	return f, dir
}

// attachChannel binds the fixture bot to a fresh channel with one chat.
func attachChannel(t *testing.T, f *fixture, chatID string) *store.TelegramChannel {
	t.Helper()
	c, err := f.st.CreateTelegramChannel(f.ctx(), f.user.ID, "family", "sealed", "family_bot", "Family")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddChannelChat(f.ctx(), f.user.ID, c.ID, chatID, "group", "Casa"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AttachChannel(f.ctx(), f.user.ID, store.ChannelAttachment{
		ChannelID: c.ID, Consumer: store.ConsumerBot, ConsumerID: f.bot.ID,
		Direction: store.DirectionReceive,
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProvisionScriptIsFixedAndCarriesNoSecrets(t *testing.T) {
	f, _ := provFixture(t)
	script := f.srv.Provision.script("test-provision", "ticket-abc123")

	for _, want := range []string{
		"command -v systemctl",                     // refuses non-systemd hosts
		"/etc/systemd/system/$INSTANCE.service",    // fixed unit path
		"$HUB/api/bots/provision/env",              // secrets fetched, not passed
		"$HUB/api/bots/binary?os=linux&arch=$ARCH", // binary from the hub
		"systemctl daemon-reload",
		"systemctl restart",
		"MemorySwapMax=0",
		"exit 78", // clean refusal
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
	// The Telegram credential must never reach the command line.
	if strings.Contains(script, "telegram-secret") || strings.Contains(script, "TELEGRAM_BOT_TOKEN=") {
		t.Fatal("script leaks the telegram credential into argv")
	}
	// Interpolated values are single-quoted shell literals.
	if !strings.Contains(script, "INSTANCE='test-provision'") ||
		!strings.Contains(script, "TICKET='ticket-abc123'") {
		t.Fatalf("values must be quoted: %s", script[:200])
	}
	// A hostile instance name cannot break out of the quoting.
	nasty := f.srv.Provision.script("a'; rm -rf /; echo '", "t")
	if strings.Contains(nasty, "; rm -rf /; echo ") && !strings.Contains(nasty, `'\''`) {
		t.Fatal("quoting failed to neutralise the payload")
	}
}

func TestProvisionEnvEndpoint(t *testing.T) {
	f, _ := provFixture(t)
	attachChannel(t, f, "-1002233445566")

	// No ticket, no secrets.
	if w, _ := f.do(t, "GET", "/api/bots/provision/env", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w, _ := f.do(t, "GET", "/api/bots/provision/env", f.token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("a bot token is not a provision ticket: %d", w.Code)
	}

	ticket, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := f.do(t, "GET", "/api/bots/provision/env", ticket, "")
	if w.Code != http.StatusOK {
		t.Fatalf("env: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`TELEGRAM_BOT_TOKEN="telegram-secret-family"`,
		`ALLOWED_CHAT_ID="-1002233445566"`,
		`TAKAN_HUB_URL="https://takan.test"`,
		"TAKAN_BOT_TOKEN=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("env missing %q:\n%s", want, body)
		}
	}
	// The minted hub token must actually authenticate the daemon.
	var hubTok string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "TAKAN_BOT_TOKEN=") {
			hubTok = strings.Trim(strings.TrimPrefix(line, "TAKAN_BOT_TOKEN="), `"`)
		}
	}
	if hubTok == "" {
		t.Fatal("no hub token issued")
	}
	got, err := f.st.BotByToken(f.ctx(), hubTok)
	if err != nil || got.ID != f.bot.ID {
		t.Fatalf("minted token does not resolve: %v", err)
	}
}

func TestProvisionEnvRequiresChannel(t *testing.T) {
	f, _ := provFixture(t)
	ticket, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	// No channel attachment and no channels at all: nothing to hand over.
	if w, _ := f.do(t, "GET", "/api/bots/provision/env", ticket, ""); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 without a channel, got %d", w.Code)
	}
}

func TestProvisionTicketLifecycle(t *testing.T) {
	f, _ := provFixture(t)
	first, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.BotByProvisionTicket(f.ctx(), first); err != nil {
		t.Fatalf("fresh ticket: %v", err)
	}
	// Issuing again invalidates the previous run's ticket.
	second, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.BotByProvisionTicket(f.ctx(), first); err == nil {
		t.Fatal("superseded ticket still valid")
	}
	// Revoking ends the run's access.
	if err := f.st.RevokeProvisionTickets(f.ctx(), f.bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.BotByProvisionTicket(f.ctx(), second); err == nil {
		t.Fatal("revoked ticket still valid")
	}

	// Expiry is enforced and the row is dropped.
	third, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB().ExecContext(f.ctx(),
		`UPDATE bot_provision_tickets SET expires_at = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.BotByProvisionTicket(f.ctx(), third); err == nil {
		t.Fatal("expired ticket still valid")
	}
}

func TestBinaryEndpointAuthAndLookup(t *testing.T) {
	f, dir := provFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "atlas-linux-amd64"), []byte("ELF-ish payload"), 0o755); err != nil {
		t.Fatal(err)
	}

	if w, _ := f.do(t, "GET", "/api/bots/binary?os=linux&arch=amd64", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w, _ := f.do(t, "GET", "/api/bots/binary?os=linux&arch=amd64", "bogus", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", w.Code)
	}

	// A provision ticket is accepted.
	ticket, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := f.do(t, "GET", "/api/bots/binary?os=linux&arch=amd64", ticket, "")
	if w.Code != http.StatusOK || w.Body.String() != "ELF-ish payload" {
		t.Fatalf("ticket fetch: %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type: %q", ct)
	}

	// A machine agent token is accepted too (the documented contract).
	_, agentTok, err := f.st.CreateMachine(f.ctx(), f.user.ID, "vps3")
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := f.do(t, "GET", "/api/bots/binary?os=linux&arch=amd64", agentTok, ""); w.Code != http.StatusOK {
		t.Fatalf("agent token fetch: %d", w.Code)
	}

	// Missing arch: a clear 404 naming the expected path.
	w, out := f.do(t, "GET", "/api/bots/binary?os=linux&arch=arm64", ticket, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing binary: %d", w.Code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, dir) {
		t.Fatalf("404 should name the directory: %v", out)
	}

	// Path traversal in the query cannot escape the binary directory: the os
	// component is rejected and falls back, and nothing outside dir is served.
	w, out = f.do(t, "GET", "/api/bots/binary?os=../../etc&arch=passwd", ticket, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("traversal attempt should 404, got %d", w.Code)
	}
	msg, _ := out["error"].(string)
	if strings.Contains(msg, "..") || !strings.Contains(msg, "linux") {
		t.Fatalf("os must be sanitised back to linux: %v", out)
	}
}

func TestProvisionStateMachine(t *testing.T) {
	f, _ := provFixture(t)
	// A bot with no machine cannot be provisioned.
	if f.bot.Provisionable() {
		t.Fatal("no machine set, should not be provisionable")
	}
	if err := f.st.SetBotProvisionState(f.ctx(), f.bot.ID, store.ProvisionQueued, ""); err != nil {
		t.Fatal(err)
	}
	b, err := f.st.BotByID(f.ctx(), f.user.ID, f.bot.ID)
	if err != nil || b.ProvisionStatus != store.ProvisionQueued {
		t.Fatalf("queued: %+v %v", b, err)
	}
	if b.ProvisionAt == nil {
		t.Fatal("provision timestamp not recorded")
	}
	if err := f.st.SetBotProvisionState(f.ctx(), f.bot.ID, store.ProvisionFailed, "exit 78: no systemd"); err != nil {
		t.Fatal(err)
	}
	b, _ = f.st.BotByID(f.ctx(), f.user.ID, f.bot.ID)
	if b.ProvisionStatus != store.ProvisionFailed || !strings.Contains(b.ProvisionError, "no systemd") {
		t.Fatalf("failed state: %+v", b)
	}
	// Success clears the previous error.
	if err := f.st.SetBotProvisionState(f.ctx(), f.bot.ID, store.ProvisionOK, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = f.st.BotByID(f.ctx(), f.user.ID, f.bot.ID)
	if b.ProvisionStatus != store.ProvisionOK || b.ProvisionError != "" {
		t.Fatalf("ok state: %+v", b)
	}
}

func TestInstanceNameSanitises(t *testing.T) {
	for in, want := range map[string]string{
		"Atlas":             "atlas",
		"Test Provision":    "test-provision",
		"a//b":              "ab",
		"  Casa  Bot  ":     "casa-bot",
		"weird!!$$chars":    "weirdchars",
		"--leading-trail--": "leading-trail",
	} {
		if got := store.InstanceName(in); got != want {
			t.Fatalf("InstanceName(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ = json.Marshal

func TestProvisionAdoptsUnmanagedUnit(t *testing.T) {
	f, _ := provFixture(t)
	script := f.srv.Provision.script("atlas", "ticket")

	// The generated unit carries the marker, so a re-provision recognises it.
	if !strings.Contains(script, "<<UNITEOF\n"+UnitMarker+"\n[Unit]") {
		t.Fatal("generated unit must start with the takan-bots marker")
	}
	// Adoption must never write the unit or the binary: those belong to whoever
	// installed the daemon by hand.
	for _, guarded := range []string{`$SUDO install -m 0755 "$TMPBIN" "$BIN"`, `$SUDO tee "$UNIT" >/dev/null <<UNITEOF`} {
		at := strings.Index(script, guarded)
		if at < 0 {
			t.Fatalf("missing %q", guarded)
		}
		if at < strings.Index(script, `if [ "$MODE" = install ]; then`) {
			t.Fatalf("%q must sit inside the install-only branch", guarded)
		}
	}
	if !strings.Contains(script, `EnvironmentFile=-$ENVFILE`) {
		t.Fatal("adoption must add an optional EnvironmentFile drop-in")
	}

	// Execute the real mode-selection logic against each case.
	snippet := modeSnippet(t, script)
	run := func(t *testing.T, contents string, exists bool) string {
		t.Helper()
		dir := t.TempDir()
		unit := filepath.Join(dir, "atlas.service")
		if exists {
			if err := os.WriteFile(unit, []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sh := "INSTANCE=atlas\nUNIT=" + unit + "\n" + snippet + "\necho MODE=$MODE\n"
		out, err := exec.Command("bash", "-c", sh).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run(t, "", false); got != "MODE=install" {
		t.Fatalf("a fresh machine installs: %q", got)
	}
	if got := run(t, UnitMarker+"\n[Unit]\nDescription=Takan bot instance atlas\n", true); got != "MODE=install" {
		t.Fatalf("a Takan-managed unit re-provisions in place: %q", got)
	}
	// The shape of the live hand-rolled atlas.service on vps2.
	foreign := "[Unit]\nDescription=Atlas\n\n[Service]\nUser=debian\n" +
		"EnvironmentFile=/home/debian/atlas.env\nWorkingDirectory=/home/debian/atlas-data\n"
	if got := run(t, foreign, true); got != "MODE=adopt" {
		t.Fatalf("a hand-rolled unit must be adopted, not overwritten: %q", got)
	}
}

// modeSnippet extracts the install-vs-adopt decision from the generated script
// so the test exercises the real template text.
func modeSnippet(t *testing.T, script string) string {
	t.Helper()
	const start = "MODE=install"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("mode selection not found in the generated script")
	}
	rest := script[i:]
	j := strings.Index(rest, "\nfi\n")
	if j < 0 {
		t.Fatal("mode selection has no terminator")
	}
	return rest[:j+len("\nfi\n")]
}
