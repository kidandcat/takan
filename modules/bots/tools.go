package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// OnlineAfter is how long a bot stays "online" after its last heartbeat.
const OnlineAfter = 3 * time.Minute

// Online reports whether the bot heartbeat is recent enough.
func Online(b store.Bot) bool {
	return b.LastSeen != nil && time.Since(*b.LastSeen) < OnlineAfter
}

// Factory returns bots_* tools. Watch (optional) wakes long-polling daemons
// immediately after an approve/deny so decisions land without waiting a poll.
// prov (optional) enables bots_provision.
func Factory(st *store.Store, watch *Watcher, prov *Provisioner) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "bots_list",
					Description: "List Telegram assistant bots managed by Takan (name, machine, online state, " +
						"pending chat approvals). A bot name is also what machine_ai_run takes as owner: the " +
						"job result is delivered to that bot when it finishes. kind=legacy means a seeded " +
						"owner placeholder with no daemon yet. Bots are created in the Takan panel → Bots.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					list, err := st.ListBots(ctx, userID)
					if err != nil {
						return "", err
					}
					if len(list) == 0 {
						return "No bots registered. Open Takan panel → Bots to add one and install the daemon.", nil
					}
					type row struct {
						Name       string `json:"name"`
						Username   string `json:"bot_username,omitempty"`
						Machine    string `json:"machine,omitempty"`
						Kind       string `json:"kind"`
						Online     bool   `json:"online"`
						LastSeen   string `json:"last_seen,omitempty"`
						Pending    int    `json:"pending_chats"`
						Approved   int    `json:"approved_chats"`
						Deliveries int    `json:"queued_deliveries,omitempty"`
						Provision  string `json:"provision_status,omitempty"`
						ProvErr    string `json:"provision_error,omitempty"`
						Channel    string `json:"channel,omitempty"`
					}
					out := make([]row, 0, len(list))
					for _, b := range list {
						r := row{
							Name: b.Name, Username: b.BotUsername, Machine: b.MachineName, Kind: b.Kind,
							Online: Online(b), Pending: b.PendingChats, Approved: b.ApprovedChats,
							Deliveries: b.PendingDeliveries,
							Provision:  b.ProvisionStatus, ProvErr: b.ProvisionError,
						}
						if ch, _, err := st.ChannelForConsumer(ctx, userID,
							store.ConsumerBot, b.ID, store.DirectionReceive); err == nil && ch != nil {
							if atts, _ := st.ConsumerAttachments(ctx, userID, store.ConsumerBot, b.ID, store.DirectionReceive); len(atts) > 0 {
								r.Channel = ch.Name
							}
						}
						if b.LastSeen != nil {
							r.LastSeen = b.LastSeen.UTC().Format(time.RFC3339)
						}
						out = append(out, r)
					}
					return marshal(out)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "bots_chats",
					Description: "List the Telegram chats a bot knows, with approval status " +
						"(pending | approved | denied). Omit bot to see every bot's chats. " +
						"Use status=pending to review what is waiting for a decision, then bots_approve / bots_deny.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"bot": map[string]any{
								"type":        "string",
								"description": "Bot name from bots_list (optional; all bots if omitted)",
							},
							"status": map[string]any{
								"type":        "string",
								"enum":        []string{"pending", "approved", "denied"},
								"description": "Filter by approval status (optional)",
							},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					status := strings.TrimSpace(str(args, "status"))
					switch status {
					case "", store.BotChatPending, store.BotChatApproved, store.BotChatDenied:
					default:
						return "", fmt.Errorf("status must be pending, approved or denied")
					}
					var wanted []store.Bot
					if name := str(args, "bot"); name != "" {
						b, err := resolveBot(ctx, st, userID, name)
						if err != nil {
							return "", err
						}
						wanted = []store.Bot{*b}
					} else {
						all, err := st.ListBots(ctx, userID)
						if err != nil {
							return "", err
						}
						wanted = all
					}
					type row struct {
						Bot      string `json:"bot"`
						ChatID   string `json:"chat_id"`
						Type     string `json:"type"`
						Label    string `json:"label"`
						Status   string `json:"status"`
						Snippet  string `json:"first_message,omitempty"`
						Reported string `json:"reported_at,omitempty"`
					}
					var out []row
					for _, b := range wanted {
						chats, err := st.ListBotChats(ctx, b.ID, status, "")
						if err != nil {
							return "", err
						}
						for _, c := range chats {
							out = append(out, row{
								Bot: b.Name, ChatID: c.ChatID, Type: c.Type, Label: c.Label(),
								Status: c.Status, Snippet: c.FirstMessage,
								Reported: c.CreatedAt.UTC().Format(time.RFC3339),
							})
						}
					}
					if len(out) == 0 {
						return "No chats match. Bots report a chat the first time someone writes to them.", nil
					}
					return marshal(out)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "bots_approve",
					Description: "Approve a Telegram chat for a bot: the daemon will answer that chat from then on. " +
						"Approving a group chat authorises every member of the group. chat_id comes from bots_chats.",
					InputSchema: decisionSchema("Approve"),
				},
				Handler: decisionHandler(st, watch, store.BotChatApproved),
			},
			{
				Tool: mcp.Tool{
					Name: "bots_provision",
					Description: "Install (or re-install) a bot daemon on its target machine through " +
						"takan-agent: downloads the binary, writes the env file from the bot's telegram " +
						"channel, installs and starts <instance>.service. Idempotent. Linux/systemd only. " +
						"Returns immediately; poll bots_list for provision status (queued|running|ok|failed).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"bot": map[string]any{
								"type":        "string",
								"description": "Bot name from bots_list",
							},
						},
						"required": []string{"bot"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					if prov == nil {
						return "", fmt.Errorf("provisioning is not configured on this hub")
					}
					b, err := resolveBot(ctx, st, userID, str(args, "bot"))
					if err != nil {
						return "", err
					}
					if !b.Provisionable() {
						return "", fmt.Errorf("bot %q has no target machine — set one in the panel", b.Name)
					}
					prov.Start(userID, b.ID)
					return marshal(map[string]any{
						"bot":      b.Name,
						"machine":  b.MachineName,
						"instance": b.Instance + ".service",
						"status":   store.ProvisionQueued,
						"hint":     "poll bots_list for provision_status",
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "bots_deny",
					Description: "Deny a Telegram chat for a bot: the daemon keeps ignoring it and stops asking. " +
						"chat_id comes from bots_chats.",
					InputSchema: decisionSchema("Deny"),
				},
				Handler: decisionHandler(st, watch, store.BotChatDenied),
			},
		}
	}
}

