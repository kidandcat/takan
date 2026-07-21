package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/kidandcat/takan/internal/store"
)

// PromptPlaceholder is replaced with a shell-quoted prompt when launching a job.
const PromptPlaceholder = "{{prompt}}"

// Config is per-user machine module settings (stored in user_modules.config_json).
type Config struct {
	// AITasksEnabled gates machine_ai_* tools.
	AITasksEnabled bool     `json:"ai_tasks_enabled"`
	Runners        []Runner `json:"runners"`
}

// Runner is a named launch command template for long-running AI jobs.
type Runner struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Command is a shell command template. Use {{prompt}} where the task prompt
	// should be injected (shell-quoted by the agent). If omitted, the prompt is
	// appended as a final quoted argument.
	Command string `json:"command"`
	// Builtin presets cannot be deleted from the panel (only disabled/edited).
	Builtin bool `json:"builtin,omitempty"`
}

// DefaultConfig returns presets for Claude Code and Grok Build (both enabled).
func DefaultConfig() Config {
	return Config{
		AITasksEnabled: true,
		Runners: []Runner{
			{
				ID:      "claude",
				Name:    "Claude Code",
				Enabled: true,
				Builtin: true,
				Command: "claude -p --dangerously-skip-permissions " + PromptPlaceholder,
			},
			{
				ID:      "grok",
				Name:    "Grok Build",
				Enabled: true,
				Builtin: true,
				Command: "grok --always-approve -p " + PromptPlaceholder,
			},
		},
	}
}

// LoadConfig reads module config for the user, applying defaults when empty/invalid.
func LoadConfig(ctx context.Context, st *store.Store, userID string) (Config, error) {
	raw, err := st.GetModuleConfig(ctx, userID, "machine")
	if err != nil {
		return DefaultConfig(), err
	}
	return ParseConfig(raw), nil
}

// ParseConfig unmarshals JSON; empty or broken input yields defaults.
func ParseConfig(raw string) Config {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return DefaultConfig()
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return DefaultConfig()
	}
	// If someone saved enabled=false with no runners, keep that; if runners empty
	// and never configured meaningfully, seed presets.
	if len(c.Runners) == 0 {
		def := DefaultConfig()
		c.Runners = def.Runners
		// Preserve explicit AITasksEnabled even when runners were empty.
	}
	c.normalize()
	return c
}

// SaveConfig validates and persists machine module config.
func SaveConfig(ctx context.Context, st *store.Store, userID string, c Config) error {
	c.normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return st.SetModuleConfig(ctx, userID, "machine", string(b))
}

func (c *Config) normalize() {
	seen := map[string]int{}
	out := make([]Runner, 0, len(c.Runners))
	for _, r := range c.Runners {
		r.ID = strings.TrimSpace(r.ID)
		r.Name = strings.TrimSpace(r.Name)
		r.Command = strings.TrimSpace(r.Command)
		if r.ID == "" {
			r.ID = slugID(r.Name)
		}
		if r.ID == "" {
			r.ID = "runner-" + uuid.NewString()[:8]
		}
		// ensure unique ids
		if n, ok := seen[r.ID]; ok {
			seen[r.ID] = n + 1
			r.ID = fmt.Sprintf("%s-%d", r.ID, n+1)
		} else {
			seen[r.ID] = 1
		}
		if r.Name == "" {
			r.Name = r.ID
		}
		out = append(out, r)
	}
	// Ensure builtin presets exist (disabled if user removed them earlier by id mismatch).
	c.Runners = ensureBuiltins(out)
}

func ensureBuiltins(runners []Runner) []Runner {
	def := DefaultConfig().Runners
	have := map[string]bool{}
	for _, r := range runners {
		have[r.ID] = true
	}
	for _, b := range def {
		if !have[b.ID] {
			// re-add missing builtin as disabled so user can re-enable
			b.Enabled = false
			runners = append([]Runner{b}, runners...)
		}
	}
	// Mark known builtin ids
	builtinIDs := map[string]bool{"claude": true, "grok": true}
	for i := range runners {
		if builtinIDs[runners[i].ID] {
			runners[i].Builtin = true
		}
	}
	return runners
}

var idRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func slugID(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = idRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// Validate checks commands and ids.
func (c Config) Validate() error {
	if len(c.Runners) == 0 {
		return fmt.Errorf("at least one runner is required")
	}
	for _, r := range c.Runners {
		if r.ID == "" {
			return fmt.Errorf("runner id required")
		}
		if r.Command == "" {
			return fmt.Errorf("runner %q: command required", r.ID)
		}
		if len(r.Command) > 4000 {
			return fmt.Errorf("runner %q: command too long", r.ID)
		}
	}
	return nil
}

// EnabledRunners returns runners that may be used by machine_ai_run.
func (c Config) EnabledRunners() []Runner {
	var out []Runner
	for _, r := range c.Runners {
		if r.Enabled && strings.TrimSpace(r.Command) != "" {
			out = append(out, r)
		}
	}
	return out
}

// RunnerByID finds a runner (enabled or not).
func (c Config) RunnerByID(id string) (Runner, bool) {
	id = strings.TrimSpace(id)
	for _, r := range c.Runners {
		if r.ID == id {
			return r, true
		}
	}
	return Runner{}, false
}
