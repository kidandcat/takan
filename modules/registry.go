package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
	machinemod "github.com/kidandcat/takan/modules/machine"
)

// Catalog entry for the panel.
type Info struct {
	ID          string
	Name        string
	Description string
}

// All known modules (static catalog). Keep IDs in sync with store.defaultModuleIDs.
var Catalog = []Info{
	{ID: "machine", Name: "Machine", Description: "Remote shell + configurable AI task runners (Claude, Grok, free commands) via takan-agent."},
	{ID: "mercadona", Name: "Mercadona", Description: "Shopping cart tools for Mercadona (credentials in panel)."},
	{ID: "email", Name: "Email", Description: "Resend: send & read mail; enable domains from your account."},
	{ID: "memory", Name: "Memory", Description: "Short-lived working memory for your AI client (per account)."},
	{ID: "people", Name: "People", Description: "People you know: relationships, context, notes (personal CRM)."},
}

// Provider builds tools for enabled modules.
type Provider struct {
	Store *store.Store
	// Hub optional: used by takan_status for machine online counts.
	Hub *agenthub.Hub
	// MercadonaLinked optional: whether Mercadona session tokens exist for user.
	MercadonaLinked func(ctx context.Context, userID string) bool

	Machine   ToolFactory
	Mercadona ToolFactory
	Email     ToolFactory
	Memory    ToolFactory
	People    ToolFactory
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
		case "people":
			if p.People != nil {
				out = append(out, p.People(ctx, userID)...)
			}
		}
	}
	return out
}

func metaTools(p *Provider) []mcp.RegisteredTool {
	return []mcp.RegisteredTool{{
		Tool: mcp.Tool{
			Name: "takan_status",
			Description: "Overview of all Takan modules for this account: enabled/off and readiness " +
				"(machines online, Mercadona linked, email domains, memory, people). " +
				"Use this instead of per-module status tools.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
			return p.statusJSON(ctx, userID)
		},
	}}
}

type moduleStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`
	Detail  string `json:"detail"`
}

func (p *Provider) statusJSON(ctx context.Context, userID string) (string, error) {
	mods, err := p.Store.ListModules(ctx, userID)
	if err != nil {
		return "", err
	}
	cat := map[string]string{}
	for _, c := range Catalog {
		cat[c.ID] = c.Name
	}
	var rows []moduleStatus
	for _, m := range mods {
		name := cat[m.ModuleID]
		if name == "" {
			name = m.ModuleID
		}
		row := moduleStatus{ID: m.ModuleID, Name: name, Enabled: m.Enabled}
		if !m.Enabled {
			row.Detail = "module off"
			rows = append(rows, row)
			continue
		}
		row.Ready, row.Detail = p.moduleReadiness(ctx, userID, m.ModuleID)
		rows = append(rows, row)
	}
	b, err := json.MarshalIndent(map[string]any{
		"modules": rows,
		"hint":    "Enable/configure modules in the Takan web panel.",
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Provider) moduleReadiness(ctx context.Context, userID, moduleID string) (ready bool, detail string) {
	switch moduleID {
	case "machine":
		ms, err := p.Store.ListMachines(ctx, userID)
		if err != nil {
			return false, "error listing machines"
		}
		if len(ms) == 0 {
			return false, "no machines registered"
		}
		online := 0
		var names []string
		for _, mac := range ms {
			on := p.Hub != nil && p.Hub.Online(mac.ID)
			if on {
				online++
				names = append(names, mac.Name)
			}
		}
		cfg, _ := machinemod.LoadConfig(ctx, p.Store, userID)
		ai := "AI tasks off"
		if cfg.AITasksEnabled {
			n := len(cfg.EnabledRunners())
			ai = fmt.Sprintf("AI tasks on (%d runners)", n)
		}
		detail = fmt.Sprintf("%d/%d online", online, len(ms))
		if len(names) > 0 && len(names) <= 4 {
			detail += " (" + strings.Join(names, ", ") + ")"
		}
		detail += "; " + ai
		return online > 0, detail
	case "mercadona":
		email, _, postal, ok, err := p.Store.GetMercadonaCreds(ctx, userID)
		if err != nil {
			return false, "error reading credentials"
		}
		linked := false
		if p.MercadonaLinked != nil {
			linked = p.MercadonaLinked(ctx, userID)
		}
		if !ok && !linked {
			return false, "not configured (panel → Mercadona)"
		}
		if !linked {
			return false, fmt.Sprintf("creds saved (%s, CP %s) but session not linked — re-save", email, postal)
		}
		return true, fmt.Sprintf("linked %s · CP %s", email, postal)
	case "email":
		_, domains, ok, err := p.Store.GetEmailSettings(ctx, userID)
		if err != nil {
			return false, "error reading email settings"
		}
		if !ok {
			return false, "no Resend API key"
		}
		en := store.EnabledEmailDomains(domains)
		if len(en) == 0 {
			return false, fmt.Sprintf("key set, 0 domains enabled (%d discovered)", len(domains))
		}
		return true, fmt.Sprintf("%d enabled domain(s): %s", len(en), strings.Join(en, ", "))
	case "memory":
		content, updated, ok, err := p.Store.GetMemory(ctx, userID)
		if err != nil {
			return false, "error reading memory"
		}
		if !ok || strings.TrimSpace(content) == "" {
			return true, "empty"
		}
		detail = fmt.Sprintf("%d chars", len(content))
		if !updated.IsZero() {
			detail += ", updated " + updated.UTC().Format("2006-01-02")
		}
		return true, detail
	case "people":
		n, err := p.Store.CountPeople(ctx, userID)
		if err != nil {
			return false, "error counting people"
		}
		return true, fmt.Sprintf("%d people", n)
	default:
		return true, "enabled"
	}
}
