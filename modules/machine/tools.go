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
		tools := []mcp.RegisteredTool{
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
						"Pass machine name from machine_list. For long-running AI work use machine_ai_run instead.",
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

		cfg, err := LoadConfig(ctx, st, userID)
		if err != nil || !cfg.AITasksEnabled {
			return tools
		}
		enabled := cfg.EnabledRunners()
		if len(enabled) == 0 {
			return tools
		}

		runnerIDs := make([]string, 0, len(enabled))
		var descParts []string
		for _, r := range enabled {
			runnerIDs = append(runnerIDs, r.ID)
			descParts = append(descParts, fmt.Sprintf("%s (%s): %s", r.ID, r.Name, r.Command))
		}
		runnersBlurb := strings.Join(descParts, "; ")

		tools = append(tools,
			mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_ai_runners",
					Description: "List AI launch runners configured for this account (enabled only). " +
						"Configure in Takan panel → Machines → AI tasks. " +
						"Use runner id with machine_ai_run.",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					cfg, err := LoadConfig(ctx, st, userID)
					if err != nil {
						return "", err
					}
					if !cfg.AITasksEnabled {
						return "AI tasks are disabled in the Takan panel (Machines → AI tasks).", nil
					}
					type row struct {
						ID      string `json:"id"`
						Name    string `json:"name"`
						Command string `json:"command"`
						Builtin bool   `json:"builtin,omitempty"`
					}
					var rows []row
					for _, r := range cfg.EnabledRunners() {
						rows = append(rows, row{ID: r.ID, Name: r.Name, Command: r.Command, Builtin: r.Builtin})
					}
					if len(rows) == 0 {
						return "No enabled runners. Enable Claude/Grok or add a free command in the panel.", nil
					}
					b, _ := json.MarshalIndent(rows, "", "  ")
					return string(b), nil
				},
			},
			mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_ai_run",
					Description: "Start a long-running AI job on a machine (returns immediately with job_id). " +
						"Pick a runner id configured in the Takan panel (not free-form agent names). " +
						"Enabled runners: " + runnersBlurb + ". " +
						"Poll with machine_ai_status. The runner command template injects {{prompt}}.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name from machine_list"},
							"runner": map[string]any{
								"type":        "string",
								"enum":        runnerIDs,
								"description": "Runner id from machine_ai_runners / panel (e.g. claude, grok, or a custom id)",
							},
							"prompt": map[string]any{
								"type":        "string",
								"description": "Task prompt injected into the runner command ({{prompt}})",
							},
							"cwd": map[string]any{
								"type":        "string",
								"description": "Working directory on the machine (optional)",
							},
						},
						"required": []string{"machine", "runner", "prompt"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					cfg, err := LoadConfig(ctx, st, userID)
					if err != nil {
						return "", err
					}
					if !cfg.AITasksEnabled {
						return "", fmt.Errorf("AI tasks are disabled — enable them in Takan panel → Machines")
					}
					name, _ := args["machine"].(string)
					runnerID, _ := args["runner"].(string)
					// accept legacy "agent" for a release
					if runnerID == "" {
						runnerID, _ = args["agent"].(string)
					}
					prompt, _ := args["prompt"].(string)
					cwd, _ := args["cwd"].(string)
					name = strings.TrimSpace(name)
					runnerID = strings.TrimSpace(runnerID)
					prompt = strings.TrimSpace(prompt)
					cwd = strings.TrimSpace(cwd)
					if name == "" || runnerID == "" || prompt == "" {
						return "", fmt.Errorf("machine, runner and prompt required")
					}
					r, ok := cfg.RunnerByID(runnerID)
					if !ok || !r.Enabled {
						var ids []string
						for _, e := range cfg.EnabledRunners() {
							ids = append(ids, e.ID)
						}
						return "", fmt.Errorf("unknown or disabled runner %q — enabled: %s", runnerID, strings.Join(ids, ", "))
					}
					if _, err := st.MachineByUserAndName(ctx, userID, name); err != nil {
						return "", fmt.Errorf("unknown machine %q", name)
					}
					res, err := hub.StartAI(ctx, userID, name, r.ID, r.Command, prompt, cwd)
					if err != nil {
						return "", err
					}
					out := map[string]any{
						"machine": name,
						"job_id":  res.JobID,
						"runner":  r.ID,
						"name":    r.Name,
						"command": r.Command,
						"status":  res.Status,
						"pid":     res.PID,
						"hint":    "Poll with machine_ai_status(machine, job_id).",
					}
					if res.Error != "" {
						out["error"] = res.Error
					}
					b, _ := json.MarshalIndent(out, "", "  ")
					return string(b), nil
				},
			},
			mcp.RegisteredTool{
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
					cfg, err := LoadConfig(ctx, st, userID)
					if err != nil {
						return "", err
					}
					if !cfg.AITasksEnabled {
						return "", fmt.Errorf("AI tasks are disabled — enable them in Takan panel → Machines")
					}
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
							Runner     string `json:"runner"`
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
							runner := j.Runner
							if runner == "" {
								runner = j.Agent
							}
							rows = append(rows, row{
								JobID: j.JobID, Runner: runner, Status: j.Status,
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
					runner := job.Runner
					if runner == "" {
						runner = job.Agent
					}
					out := map[string]any{
						"machine":     name,
						"job_id":      job.JobID,
						"runner":      runner,
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
		)
		return tools
	}
}
