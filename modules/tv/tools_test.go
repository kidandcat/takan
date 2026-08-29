package tv

import (
	"context"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

func TestFactoryToolWiring(t *testing.T) {
	st, userID := testUser(t)
	tools := Factory(st, agenthub.New(nil, nil))(context.Background(), userID)
	got := toolNames(tools)
	want := []string{"tv_status", "tv_app", "tv_key", "tv_text"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools: %v want %v", got, want)
	}
	required := map[string][]string{}
	byName := map[string]string{}
	for _, tl := range tools {
		byName[tl.Name] = tl.Description
		if req, ok := tl.InputSchema["required"].([]string); ok {
			required[tl.Name] = req
		}
	}
	for _, name := range want {
		desc := strings.ToLower(byName[name])
		if !strings.Contains(desc, "takan-agent") && !strings.Contains(desc, "lan") {
			t.Fatalf("%s should say it runs on the LAN agent: %s", name, byName[name])
		}
	}
	mustContain(t, required["tv_app"], "action", "app")
	mustContain(t, required["tv_key"], "key")
	mustContain(t, required["tv_text"], "text")
}

func TestHandlersValidateBeforeAgent(t *testing.T) {
	st, userID := testUser(t)
	ctx := context.Background()
	if _, _, err := st.CreateMachine(ctx, userID, "mac"); err != nil {
		t.Fatal(err)
	}
	hub := agenthub.New(nil, nil)
	handlers := map[string]func(context.Context, string, map[string]any) (string, error){}
	for _, tl := range Factory(st, hub)(ctx, userID) {
		handlers[tl.Name] = tl.Handler
	}

	if _, err := handlers["tv_app"](ctx, userID, map[string]any{"action": "launch"}); err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("missing app: %v", err)
	}
	if _, err := handlers["tv_app"](ctx, userID, map[string]any{"action": "dance", "app": "Netflix"}); err == nil || !strings.Contains(err.Error(), "launch or close") {
		t.Fatalf("bad action: %v", err)
	}
	if _, err := handlers["tv_key"](ctx, userID, map[string]any{}); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := handlers["tv_status"](ctx, userID, map[string]any{"machine": "nope"}); err == nil || !strings.Contains(err.Error(), "unknown machine") {
		t.Fatalf("unknown machine: %v", err)
	}
	if _, err := handlers["tv_status"](ctx, userID, map[string]any{"host": "192.168.1.1;id"}); err == nil || !strings.Contains(err.Error(), "invalid TV host") {
		t.Fatalf("bad host override: %v", err)
	}
	// Registered machine but agent offline — hub.RunBash, not a hub-side TV socket.
	if _, err := handlers["tv_status"](ctx, userID, map[string]any{}); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline mac: %v", err)
	}
}

func TestFormatAgent(t *testing.T) {
	got := formatAgent(DefaultConfig(), &agenthub.Result{
		ExitCode: 0,
		Stdout:   `{"device":{"PowerState":"on"}}`,
	})
	if !strings.Contains(got, `"via": "takan-agent mac"`) {
		t.Fatal(got)
	}
	if !strings.Contains(got, `"PowerState"`) {
		t.Fatal(got)
	}
	fail := formatAgent(DefaultConfig(), &agenthub.Result{ExitCode: 7, Stderr: "failed"})
	if !strings.Contains(fail, "Allow?") {
		t.Fatal(fail)
	}
}

func testUser(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	u, err := st.CreateUserOpts(context.Background(), "tv-tools@example.com", "password1", store.CreateUserOpts{AllowOpen: true})
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
