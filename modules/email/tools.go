// Package email exposes Resend-backed email tools for Takan.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns email_* tools. API key + allowed domains are per-user (panel).
func Factory(st *store.Store, box *cryptox.Box) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "email_available_domains",
					Description: "List Resend domains configured for this account. " +
						"Call this before email_send if you do not know which domain/sender to use — " +
						"then ask the user which domain and local sender name they want.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					_, domains, ok, err := st.GetEmailSettings(ctx, userID)
					if err != nil {
						return "", err
					}
					if !ok || len(domains) == 0 {
						return "", fmt.Errorf("email not configured — open Takan panel → Email, add Resend API key and domains")
					}
					return marshal(map[string]any{
						"domains": domains,
						"hint":    "Use email_send with domain + sender (local part, e.g. hello). Ask the user if unsure.",
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "email_send",
					Description: "Send an email via Resend. Requires domain and sender (local part). " +
						"If domain/sender are unknown, call email_available_domains and ask the user. " +
						"Example: domain=example.com sender=hello → From: hello@example.com",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"domain": map[string]any{
								"type":        "string",
								"description": "Allowed sending domain from email_available_domains (e.g. example.com)",
							},
							"sender": map[string]any{
								"type":        "string",
								"description": "Local part of From (e.g. hello or noreply). Not a full address.",
							},
							"to":      map[string]any{"type": "string", "description": "Recipient email"},
							"subject": map[string]any{"type": "string"},
							"body":    map[string]any{"type": "string", "description": "Plain-text body"},
							"html":    map[string]any{"type": "string", "description": "Optional HTML body"},
						},
						"required": []string{"domain", "sender", "to", "subject", "body"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					keyEnc, domains, ok, err := st.GetEmailSettings(ctx, userID)
					if err != nil {
						return "", err
					}
					if !ok || len(domains) == 0 {
						return "", fmt.Errorf("email not configured — open Takan panel → Email and save Resend API key + domains")
					}
					apiKey, err := box.Open(keyEnc)
					if err != nil {
						return "", fmt.Errorf("decrypt api key: %w", err)
					}
					domain, _ := args["domain"].(string)
					sender, _ := args["sender"].(string)
					to, _ := args["to"].(string)
					subject, _ := args["subject"].(string)
					body, _ := args["body"].(string)
					html, _ := args["html"].(string)

					from, err := buildFrom(domain, sender, domains)
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
					return marshal(map[string]any{
						"status": "sent",
						"id":     id,
						"to":     to,
						"from":   from,
						"domain": normalizeDomain(domain),
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name:        "email_status",
					Description: "Check whether Resend email is configured and list allowed domains.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					_, domains, ok, err := st.GetEmailSettings(ctx, userID)
					if err != nil {
						return "", err
					}
					if !ok || len(domains) == 0 {
						return "Email module ON but not configured. Add Resend API key and domains in the panel.", nil
					}
					return marshal(map[string]any{
						"status":  "ready",
						"domains": domains,
					})
				},
			},
		}
	}
}

func buildFrom(domain, sender string, allowed []string) (string, error) {
	domain = normalizeDomain(domain)
	sender = strings.TrimSpace(sender)
	if domain == "" {
		return "", fmt.Errorf("domain required — call email_available_domains and ask the user which domain to use")
	}
	if sender == "" {
		return "", fmt.Errorf("sender required — local part only (e.g. hello). Ask the user if unsure")
	}
	// If AI passed full email as sender, extract local + verify domain.
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
		return "", fmt.Errorf("domain %q is not allowed; available: %s — ask the user which to use",
			domain, strings.Join(allowed, ", "))
	}
	return sender + "@" + domain, nil
}

func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "@")
	d = strings.TrimSuffix(d, ".")
	return d
}

func sendResend(ctx context.Context, apiKey, from, to, subject, text, html string) (string, error) {
	payload := map[string]any{
		"from":    from,
		"to":      []string{to},
		"subject": subject,
		"text":    text,
	}
	if strings.TrimSpace(html) != "" {
		payload["html"] = html
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("resend %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.ID == "" {
		out.ID = "ok"
	}
	return out.ID, nil
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
