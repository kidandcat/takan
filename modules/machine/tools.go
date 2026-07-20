package machine

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

// BashLimiter optional per-user rate limit for machine_bash.
// Returns false when the call should be rejected.
type BashLimiter func(userID string) bool

// Factory returns machine_* tools.
func Factory(st *store.Store, hub *agenthub.Hub, limit BashLimiter) func(ctx context.Context, userID string) []mcp.RegisteredTool {
	return func(ctx context.Context, userID string) []mcp.RegisteredTool {
		return []mcp.RegisteredTool{
			{
				Tool: mcp.Tool{
					Name: "machine_list",
					Description: "List machines registered for this Takan account and whether the agent is online. " +
						"Install agents from the Takan web panel.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					ms, err := st.ListMachines(ctx, userID)
					if err != nil {
						return "", err
					}
					type row struct {
						Name   string `json:"name"`
						Online bool   `json:"online"`
						ID     string `json:"id"`
					}
					out := make([]row, 0, len(ms))
					for _, m := range ms {
						out = append(out, row{Name: m.Name, Online: hub.Online(m.ID), ID: m.ID})
					}
					if len(out) == 0 {
						return "No machines registered. Open the Takan panel → Machine → add a machine and run the install command.", nil
					}
					b, _ := json.MarshalIndent(out, "", "  ")
					return string(b), nil
				},
			},
			{
				Tool: mcp.Tool{
					Name: "machine_bash",
					Description: "Run a shell command on a registered machine via its takan-agent. " +
						"The agent must be online. Prefer short non-interactive commands. " +
						"Pass machine name from machine_list.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name"},
							"command": map[string]any{"type": "string", "description": "Shell command"},
							"timeout_seconds": map[string]any{
								"type": "integer", "description": "Timeout (default 60, max 300)",
							},
						},
						"required": []string{"machine", "command"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					if limit != nil && !limit(userID) {
						return "", fmt.Errorf("rate limit: too many machine_bash calls — try again shortly")
					}
					name, _ := args["machine"].(string)
					cmd, _ := args["command"].(string)
					name = strings.TrimSpace(name)
					cmd = strings.TrimSpace(cmd)
					if name == "" || cmd == "" {
						return "", fmt.Errorf("machine and command required")
					}
					// ownership check
					if _, err := st.MachineByUserAndName(ctx, userID, name); err != nil {
						return "", fmt.Errorf("unknown machine %q", name)
					}
					secs := 60
					if v, ok := args["timeout_seconds"].(float64); ok && v > 0 {
						secs = int(v)
					}
					if secs > 300 {
						secs = 300
					}
					res, err := hub.RunBash(ctx, userID, name, cmd, time.Duration(secs)*time.Second)
					if err != nil {
						return "", err
					}
					var b strings.Builder
					fmt.Fprintf(&b, "machine: %s\nexit_code: %d\n", name, res.ExitCode)
					if res.Error != "" {
						fmt.Fprintf(&b, "error: %s\n", res.Error)
					}
					if res.Stdout != "" {
						b.WriteString("\n--- stdout ---\n")
						b.WriteString(res.Stdout)
						if !strings.HasSuffix(res.Stdout, "\n") {
							b.WriteByte('\n')
						}
					}
					if res.Stderr != "" {
						b.WriteString("\n--- stderr ---\n")
						b.WriteString(res.Stderr)
						if !strings.HasSuffix(res.Stderr, "\n") {
							b.WriteByte('\n')
						}
					}
					if res.Stdout == "" && res.Stderr == "" && res.Error == "" {
						b.WriteString("\n(no output)\n")
					}
					return b.String(), nil
				},
			},
		}
	}
}
