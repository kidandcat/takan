package sip

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Factory returns sip_* MCP tools. Hub may be nil (tools still list offline state).
func Factory(st *store.Store, hub *Hub) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "sip_status",
					Description: "SIP module snapshot: xAI key configured, voice/instructions, online gateways, " +
						"active calls. Call first when the user mentions phone, SIM, SIP, or Grok Voice on mobile.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					return statusJSON(ctx, st, hub, userID)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "sip_devices",
					Description: "List SIP phone gateways for this account (name, SIM E.164, online). " +
						"Tokens are never returned — manage devices in the Takan panel → SIP.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					devs, err := st.ListSIPDevices(ctx, userID)
					if err != nil {
						return "", err
					}
					var rows []map[string]any
					for _, d := range devs {
						row := map[string]any{
							"id":       d.ID,
							"name":     d.Name,
							"sim_e164": d.SimE164,
							"online":   hub != nil && hub.Online(d.ID),
						}
						if d.LastSeen != nil {
							row["last_seen"] = d.LastSeen.UTC().Format("2006-01-02T15:04:05Z")
						}
						rows = append(rows, row)
					}
					return marshal(map[string]any{
						"devices": rows,
						"count":   len(rows),
						"ws_path": "/sip/ws?token=<device_token>",
						"hint":    "Create devices in panel → SIP; phones connect outbound only.",
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "sip_calls",
					Description: "List active SIP/cellular calls being bridged to Grok Voice right now.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					var calls []map[string]any
					if hub != nil {
						calls = hub.Calls().Snapshot(userID)
					}
					if calls == nil {
						calls = []map[string]any{}
					}
					return marshal(map[string]any{"calls": calls, "count": len(calls)})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "sip_hangup",
					Description: "Hang up an active bridged call. Pass device_id (uuid from sip_devices) " +
						"and optional call_id (from sip_calls). Ends Grok session and asks the phone to drop cellular.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"device_id": map[string]any{"type": "string", "description": "SIP device uuid"},
							"call_id":   map[string]any{"type": "string", "description": "Optional local call id"},
						},
						"required": []string{"device_id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					if hub == nil {
						return "", fmt.Errorf("sip hub not available")
					}
					deviceID, _ := args["device_id"].(string)
					callID, _ := args["call_id"].(string)
					if deviceID == "" {
						return "", fmt.Errorf("device_id required")
					}
					// Accept device name as well as uuid.
					devs, _ := st.ListSIPDevices(ctx, userID)
					resolved := deviceID
					for _, d := range devs {
						if d.ID == deviceID || d.Name == deviceID {
							resolved = d.ID
							break
						}
					}
					if err := hub.Calls().HangupUserCall(userID, resolved, callID); err != nil {
						return "", err
					}
					return marshal(map[string]any{"status": "hangup_sent", "device_id": resolved})
				},
			},
		}
	}
}

func statusJSON(ctx context.Context, st *store.Store, hub *Hub, userID string) (string, error) {
	settings, ok, err := st.GetSIPSettings(ctx, userID)
	if err != nil {
		return "", err
	}
	devs, err := st.ListSIPDevices(ctx, userID)
	if err != nil {
		return "", err
	}
	online := 0
	if hub != nil {
		online = hub.OnlineCount(userID)
	}
	var calls []map[string]any
	if hub != nil {
		calls = hub.Calls().Snapshot(userID)
	}
	ready := ok && settings.HasKey && len(devs) > 0
	detail := "not configured"
	if ok && settings.HasKey {
		detail = fmt.Sprintf("key set · voice %s · %d devices · %d online · %d calls",
			settings.Voice, len(devs), online, len(calls))
	} else if ok {
		detail = "settings saved but no xAI API key"
	}
	out := map[string]any{
		"ready":       ready,
		"detail":      detail,
		"has_api_key": ok && settings.HasKey,
		"voice":       "",
		"auto_answer": true,
		"audio_rate":  16000,
		"bridge_mode": "realtime",
		"devices":     len(devs),
		"online":      online,
		"active_calls": len(calls),
		"hint":        "Configure in panel → SIP. Phones connect to wss://<host>/sip/ws?token=…",
	}
	if ok {
		out["voice"] = settings.Voice
		out["auto_answer"] = settings.AutoAnswer
		out["audio_rate"] = settings.AudioRate
		out["bridge_mode"] = settings.BridgeMode
		if settings.Instructions != "" {
			out["instructions_preview"] = truncate(settings.Instructions, 120)
		}
	}
	return marshal(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
