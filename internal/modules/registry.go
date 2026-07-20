package modules

import (
	"context"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Catalog entry for the panel.
type Info struct {
	ID          string
	Name        string
	Description string
}

// All known modules (static catalog).
var Catalog = []Info{
	{ID: "machine", Name: "Machine", Description: "Remote shell on your computers via takan-agent (outbound)."},
	{ID: "mercadona", Name: "Mercadona", Description: "Shopping cart tools for Mercadona (credentials in panel)."},
}

// Provider builds tools for enabled modules.
type Provider struct {
	Store   *store.Store
	Machine ToolFactory
	Mercadona ToolFactory
}

// ToolFactory produces tools when the module is enabled.
type ToolFactory func(ctx context.Context, userID string) []mcp.RegisteredTool

func (p *Provider) ToolsFor(ctx context.Context, userID string) []mcp.RegisteredTool {
	mods, err := p.Store.ListModules(ctx, userID)
	if err != nil {
		return nil
	}
	var out []mcp.RegisteredTool
	// Always expose a tiny meta tool
	out = append(out, metaTools(p)...)
	for _, m := range mods {
		if !m.Enabled {
			continue
		}
		switch m.ModuleID {
		case "machine":
			if p.Machine != nil {
				out = append(out, p.Machine(ctx, userID)...)
			}
		case "mercadona":
			if p.Mercadona != nil {
				out = append(out, p.Mercadona(ctx, userID)...)
			}
		}
	}
	return out
}

func metaTools(p *Provider) []mcp.RegisteredTool {
	return []mcp.RegisteredTool{{
		Tool: mcp.Tool{
			Name:        "takan_status",
			Description: "List which Takan modules are enabled for this account and basic readiness.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
			mods, err := p.Store.ListModules(ctx, userID)
			if err != nil {
				return "", err
			}
			s := "Takan modules:\n"
			for _, m := range mods {
				st := "OFF"
				if m.Enabled {
					st = "ON"
				}
				s += "- " + m.ModuleID + ": " + st + "\n"
			}
			s += "\nEnable/disable modules in the web panel."
			return s, nil
		},
	}}
}
