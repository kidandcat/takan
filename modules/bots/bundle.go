package bots

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
)

// Runtime bundle: everything a freshly provisioned bot needs to have a working
// brain without a human touching the target machine.
//
// It is imported once, on the hub host, from a machine that already has a
// working daemon (vps2/Atlas). The import is strictly READ-ONLY on the source
// files — see TAKAN_BOTS.md §9: a root-run process that so much as executes
// grok re-owns ~/.grok/auth.json and breaks the live daemon.

// Placeholders written into the stored bundle at import time and expanded per
// bot at provision time. They deliberately look nothing like shell or TOML
// syntax, so an un-rendered one is obvious rather than silently valid.
const (
	PlaceholderName     = "__TAKAN_NAME__"
	PlaceholderInstance = "__TAKAN_INSTANCE__"
	PlaceholderDataDir  = "__TAKAN_DATADIR__"
)

// DefaultServiceHome is the home of a provisioned unit's service user.
// Provisioned units run as root (the generated unit sets no User=), so the grok
// CLI and its credentials live under /root.
const DefaultServiceHome = "/root"

// BundleDataDir is where a provisioned daemon keeps config.toml, its workspace
// and its state. /var/lib/<instance> is the FHS home for that, and it keeps the
// daemon out of any human's home directory.
func BundleDataDir(instance string) string { return "/var/lib/" + instance }

// Bundle is the plaintext payload. It is never persisted in this form: the
// store keeps only the sealed JSON (cryptox.Box, the same key that seals
// Telegram channel credentials).
type Bundle struct {
	// GrokVersion pins the CLI version, e.g. "0.2.118".
	GrokVersion string `json:"grok_version"`
	// GrokAuth is ~/.grok/auth.json (the subscription credential).
	GrokAuth string `json:"grok_auth"`
	// GrokConfig is ~/.grok/config.toml, which carries the Takan MCP entry.
	GrokConfig string `json:"grok_config"`
	// GroqAPIKey is the Whisper transcription key from the daemon env file.
	GroqAPIKey string `json:"groq_api_key"`
	// AgentConfig is the daemon's base config.toml, templated.
	AgentConfig string `json:"agent_config"`
	// AgentsMD is workspace/AGENTS.md, templated.
	AgentsMD string `json:"agents_md"`
}

