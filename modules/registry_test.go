package modules

import (
	"context"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules/display"
	"github.com/kidandcat/takan/modules/email"
	"github.com/kidandcat/takan/modules/health"
	"github.com/kidandcat/takan/modules/machine"
	"github.com/kidandcat/takan/modules/people"
	"github.com/kidandcat/takan/modules/tv"
	"github.com/kidandcat/takan/modules/vault"
)

func TestCatalogShape(t *testing.T) {
	ids := map[string]string{}
	for _, c := range Catalog {
		if c.Name == "" || c.Description == "" {
			t.Fatalf("catalog entry %q needs a name and a description", c.ID)
		}
		if _, dup := ids[c.ID]; dup {
			t.Fatalf("duplicate catalog id %q", c.ID)
		}
		ids[c.ID] = c.Name
	}
	for _, want := range []string{"machine", "display", "tv", "mercadona", "email", "people", "health", "vault"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("module %q missing from the catalog", want)
		}
	}
	// The bot fleet, the Telegram channel layer, SIP and the Atlas assistant
	// were all retired: Takan is the MCP hub and its panel.
	for _, gone := range []string{"bots", "telegram", "sip", "assistant"} {
		if _, ok := ids[gone]; ok {
			t.Fatalf("module %q should be gone from the catalog", gone)
		}
	}
}

func newProviderOwner(t *testing.T) (*store.Store, string, context.Context) {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return st, owner.ID, ctx
}

// TestRetiredToolsAreGone pins the surface other agents see with EVERY module
// on: the bot registry, the channel picker and the assistant no longer exist.
func TestRetiredToolsAreGone(t *testing.T) {
	st, userID, ctx := newProviderOwner(t)
	box, err := cryptox.NewBox("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	hub := agenthub.New(nil, nil)
	p := &Provider{
		Store:   st,
		Hub:     hub,
		Machine: machine.Factory(st, hub, nil),
		Email:   email.Factory(st, box),
		People:  people.Factory(st),
		Health:  health.Factory(st),
		Vault:   vault.Factory(st, box),
		Display: display.Factory(st, hub),
		TV:      tv.Factory(st, hub),
	}
	for _, c := range Catalog {
		if err := st.SetModuleEnabled(ctx, userID, c.ID, true); err != nil {
			t.Fatal(err)
		}
	}

	names := namesOf(p.ToolsFor(ctx, userID))
	if len(names) < 20 {
		t.Fatalf("expected a full tool list, got %v", names)
	}
	for _, name := range names {
		for _, prefix := range []string{"bots_", "telegram_", "assistant_", "task_", "sip_"} {
			if strings.HasPrefix(name, prefix) {
				t.Fatalf("retired tool %q is still exposed", name)
			}
		}
	}
	// machine_ai_run routed its result through a Telegram chat; nothing does now.
	for _, tl := range p.ToolsFor(ctx, userID) {
		props, _ := tl.InputSchema["properties"].(map[string]any)
		if _, ok := props["chat_id"]; ok {
			t.Fatalf("%s still takes a chat_id", tl.Name)
		}
	}
}

// TestStatusHasNoAssistantRow: takan_status is how another agent discovers what
// this hub can do, so a module that no longer exists must not be listed — even
// if an old database still carries the row.
func TestStatusHasNoAssistantRow(t *testing.T) {
	st, userID, ctx := newProviderOwner(t)
	// Simulate a database that predates the removal.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT OR IGNORE INTO user_modules (user_id, module_id, enabled) VALUES (?, 'assistant', 1)`,
		userID); err != nil {
		t.Fatal(err)
	}

	p := &Provider{Store: st}
	if names := namesOf(p.ToolsFor(ctx, userID)); len(names) != 1 || names[0] != "takan_status" {
		t.Fatalf("a stale assistant row must produce no tools, got %v", names)
	}
	status, err := p.StatusJSON(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status, `"assistant"`) {
		t.Fatalf("status still lists the assistant:\n%s", status)
	}
}
