// Package assistant is the personal assistant that used to be a separate
// daemon: the Telegram long-poll loop, the CLI agent runner, background tasks,
// the scheduler, and the phone app channel. It runs inside the hub process and
// keeps all of its state in the hub's SQLite database.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/kidandcat/takan/internal/store"
)

// ModuleID is the user_modules row the assistant's configuration lives in.
const ModuleID = "assistant"

// InstanceName is what the assistant calls itself in replies and in AGENTS.md.
const InstanceName = "Atlas"

// Template placeholders substituted into the configured argument lists.
const (
	promptPlaceholder  = "{prompt}"
	sessionPlaceholder = "{session}"
	parentPlaceholder  = "{parent}"
)

// Options is the panel-editable runner configuration, stored as JSON in
// user_modules.config_json for module "assistant".
type Options struct {
	// Enabled gates the Telegram poller and the scheduler. The /v1 app channel
	// and the CLIs keep working either way.
	Enabled bool `json:"enabled"`
	// RespondToAll makes the assistant answer every message the owner writes in
	// a group, not only those mentioning it. It never lifts the owner check.
	RespondToAll bool         `json:"respond_to_all"`
	Agent        AgentOpts    `json:"agent"`
	Telegram     TelegramOpts `json:"telegram"`
}

// AgentOpts describes how to invoke the CLI coding agent that writes replies.
type AgentOpts struct {
	// Command is the executable to run, e.g. "grok".
	Command string `json:"command"`
	// Args is the argument template. "{prompt}" is replaced by the user's
	// message, and may appear inside a larger argument.
	Args []string `json:"args"`
	// NewSessionArgs are appended when starting a new session (gets {session}).
	NewSessionArgs []string `json:"new_session_args"`
	// ResumeArgs are appended when continuing a session by id (gets {session}).
	ResumeArgs []string `json:"resume_args"`
	// ForkArgs branch {parent} into a new {session}. This is how the
	// conversation continues while a promoted run still owns the parent.
	ForkArgs []string `json:"fork_args"`
	// Workdir is the agent's working directory. Empty means <data>/workspace.
	Workdir string `json:"workdir"`
	// SoftTimeout is the conversational budget. A run that crosses it is not
	// killed: it is promoted to a background task and the chat is freed.
	SoftTimeout string `json:"soft_timeout"`
	// TaskTimeout is the hard cap on any run, conversational or background.
	TaskTimeout string `json:"task_timeout"`
	// MaxConcurrentTasks bounds background tasks. They share the hub's cgroup
	// now, so an unbounded fan-out is an OOM waiting to happen.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`
	// Env adds or overrides variables in the child environment.
	Env map[string]string `json:"env"`
	// UnsetEnv removes variables from the child environment on top of the
	// allowlist. XAI_API_KEY must stay unset so grok uses ~/.grok/auth.json.
	UnsetEnv []string `json:"unset_env"`
}

// TelegramOpts tunes the polling loop and the typing indicator.
type TelegramOpts struct {
	PollTimeout    string `json:"poll_timeout"`
	TypingInterval string `json:"typing_interval"`
}

// DefaultOptions is the configuration a fresh instance starts from.
func DefaultOptions() Options {
	o := Options{Enabled: true}
	o.applyDefaults()
	return o
}

func (o *Options) applyDefaults() {
	if o.Agent.Command == "" {
		o.Agent.Command = "grok"
	}
	if len(o.Agent.Args) == 0 {
		o.Agent.Args = []string{"--single", promptPlaceholder, "--output-format", "plain", "--always-approve"}
	}
	if len(o.Agent.NewSessionArgs) == 0 {
		o.Agent.NewSessionArgs = []string{"--session-id", sessionPlaceholder}
	}
	if len(o.Agent.ResumeArgs) == 0 {
		o.Agent.ResumeArgs = []string{"--resume", sessionPlaceholder}
	}
	if len(o.Agent.ForkArgs) == 0 {
		o.Agent.ForkArgs = []string{"--resume", parentPlaceholder, "--fork-session", "--session-id", sessionPlaceholder}
	}
	if o.Agent.SoftTimeout == "" {
		o.Agent.SoftTimeout = "60s"
	}
	if o.Agent.TaskTimeout == "" {
		o.Agent.TaskTimeout = "1h"
	}
	if o.Agent.MaxConcurrentTasks <= 0 {
		o.Agent.MaxConcurrentTasks = 3
	}
	if len(o.Agent.UnsetEnv) == 0 {
		o.Agent.UnsetEnv = []string{"XAI_API_KEY"}
	}
	if o.Telegram.PollTimeout == "" {
		o.Telegram.PollTimeout = "50s"
	}
	if o.Telegram.TypingInterval == "" {
		o.Telegram.TypingInterval = "5s"
	}
}

