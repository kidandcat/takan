package bots

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
)

// --- source fixture -------------------------------------------------------

// fixtureAgentConfig / fixtureAgentsMD mirror the real vps2 files: the daemon
// config points at its own data dir and the guide names the instance, its
// helper CLIs and its inbox path — every one of which has to be rewritten.
// Like the live vps2 file, this one has no [instance] section at all: the
// daemon falls back to its built-in default there.
const fixtureAgentConfig = `# Atlas configuration.
# Secrets are NOT stored here: they come from %ENV%

[agent]
command = "grok"
workdir = "%DATA%/workspace"
unset_env = ["XAI_API_KEY"]
`

const fixtureAgentsMD = `# Atlas

You are Atlas, Jairo's assistant. Push with ` + "`atlas-send`" + `.
Long jobs: machine_ai_run owner="Atlas".
Inbox lives in %DATA%/inbox.
`

// writeSource lays out a believable grok home + daemon data dir + env file.
func writeSource(t *testing.T) ImportPaths {
	t.Helper()
	root := t.TempDir()
	grokHome := filepath.Join(root, ".grok")
	dataDir := filepath.Join(root, "atlas-data")
	envFile := filepath.Join(root, "atlas.env")
	sub := func(s string) string {
		return strings.NewReplacer("%DATA%", dataDir, "%ENV%", envFile).Replace(s)
	}
	mustWrite(t, filepath.Join(grokHome, "auth.json"), `{"https://auth.x.ai::abc":{"refresh":"r"}}`, 0o600)
	mustWrite(t, filepath.Join(grokHome, "config.toml"), "[mcp_servers.takan]\nurl = \"https://takan.es/mcp\"\n", 0o644)
	mustWrite(t, filepath.Join(grokHome, "version.json"), `{"version":"0.2.118"}`, 0o644)
	mustWrite(t, filepath.Join(dataDir, "config.toml"), sub(fixtureAgentConfig), 0o644)
	mustWrite(t, filepath.Join(dataDir, "workspace", "AGENTS.md"), sub(fixtureAgentsMD), 0o644)
	mustWrite(t, envFile, "TELEGRAM_BOT_TOKEN=\"tg\"\nGROQ_API_KEY=gsk_fixture\n", 0o600)
	return ImportPaths{GrokHome: grokHome, AgentData: dataDir, EnvFile: envFile}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// --- seal / unseal --------------------------------------------------------

func TestBundleSealUnsealRoundTrip(t *testing.T) {
	box, err := cryptox.NewBox("a-test-session-key")
	if err != nil {
		t.Fatal(err)
	}
	in := &Bundle{
		GrokVersion: "0.2.118",
		GrokAuth:    `{"https://auth.x.ai::abc":{"refresh":"r"}}`,
		GrokConfig:  "[mcp_servers.takan]\n",
		GroqAPIKey:  "gsk_fixture",
		AgentConfig: fixtureAgentConfig,
		AgentsMD:    fixtureAgentsMD,
	}
	sealed, err := SealBundle(box, in)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing readable at rest: the ciphertext must not leak any component.
	for _, secret := range []string{"gsk_fixture", "auth.x.ai", "mcp_servers"} {
		if strings.Contains(sealed, secret) {
			t.Fatalf("sealed bundle leaks %q", secret)
		}
	}
	out, err := OpenBundle(box, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Fatalf("round trip changed the bundle:\n got %#v\nwant %#v", *out, *in)
	}

	other, err := cryptox.NewBox("a-different-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBundle(other, sealed); err == nil {
		t.Fatal("a foreign key opened the bundle")
	}
}

// --- import parser --------------------------------------------------------

func TestReadBundleParsesSourceAndTemplatizes(t *testing.T) {
	paths := writeSource(t)
	res, err := ReadBundle(paths)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "Atlas" || res.Slug != "atlas" {
		t.Fatalf("identity: got %q/%q, want Atlas/atlas", res.Name, res.Slug)
	}
	if res.Bundle.GrokVersion != "0.2.118" {
		t.Fatalf("grok version: got %q", res.Bundle.GrokVersion)
	}
	if res.Bundle.GroqAPIKey != "gsk_fixture" {
		t.Fatalf("groq key: got %q", res.Bundle.GroqAPIKey)
	}
	if !strings.Contains(res.Bundle.GrokConfig, "mcp_servers.takan") {
		t.Fatal("grok config.toml (Takan MCP entry) was not captured")
	}
	// The source identity must be gone from the stored copies, otherwise every
	// provisioned bot would call itself Atlas and write Atlas's data dir.
	for _, body := range []string{res.Bundle.AgentConfig, res.Bundle.AgentsMD} {
		for _, leak := range []string{"Atlas", res.DataDir, paths.EnvFile} {
			if strings.Contains(body, leak) {
				t.Fatalf("templatize left %q behind in %q", leak, body)
			}
		}
	}
	if !strings.Contains(res.Bundle.AgentsMD, PlaceholderName) ||
		!strings.Contains(res.Bundle.AgentsMD, PlaceholderInstance) ||
		!strings.Contains(res.Bundle.AgentConfig, PlaceholderDataDir) ||
		!strings.Contains(res.Bundle.AgentConfig, PlaceholderEnvFile) {
		t.Fatal("placeholders missing from the templated bundle")
	}
	for _, want := range []string{filepath.Join(paths.GrokHome, "auth.json"), paths.EnvFile} {
		if !contains(res.Guarded, want) {
			t.Fatalf("guarded list missing %s", want)
		}
	}
}

func TestReadBundleFailsLoudlyOnBadSource(t *testing.T) {
	paths := writeSource(t)
	mustWrite(t, filepath.Join(paths.GrokHome, "auth.json"), "not json", 0o600)
	if _, err := ReadBundle(paths); err == nil {
		t.Fatal("a non-JSON auth.json was accepted")
	}

	paths = writeSource(t)
	mustWrite(t, paths.EnvFile, "TELEGRAM_BOT_TOKEN=tg\n", 0o600)
	if _, err := ReadBundle(paths); err == nil || !strings.Contains(err.Error(), "GROQ_API_KEY") {
		t.Fatalf("missing GROQ_API_KEY not reported: %v", err)
	}
}

func TestReadBundleLeavesSourceUntouched(t *testing.T) {
	paths := writeSource(t)
	guard := []string{
		filepath.Join(paths.GrokHome, "auth.json"),
		filepath.Join(paths.GrokHome, "config.toml"),
		filepath.Join(paths.AgentData, "config.toml"),
		filepath.Join(paths.AgentData, "workspace", "AGENTS.md"),
		paths.EnvFile,
	}
	before, err := StatOwners(guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundle(paths); err != nil {
		t.Fatal(err)
	}
	if err := AssertOwnersUnchanged(before); err != nil {
		t.Fatalf("import touched the source: %v", err)
	}
	// And the assertion is not vacuous.
	if err := os.Chmod(guard[0], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AssertOwnersUnchanged(before); err == nil {
		t.Fatal("a mode change was not detected")
	}
}

// --- rendering ------------------------------------------------------------

func TestBundleTarRendersPerBotIdentity(t *testing.T) {
	res, err := ReadBundle(writeSource(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := res.Bundle.Tar("Casa", "casa", BundleDataDir("casa"))
	if err != nil {
		t.Fatal(err)
	}
	files := readTar(t, raw)

	for _, want := range []string{"grok-version", "grok/auth.json", "grok/config.toml",
		"data/config.toml", "data/workspace/AGENTS.md"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("tar missing %s", want)
		}
	}
	guide := files["data/workspace/AGENTS.md"]
	// The machine_ai_run owner has to be the bot itself, not the bundle source.
	if !strings.Contains(guide, `machine_ai_run owner="Casa"`) {
		t.Fatalf("machine_ai_run owner not rewritten:\n%s", guide)
	}
	if !strings.Contains(guide, "casa-send") {
		t.Fatalf("helper CLI name not rewritten:\n%s", guide)
	}
	cfg := files["data/config.toml"]
	if !strings.Contains(cfg, `name = "Casa"`) || !strings.Contains(cfg, "/var/lib/casa/workspace") {
		t.Fatalf("daemon config not rendered for the target:\n%s", cfg)
	}
	if !strings.Contains(cfg, "/etc/casa/casa.env") {
		t.Fatalf("the env path still points at the source machine:\n%s", cfg)
	}
	for name, body := range files {
		if strings.Contains(body, "Atlas") || strings.Contains(body, PlaceholderName) {
			t.Fatalf("%s still carries the source identity or an unrendered placeholder", name)
		}
	}
	// Credentials pass through byte for byte.
	if !json.Valid([]byte(files["grok/auth.json"])) {
		t.Fatal("auth.json was mangled in transit")
	}
}

// TestBundleTarNamesTheInstance covers both source shapes: a config without an
// [instance] section (the live vps2 one — the daemon would otherwise default to
// "Atlas" and every provisioned bot would introduce itself as Atlas) and one
// that names the instance explicitly.
func TestBundleTarNamesTheInstance(t *testing.T) {
	paths := writeSource(t)
	res, err := ReadBundle(paths)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := res.Bundle.Tar("Casa", "casa", BundleDataDir("casa"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := readTar(t, raw)["data/config.toml"]
	if !strings.Contains(cfg, "[instance]") || !strings.Contains(cfg, `name = "Casa"`) {
		t.Fatalf("a nameless source config was not given the bot's name:\n%s", cfg)
	}

	// An explicit name in the source is rewritten in place, not duplicated.
	mustWrite(t, filepath.Join(paths.AgentData, "config.toml"),
		"[instance]\nname = \"Atlas\"\n\n[agent]\ncommand = \"grok\"\n", 0o644)
	res, err = ReadBundle(paths)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = res.Bundle.Tar("Casa", "casa", BundleDataDir("casa"))
	if err != nil {
		t.Fatal(err)
	}
	cfg = readTar(t, raw)["data/config.toml"]
	if strings.Count(cfg, "[instance]") != 1 || !strings.Contains(cfg, `name = "Casa"`) {
		t.Fatalf("an explicitly named source config was not rewritten cleanly:\n%s", cfg)
	}
}

func readTar(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(body)
	}
	return out
}

// --- endpoint + env -------------------------------------------------------

// seedBundle imports the fixture source into the store for the fixture user.
func seedBundle(t *testing.T, f *fixture, box *cryptox.Box) {
	t.Helper()
	res, err := ReadBundle(writeSource(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealBundle(box, res.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SaveRuntimeBundle(f.ctx(), &store.RuntimeBundle{
		UserID: f.user.ID, GrokVersion: res.Bundle.GrokVersion, SourceName: res.Name,
		SourceSlug: res.Slug, SourceDataDir: res.DataDir, PayloadEnc: sealed,
		Components: res.Bundle.Components(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionBundleEndpoint(t *testing.T) {
	f, _ := provFixture(t)
	box, err := cryptox.NewBox("endpoint-key")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Provision.Box = box
	attachChannel(t, f, "-100999")

	ticket, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps-test")
	if err != nil {
		t.Fatal(err)
	}

	// No bundle imported yet: a clean 404, not a 500 and not an empty 200.
	if rec, _ := f.do(t, http.MethodGet, "/api/bots/provision/bundle", ticket, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bundle without import: got %d, want 404", rec.Code)
	}
	// And no ticket at all is a 401.
	if rec, _ := f.do(t, http.MethodGet, "/api/bots/provision/bundle", "nope", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad ticket: got %d, want 401", rec.Code)
	}

	seedBundle(t, f, box)
	rec, _ := f.do(t, http.MethodGet, "/api/bots/provision/bundle", ticket, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle fetch: got %d", rec.Code)
	}
	files := readTar(t, rec.Body.Bytes())
	if !strings.Contains(files["data/workspace/AGENTS.md"], f.bot.Name) {
		t.Fatal("bundle was not rendered for the requesting bot")
	}
}

func TestProvisionEnvCarriesBundleExtras(t *testing.T) {
	f, _ := provFixture(t)
	box, err := cryptox.NewBox("env-key")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Provision.Box = box
	attachChannel(t, f, "-100999")
	seedBundle(t, f, box)

	ticket, err := f.st.IssueProvisionTicket(f.ctx(), f.bot.ID, f.user.ID, "vps-test")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := f.do(t, http.MethodGet, "/api/bots/provision/env?mode=install", ticket, "")
	body := rec.Body.String()
	if !strings.Contains(body, `GROQ_API_KEY="gsk_fixture"`) {
		t.Fatalf("env missing the transcription key:\n%s", body)
	}
	if !strings.Contains(body, `ATLAS_DATA_DIR="`+BundleDataDir(f.bot.Instance)+`"`) {
		t.Fatalf("env missing the data dir:\n%s", body)
	}

	// Adoption must never repoint a daemon the operator installed.
	rec, _ = f.do(t, http.MethodGet, "/api/bots/provision/env?mode=adopt", ticket, "")
	body = rec.Body.String()
	if strings.Contains(body, "ATLAS_DATA_DIR") {
		t.Fatalf("adopt mode moved the data dir:\n%s", body)
	}
	if !strings.Contains(body, "GROQ_API_KEY") {
		t.Fatalf("adopt mode dropped the transcription key:\n%s", body)
	}
}

// --- script template ------------------------------------------------------

func TestProvisionScriptBundleStepsFollowTheUnitGuard(t *testing.T) {
	f, _ := provFixture(t)
	script := f.srv.Provision.script("casa", "ticket-abc123")

	guard := strings.Index(script, "MODE=adopt")
	bundle := strings.Index(script, "$HUB/api/bots/provision/bundle")
	if guard < 0 || bundle < 0 {
		t.Fatal("script lost either the unit guard or the bundle fetch")
	}
	if bundle < guard {
		t.Fatal("the bundle is fetched before the unit guard decides install vs adopt")
	}
	// Every write into the service user's home must sit inside the install
	// branch, so an adopted unit's owner never has their ~/.grok rewritten.
	installBranch := script[strings.Index(script, `if [ "$MODE" = install ]; then`):strings.Index(script, "  # Adoption: the operator owns the unit")]
	for _, want := range []string{
		"$GROKHOME/auth.json",
		"$GROKHOME/config.toml",
		"x.ai/cli/install.sh",
		"$HUB/api/bots/provision/bundle",
		"$DATADIR/workspace/AGENTS.md",
	} {
		if !strings.Contains(installBranch, want) {
			t.Fatalf("install branch missing %q", want)
		}
		if strings.Count(script, want) != strings.Count(installBranch, want) {
			t.Fatalf("%q also appears outside the install branch", want)
		}
	}

	for _, want := range []string{
		"$HUB/api/bots/provision/env?mode=$MODE",      // adoption tells the hub
		`[ ! -x "$GROKHOME/bin/grok" ]`,               // the CLI is the service user's own
		"[ ! -e /usr/local/bin/grok ]",                // never replace an existing wrapper
		`$SUDO cp -a /usr/local/bin/grok "$HOSTGROK"`, // and restore it after the installer
		`Environment=HOME=$SVCHOME`,                   // grok finds its own home
		"$HUB/api/bots/binary?os=linux&arch=$ARCH",    // unchanged
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
	// The service user's own grok must win over any host-wide wrapper, which
	// typically pins HOME at a human account and would read that human's
	// credentials (and, run as root, re-own them).
	unitPath := script[strings.Index(script, "Environment=PATH="):]
	unitPath = unitPath[:strings.Index(unitPath, "\n")]
	if !strings.HasPrefix(unitPath, "Environment=PATH=$GROKHOME/bin:") {
		t.Fatalf("the service user's grok is not first on PATH: %s", unitPath)
	}
	// The x.ai installer drops its own /usr/local/bin/grok. Saving that path
	// has to happen before the installer runs and restoring it after, or a
	// host wrapper (and its root-drop guard) is silently replaced.
	save := strings.Index(script, `$SUDO cp -a /usr/local/bin/grok "$HOSTGROK"`)
	install := strings.Index(script, `bash "$INSTALLER"`)
	restore := strings.Index(script, `$SUDO cp -a "$HOSTGROK" /usr/local/bin/grok`)
	if save < 0 || install < 0 || restore < 0 || !(save < install && install < restore) {
		t.Fatal("the host grok wrapper is not saved before and restored after the installer")
	}
	if strings.Contains(script, "gsk_") || strings.Contains(script, "auth.x.ai") {
		t.Fatal("the script carries bundle secrets; they must travel in a response body")
	}
}

// TestProvisionScriptIsValidShell parses the whole rendered template with a
// real shell. The script only ever runs on a target machine, so a quoting slip
// in the heredocs would otherwise surface as a failed provision in the field.
func TestProvisionScriptIsValidShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	f, _ := provFixture(t)
	path := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(path, []byte(f.srv.Provision.script("casa", "ticket-abc")), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("generated script is not valid shell: %v\n%s", err, out)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
