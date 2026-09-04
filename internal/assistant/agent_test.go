package assistant

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestChildEnvIsAnAllowlist is the security test for the merge. The assistant
// now shares a process with the hub, whose environment holds the session key,
// the mail key, the backup credentials and the app token. The CLI agent runs
// model-authored commands, so it must inherit an allowlist and nothing else.
func TestChildEnvIsAnAllowlist(t *testing.T) {
	secrets := map[string]string{
		"ATLAS_SESSION_KEY":     "must-not-leak",
		"TAKAN_SESSION_KEY":     "must-not-leak",
		"ATLAS_RESEND_API_KEY":  "re_must-not-leak",
		"TAKAN_RESEND_API_KEY":  "re_must-not-leak",
		"AWS_ACCESS_KEY_ID":     "AKIAMUSTNOTLEAK",
		"AWS_SECRET_ACCESS_KEY": "must-not-leak",
		"ATLAS_APP_TOKEN":       "must-not-leak",
		"TELEGRAM_BOT_TOKEN":    "123456:must-not-leak",
		"GROQ_API_KEY":          "gsk_must-not-leak",
		"XAI_API_KEY":           "xai-must-not-leak",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LANG", "en_US.UTF-8")

	opts := DefaultOptions()
	opts.Agent.Env = map[string]string{"ATLAS_TEST": "1"}
	env := NewAgent(opts.Agent, t.TempDir(), "/home/debian", time.Minute).childEnv()

	joined := strings.Join(env, "\n")
	for _, banned := range []string{
		"SESSION_KEY", "RESEND", "AWS_", "APP_TOKEN", "TELEGRAM_BOT_TOKEN",
		"GROQ_API_KEY", "XAI_API_KEY", "must-not-leak",
	} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%q leaked into the agent environment:\n%s", banned, joined)
		}
	}

	got := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
	}
	if got["HOME"] != "/home/debian" {
		t.Fatalf("HOME must be the agent home, got %q", got["HOME"])
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH must be inherited, got %q", got["PATH"])
	}
	if got["LANG"] != "en_US.UTF-8" {
		t.Fatalf("LANG must be inherited, got %q", got["LANG"])
	}
	if got["ATLAS_TEST"] != "1" {
		t.Fatal("a configured env addition must be applied")
	}
}

func TestChildEnvHonoursUnsetEnv(t *testing.T) {
	t.Setenv("TERM", "xterm")
	opts := DefaultOptions()
	opts.Agent.UnsetEnv = []string{"XAI_API_KEY", "TERM"}
	opts.Agent.Env = map[string]string{"TERM": "should-be-dropped-too"}

	for _, kv := range NewAgent(opts.Agent, t.TempDir(), "", time.Minute).childEnv() {
		if strings.HasPrefix(kv, "TERM=") {
			t.Fatalf("unset_env must win over both the allowlist and the additions: %q", kv)
		}
	}
}

func TestChildEnvSuppliesHomeWhenTheProcessHasNone(t *testing.T) {
	// systemd units without Environment=HOME leave the hub with no HOME. The
	// agent still needs one: that is where its credentials live.
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	env := NewAgent(DefaultOptions().Agent, t.TempDir(), "/var/lib/atlas", time.Minute).childEnv()
	found := false
	for _, kv := range env {
		if kv == "HOME=/var/lib/atlas" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the configured agent home must be supplied: %v", env)
	}
}