// SoftTimeout is the conversational budget after which a run is promoted.
func (o Options) SoftTimeout() time.Duration {
	return parseDuration(o.Agent.SoftTimeout, 60*time.Second)
}

// TaskTimeout is the hard cap on any run.
func (o Options) TaskTimeout() time.Duration { return parseDuration(o.Agent.TaskTimeout, time.Hour) }

// PollTimeout is the getUpdates long-poll duration.
func (o Options) PollTimeout() time.Duration {
	return parseDuration(o.Telegram.PollTimeout, 50*time.Second)
}

// TypingInterval is how often the typing action is refreshed during a run.
func (o Options) TypingInterval() time.Duration {
	return parseDuration(o.Telegram.TypingInterval, 5*time.Second)
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// LoadOptions reads the assistant configuration from the panel, filling in
// defaults for anything unset.
func LoadOptions(ctx context.Context, st *store.Store, userID string) (Options, error) {
	raw, err := st.GetModuleConfig(ctx, userID, ModuleID)
	if err != nil {
		return DefaultOptions(), err
	}
	o := Options{Enabled: true}
	if strings.TrimSpace(raw) != "" && strings.TrimSpace(raw) != "{}" {
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			return DefaultOptions(), fmt.Errorf("assistant config: %w", err)
		}
	}
	o.applyDefaults()
	return o, nil
}

// SaveOptions writes the assistant configuration back to the panel row.
func SaveOptions(ctx context.Context, st *store.Store, userID string, o Options) error {
	o.applyDefaults()
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return st.SetModuleConfig(ctx, userID, ModuleID, string(raw))
}

// legacyTOML mirrors the standalone daemon's config.toml, for the one-shot
// import. Only the fields that still exist are carried over.
type legacyTOML struct {
	Instance struct {
		RespondToAll bool `toml:"respond_to_all"`
	} `toml:"instance"`
	Agent struct {
		Command        string            `toml:"command"`
		Args           []string          `toml:"args"`
		NewSessionArgs []string          `toml:"new_session_args"`
		ResumeArgs     []string          `toml:"resume_args"`
		ForkArgs       []string          `toml:"fork_args"`
		Workdir        string            `toml:"workdir"`
		SoftTimeout    string            `toml:"soft_timeout"`
		TaskTimeout    string            `toml:"task_timeout"`
		UnsetEnv       []string          `toml:"unset_env"`
		Env            map[string]string `toml:"env"`
	} `toml:"agent"`
	Telegram struct {
		PollTimeout    string `toml:"poll_timeout"`
		TypingInterval string `toml:"typing_interval"`
	} `toml:"telegram"`
}

// ImportLegacyTOML seeds the panel configuration from the standalone daemon's
// config.toml, once. It is a no-op when the panel row already holds a
// configuration or the file is absent, and it never overwrites a live setting.
func ImportLegacyTOML(ctx context.Context, st *store.Store, userID, legacyDir string) error {
	if strings.TrimSpace(legacyDir) == "" {
		return nil
	}
	raw, err := st.GetModuleConfig(ctx, userID, ModuleID)
	if err != nil {
		return err
	}
	if trimmed := strings.TrimSpace(raw); trimmed != "" && trimmed != "{}" {
		return nil
	}
	path := filepath.Join(legacyDir, "config.toml")
	if _, err := os.Stat(path); err != nil {
		return nil
	}

	var legacy legacyTOML
	if _, err := toml.DecodeFile(path, &legacy); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	o := Options{
		Enabled:      true,
		RespondToAll: legacy.Instance.RespondToAll,
		Agent: AgentOpts{
			Command:        legacy.Agent.Command,
			Args:           legacy.Agent.Args,
			NewSessionArgs: legacy.Agent.NewSessionArgs,
			ResumeArgs:     legacy.Agent.ResumeArgs,
			ForkArgs:       legacy.Agent.ForkArgs,
			Workdir:        legacy.Agent.Workdir,
			SoftTimeout:    legacy.Agent.SoftTimeout,
			TaskTimeout:    legacy.Agent.TaskTimeout,
			UnsetEnv:       legacy.Agent.UnsetEnv,
			Env:            legacy.Agent.Env,
		},
		Telegram: TelegramOpts{
			PollTimeout:    legacy.Telegram.PollTimeout,
			TypingInterval: legacy.Telegram.TypingInterval,
		},
	}
	if err := SaveOptions(ctx, st, userID, o); err != nil {
		return err
	}
	log.Printf("assistant: imported runner configuration from %s", path)
	return os.Rename(path, path+".imported")
}
