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
	tvmod "github.com/kidandcat/takan/modules/tv"
)

// Catalog entry for the panel.
type Info struct {
	ID          string
	Name        string
	Description string
}

// All known modules (static catalog). Keep IDs in sync with store.defaultModuleIDs.
var Catalog = []Info{
	{ID: "assistant", Name: "Assistant", Description: "Your personal assistant: one Telegram bot, the phone app channel, background tasks and scheduled routines, driven by a CLI coding agent."},
	{ID: "machine", Name: "Machine", Description: "Remote shell + configurable AI task runners (Claude, Grok, free commands) via takan-agent."},
	{ID: "display", Name: "Display", Description: "Remote kiosk screens: push static HTML to a takan-agent that serves it locally."},
	{ID: "tv", Name: "TV", Description: "Samsung Tizen TV on the home LAN: status, apps, keys, volume, mute, power, now playing — via a takan-agent on the same WiFi."},
	{ID: "mercadona", Name: "Mercadona", Description: "Shopping cart tools for Mercadona (credentials in panel)."},
	{ID: "email", Name: "Email", Description: "Resend: send & read mail; enable domains from your account."},
	{ID: "people", Name: "People", Description: "People you know: relationships, context, notes (personal CRM)."},
	{ID: "health", Name: "Health", Description: "Personal health: profile, daily diary, injuries and conditions."},
	{ID: "vault", Name: "Vault", Description: "Password manager: encrypted logins + agent secret grants (approve in panel / mobile)."},
}

// Provider builds tools for enabled modules.
type Provider struct {
	Store *store.Store
	// Hub optional: used by takan_status for machine online counts.
	Hub *agenthub.Hub
	// MercadonaLinked optional: whether Mercadona session tokens exist for user.
	MercadonaLinked func(ctx context.Context, userID string) bool

	Assistant ToolFactory
	Machine   ToolFactory
	Mercadona ToolFactory
	Email     ToolFactory
	People    ToolFactory
	Health    ToolFactory
	Vault     ToolFactory
	Display   ToolFactory
	TV        ToolFactory

	// AssistantStatus optional: readiness detail for the assistant module.
	AssistantStatus func(ctx context.Context) (ready bool, detail string)
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
		case "assistant":
			if p.Assistant != nil {
				out = append(out, p.Assistant(ctx, userID)...)
			}
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
		case "people":
			if p.People != nil {
				out = append(out, p.People(ctx, userID)...)
			}
		case "health":
			if p.Health != nil {
				out = append(out, p.Health(ctx, userID)...)
			}
		case "vault":
			if p.Vault != nil {
				out = append(out, p.Vault(ctx, userID)...)
			}
		case "display":
			if p.Display != nil {
				out = append(out, p.Display(ctx, userID)...)
			}
		case "tv":
			if p.TV != nil {
				out = append(out, p.TV(ctx, userID)...)
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
				"(assistant, machines online, displays, TV, Mercadona linked, email domains, people, health, vault). " +
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

// StatusJSON returns takan_status payload for API/MCP.
func (p *Provider) StatusJSON(ctx context.Context, userID string) (string, error) {
	return p.statusJSON(ctx, userID)
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
	case "assistant":
		if p.AssistantStatus == nil {
			return false, "assistant not running (check TELEGRAM_BOT_TOKEN and OWNER_TELEGRAM_ID)"
		}
		return p.AssistantStatus(ctx)
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
		bash := "bash off"
		if cfg.BashEnabled {
			bash = "bash on"
		}
		ai := "AI tasks off"
		if cfg.AITasksEnabled {
			n := len(cfg.EnabledRunners())
			ai = fmt.Sprintf("AI tasks on (%d runners)", n)
		}
		detail = fmt.Sprintf("%d/%d online", online, len(ms))
		if len(names) > 0 && len(names) <= 4 {
			detail += " (" + strings.Join(names, ", ") + ")"
		}
		detail += "; " + bash + "; " + ai
		return online > 0, detail
	case "display":
		ds, err := p.Store.ListDisplays(ctx, userID)
		if err != nil {
			return false, "error listing displays"
		}
		if len(ds) == 0 {
			return false, "no screens registered"
		}
		online := 0
		var names []string
		for _, d := range ds {
			on := p.Hub != nil && p.Hub.Online(d.MachineID)
			if on {
				online++
				names = append(names, d.Name)
			}
		}
		detail = fmt.Sprintf("%d/%d online", online, len(ds))
		if len(names) > 0 && len(names) <= 4 {
			detail += " (" + strings.Join(names, ", ") + ")"
		}
		return online > 0, detail
	case "tv":
		cfg, err := tvmod.LoadConfig(ctx, p.Store, userID)
		if err != nil {
			return false, "error reading TV config"
		}
		if cfg.Host == "" || cfg.Machine == "" {
			return false, "not configured (panel → TV)"
		}
		mac, err := p.Store.MachineByUserAndName(ctx, userID, cfg.Machine)
		if err != nil {
			return false, "machine " + cfg.Machine + " not registered"
		}
		online := p.Hub != nil && p.Hub.Online(mac.ID)
		detail = cfg.Host + " via " + cfg.Machine
		if online {
			detail += " (agent online)"
		} else {
			detail += " (agent offline)"
		}
		return online, detail
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
	case "people":
		n, err := p.Store.CountPeople(ctx, userID)
		if err != nil {
			return false, "error counting people"
		}
		return true, fmt.Sprintf("%d people", n)
	case "health":
		prof, hasProf, err := p.Store.GetHealthProfile(ctx, userID)
		if err != nil {
			return false, "error reading health profile"
		}
		nLog, _ := p.Store.CountHealthLog(ctx, userID)
		nIss, _ := p.Store.CountHealthIssues(ctx, userID, "")
		nOpen, _ := p.Store.CountHealthIssues(ctx, userID, "recovering")
		nActive, _ := p.Store.CountHealthIssues(ctx, userID, "active")
		open := nOpen + nActive
		if !hasProf && nLog == 0 && nIss == 0 {
			return true, "empty"
		}
		bits := []string{}
		if prof.WeightKG != nil {
			bits = append(bits, fmt.Sprintf("%.1f kg", *prof.WeightKG))
		}
		if prof.HeightCM != nil {
			bits = append(bits, fmt.Sprintf("%.0f cm", *prof.HeightCM))
		}
		bits = append(bits, fmt.Sprintf("%d log days", nLog), fmt.Sprintf("%d open issues", open))
		return true, strings.Join(bits, " · ")
	case "vault":
		n, err := p.Store.CountVaultItems(ctx, userID)
		if err != nil {
			return false, "error counting vault items"
		}
		pending, _ := p.Store.CountVaultGrantsPending(ctx, userID)
		if n == 0 {
			detail := "empty"
			if pending > 0 {
				detail = fmt.Sprintf("empty · %d pending grant(s)", pending)
			}
			return true, detail
		}
		detail := fmt.Sprintf("%d login(s)", n)
		if pending > 0 {
			detail += fmt.Sprintf(" · %d pending grant(s)", pending)
		}
		return true, detail
	default:
		return true, "enabled"
	}
}
