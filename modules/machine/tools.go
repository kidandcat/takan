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
		cfg, _ := LoadConfig(ctx, st, userID)

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
		}

		if cfg.BashEnabled {
			tools = append(tools, mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_bash",
					Description: "Run a shell command on a registered machine via its takan-agent. " +
						"The agent must be online. Prefer short non-interactive commands. " +
						"Pass machine name from machine_list. For long-running AI work use machine_ai_run instead. " +
						"Disable in Takan panel → Machines if you want AI-agent-only access.",
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
					cfg, err := LoadConfig(ctx, st, userID)
					if err != nil {
						return "", err
					}
					if !cfg.BashEnabled {
						return "", fmt.Errorf("direct bash is disabled — enable it in Takan panel → Machines, or use machine_ai_run")
					}
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
			})
		}

		if !cfg.AITasksEnabled {
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
					Description: "Launch an autonomous AI agent on a machine. Returns immediately with job_id " +
						"(does not wait for the agent to finish). owner is required: the Grok Bot launching the job " +
						"(Minerva, Menta, TPVLINE, Gestor, Hardware, Games). After launch, follow the job: " +
						"machine_ai_watch waits until it finishes; machine_ai_status is a quick status + log tail; " +
						"machine_ai_log fetches the full transcript; machine_ai_cancel kills a running job; " +
						"machine_ai_reply continues as a new job (runners are one-shot and cannot be interrupted in-process). " +
						"Clients with an MCP SSE stream may also receive notifications/takan/machine_ai_job when the job ends. " +
						"State a clear high-level goal only; agents are capable and do not need step-by-step instructions. " +
						"Pick a runner id configured in the Takan panel (not free-form agent names). " +
						"Enabled runners: " + runnersBlurb + ". " +
						"The runner command template injects {{prompt}}.",
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
							"owner": map[string]any{
								"type":        "string",
								"description": "Grok Bot that launched the job (Minerva, Menta, TPVLINE, Gestor, Hardware, Games)",
							},
						},
						"required": []string{"machine", "runner", "prompt", "owner"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					name, runnerID, prompt, cwd, owner, err := parseRunArgs(ctx, st, userID, args)
					if err != nil {
						return "", err
					}
					cfg, err := LoadConfig(ctx, st, userID)
					if err != nil {
						return "", err
					}
					r, err := enabledRunner(cfg, runnerID)
					if err != nil {
						return "", err
					}
					res, err := hub.StartAI(ctx, userID, name, r.ID, r.Command, prompt, cwd, "", owner)
					if err != nil {
						return "", err
					}
					out := map[string]any{
						"machine": name,
						"job_id":  res.JobID,
						"runner":  r.ID,
						"owner":   owner,
						"name":    r.Name,
						"command": r.Command,
						"status":  res.Status,
						"pid":     res.PID,
						"hint":    followHint(),
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
					Description: "Quick status of an AI job started with machine_ai_run (or a list of recent jobs). " +
						"Pass job_id for one job including a short log tail. Omit job_id to list recent jobs. " +
						"To wait until the job finishes use machine_ai_watch. For the full transcript use machine_ai_log.",
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
					if err := requireAI(ctx, st, userID); err != nil {
						return "", err
					}
					name, err := requireMachine(ctx, st, userID, strArg(args, "machine"))
					if err != nil {
						return "", err
					}
					jobID := strArg(args, "job_id")
					tail := intArg(args, "tail_bytes", 12_000, 100_000)
					job, list, err := hub.AIStatus(ctx, userID, name, jobID, tail)
					if err != nil {
						return "", err
					}
					if jobID == "" {
						return formatJobList(name, list), nil
					}
					return formatJob(name, job, nil), nil
				},
			},
			mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_ai_watch",
					Description: "Wait until an AI job finishes (done, failed, or cancelled) and return status plus a log tail. " +
						"Blocks this tool call; the hub is notified by the agent when the process exits, and polls status as a fallback — " +
						"you do not need to babysit machine_ai_status. If the wait times out and the job is still running, call again with the same job_id. " +
						"Default wait 90s, max 300s. machine_ai_run stays non-blocking; use this (or the SSE notification) to follow.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name"},
							"job_id":  map[string]any{"type": "string", "description": "Job id from machine_ai_run"},
							"timeout_seconds": map[string]any{
								"type":        "integer",
								"description": "How long to wait (default 90, max 300)",
							},
							"tail_bytes": map[string]any{
								"type":        "integer",
								"description": "Max bytes of log tail (default 12000, max 100000)",
							},
						},
						"required": []string{"machine", "job_id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					if err := requireAI(ctx, st, userID); err != nil {
						return "", err
					}
					name, err := requireMachine(ctx, st, userID, strArg(args, "machine"))
					if err != nil {
						return "", err
					}
					jobID := strArg(args, "job_id")
					if jobID == "" {
						return "", fmt.Errorf("job_id required")
					}
					wait := time.Duration(intArg(args, "timeout_seconds", 90, 300)) * time.Second
					tail := intArg(args, "tail_bytes", 12_000, 100_000)
					job, timedOut, err := hub.WatchAI(ctx, userID, name, jobID, wait, tail)
					if err != nil {
						return "", err
					}
					extra := map[string]any{"timed_out": timedOut}
					if timedOut && !agenthub.JobTerminal(job.Status) {
						extra["hint"] = "still running — call machine_ai_watch again with the same job_id"
					}
					return formatJob(name, job, extra), nil
				},
			},
			mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_ai_log",
					Description: "Fetch the full transcript (output.log) for an AI job, from the start — not just the status tail. " +
						"May be truncated if the log is very large; check truncated and total_bytes. " +
						"Use machine_ai_status for a short recent tail.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name"},
							"job_id":  map[string]any{"type": "string", "description": "Job id from machine_ai_run"},
							"max_bytes": map[string]any{
								"type":        "integer",
								"description": "Max transcript bytes to return (default 500000, max 1048576)",
							},
						},
						"required": []string{"machine", "job_id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					if err := requireAI(ctx, st, userID); err != nil {
						return "", err
					}
					name, err := requireMachine(ctx, st, userID, strArg(args, "machine"))
					if err != nil {
						return "", err
					}
					jobID := strArg(args, "job_id")
					if jobID == "" {
						return "", fmt.Errorf("job_id required")
					}
					maxBytes := intArg(args, "max_bytes", 500_000, 1_048_576)
					job, err := hub.AILog(ctx, userID, name, jobID, maxBytes)
					if err != nil {
						return "", err
					}
					extra := map[string]any{
						"truncated":   job.Truncated,
						"total_bytes": job.TotalBytes,
						"output":      job.Output,
					}
					return formatJob(name, job, extra), nil
				},
			},
			mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_ai_cancel",
					Description: "Cancel a running AI job. The agent kills the process group and records status cancelled. " +
						"If the job already finished, returns its current status (not rewritten to cancelled).",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name"},
							"job_id":  map[string]any{"type": "string", "description": "Job id from machine_ai_run"},
						},
						"required": []string{"machine", "job_id"},
					},
				},
				Handler: func(ctx context.Context, userID string, args map[string]any) (string, error) {
					if err := requireAI(ctx, st, userID); err != nil {
						return "", err
					}
					name, err := requireMachine(ctx, st, userID, strArg(args, "machine"))
					if err != nil {
						return "", err
					}
					jobID := strArg(args, "job_id")
					if jobID == "" {
						return "", fmt.Errorf("job_id required")
					}
					job, err := hub.CancelAI(ctx, userID, name, jobID)
					if err != nil {
						return "", err
					}
					return formatJob(name, job, nil), nil
				},
			},
			mcp.RegisteredTool{
				Tool: mcp.Tool{
					Name: "machine_ai_reply",
					Description: "Continue an existing AI job by starting a NEW job on the same machine. " +
						"The new prompt includes the parent prompt, a slice of the parent log, and your follow-up; " +
						"the new job stores parent_job_id. " +
						"Limitation: typical runners (e.g. grok --always-approve -p, claude -p) are one-shot with no live stdin session — " +
						"this cannot attach to or interrupt a running process. To stop the parent first, call machine_ai_cancel. " +
						"Defaults to the parent job's runner, cwd, and owner.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"machine": map[string]any{"type": "string", "description": "Machine name"},
							"job_id":  map[string]any{"type": "string", "description": "Parent job id to continue"},
							"message": map[string]any{
								"type":        "string",
								"description": "Follow-up instruction for the new job",
							},
							"runner": map[string]any{
								"type":        "string",
								"enum":        runnerIDs,
								"description": "Override runner (default: parent job's runner)",
							},
							"cwd": map[string]any{
								"type":        "string",
								"description": "Override working directory (default: parent job's cwd)",
							},
							"owner": map[string]any{
								"type":        "string",
								"description": "Override owner bot name (default: parent job's owner)",
							},
						},
						"required": []string{"machine", "job_id", "message"},
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
					name, err := requireMachine(ctx, st, userID, strArg(args, "machine"))
					if err != nil {
						return "", err
					}
					parentID := strArg(args, "job_id")
					message := strArg(args, "message")
					if parentID == "" || message == "" {
						return "", fmt.Errorf("job_id and message required")
					}
					parent, _, err := hub.AIStatus(ctx, userID, name, parentID, replyLogContext)
					if err != nil {
						return "", err
					}
					if parent.JobID == "" {
						return "", fmt.Errorf("unknown job %q", parentID)
					}
					runnerID := strArg(args, "runner")
					if runnerID == "" {
						runnerID = parent.Runner
						if runnerID == "" {
							runnerID = parent.Agent
						}
					}
					r, err := enabledRunner(cfg, runnerID)
					if err != nil {
						return "", err
					}
					cwd := strArg(args, "cwd")
					if cwd == "" {
						cwd = strings.TrimSpace(parent.Cwd)
					}
					owner, err := resolveOwner(strArg(args, "owner"), parent.Owner)
					if err != nil {
						return "", err
					}
					prompt := BuildContinuePrompt(parent.Prompt, parent.Output, message)
					res, err := hub.StartAI(ctx, userID, name, r.ID, r.Command, prompt, cwd, parent.JobID, owner)
					if err != nil {
						return "", err
					}
					out := map[string]any{
						"machine":       name,
						"job_id":        res.JobID,
						"parent_job_id": parent.JobID,
						"runner":        r.ID,
						"owner":         owner,
						"name":          r.Name,
						"command":       r.Command,
						"status":        res.Status,
						"pid":           res.PID,
						"hint":          followHint() + " This is a new job; the parent was not interrupted.",
					}
					if res.Error != "" {
						out["error"] = res.Error
					}
					b, _ := json.MarshalIndent(out, "", "  ")
					return string(b), nil
				},
			},
		)
		return tools
	}
}

