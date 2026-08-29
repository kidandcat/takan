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
			{
				Tool: mcp.Tool{
					Name: "tv_volume",
					Description: "Samsung TV volume via UPnP RenderingControl on the LAN takan-agent (port 9197). " +
						"Omit level to GetVolume. Pass level 0–100 to SetVolume then GetVolume. " +
						"InstanceID 0, Channel Master. curl -m 8 — hub does not talk to the TV.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"level":   map[string]any{"type": "integer", "description": "0–100 to set; omit to read current volume"},
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					level, set := intArg(args, "level")
					return runTVParsed(ctx, st, hub, userID, args, func(c Config) (string, error) {
						if set {
							return curlVolumeSet(c.Host, level)
						}
						return curlVolumeGet(c.Host)
					}, func(cfg Config, res *agenthub.Result) string {
						return formatVolume(cfg, res, set, level)
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "tv_mute",
					Description: "Samsung TV mute via UPnP GetMute/SetMute on the LAN takan-agent (port 9197). " +
						"Omit mute to read. Pass true/false to set. Pass toggle to flip. curl -m 8.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"mute": map[string]any{
								"description": "true/false to set, toggle to flip; omit to get",
							},
							"action":  map[string]any{"type": "string", "description": "get, set, or toggle (optional)"},
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					mode, muted, err := parseMuteArgs(args)
					if err != nil {
						return "", err
					}
					return runTVParsed(ctx, st, hub, userID, args, func(c Config) (string, error) {
						switch mode {
						case "set":
							return curlMuteSet(c.Host, muted)
						case "toggle":
							return curlMuteToggle(c.Host)
						default:
							return curlMuteGet(c.Host)
						}
					}, func(cfg Config, res *agenthub.Result) string {
						return formatMute(cfg, res, mode)
					})
				},
			},
			{
				Tool: mcp.Tool{
					Name: "tv_power",
					Description: "Samsung TV power via the LAN takan-agent. Reports PowerState from REST /api/v2/. " +
						"action=off sends KEY_POWER if on. action=on sends Wake-on-LAN to the TV wifi MAC " +
						"(from device info, else panel default) then KEY_POWER if still off. No SmartThings.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action":  map[string]any{"type": "string", "description": "on or off; omit to report PowerState only", "enum": []string{"on", "off"}},
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					return handlePower(ctx, st, hub, userID, args)
				},
			},
			{
				Tool: mcp.Tool{
					Name: "tv_now",
					Description: "What the Samsung TV is doing right now, via the LAN takan-agent. " +
						"GET REST /api/v2/ for power, then GET /api/v2/applications/{id} for panel aliases " +
						"(Netflix, YouTube, HBO Max). REST only — samsungtvws listing hangs on this TV.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "takan-agent machine name (optional)"},
							"host":    map[string]any{"type": "string", "description": "TV host override (optional)"},
						},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					return handleNow(ctx, st, hub, userID, args)
				},
			},
		}
	}
}

func runTV(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any, build func(Config) (string, error)) (string, error) {
	return runTVParsed(ctx, st, hub, userID, args, build, formatAgent)
}

func runTVParsed(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any, build func(Config) (string, error), format func(Config, *agenthub.Result) string) (string, error) {
	cfg, res, err := execTV(ctx, st, hub, userID, args, build)
	if err != nil {
		return "", err
	}
	return format(cfg, res), nil
}

