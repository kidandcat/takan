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

// All known modules (static catalog). Keep IDs in sync with store.defaultModuleIDs.
var Catalog = []Info{
	{ID: "machine", Name: "Machine", Description: "Remote shell on your computers via takan-agent (outbound)."},
	{ID: "mercadona", Name: "Mercadona", Description: "Shopping cart tools for Mercadona (credentials in panel)."},
	{ID: "email", Name: "Email", Description: "Send email via Resend (you bring the API key)."},
	{ID: "memory", Name: "Memory", Description: "Short-lived working memory for your AI client (per account)."},
	{ID: "files", Name: "Files", Description: "Upload files/images to public object storage and share URLs."},
}

// Provider builds tools for enabled modules.
type Provider struct {
	Store     *store.Store
	Machine   ToolFactory
	Mercadona ToolFactory
	Email     ToolFactory
	Memory    ToolFactory
	Files     ToolFactory
}

// ToolFactory produces tools when the module is enabled.
type ToolFactory func(ctx context.Context, userID string) []mcp.RegisteredTool

func (p *Provider) ToolsFor(ctx context.Context, userID string) []mcp.RegisteredTool {
	mods, err := p.Store.ListModules(ctx, userID)
	if err != nil {
		return nil
	}
	var out []mcp.RegisteredTool
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
		case "email":
			if p.Email != nil {
				out = append(out, p.Email(ctx, userID)...)
			}
		case "memory":
			if p.Memory != nil {
				out = append(out, p.Memory(ctx, userID)...)
			}
		case "files":
			if p.Files != nil {
				out = append(out, p.Files(ctx, userID)...)
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