func requireAI(ctx context.Context, st *store.Store, userID string) error {
	cfg, err := LoadConfig(ctx, st, userID)
	if err != nil {
		return err
	}
	if !cfg.AITasksEnabled {
		return fmt.Errorf("AI tasks are disabled — enable them in Takan panel → Machines")
	}
	return nil
}

func requireMachine(ctx context.Context, st *store.Store, userID, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("machine required")
	}
	if _, err := st.MachineByUserAndName(ctx, userID, name); err != nil {
		return "", fmt.Errorf("unknown machine %q", name)
	}
	return name, nil
}

func parseRunArgs(ctx context.Context, st *store.Store, userID string, args map[string]any) (name, runnerID, prompt, cwd, owner string, err error) {
	if err = requireAI(ctx, st, userID); err != nil {
		return "", "", "", "", "", err
	}
	name = strArg(args, "machine")
	runnerID = strArg(args, "runner")
	if runnerID == "" {
		runnerID = strArg(args, "agent") // legacy
	}
	prompt = strArg(args, "prompt")
	cwd = strArg(args, "cwd")
	owner, err = resolveOwner(strArg(args, "owner"), "")
	if err != nil {
		return "", "", "", "", "", err
	}
	if name == "" || runnerID == "" || prompt == "" {
		return "", "", "", "", "", fmt.Errorf("machine, runner and prompt required")
	}
	name, err = requireMachine(ctx, st, userID, name)
	if err != nil {
		return "", "", "", "", "", err
	}
	return name, runnerID, prompt, cwd, owner, nil
}

