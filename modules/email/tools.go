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

// Factory returns email_* tools. API keys are per-user (panel).
func Factory(st *store.Store, box *cryptox.Box) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "email_send",
					Description: "Send an email via the user's Resend API key. " +
						"Requires Email module configured in the Takan panel (API key + default from).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"to":      map[string]any{"type": "string", "description": "Recipient email"},
							"subject": map[string]any{"type": "string"},
							"body":    map[string]any{"type": "string", "description": "Plain-text body"},
							"html":    map[string]any{"type": "string", "description": "Optional HTML body"},
							"from":    map[string]any{"type": "string", "description": "Override from address"},
						},
						"required": []string{"to", "subject", "body"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					keyEnc, defaultFrom, ok, err := st.GetEmailSettings(ctx, userID)
					if err != nil {
						return "", err
					}
					if !ok {
						return "", fmt.Errorf("email not configured — open Takan panel → Email and save your Resend API key")
					}
					apiKey, err := box.Open(keyEnc)
					if err != nil {
						return "", fmt.Errorf("decrypt api key: %w", err)
					}
					to, _ := args["to"].(string)
					subject, _ := args["subject"].(string)
					body, _ := args["body"].(string)
					html, _ := args["html"].(string)
					from, _ := args["from"].(string)
					to = strings.TrimSpace(to)
					subject = strings.TrimSpace(subject)
					from = strings.TrimSpace(from)
					if from == "" {
						from = defaultFrom
					}
					if to == "" || subject == "" || from == "" {
						return "", fmt.Errorf("to, subject and from are required (set default from in panel)")
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
					Name:        "email_status",
					Description: "Check whether Resend email is configured for this account.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					_, from, ok, err := st.GetEmailSettings(ctx, userID)
					if err != nil {
						return "", err
					}
					if !ok {
						return "Email module ON but not configured. Add Resend API key in the panel.", nil
					}
					return fmt.Sprintf("Email ready. Default from: %s", from), nil
				},
			},
		}
	}
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