// SealBundle encrypts a bundle for storage.
func SealBundle(box *cryptox.Box, b *Bundle) (string, error) {
	if box == nil {
		return "", fmt.Errorf("no crypto box configured")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return box.Seal(string(raw))
}

// OpenBundle decrypts a stored bundle.
func OpenBundle(box *cryptox.Box, enc string) (*Bundle, error) {
	if box == nil {
		return nil, fmt.Errorf("no crypto box configured")
	}
	raw, err := box.Open(enc)
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// Components lists the non-secret inventory shown in the panel.
func (b *Bundle) Components() []store.BundleComponent {
	out := make([]store.BundleComponent, 0, 5)
	add := func(name, body string) {
		if body != "" {
			out = append(out, store.BundleComponent{Name: name, Bytes: len(body)})
		}
	}
	add("grok auth.json", b.GrokAuth)
	add("grok config.toml", b.GrokConfig)
	add("GROQ_API_KEY", b.GroqAPIKey)
	add("daemon config.toml", b.AgentConfig)
	add("workspace/AGENTS.md", b.AgentsMD)
	return out
}

// --- templating ---

// templatize replaces every trace of the source instance identity with the
// placeholders, longest match first so the data dir path is not shredded by the
// slug rule before it is recognised.
func templatize(text, name, slug, dataDir string) string {
	if text == "" {
		return text
	}
	if dataDir != "" {
		text = strings.ReplaceAll(text, dataDir, PlaceholderDataDir)
	}
	if name != "" {
		text = wordRe(name).ReplaceAllLiteralString(text, PlaceholderName)
	}
	if slug != "" && slug != name {
		text = wordRe(slug).ReplaceAllLiteralString(text, PlaceholderInstance)
	}
	return text
}

// render expands the placeholders for one target bot.
func render(text, name, instance, dataDir string) string {
	return strings.NewReplacer(
		PlaceholderDataDir, dataDir,
		PlaceholderName, name,
		PlaceholderInstance, instance,
	).Replace(text)
}

// wordRe matches a literal only when it stands alone, so "Atlas" is rewritten
// but "Atlassian" is not. Hyphens count as boundaries, which is what makes
// "atlas-send" become "<instance>-send".
func wordRe(lit string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(lit) + `\b`)
}

// --- tar rendering for the provision script ---

// Tar renders the bundle as a tar archive for one bot. The layout is flat and
// stable so the provision script can place each file without parsing anything.
func (b *Bundle) Tar(name, instance, dataDir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(path string, mode int64, body string) error {
		if body == "" {
			return nil
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: path, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := tw.Write([]byte(body))
		return err
	}
	if b.GrokVersion != "" {
		if err := write("grok-version", 0o644, b.GrokVersion+"\n"); err != nil {
			return nil, err
		}
	}
	if err := write("grok/auth.json", 0o600, b.GrokAuth); err != nil {
		return nil, err
	}
	if err := write("grok/config.toml", 0o600, b.GrokConfig); err != nil {
		return nil, err
	}
	if err := write("data/config.toml", 0o600, render(b.AgentConfig, name, instance, dataDir)); err != nil {
		return nil, err
	}
	if err := write("data/workspace/AGENTS.md", 0o644, render(b.AgentsMD, name, instance, dataDir)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- import (hub-side CLI only) ---

// ImportPaths are the source files on the hub host.
type ImportPaths struct {
	GrokHome  string // e.g. /home/debian/.grok
	AgentData string // e.g. /home/debian/atlas-data
	EnvFile   string // e.g. /home/debian/atlas.env
}

// ImportResult carries the bundle plus what was learned about the source.
type ImportResult struct {
	Bundle *Bundle
	// Name / Slug / DataDir identify the source instance the bundle was
	// templated against; they are stored for display.
	Name    string
	Slug    string
	DataDir string
	// Guarded is every source file whose ownership must be unchanged afterwards.
	Guarded []string
}

// ReadBundle collects a bundle from a live install. Every access is a plain
// read: no file is created, executed, chowned or chmodded, and the grok CLI is
// never invoked (invoking it would refresh and re-own auth.json).
func ReadBundle(p ImportPaths) (*ImportResult, error) {
	grokHome := strings.TrimRight(p.GrokHome, "/")
	agentData := strings.TrimRight(p.AgentData, "/")
	if grokHome == "" || agentData == "" {
		return nil, fmt.Errorf("both --grok-home and --atlas-data are required")
	}

	res := &ImportResult{DataDir: agentData}
	b := &Bundle{}

	authPath := filepath.Join(grokHome, "auth.json")
	auth, err := readFile(authPath)
	if err != nil {
		return nil, fmt.Errorf("grok auth: %w", err)
	}
	if !json.Valid([]byte(auth)) {
		return nil, fmt.Errorf("grok auth: %s is not valid JSON", authPath)
	}
	b.GrokAuth = auth
	res.Guarded = append(res.Guarded, authPath)

	// config.toml is optional but is what carries the Takan MCP server entry.
	if cfgPath := filepath.Join(grokHome, "config.toml"); fileExists(cfgPath) {
		if b.GrokConfig, err = readFile(cfgPath); err != nil {
			return nil, fmt.Errorf("grok config: %w", err)
		}
		res.Guarded = append(res.Guarded, cfgPath)
	}
	b.GrokVersion = grokVersion(grokHome)

	agentCfgPath := filepath.Join(agentData, "config.toml")
	agentCfg, err := readFile(agentCfgPath)
	if err != nil {
		return nil, fmt.Errorf("daemon config: %w", err)
	}
	res.Guarded = append(res.Guarded, agentCfgPath)

	guidePath := filepath.Join(agentData, "workspace", "AGENTS.md")
	guide := ""
	if fileExists(guidePath) {
		if guide, err = readFile(guidePath); err != nil {
			return nil, fmt.Errorf("workspace guide: %w", err)
		}
		res.Guarded = append(res.Guarded, guidePath)
	}

	if p.EnvFile != "" {
		env, err := readFile(p.EnvFile)
		if err != nil {
			return nil, fmt.Errorf("env file: %w", err)
		}
		b.GroqAPIKey = envValue(env, "GROQ_API_KEY")
		if b.GroqAPIKey == "" {
			return nil, fmt.Errorf("env file: GROQ_API_KEY not found in %s", p.EnvFile)
		}
		res.Guarded = append(res.Guarded, p.EnvFile)
	}

	res.Name = instanceName(agentCfg)
	res.Slug = instanceSlug(agentData, res.Name)
	b.AgentConfig = templatize(agentCfg, res.Name, res.Slug, agentData)
	b.AgentsMD = templatize(guide, res.Name, res.Slug, agentData)
	res.Bundle = b
	return res, nil
}

func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// grokVersion reads the pinned CLI version, preferring version.json and falling
// back to the versioned binary the bin/ symlink points at.
func grokVersion(grokHome string) string {
	if raw, err := os.ReadFile(filepath.Join(grokHome, "version.json")); err == nil {
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v.Version) != "" {
			return strings.TrimSpace(v.Version)
		}
	}
	if target, err := os.Readlink(filepath.Join(grokHome, "bin", "grok")); err == nil {
		if m := grokBinRe.FindStringSubmatch(filepath.Base(target)); m != nil {
			return m[1]
		}
	}
	return ""
}

// grokBinRe pulls the version out of e.g. grok-0.2.118-linux-x86_64.
var grokBinRe = regexp.MustCompile(`^grok-([0-9][0-9A-Za-z.+-]*?)-[a-z]+-[a-z0-9_]+$`)

// instanceName reads [instance] name from the daemon's config.toml. The
// daemon's own default is used when the section is absent, which is the common
// case on the machine the bundle is imported from.
func instanceName(cfg string) string {
	sc := bufio.NewScanner(strings.NewReader(cfg))
	section := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[]")
			continue
		}
		if section != "instance" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "name" {
			if name := strings.Trim(strings.TrimSpace(v), `"'`); name != "" {
				return name
			}
		}
	}
	return DefaultInstanceName
}

// DefaultInstanceName matches the daemon's own default when config.toml does
// not name the instance.
const DefaultInstanceName = "Atlas"

// instanceSlug derives the unit/binary base name from the data directory
// ("/home/debian/atlas-data" -> "atlas"), falling back to the display name.
func instanceSlug(dataDir, name string) string {
	base := strings.TrimSuffix(filepath.Base(dataDir), "-data")
	if base != "" && base != "." && base != "/" {
		return strings.ToLower(base)
	}
	return store.InstanceName(name)
}

// envValue reads one KEY=value out of a systemd EnvironmentFile body.
func envValue(body, key string) string {
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// --- ownership guard ---

// OwnerStat is the identity of a source file, captured before and after import.
type OwnerStat struct {
	Path string
	UID  uint32
	GID  uint32
	Mode os.FileMode
}

// StatOwners snapshots ownership and mode of every guarded path.
func StatOwners(paths []string) ([]OwnerStat, error) {
	out := make([]OwnerStat, 0, len(paths))
	for _, p := range paths {
		st, err := statOwner(p)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// AssertOwnersUnchanged compares a snapshot against the current state. A
// difference means the import re-owned a file the daemon needs (TAKAN_BOTS.md
// §9) and the caller must treat it as a failure, loudly.
func AssertOwnersUnchanged(before []OwnerStat) error {
	var drift []string
	for _, b := range before {
		now, err := statOwner(b.Path)
		if err != nil {
			drift = append(drift, fmt.Sprintf("%s: %v", b.Path, err))
			continue
		}
		if now.UID != b.UID || now.GID != b.GID || now.Mode != b.Mode {
			drift = append(drift, fmt.Sprintf("%s: was %d:%d %o, now %d:%d %o",
				b.Path, b.UID, b.GID, b.Mode.Perm(), now.UID, now.GID, now.Mode.Perm()))
		}
	}
	if len(drift) > 0 {
		return fmt.Errorf("source files were modified by the import: %s", strings.Join(drift, "; "))
	}
	return nil
}
