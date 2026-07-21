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
						"Pass machine name from machine_list. For long-running Claude/Grok work use machine_ai_run instead.",
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
			{
				Tool: mcp.Tool{
					Name: "machine_ai_run",
					Description: "Start a long-running headless AI agent job on a machine (returns immediately with job_id). " +
						"Runs Claude Code (`claude -p`) or Grok Build (`grok -p`) in script/print mode. " +
						"Poll progress with machine_ai_status. Requires claude/grok installed and authenticated on the machine. " +
						"auto_approve defaults to true (needed for unattended tool use).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name from machine_list"},
							"agent": map[string]any{
								"type":        "string",
								"enum":        []string{"claude", "grok"},
								"description": `AI agent binary: "claude" (Claude Code) or "grok" (Grok Build)`,
							},
							"prompt": map[string]any{
								"type":        "string",
								"description": "Task prompt passed to the agent in -p / script mode",
							},
							"cwd": map[string]any{
								"type":        "string",
								"description": "Working directory on the machine (optional; default is the agent process cwd)",
							},
							"auto_approve": map[string]any{
								"type":        "boolean",
								"description": "Auto-approve tool permissions (default true). Claude: --dangerously-skip-permissions; Grok: --always-approve",
							},
						},
						"required": []string{"machine", "agent", "prompt"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					name, _ := args["machine"].(string)
					agentName, _ := args["agent"].(string)
					prompt, _ := args["prompt"].(string)
					cwd, _ := args["cwd"].(string)
					name = strings.TrimSpace(name)
					agentName = strings.ToLower(strings.TrimSpace(agentName))
					prompt = strings.TrimSpace(prompt)
					cwd = strings.TrimSpace(cwd)
					if name == "" || agentName == "" || prompt == "" {
						return "", fmt.Errorf("machine, agent and prompt required")
					}
					if agentName != "claude" && agentName != "grok" {
						return "", fmt.Errorf(`agent must be "claude" or "grok"`)
					}
					if _, err := st.MachineByUserAndName(ctx, userID, name); err != nil {
						return "", fmt.Errorf("unknown machine %q", name)
					}
					autoApprove := true
					if v, ok := args["auto_approve"].(bool); ok {
						autoApprove = v
					}
					res, err := hub.StartAI(ctx, userID, name, agentName, prompt, cwd, autoApprove)
					if err != nil {
						return "", err
					}
					out := map[string]any{
						"machine": name,
						"job_id":  res.JobID,
						"agent":   res.Agent,
						"status":  res.Status,
						"pid":     res.PID,
						"hint":    "Poll with machine_ai_status(machine, job_id). Jobs keep running if the agent reconnects.",
					}
					if res.Error != "" {
						out["error"] = res.Error
					}
					b, _ := json.MarshalIndent(out, "", "  ")
					return string(b), nil
				},
			},
			{
				Tool: mcp.Tool{
					Name: "machine_ai_status",
					Description: "Check status of a long-running AI job started with machine_ai_run. " +
						"Pass job_id for one job (includes log tail). Omit job_id to list recent jobs on the machine.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name"},
							"job_id": map[string]any{
								"type":        "string",
								"description": "Job id from machine_ai_run. Omit to list recent jobs.",
							},
							"tail_bytes": map[string]any{
								"type":        "integer",
								"description": "Max bytes of log tail to return (default 12000, max 100000)",
							},
						},
						"required": []string{"machine"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					name, _ := args["machine"].(string)
					jobID, _ := args["job_id"].(string)
					name = strings.TrimSpace(name)
					jobID = strings.TrimSpace(jobID)
					if name == "" {
						return "", fmt.Errorf("machine required")
					}
					if _, err := st.MachineByUserAndName(ctx, userID, name); err != nil {
						return "", fmt.Errorf("unknown machine %q", name)
					}
					tail := 12_000
					if v, ok := args["tail_bytes"].(float64); ok && v > 0 {
						tail = int(v)
					}
					job, list, err := hub.AIStatus(ctx, userID, name, jobID, tail)
					if err != nil {
						return "", err
					}
					if jobID == "" {
						type row struct {
							JobID      string `json:"job_id"`
							Agent      string `json:"agent"`
							Status     string `json:"status"`
							ExitCode   int    `json:"exit_code,omitempty"`
							PID        int    `json:"pid,omitempty"`
							Cwd        string `json:"cwd,omitempty"`
							Prompt     string `json:"prompt,omitempty"`
							StartedAt  string `json:"started_at,omitempty"`
							FinishedAt string `json:"finished_at,omitempty"`
						}
						rows := make([]row, 0, len(list))
						for _, j := range list {
							rows = append(rows, row{
								JobID: j.JobID, Agent: j.Agent, Status: j.Status,
								ExitCode: j.ExitCode, PID: j.PID, Cwd: j.Cwd, Prompt: j.Prompt,
								StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
							})
						}
						if len(rows) == 0 {
							return "No AI jobs on this machine yet.", nil
						}
						b, _ := json.MarshalIndent(map[string]any{"machine": name, "jobs": rows}, "", "  ")
						return string(b), nil
					}
					out := map[string]any{
						"machine":     name,
						"job_id":      job.JobID,
						"agent":       job.Agent,
						"status":      job.Status,
						"exit_code":   job.ExitCode,
						"pid":         job.PID,
						"cwd":         job.Cwd,
						"prompt":      job.Prompt,
						"started_at":  job.StartedAt,
						"finished_at": job.FinishedAt,
					}
					if job.Error != "" {
						out["error"] = job.Error
					}
					if job.Output != "" {
						out["output_tail"] = job.Output
					}
					b, _ := json.MarshalIndent(out, "", "  ")
					return string(b), nil
				},
			},
		}
	}
}
