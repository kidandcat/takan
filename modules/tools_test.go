package modules

import (
	"context"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules/tv"
)

func TestToolsForTV(t *testing.T) {
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "tv-reg@example.com", "password1")
	if err != nil {
		t.Fatal(err)
	}
	hub := agenthub.New(nil, nil)
	p := &Provider{
		Store: st,
		Hub:   hub,
		TV:    tv.Factory(st, hub),
	}

	off := namesOf(p.ToolsFor(ctx, u.ID))
	for _, n := range off {
		if strings.HasPrefix(n, "tv_") {
			t.Fatalf("tv tools while disabled: %v", off)
		}
	}

	if err := st.SetModuleEnabled(ctx, u.ID, "tv", true); err != nil {
		t.Fatal(err)
	}
	on := namesOf(p.ToolsFor(ctx, u.ID))
	for _, need := range []string{"takan_status", "tv_status", "tv_app", "tv_key", "tv_text", "tv_volume", "tv_mute", "tv_power", "tv_now"} {
		found := false
		for _, n := range on {
			if n == need {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", need, on)
		}
	}
}

func namesOf(tools []mcp.RegisteredTool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}
