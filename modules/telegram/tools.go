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
	svc := &Service{Store: st, Box: box}
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "telegram_chats",
					Description: "List Telegram channels for this account and the chats each one serves. " +
						"A channel is a bot credential plus its chats; telegram_send addresses one. " +
						"Call before telegram_send if you do not know which channel or chat_id to use.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					channels, err := st.ListTelegramChannels(ctx, userID)
					if err != nil {
						return "", err
					}
					if len(channels) == 0 {
						return "", fmt.Errorf("no telegram channels — open Takan panel → Telegram and add one")
					}
					type chatRow struct {
						ID    string `json:"chat_id"`
						Type  string `json:"type"`
						Label string `json:"label,omitempty"`
					}
					type row struct {
						Name     string    `json:"channel"`
						Bot      string    `json:"bot,omitempty"`
						Default  bool      `json:"default,omitempty"`
						Receiver string    `json:"receiving_bot,omitempty"`
						Chats    []chatRow `json:"chats"`
					}
					out := make([]row, 0, len(channels))
					for _, c := range channels {
						r := row{Name: c.Name, Bot: c.BotUser, Default: c.IsDefault}
						for _, a := range c.Attachments {
							if a.ReceiveConsumer() {
								r.Receiver = a.Consumer
							}
						}
						for _, ch := range c.Chats {
							r.Chats = append(r.Chats, chatRow{ID: ch.ChatID, Type: ch.Type, Label: ch.Label})
						}
						out = append(out, r)
					}
					return marshal(map[string]any{
						"channels": out,
						"hint":     "telegram_send takes channel (name, default when omitted) and chat_id (first chat when omitted).",
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "telegram_send",
					Description: "Send a Telegram message. channel selects which bot credential to send as " +
						"(default channel when omitted); chat_id selects one of that channel's chats " +
						"(its first chat when omitted). Call telegram_chats to see both. " +
						"parse_mode: empty (plain), HTML, Markdown, or MarkdownV2. Max 4096 characters.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{
								"type":        "string",
								"description": "Message body (required)",
							},
							"channel": map[string]any{
								"type":        "string",
								"description": "Channel name from telegram_chats (optional; default channel when omitted)",
							},
							"chat_id": map[string]any{
								"type":        "string",
								"description": "Destination chat id within the channel (optional; its first chat when omitted)",
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
					chatID, _ := args["chat_id"].(string)
					parseMode, _ := args["parse_mode"].(string)
					name, _ := args["channel"].(string)

					c, err := st.ResolveTelegramChannel(ctx, userID, name)
					if err != nil || c == nil {
						return "", fmt.Errorf("unknown telegram channel %q — call telegram_chats", strings.TrimSpace(name))
					}
					chatID = strings.TrimSpace(chatID)
					if chatID != "" && c.PrimaryChat(chatID) != chatID {
						return "", fmt.Errorf("chat_id %q is not a chat of channel %q — call telegram_chats", chatID, c.Name)
					}
					msgID, err := svc.SendVia(ctx, c, chatID, text, parseMode)
					if err != nil {
						return "", err
					}
					return marshal(map[string]any{
						"status":     "sent",
						"message_id": msgID,
						"channel":    c.Name,
						"chat_id":    c.PrimaryChat(chatID),
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
