// Package telegram exposes Telegram Bot API tools for Takan.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns telegram_* tools. Bot token and allowed chats come from the panel.
func Factory(st *store.Store, box *cryptox.Box) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "telegram_chats",
					Description: "List configured Telegram destinations for this account (default chat + allowlist) " +
						"and the bot username. Call before telegram_send if you do not know which chat_id to use.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					ts, token, err := loadSettings(ctx, st, box, userID)
					if err != nil {
						return "", err
					}
					_ = token
					var rows []map[string]any
					for _, c := range ts.AllowedChats {
						row := map[string]any{"id": c.ID}
						if c.Label != "" {
							row["label"] = c.Label
						}
						if c.ID == ts.DefaultChatID {
							row["default"] = true
						}
						rows = append(rows, row)
					}
					if ts.DefaultChatID != "" && !store.ChatAllowed("", ts.AllowedChats, ts.DefaultChatID) {
						// defensive: default always listed via NormalizeTelegramChats on save
						rows = append([]map[string]any{{
							"id": ts.DefaultChatID, "label": "default", "default": true,
						}}, rows...)
					}
					out := map[string]any{
						"bot":            strings.TrimPrefix(ts.BotUsername, "@"),
						"default_chat":   ts.DefaultChatID,
						"chats":          rows,
						"hint":           "telegram_send uses default_chat when chat_id is omitted. Only listed chats are allowed.",
					}
					if out["bot"] == "" {
						out["bot"] = nil
					}
					return marshal(out)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "telegram_send",
					Description: "Send a Telegram message via the configured bot. " +
						"chat_id is optional (defaults to the panel default chat). " +
						"Only chat_ids in the panel allowlist are accepted. " +
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
								"description": "Destination chat id (optional; uses default from panel)",
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
					ts, token, err := loadSettings(ctx, st, box, userID)
					if err != nil {
						return "", err
					}
					text, _ := args["text"].(string)
					chatID, _ := args["chat_id"].(string)
					parseMode, _ := args["parse_mode"].(string)
					chatID = strings.TrimSpace(chatID)
					if chatID == "" {
						chatID = strings.TrimSpace(ts.DefaultChatID)
					}
					if chatID == "" {
						return "", fmt.Errorf("no chat_id: set a default chat in Takan panel → Telegram, or pass chat_id")
					}
					if !store.ChatAllowed(ts.DefaultChatID, ts.AllowedChats, chatID) {
						return "", fmt.Errorf("chat_id %q is not in the allowlist — open panel → Telegram or call telegram_chats", chatID)
					}
					msgID, err := SendMessage(ctx, token, chatID, text, parseMode)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{
						"status":     "sent",
						"message_id": msgID,
						"chat_id":    chatID,
					})
				},
			},
		}
	}
}

func loadSettings(ctx context.Context, st *store.Store, box *cryptox.Box, userID string) (store.TelegramSettings, string, error) {
	ts, ok, err := st.GetTelegramSettings(ctx, userID)
	if err != nil {
		return store.TelegramSettings{}, "", err
	}
	if !ok {
		return store.TelegramSettings{}, "", fmt.Errorf("telegram not configured — open Takan panel → Telegram and save your bot token")
	}
	token, err := box.Open(ts.BotTokenEnc)
	if err != nil {
		return store.TelegramSettings{}, "", fmt.Errorf("decrypt bot token: %w", err)
	}
	if strings.TrimSpace(ts.DefaultChatID) == "" && len(ts.AllowedChats) == 0 {
		return store.TelegramSettings{}, "", fmt.Errorf("no chats configured — open panel → Telegram and set a default chat id (message the bot, then Discover)")
	}
	return ts, token, nil
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
