package machine

import (
	"context"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

func TestFactoryAIToolWiring(t *testing.T) {
	st, userID := testUser(t)
	ctx := context.Background()
	hub := agenthub.New(nil, nil)
	tools := Factory(st, hub, nil)(ctx, userID)

	want := []string{
		"machine_list",
		"machine_bash",
		"machine_ai_runners",
		"machine_ai_run",
		"machine_ai_status",
		"machine_ai_watch",
		"machine_ai_log",
		"machine_ai_cancel",
		"machine_ai_reply",
	}
	got := toolNames(tools)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools: %v want %v", got, want)
	}

	byName := map[string]string{}
	required := map[string][]string{}
	for _, tl := range tools {
		byName[tl.Name] = tl.Description
		if req, ok := tl.InputSchema["required"].([]string); ok {
			required[tl.Name] = req
		}
	}

	runDesc := strings.ToLower(byName["machine_ai_run"])
	for _, banned := range []string{"fire and forget", "do not wait", "do not wait for completion", "no need to wait or poll"} {
		if strings.Contains(runDesc, banned) {
			t.Fatalf("machine_ai_run still tells clients not to follow: %q in %s", banned, byName["machine_ai_run"])
		}
	}
	for _, need := range []string{"machine_ai_watch", "machine_ai_log", "machine_ai_cancel", "machine_ai_reply"} {
		if !strings.Contains(byName["machine_ai_run"], need) {
			t.Fatalf("machine_ai_run should mention %s", need)
		}
	}
	if !strings.Contains(byName["machine_ai_reply"], "NEW job") && !strings.Contains(byName["machine_ai_reply"], "new job") {
		t.Fatal("reply must label that it starts a new job")
	}
	if !strings.Contains(strings.ToLower(byName["machine_ai_reply"]), "one-shot") &&
		!strings.Contains(strings.ToLower(byName["machine_ai_reply"]), "cannot attach") {
		t.Fatal("reply must label the one-shot / no-interrupt limitation")
	}

	mustContain(t, required["machine_ai_run"], "machine", "runner", "prompt")
	mustContain(t, required["machine_ai_watch"], "machine", "job_id")
	mustContain(t, required["machine_ai_log"], "machine", "job_id")
	mustContain(t, required["machine_ai_cancel"], "machine", "job_id")
	mustContain(t, required["machine_ai_reply"], "machine", "job_id", "message")
	mustContain(t, required["machine_ai_status"], "machine")
}

func TestFactoryAIDisabledHidesFollowTools(t *testing.T) {
	st, userID := testUser(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.AITasksEnabled = false
	if err := SaveConfig(ctx, st, userID, cfg); err != nil {
		t.Fatal(err)
	}
	tools := Factory(st, agenthub.New(nil, nil), nil)(ctx, userID)
	got := toolNames(tools)
	for _, name := range got {
		if strings.HasPrefix(name, "machine_ai_") {
			t.Fatalf("AI tools leaked while disabled: %v", got)
		}
	}
	if strings.Join(got, ",") != "machine_list,machine_bash" {
		t.Fatalf("got %v", got)
	}
}

func TestFactoryHandlersValidateMachineAndJob(t *testing.T) {
	st, userID := testUser(t)
	ctx := context.Background()
	if _, _, err := st.CreateMachine(ctx, userID, "mac"); err != nil {
		t.Fatal(err)
	}
	hub := agenthub.New(nil, nil)
	tools := Factory(st, hub, nil)(ctx, userID)
	handlers := map[string]func(context.Context, string, map[string]any) (string, error){}
	for _, tl := range tools {
		handlers[tl.Name] = tl.Handler
	}

	if _, err := handlers["machine_ai_watch"](ctx, userID, map[string]any{"machine": "nope", "job_id": "j"}); err == nil || !strings.Contains(err.Error(), "unknown machine") {
		t.Fatalf("watch unknown machine: %v", err)
	}
	if _, err := handlers["machine_ai_cancel"](ctx, userID, map[string]any{"machine": "mac"}); err == nil || !strings.Contains(err.Error(), "job_id") {
		t.Fatalf("cancel missing job: %v", err)
	}
	if _, err := handlers["machine_ai_log"](ctx, userID, map[string]any{"machine": "mac"}); err == nil || !strings.Contains(err.Error(), "job_id") {
		t.Fatalf("log missing job: %v", err)
	}
	if _, err := handlers["machine_ai_reply"](ctx, userID, map[string]any{"machine": "mac", "job_id": "j"}); err == nil || !strings.Contains(err.Error(), "message") {
		t.Fatalf("reply missing message: %v", err)
	}
	// Online check happens after local validation — offline machine is an agent-hub error.
	if _, err := handlers["machine_ai_run"](ctx, userID, map[string]any{
		"machine": "mac", "runner": "grok", "prompt": "hi",
	}); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("run offline: %v", err)
	}
}

func TestBuildContinuePrompt(t *testing.T) {
	got := BuildContinuePrompt("fix the bug", "compiled ok", "also add tests")
	for _, need := range []string{"Previous prompt", "fix the bug", "Previous output", "compiled ok", "Follow-up", "also add tests"} {
		if !strings.Contains(got, need) {
			t.Fatalf("missing %q in:\n%s", need, got)
		}
	}
	long := strings.Repeat("z", replyLogContext+50)
	got = BuildContinuePrompt("p", long, "go")
	if strings.Contains(got, strings.Repeat("z", replyLogContext+1)) {
		t.Fatal("expected parent log to be capped")
	}
}

func TestIntArg(t *testing.T) {
	args := map[string]any{"timeout_seconds": float64(12), "tail_bytes": float64(999999)}
	if got := intArg(args, "timeout_seconds", 90, 300); got != 12 {
		t.Fatalf("got %d", got)
	}
	if got := intArg(args, "tail_bytes", 12_000, 100_000); got != 100_000 {
		t.Fatalf("cap: %d", got)
	}
	if got := intArg(args, "missing", 5, 10); got != 5 {
		t.Fatalf("default: %d", got)
	}
}

func testUser(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	u, err := st.CreateUserOpts(context.Background(), "mcp-tools@example.com", "password1", store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	return st, u.ID
}

func toolNames(tools []mcp.RegisteredTool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}

func mustContain(t *testing.T, have []string, need ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, s := range have {
		set[s] = true
	}
	for _, n := range need {
		if !set[n] {
			t.Fatalf("required %q not in %v", n, have)
		}
	}
}
