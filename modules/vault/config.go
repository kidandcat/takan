package vault

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kidandcat/takan/internal/store"
)

// Config is per-user vault module settings (stored in user_modules.config_json).
type Config struct {
	// RequireApproval gates agent secret reads. When true (default), secrets_request
	// creates a pending grant the user must approve in the panel. When false, grants
	// with a resolved item_id are auto-approved so agents can use secrets_status immediately.
	RequireApproval bool `json:"require_approval"`
}

// DefaultConfig is the safe default: agents need user approval for secrets.
func DefaultConfig() Config {
	return Config{RequireApproval: true}
}

// LoadConfig reads vault module config for the user, applying defaults when empty/invalid.
func LoadConfig(ctx context.Context, st *store.Store, userID string) (Config, error) {
	raw, err := st.GetModuleConfig(ctx, userID, "vault")
	if err != nil {
		return DefaultConfig(), err
	}
	return ParseConfig(raw), nil
}

// ParseConfig unmarshals JSON; empty or broken input yields defaults.
// Missing require_approval keeps the safe default (true).
func ParseConfig(raw string) Config {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return DefaultConfig()
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return DefaultConfig()
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return DefaultConfig()
	}
	if _, ok := probe["require_approval"]; !ok {
		c.RequireApproval = true
	}
	return c
}

// SaveConfig persists vault module config.
func SaveConfig(ctx context.Context, st *store.Store, userID string, c Config) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return st.SetModuleConfig(ctx, userID, "vault", string(b))
}
