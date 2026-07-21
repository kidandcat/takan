// Package email exposes Resend-backed email tools for Takan.
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns email_* tools. API key from panel; only user-enabled domains apply.
func Factory(st *store.Store, box *cryptox.Box) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "email_available_domains",
					Description: "List domains enabled for this account (from Resend, user toggles in panel). " +
						"Call before email_send / email_list if you do not know domain/sender — ask the user.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					_, domains, ok, err := st.GetEmailSettings(ctx, userID)
					if err != nil {
						return "", err
					}
					enabled := store.EnabledEmailDomains(domains)
					if !ok || len(enabled) == 0 {
						return "", fmt.Errorf("no enabled domains — configure Resend API key and enable domains in Takan panel → Email")
					}
					var detail []map[string]any
					for _, d := range domains {
						if !d.Enabled {
							continue
						}
						detail = append(detail, map[string]any{
							"name": d.Name, "status": d.Status,
							"sending": d.Sending, "receiving": d.Receiving,
						})
					}
					return marshal(map[string]any{
						"domains": detail,
						"hint":    "email_send needs domain + sender (local part). Ask the user if unsure.",
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "email_send",
					Description: "Send email via Resend. Requires domain + sender (local part). " +
						"Domain must be enabled in the panel. Example: domain=example.com sender=hello → hello@example.com. " +
						"If unknown, call email_available_domains and ask the user.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"domain":  map[string]any{"type": "string", "description": "Enabled domain (e.g. example.com)"},
							"sender":  map[string]any{"type": "string", "description": "Local part (e.g. hello, noreply)"},
							"to":      map[string]any{"type": "string"},
							"subject": map[string]any{"type": "string"},
							"body":    map[string]any{"type": "string"},
							"html":    map[string]any{"type": "string"},
						},
						"required": []string{"domain", "sender", "to", "subject", "body"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					apiKey, enabled, err := loadKeyAndEnabled(ctx, st, box, userID)
					if err != nil {
						return "", err
					}
					domain, _ := args["domain"].(string)
					sender, _ := args["sender"].(string)
					to, _ := args["to"].(string)
					subject, _ := args["subject"].(string)
					body, _ := args["body"].(string)
					html, _ := args["html"].(string)
					from, err := buildFrom(domain, sender, enabled)
					if err != nil {
						return "", err
					}
					to = strings.TrimSpace(to)
					subject = strings.TrimSpace(subject)
					if to == "" || subject == "" {
						return "", fmt.Errorf("to and subject are required")
					}
					id, err := sendResend(ctx, apiKey, from, to, subject, body, html)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "sent", "id": id, "to": to, "from": from})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "email_list",
					Description: "List recently received emails for enabled domains (Resend receiving). " +
						"Only messages to/from enabled domains are returned. Use email_get for full body.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"limit": map[string]any{"type": "integer", "description": "Max items (default 20, max 100)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					apiKey, enabled, err := loadKeyAndEnabled(ctx, st, box, userID)
					if err != nil {
						return "", err
					}
					limit := 20
					if v, ok := args["limit"].(float64); ok && v > 0 {
						limit = int(v)
					}
					list, err := listReceived(ctx, apiKey, limit)
					if err != nil {
						return "", err
					}
					filtered := filterReceivedByDomains(list, enabled)
					// compact rows
					var rows []map[string]any
					for _, it := range filtered {
						rows = append(rows, map[string]any{
							"id": it["id"], "from": it["from"], "to": it["to"],
							"subject": it["subject"], "created_at": it["created_at"],
						})
					}
					return marshal(map[string]any{
						"count":   len(rows),
						"domains": enabled,
						"emails":  rows,
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "email_get",
					Description: "Fetch full received email by id (from email_list). " +
						"Only allowed if the message belongs to an enabled domain.",
					InputSchema: map[string]any{
						"type":       "object",
						"properties": map[string]any{"id": map[string]any{"type": "string"}},
						"required":   []string{"id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					apiKey, enabled, err := loadKeyAndEnabled(ctx, st, box, userID)
					if err != nil {
						return "", err
					}
					id, _ := args["id"].(string)
					msg, err := getReceived(ctx, apiKey, id)
					if err != nil {
						return "", err
					}
					if !addressesMatchEnabled(msg["to"], enabled) && !addressesMatchEnabled(msg["from"], enabled) {
						return "", fmt.Errorf("email not on an enabled domain (enabled: %s)", strings.Join(enabled, ", "))
					}
					return marshal(msg)
				},
			},
		}
	}
}

func loadKeyAndEnabled(ctx context.Context, st *store.Store, box *cryptox.Box, userID string) (apiKey string, enabled []string, err error) {
	keyEnc, domains, ok, err := st.GetEmailSettings(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("email not configured — open Takan panel → Email and save your Resend API key")
	}
	enabled = store.EnabledEmailDomains(domains)
	if len(enabled) == 0 {
		return "", nil, fmt.Errorf("no domains enabled — open panel → Email and enable at least one domain")
	}
	apiKey, err = box.Open(keyEnc)
	if err != nil {
		return "", nil, fmt.Errorf("decrypt api key: %w", err)
	}
	return apiKey, enabled, nil
}

func buildFrom(domain, sender string, allowed []string) (string, error) {
	domain = normalizeDomain(domain)
	sender = strings.TrimSpace(sender)
	if domain == "" {
		return "", fmt.Errorf("domain required — call email_available_domains and ask the user")
	}
	if sender == "" {
		return "", fmt.Errorf("sender required — local part (e.g. hello). Ask the user if unsure")
	}
	if strings.Contains(sender, "@") {
		parts := strings.SplitN(sender, "@", 2)
		sender = strings.TrimSpace(parts[0])
		sd := normalizeDomain(parts[1])
		if sd != "" && sd != domain {
			return "", fmt.Errorf("sender domain %q does not match domain %q", sd, domain)
		}
	}
	sender = strings.Trim(sender, "<>\"' ")
	if sender == "" || strings.ContainsAny(sender, " @<>") {
		return "", fmt.Errorf("invalid sender local part %q", sender)
	}
	ok := false
	for _, d := range allowed {
		if d == domain {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("domain %q not enabled; available: %s — ask the user",
			domain, strings.Join(allowed, ", "))
	}
	return sender + "@" + domain, nil
}

func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "@")
	return strings.TrimSuffix(d, ".")
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