func execTV(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any, build func(Config) (string, error)) (Config, *agenthub.Result, error) {
	if hub == nil {
		return Config{}, nil, fmt.Errorf("agent hub not available")
	}
	cfg, err := LoadConfig(ctx, st, userID)
	if err != nil {
		return Config{}, nil, err
	}
	if m := strArg(args, "machine"); m != "" {
		cfg.Machine = m
	}
	if h := strArg(args, "host"); h != "" {
		cfg.Host = h
	}
	if err := validMachine(cfg.Machine); err != nil {
		return Config{}, nil, err
	}
	if err := validHost(cfg.Host); err != nil {
		return Config{}, nil, err
	}
	if _, err := st.MachineByUserAndName(ctx, userID, cfg.Machine); err != nil {
		return Config{}, nil, fmt.Errorf("unknown machine %q — register takan-agent in Takan panel → Machines (TV needs a LAN box, usually mac)", cfg.Machine)
	}
	cmd, err := build(cfg)
	if err != nil {
		return cfg, nil, err
	}
	res, err := hub.RunBash(ctx, userID, cfg.Machine, cmd, agentWait)
	if err != nil {
		if strings.Contains(err.Error(), "timeout") {
			return cfg, nil, fmt.Errorf("%w — if the TV shows Allow?, accept on the TV and retry (do not wait here for pairing)", err)
		}
		return cfg, nil, err
	}
	return cfg, res, nil
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

func intArg(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				return 0, false
			}
			n = n*10 + int(r-'0')
		}
		return n, true
	default:
		return 0, false
	}
}

func parseMuteArgs(args map[string]any) (mode string, muted bool, err error) {
	action := strings.ToLower(strArg(args, "action"))
	switch action {
	case "toggle":
		return "toggle", false, nil
	case "get", "":
	case "set":
		mode = "set"
	default:
		return "", false, fmt.Errorf("action must be get, set, or toggle")
	}
	if _, ok := args["mute"]; !ok {
		if mode == "set" {
			return "", false, fmt.Errorf("mute true/false required when action=set")
		}
		return "get", false, nil
	}
	raw := args["mute"]
	switch t := raw.(type) {
	case bool:
		return "set", t, nil
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		switch s {
		case "toggle":
			return "toggle", false, nil
		case "true", "1", "on", "yes":
			return "set", true, nil
		case "false", "0", "off", "no":
			return "set", false, nil
		default:
			return "", false, fmt.Errorf("mute must be true, false, or toggle")
		}
	case float64:
		return "set", t != 0, nil
	default:
		return "", false, fmt.Errorf("mute must be true, false, or toggle")
	}
}

func formatVolume(cfg Config, res *agenthub.Result, set bool, level int) string {
	out := baseOut(cfg, res)
	if set {
		out["requested"] = level
	}
	if res != nil {
		if v, ok := parseVolume(res.Stdout); ok {
			out["volume"] = v
		}
	}
	return mustIndent(out)
}

func formatMute(cfg Config, res *agenthub.Result, mode string) string {
	out := baseOut(cfg, res)
	out["action"] = mode
	if res != nil {
		if m, ok := parseMute(res.Stdout); ok {
			out["muted"] = m
		}
	}
	return mustIndent(out)
}

func handlePower(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any) (string, error) {
	action := strings.ToLower(strArg(args, "action"))
	switch action {
	case "", "on", "off":
	default:
		return "", fmt.Errorf("action must be on or off")
	}
	cfg, res, err := execTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
		return curlStatus(c.Host)
	})
	if err != nil {
		return "", err
	}
	power, mac := "", ""
	if res != nil {
		power, mac = parsePowerAndMAC(res.Stdout)
	}
	if mac == "" {
		mac = cfg.WifiMAC
	}
	out := baseOut(cfg, res)
	if power == "" && (res == nil || res.ExitCode != 0) {
		power = "off"
	}
	out["power"] = power
	if mac != "" {
		out["wifi_mac"] = mac
	}
	if action == "" {
		return mustIndent(out), nil
	}
	out["action"] = action
	if action == "off" {
		if powerOn(power) {
			keyOK, keyErr := runStep(ctx, st, hub, userID, args, func(c Config) (string, error) {
				return pyKey(c.Host, c.TokenPath, c.ClientName, "KEY_POWER")
			})
			out["key_power"] = keyOK
			if keyErr != "" {
				out["key_error"] = keyErr
			}
			if p, _, stErr := readPower(ctx, st, hub, userID, args); stErr == "" && p != "" {
				power = p
			}
		}
		out["power"] = power
		return mustIndent(out), nil
	}
	if powerOn(power) {
		return mustIndent(out), nil
	}
	if mac == "" {
		return "", fmt.Errorf("no wifi MAC for Wake-on-LAN — set wifi_mac in Takan panel → TV")
	}
	wolOK, wolErr := runStep(ctx, st, hub, userID, args, func(c Config) (string, error) {
		return wolCmd(mac, c.Host)
	})
	out["wol"] = wolOK
	if wolErr != "" {
		out["wol_error"] = wolErr
	}
	power, mac2, _ := readPower(ctx, st, hub, userID, args)
	if power != "" {
		out["power"] = power
	}
	if mac2 != "" {
		out["wifi_mac"] = mac2
	}
	if powerOn(power) {
		return mustIndent(out), nil
	}
	keyOK, keyErr := runStep(ctx, st, hub, userID, args, func(c Config) (string, error) {
		return pyKey(c.Host, c.TokenPath, c.ClientName, "KEY_POWER")
	})
	out["key_power"] = keyOK
	if keyErr != "" {
		out["key_error"] = keyErr
	}
	if p, _, stErr := readPower(ctx, st, hub, userID, args); stErr == "" && p != "" {
		out["power"] = p
	}
	return mustIndent(out), nil
}