// resolveOwner returns the explicit owner, or parentOwner if the arg is empty.
func resolveOwner(arg, parentOwner string) (string, error) {
	owner := strings.TrimSpace(arg)
	if owner == "" {
		owner = strings.TrimSpace(parentOwner)
	}
	if owner == "" {
		return "", fmt.Errorf("owner required")
	}
	return owner, nil
}

func enabledRunner(cfg Config, runnerID string) (Runner, error) {
	r, ok := cfg.RunnerByID(runnerID)
	if !ok || !r.Enabled {
		var ids []string
		for _, e := range cfg.EnabledRunners() {
			ids = append(ids, e.ID)
		}
		return Runner{}, fmt.Errorf("unknown or disabled runner %q — enabled: %s", runnerID, strings.Join(ids, ", "))
	}
	return r, nil
}

func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

func intArg(args map[string]any, key string, def, max int) int {
	v := def
	if n, ok := args[key].(float64); ok && n > 0 {
		v = int(n)
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

func followHint() string {
	return "Job launched. Follow with machine_ai_watch (waits for completion), machine_ai_status (tail), or machine_ai_log (full transcript). Cancel: machine_ai_cancel. Continue later: machine_ai_reply (starts a new job)."
}

func runnerOf(j agenthub.AIJob) string {
	if j.Runner != "" {
		return j.Runner
	}
	return j.Agent
}

func formatJobList(machine string, list []agenthub.AIJob) string {
	type row struct {
		JobID       string `json:"job_id"`
		Runner      string `json:"runner"`
		Owner       string `json:"owner,omitempty"`
		Status      string `json:"status"`
		ExitCode    int    `json:"exit_code,omitempty"`
		PID         int    `json:"pid,omitempty"`
		Cwd         string `json:"cwd,omitempty"`
		Prompt      string `json:"prompt,omitempty"`
		ParentJobID string `json:"parent_job_id,omitempty"`
		StartedAt   string `json:"started_at,omitempty"`
		FinishedAt  string `json:"finished_at,omitempty"`
	}
	rows := make([]row, 0, len(list))
	for _, j := range list {
		rows = append(rows, row{
			JobID: j.JobID, Runner: runnerOf(j), Owner: j.Owner, Status: j.Status,
			ExitCode: j.ExitCode, PID: j.PID, Cwd: j.Cwd, Prompt: j.Prompt,
			ParentJobID: j.ParentJobID,
			StartedAt:   j.StartedAt, FinishedAt: j.FinishedAt,
		})
	}
	if len(rows) == 0 {
		return "No AI jobs on this machine yet."
	}
	b, _ := json.MarshalIndent(map[string]any{"machine": machine, "jobs": rows}, "", "  ")
	return string(b)
}

func formatJob(machine string, job *agenthub.AIJob, extra map[string]any) string {
	if job == nil {
		return `{"error":"no job"}`
	}
	out := map[string]any{
		"machine":     machine,
		"job_id":      job.JobID,
		"runner":      runnerOf(*job),
		"owner":       job.Owner,
		"status":      job.Status,
		"exit_code":   job.ExitCode,
		"pid":         job.PID,
		"cwd":         job.Cwd,
		"prompt":      job.Prompt,
		"started_at":  job.StartedAt,
		"finished_at": job.FinishedAt,
	}
	if job.ParentJobID != "" {
		out["parent_job_id"] = job.ParentJobID
	}
	if job.Error != "" {
		out["error"] = job.Error
	}
	if job.Output != "" {
		if _, hasFull := extra["output"]; !hasFull {
			out["output_tail"] = job.Output
		}
	}
	for k, v := range extra {
		out[k] = v
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}

const replyLogContext = 32_000

// BuildContinuePrompt assembles a follow-up prompt from parent context.
// parentLog is typically a tail of the previous transcript.
func BuildContinuePrompt(parentPrompt, parentLog, followUp string) string {
	parentPrompt = strings.TrimSpace(parentPrompt)
	parentLog = strings.TrimSpace(parentLog)
	followUp = strings.TrimSpace(followUp)
	if len(parentLog) > replyLogContext {
		parentLog = parentLog[len(parentLog)-replyLogContext:]
	}
	var b strings.Builder
	b.WriteString("You are continuing a previous task on this machine.\n\n")
	b.WriteString("## Previous prompt\n")
	if parentPrompt == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(parentPrompt)
		b.WriteByte('\n')
	}
	b.WriteString("\n## Previous output\n")
	if parentLog == "" {
		b.WriteString("(no output captured)\n")
	} else {
		b.WriteString(parentLog)
		if !strings.HasSuffix(parentLog, "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n## Follow-up\n")
	b.WriteString(followUp)
	return b.String()
}
