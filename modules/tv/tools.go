package tv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

// Agent timeout is slightly above curl/samsungtvws -m 8 so the hub is not the first to fire.
const agentWait = 12 * time.Second

// Factory returns tv_* tools. Commands run on a registered takan-agent (LAN), never from the hub.
func Factory(st *store.Store, hub *agenthub.Hub) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "tv_status",
					Description: "Check whether the Samsung TV is reachable on the home LAN (REST /api/v2/). " +
						"Runs on the configured takan-agent machine (default mac) — the hub cannot reach the TV. " +
						"machine and host are optional overrides (defaults come from Takan panel → TV).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (default from panel, usually mac)"},
							"host":    map[string]any{"type": "string", "description": "TV IPv4/hostname on the LAN (default from panel)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					return runTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
						return curlStatus(c.Host)
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "tv_app",
					Description: "Launch or close a Samsung Tizen app by friendly name (Netflix, YouTube, HBO Max) or app id. " +
						"Uses REST POST/DELETE /api/v2/applications/{appId} via the LAN takan-agent (default machine mac). " +
						"action is launch or close. Aliases are configured in Takan panel → TV.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action": map[string]any{
								"type":        "string",
								"description": "launch or close",
								"enum":        []string{"launch", "close"},
							},
							"app": map[string]any{
								"type":        "string",
								"description": "Friendly name (Netflix, YouTube, HBO Max) or Tizen app id",
							},
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
						"required": []string{"action", "app"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					action := strings.ToLower(strArg(args, "action"))
					app := strArg(args, "app")
					var method string
					switch action {
					case "launch", "open", "start":
						method = "POST"
					case "close", "kill", "stop":
						method = "DELETE"
					default:
						return "", fmt.Errorf("action must be launch or close")
					}
					return runTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
						id, err := c.ResolveApp(app)
						if err != nil {
							return "", err
						}
						return curlApp(c.Host, method, id)
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "tv_key",
					Description: "Send a Samsung remote key over the TV websocket (port 8002) via the LAN takan-agent. " +
						"Accepts KEY_HOME or friendly names (home, enter, back, volup, mute, power, …). " +
						"Needs the token file on the agent machine (panel default). " +
						"If the TV shows Allow?, accept on the TV and retry — pairing is not waited on.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"key":     map[string]any{"type": "string", "description": "Remote key (KEY_HOME or home, enter, back, …)"},
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
						"required": []string{"key"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					key := strArg(args, "key")
					return runTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
						return pyKey(c.Host, c.TokenPath, c.ClientName, key)
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "tv_text",
					Description: "Type text on the Samsung TV (search/on-screen keyboard) via the LAN takan-agent websocket. " +
						"Max 200 characters. If the TV shows Allow?, accept on the TV and retry.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":    map[string]any{"type": "string", "description": "Text to type on the TV"},
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
						"required": []string{"text"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					text, _ := args["text"].(string)
					return runTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
						return pyText(c.Host, c.TokenPath, c.ClientName, text)
					})
				},
			},
		}
	}
}

func runTV(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any, build func(Config) (string, error)) (string, error) {
	if hub == nil {
		return "", fmt.Errorf("agent hub not available")
	}
	cfg, err := LoadConfig(ctx, st, userID)
	if err != nil {
		return "", err
	}
	if m := strArg(args, "machine"); m != "" {
		cfg.Machine = m
	}
	if h := strArg(args, "host"); h != "" {
		cfg.Host = h
	}
	if err := validMachine(cfg.Machine); err != nil {
		return "", err
	}
	if err := validHost(cfg.Host); err != nil {
		return "", err
	}
	if _, err := st.MachineByUserAndName(ctx, userID, cfg.Machine); err != nil {
		return "", fmt.Errorf("unknown machine %q — register takan-agent in Takan panel → Machines (TV needs a LAN box, usually mac)", cfg.Machine)
	}
	cmd, err := build(cfg)
	if err != nil {
		return "", err
	}
	res, err := hub.RunBash(ctx, userID, cfg.Machine, cmd, agentWait)
	if err != nil {
		if strings.Contains(err.Error(), "timeout") {
			return "", fmt.Errorf("%w — if the TV shows Allow?, accept on the TV and retry (do not wait here for pairing)", err)
		}
		return "", err
	}
	return formatAgent(cfg, res), nil
}

func formatAgent(cfg Config, res *agenthub.Result) string {
	via := "takan-agent " + cfg.Machine
	out := map[string]any{
		"via":       via,
		"machine":   cfg.Machine,
		"host":      cfg.Host,
		"exit_code": 0,
	}
	if res != nil {
		out["exit_code"] = res.ExitCode
		if res.Error != "" {
			out["error"] = res.Error
		}
		if body := strings.TrimSpace(res.Stdout); body != "" {
			var parsed any
			if json.Unmarshal([]byte(body), &parsed) == nil {
				out["result"] = parsed
			} else {
				out["stdout"] = body
			}
		}
		if errOut := strings.TrimSpace(res.Stderr); errOut != "" {
			out["stderr"] = errOut
		}
	}
	if res != nil && res.ExitCode != 0 {
		out["hint"] = "TV unreachable or command failed on " + via + ". REST needs the TV on the LAN; keys/text need the websocket token. If Allow? is on screen, accept it."
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Sprintf("machine: %s\nexit_code: %v\n", cfg.Machine, out["exit_code"])
	}
	return string(b)
}

func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}