func decisionSchema(verb string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"bot": map[string]any{
				"type":        "string",
				"description": "Bot name from bots_list",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": verb + " this Telegram chat id (from bots_chats)",
			},
		},
		"required": []string{"bot", "chat_id"},
	}
}

func decisionHandler(st *store.Store, watch *Watcher, status string) func(ctx context.Context, userID string, args map[string]any) (string, error) {
	return func(ctx context.Context, userID string, args map[string]any) (string, error) {
		name := str(args, "bot")
		chatID := str(args, "chat_id")
		if name == "" || chatID == "" {
			return "", fmt.Errorf("bot and chat_id required")
		}
		b, err := resolveBot(ctx, st, userID, name)
		if err != nil {
			return "", err
		}
		c, err := st.DecideBotChat(ctx, userID, b.ID, chatID, status, "mcp")
		if err != nil {
			if NotFound(err) {
				return "", fmt.Errorf("bot %q has no chat %q — call bots_chats", b.Name, chatID)
			}
			return "", err
		}
		watch.NotifyChats(b.ID)
		return marshal(map[string]any{
			"bot":     b.Name,
			"chat_id": c.ChatID,
			"label":   c.Label(),
			"type":    c.Type,
			"status":  c.Status,
			"hint":    "the bot daemon picks the decision up on its next poll (seconds)",
		})
	}
}

func resolveBot(ctx context.Context, st *store.Store, userID, name string) (*store.Bot, error) {
	b, err := st.BotByUserAndName(ctx, userID, name)
	if err != nil {
		return nil, fmt.Errorf("unknown bot %q — call bots_list", name)
	}
	return b, nil
}

func str(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
