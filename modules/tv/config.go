// Package tv exposes Samsung Tizen Smart TV controls via a LAN takan-agent.
package tv

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kidandcat/takan/internal/store"
)

// Household defaults for the panel (overridable; not the only way to run).
const (
	DefaultMachine    = "mac"
	DefaultHost       = "192.168.68.102"
	DefaultTokenPath  = "/Users/jairo/.samsung-tv-token"
	DefaultClientName = "Gamma"
	DefaultWifiMAC    = "04:B9:E3:86:CD:C0"
)

// Builtin app aliases (friendly name → Tizen app id).
var defaultApps = map[string]string{
	"netflix": "3201907018807",
	"youtube": "9Ur5IzDKqV.TizenYouTube",
	"hbo":     "3201601007230",
	"hbo max": "3201601007230",
	"hbomax":  "3201601007230",
	"max":     "3201601007230",
}

// Config is per-user TV module settings (stored in user_modules.config_json).
type Config struct {
	Machine    string            `json:"machine"`
	Host       string            `json:"host"`
	TokenPath  string            `json:"token_path"`
	ClientName string            `json:"client_name"`
	WifiMAC    string            `json:"wifi_mac,omitempty"`
	Apps       map[string]string `json:"apps,omitempty"`
}

// AppRef is a unique Tizen app (one row per id) for REST probes.
type AppRef struct {
	Alias string
	ID    string
}

// DefaultConfig returns household defaults (machine mac on the TV LAN).
func DefaultConfig() Config {
	return Config{
		Machine:    DefaultMachine,
		Host:       DefaultHost,
		TokenPath:  DefaultTokenPath,
		ClientName: DefaultClientName,
		WifiMAC:    DefaultWifiMAC,
		Apps:       cloneApps(defaultApps),
	}
}

// LoadConfig reads module config for the user, applying defaults when empty/invalid.
func LoadConfig(ctx context.Context, st *store.Store, userID string) (Config, error) {
	raw, err := st.GetModuleConfig(ctx, userID, "tv")
	if err != nil {
		return DefaultConfig(), err
	}
	return ParseConfig(raw), nil
}

// ParseConfig unmarshals JSON; empty or broken input yields defaults.
// Missing fields keep household defaults so a partial save still works.
func ParseConfig(raw string) Config {
	def := DefaultConfig()
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return def
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return def
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return def
	}
	if _, ok := probe["machine"]; !ok || strings.TrimSpace(c.Machine) == "" {
		c.Machine = def.Machine
	}
	if _, ok := probe["host"]; !ok || strings.TrimSpace(c.Host) == "" {
		c.Host = def.Host
	}
	if _, ok := probe["token_path"]; !ok || strings.TrimSpace(c.TokenPath) == "" {
		c.TokenPath = def.TokenPath
	}
	if _, ok := probe["client_name"]; !ok || strings.TrimSpace(c.ClientName) == "" {
		c.ClientName = def.ClientName
	}
	if _, ok := probe["wifi_mac"]; !ok || strings.TrimSpace(c.WifiMAC) == "" {
		c.WifiMAC = def.WifiMAC
	}
	c.Machine = strings.TrimSpace(c.Machine)
	c.Host = strings.TrimSpace(c.Host)
	c.TokenPath = strings.TrimSpace(c.TokenPath)
	c.ClientName = strings.TrimSpace(c.ClientName)
	c.WifiMAC = strings.ToUpper(strings.TrimSpace(c.WifiMAC))
	c.Apps = mergeApps(def.Apps, c.Apps)
	return c
}

// SaveConfig validates and persists TV module config.
func SaveConfig(ctx context.Context, st *store.Store, userID string, c Config) error {
	c = ParseConfig(mustJSON(c))
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return st.SetModuleConfig(ctx, userID, "tv", string(b))
}

