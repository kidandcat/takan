// Package assistant exposes the personal assistant's MCP tools. The assistant
// itself lives in internal/assistant; this is only the capability surface other
// agents see.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	asst "github.com/kidandcat/takan/internal/assistant"
	"github.com/kidandcat/takan/internal/mcp"
)

// Sender is the part of the assistant this module needs. Keeping it an
// interface means the tools can be exercised without a live Telegram client.
//
// SendTelegram validates the target itself: a chat id reaching this tool comes
// from a model, and an invented one would deliver the operator's private
// conversation to a stranger.
type Sender interface {
	SendTelegram(ctx context.Context, chatID int64, text, parseMode string) (int64, error)
}

// Factory returns the assistant's tools. It yields nothing when the assistant
// failed to start, so a broken credential removes the tool instead of turning
// every call into an error.
func Factory(a Sender) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		if a == nil {
			return nil
		}
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "telegram_send",
					Description: "Send a Telegram message as " + asst.InstanceName + ", the operator's assistant bot. " +
						"With no chat_id it lands in the operator's own chat, which is what you almost always want. " +
						"Pass chat_id only to answer in a chat the assistant already serves; an unknown id is rejected. " +
						"parse_mode: empty (plain), HTML, Markdown, or MarkdownV2. Max 4096 characters.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{
								"type":        "string",
								"description": "Message body (required)",
							},
							"chat_id": map[string]any{
								"type":        "string",
								"description": "Destination chat id, which must be a chat the assistant already serves (optional; default: the operator's own chat)",
							},
							"parse_mode": map[string]any{
								"type":        "string",
								"description": "Optional: HTML, Markdown, MarkdownV2, or empty for plain text",
							},
						},
						"required": []string{"text"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					text, _ := args["text"].(string)
					parseMode, _ := args["parse_mode"].(string)
					raw, _ := args["chat_id"].(string)

					var chatID int64
					if raw = strings.TrimSpace(raw); raw != "" {
						parsed, err := strconv.ParseInt(raw, 10, 64)
						if err != nil {
							return "", fmt.Errorf("chat_id must be a numeric Telegram chat id, got %q", raw)
						}
						chatID = parsed
					}
					msgID, err := a.SendTelegram(ctx, chatID, text, parseMode)
					if err != nil {
						return "", err
					}
					out := map[string]any{"status": "sent", "message_id": msgID}
					if chatID != 0 {
						out["chat_id"] = strconv.FormatInt(chatID, 10)
					} else {
						out["chat_id"] = "operator"
					}
					b, err := json.MarshalIndent(out, "", "  ")
					if err != nil {
						return "", err
					}
					return string(b), nil
				},
			},
		}
	}
}