func TestBuildArgsPerMode(t *testing.T) {
	agent := NewAgent(DefaultOptions().Agent, t.TempDir(), "", time.Minute)
	joined := func(spec RunSpec) string { return strings.Join(agent.buildArgs(spec), " ") }

	got := joined(RunSpec{Prompt: "hi", SessionID: "S1", Mode: RunNew})
	if !strings.Contains(got, "--session-id S1") || strings.Contains(got, "--resume") {
		t.Fatalf("new session args wrong: %s", got)
	}
	got = joined(RunSpec{Prompt: "hi", SessionID: "S1", Mode: RunResume})
	if !strings.Contains(got, "--resume S1") || strings.Contains(got, "--fork-session") {
		t.Fatalf("resume args wrong: %s", got)
	}
	got = joined(RunSpec{Prompt: "hi", SessionID: "S2", ParentSession: "S1", Mode: RunFork})
	if !strings.Contains(got, "--resume S1") || !strings.Contains(got, "--fork-session") || !strings.Contains(got, "--session-id S2") {
		t.Fatalf("fork args wrong: %s", got)
	}
	// The legacy cwd-scoped continue flag must be gone.
	if strings.Contains(got, " -c") {
		t.Fatalf("fork args should not use the legacy -c flag: %s", got)
	}
	if !strings.Contains(got, "hi") {
		t.Fatalf("prompt was not substituted: %s", got)
	}
}

func TestLooksRateLimited(t *testing.T) {
	for _, yes := range []string{
		"Error: rate limit exceeded", "status 429", "HTTP 503 Service Unavailable",
		"529 overloaded", "Too Many Requests",
	} {
		if !looksRateLimited(yes) {
			t.Fatalf("%q should count as an upstream refusal", yes)
		}
	}
	for _, no := range []string{"", "permission denied", "no such file or directory"} {
		if looksRateLimited(no) {
			t.Fatalf("%q is a local failure, not a rate limit", no)
		}
	}
}

func TestConfigTimeoutDefaults(t *testing.T) {
	o := DefaultOptions()
	if got := o.SoftTimeout(); got != 60*time.Second {
		t.Fatalf("soft budget should default to 60s, got %s", got)
	}
	if got := o.TaskTimeout(); got != time.Hour {
		t.Fatalf("task timeout should default to 1h, got %s", got)
	}
	if o.Agent.MaxConcurrentTasks != 3 {
		t.Fatalf("background tasks share the hub's cgroup, so they are capped: %d", o.Agent.MaxConcurrentTasks)
	}
	if len(o.Agent.UnsetEnv) == 0 || o.Agent.UnsetEnv[0] != "XAI_API_KEY" {
		t.Fatalf("XAI_API_KEY must stay unset by default: %v", o.Agent.UnsetEnv)
	}
}

func TestReorderFlagsFirst(t *testing.T) {
	valueFlags := map[string]bool{"title": true, "prompt": true}

	// The bug this guards: a trailing --title must not land in the prompt.
	got := ReorderFlagsFirst([]string{"do the thing", "--title", "my label"}, valueFlags)
	want := []string{"--title", "my label", "do the thing"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %#v, want %#v", got, want)
	}

	// --flag=value form needs no lookahead.
	got = ReorderFlagsFirst([]string{"prompt text", "--title=label"}, valueFlags)
	if strings.Join(got, "|") != "--title=label|prompt text" {
		t.Fatalf("unexpected: %#v", got)
	}

	// Everything after "--" stays positional.
	got = ReorderFlagsFirst([]string{"--", "--not-a-flag"}, valueFlags)
	if strings.Join(got, "|") != "--not-a-flag" {
		t.Fatalf("unexpected: %#v", got)
	}

	// Already-correct order is preserved.
	got = ReorderFlagsFirst([]string{"--title", "x", "body"}, valueFlags)
	if strings.Join(got, "|") != "--title|x|body" {
		t.Fatalf("unexpected: %#v", got)
	}
}

func TestInboxPathIsSanitisedAndUnique(t *testing.T) {
	dir := t.TempDir()
	path := inboxPath(dir, "../../etc/passwd")
	if strings.Contains(path, "..") || !strings.HasPrefix(path, dir) {
		t.Fatalf("path escaped the inbox: %s", path)
	}
	if !strings.HasSuffix(path, "passwd") {
		t.Fatalf("unexpected name: %s", path)
	}
}
