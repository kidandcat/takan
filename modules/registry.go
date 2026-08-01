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
	{ID: "people", Name: "People", Description: "People you know: relationships, context, notes (personal CRM)."},
	{ID: "health", Name: "Health", Description: "Personal health: profile, daily diary, injuries and conditions."},
	{ID: "telegram", Name: "Telegram", Description: "Send messages via your Telegram bot (token + allowed chats in panel)."},
	{ID: "sip", Name: "SIP", Description: "Android SIM gateways → Grok Voice. Central proxy; phones connect outbound only."},
	{ID: "vault", Name: "Vault", Description: "Password manager: encrypted logins + agent secret grants (approve in panel / mobile)."},
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
	People    ToolFactory
	Health    ToolFactory
	Telegram  ToolFactory
	SIP       ToolFactory
	Vault     ToolFactory

	// SIPHub optional: online device / call counts for takan_status.
	SIPHub interface {
		OnlineCount(userID string) int
		CallsSnapshot(userID string) []map[string]any
	}
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
		case "people":
			if p.People != nil {
				out = append(out, p.People(ctx, userID)...)
			}
		case "health":
			if p.Health != nil {
				out = append(out, p.Health(ctx, userID)...)
			}
		case "telegram":
			if p.Telegram != nil {
				out = append(out, p.Telegram(ctx, userID)...)
			}
		case "sip":
			if p.SIP != nil {
				out = append(out, p.SIP(ctx, userID)...)
			}
		case "vault":
			if p.Vault != nil {
				out = append(out, p.Vault(ctx, userID)...)
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
				"(machines online, Mercadona linked, email domains, people, health, telegram, SIP, vault). " +
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
	case "telegram":
		ts, ok, err := p.Store.GetTelegramSettings(ctx, userID)
		if err != nil {
			return false, "error reading telegram settings"
		}
		if !ok {
			return false, "not configured (panel → Telegram)"
		}
		bot := strings.TrimPrefix(ts.BotUsername, "@")
		if bot == "" {
			bot = "bot"
		}
		if strings.TrimSpace(ts.DefaultChatID) == "" && len(ts.AllowedChats) == 0 {
			return false, fmt.Sprintf("@%s · no chats", bot)
		}
		n := len(ts.AllowedChats)
		if n == 0 && ts.DefaultChatID != "" {
			n = 1
		}
		detail := fmt.Sprintf("@%s · %d chat(s)", bot, n)
		if ts.DefaultChatID != "" {
			detail += " · default " + ts.DefaultChatID
		}
		return true, detail
	case "sip":
		settings, ok, err := p.Store.GetSIPSettings(ctx, userID)
		if err != nil {
			return false, "error reading sip settings"
		}
		nDev, _ := p.Store.CountSIPDevices(ctx, userID)
		if !ok || !settings.HasKey {
			return false, "not configured (panel → SIP: xAI API key)"
		}
		if nDev == 0 {
			return false, "API key set · no phone gateways"
		}
		online := 0
		nCalls := 0
		if p.SIPHub != nil {
			online = p.SIPHub.OnlineCount(userID)
			nCalls = len(p.SIPHub.CallsSnapshot(userID))
		}
		detail := fmt.Sprintf("%d/%d online · voice %s", online, nDev, settings.Voice)
		if nCalls > 0 {
			detail += fmt.Sprintf(" · %d call(s)", nCalls)
		}
		return online > 0 || nDev > 0, detail
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