// Validate checks machine/host/token/app ids are safe to embed in an agent shell command.
func (c Config) Validate() error {
	if err := validMachine(c.Machine); err != nil {
		return err
	}
	if err := validHost(c.Host); err != nil {
		return err
	}
	if err := validTokenPath(c.TokenPath); err != nil {
		return err
	}
	if err := validClientName(c.ClientName); err != nil {
		return err
	}
	if err := validMAC(c.WifiMAC); err != nil {
		return err
	}
	for alias, id := range c.Apps {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("app alias required")
		}
		if err := validAppID(id); err != nil {
			return fmt.Errorf("app %q: %w", alias, err)
		}
	}
	return nil
}

// ResolveApp maps a friendly name or raw Tizen app id.
func (c Config) ResolveApp(nameOrID string) (string, error) {
	s := strings.TrimSpace(nameOrID)
	if s == "" {
		return "", fmt.Errorf("app name or id required")
	}
	if id, ok := c.Apps[strings.ToLower(s)]; ok {
		return id, nil
	}
	// Raw Tizen ids are numeric or dotted (e.g. 3201907018807, 9Ur5IzDKqV.TizenYouTube).
	if looksLikeAppID(s) {
		return s, nil
	}
	var names []string
	for alias := range c.Apps {
		names = append(names, alias)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("unknown app %q", s)
	}
	return "", fmt.Errorf("unknown app %q — try %s or a Tizen app id", s, strings.Join(sortedKeys(c.Apps), ", "))
}

// UniqueApps returns one entry per app id (Netflix / YouTube / HBO Max first).
func (c Config) UniqueApps() []AppRef {
	seen := map[string]bool{}
	var out []AppRef
	add := func(alias string) {
		id, ok := c.Apps[alias]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, AppRef{Alias: displayAlias(alias), ID: id})
	}
	for _, alias := range []string{"netflix", "youtube", "hbo max", "hbo", "hbomax", "max"} {
		add(alias)
	}
	for _, alias := range sortedKeys(c.Apps) {
		add(alias)
	}
	return out
}

// AppsText formats aliases for the panel textarea (alias|appId per line).
func (c Config) AppsText() string {
	keys := sortedKeys(c.Apps)
	var lines []string
	seenID := map[string]bool{}
	for _, k := range keys {
		id := c.Apps[k]
		// Prefer the nicest alias per id (skip hbomax/max when "hbo max" exists).
		if (k == "hbo" || k == "hbomax" || k == "max") && hasAlias(c.Apps, "hbo max") {
			continue
		}
		if seenID[id] && (k == "hbo" || k == "hbomax" || k == "max") {
			continue
		}
		seenID[id] = true
		lines = append(lines, displayAlias(k)+"|"+id)
	}
	return strings.Join(lines, "\n")
}

// ParseAppsText reads alias|appId or alias=appId lines.
func ParseAppsText(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		alias, id, ok := splitAliasID(line)
		if !ok {
			return nil, fmt.Errorf("app line %q: use alias|appId", line)
		}
		alias = strings.ToLower(strings.TrimSpace(alias))
		id = strings.TrimSpace(id)
		if alias == "" {
			return nil, fmt.Errorf("app alias required")
		}
		if err := validAppID(id); err != nil {
			return nil, fmt.Errorf("app %q: %w", alias, err)
		}
		out[alias] = id
	}
	return out, nil
}

func splitAliasID(line string) (alias, id string, ok bool) {
	for _, sep := range []string{"|", "="} {
		if i := strings.Index(line, sep); i > 0 {
			return line[:i], line[i+1:], true
		}
	}
	return "", "", false
}

func mergeApps(base, extra map[string]string) map[string]string {
	out := cloneApps(base)
	for k, v := range extra {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func cloneApps(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func hasAlias(apps map[string]string, name string) bool {
	_, ok := apps[name]
	return ok
}

func displayAlias(k string) string {
	switch k {
	case "netflix":
		return "Netflix"
	case "youtube":
		return "YouTube"
	case "hbo max":
		return "HBO Max"
	default:
		return k
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mustJSON(c Config) string {
	b, err := json.Marshal(c)
	if err != nil {
		return "{}"
	}
	return string(b)
}