func runStep(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any, build func(Config) (string, error)) (ok bool, errText string) {
	_, res, err := execTV(ctx, st, hub, userID, args, build)
	if err != nil {
		return false, err.Error()
	}
	return res == nil || res.ExitCode == 0, ""
}

func readPower(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any) (power, mac, errText string) {
	_, res, err := execTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
		return curlStatus(c.Host)
	})
	if err != nil {
		return "", "", err.Error()
	}
	if res != nil {
		power, mac = parsePowerAndMAC(res.Stdout)
	}
	return power, mac, ""
}

func handleNow(ctx context.Context, st *store.Store, hub *agenthub.Hub, userID string, args map[string]any) (string, error) {
	cfg, res, err := execTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
		return curlStatus(c.Host)
	})
	if err != nil {
		return "", err
	}
	power := ""
	if res != nil {
		power, _ = parsePowerAndMAC(res.Stdout)
	}
	out := baseOut(cfg, res)
	if power == "" && (res == nil || res.ExitCode != 0) {
		power = "off"
	}
	out["power"] = power
	if !powerOn(power) {
		out["apps"] = []any{}
		out["now"] = nil
		return mustIndent(out), nil
	}
	var apps []map[string]any
	var nowName string
	for _, ref := range cfg.UniqueApps() {
		_, appRes, appErr := execTV(ctx, st, hub, userID, args, func(c Config) (string, error) {
			return curlApp(c.Host, "GET", ref.ID)
		})
		row := map[string]any{"alias": ref.Alias, "id": ref.ID}
		if appErr != nil || appRes == nil || appRes.ExitCode != 0 {
			row["running"] = false
			row["visible"] = false
			if appErr != nil {
				row["error"] = appErr.Error()
			}
			apps = append(apps, row)
			continue
		}
		running, visible, name := parseAppStatus(appRes.Stdout)
		row["running"] = running
		row["visible"] = visible
		if name != "" {
			row["name"] = name
		}
		if visible {
			nowName = ref.Alias
		} else if running && nowName == "" {
			nowName = ref.Alias
		}
		apps = append(apps, row)
	}
	out["apps"] = apps
	if nowName != "" {
		out["now"] = nowName
	} else {
		out["now"] = nil
	}
	return mustIndent(out), nil
}

func baseOut(cfg Config, res *agenthub.Result) map[string]any {
	out := map[string]any{
		"via":     "takan-agent " + cfg.Machine,
		"machine": cfg.Machine,
		"host":    cfg.Host,
	}
	if res != nil {
		out["exit_code"] = res.ExitCode
		if res.Error != "" {
			out["error"] = res.Error
		}
		if errOut := strings.TrimSpace(res.Stderr); errOut != "" {
			out["stderr"] = errOut
		}
	}
	return out
}

func mustIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
