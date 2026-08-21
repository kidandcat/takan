// Package agenthub tracks connected machine agents (outbound WebSocket).
package agenthub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Authenticator validates agent bearer and returns machine identity.
type Authenticator func(ctx context.Context, token string) (machineID, userID, name string, err error)

// Touch is called on hello / activity.
type Touch func(ctx context.Context, machineID string)

// JobEventHandler is invoked when an agent reports that a job reached a
// terminal status (done, failed, cancelled). Best-effort; may be missed if
// the agent is disconnected when the process exits.
type JobEventHandler func(userID, machineName string, job AIJob)

// Hub is the agent WebSocket registry.
type Hub struct {
	Auth  Authenticator
	Touch Touch
	// OnJobEvent optional: MCP / other listeners for job completion.
	OnJobEvent JobEventHandler

	mu      sync.RWMutex
	agents  map[string]*agent // machineID -> conn
	pending map[string]*pendingTask

	waitMu  sync.Mutex
	waiters map[string][]chan AIJob // machineID\x00jobID -> waiters
}

type pendingTask struct {
	machineID string
	ch        chan wireMsg
}

type agent struct {
	machineID string
	userID    string
	name      string
	conn      *websocket.Conn
	writeMu   sync.Mutex
}

// Result is a remote bash outcome.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Error    string
}

// AIJob is a long-running AI task on a machine (any configured runner).
type AIJob struct {
	JobID       string `json:"job_id"`
	Agent       string `json:"agent"` // runner id (legacy field name)
	Runner      string `json:"runner,omitempty"`
	ParentJobID string `json:"parent_job_id,omitempty"`
	Status      string `json:"status"` // running | done | failed | cancelled | unknown
	ExitCode    int    `json:"exit_code,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Output      string `json:"output,omitempty"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	TotalBytes  int    `json:"total_bytes,omitempty"`
}

// AIStartResult is returned when a headless AI job is launched.
type AIStartResult struct {
	JobID       string `json:"job_id"`
	Agent       string `json:"agent"`
	Runner      string `json:"runner,omitempty"`
	ParentJobID string `json:"parent_job_id,omitempty"`
	Status      string `json:"status"`
	PID         int    `json:"pid,omitempty"`
	Error       string `json:"error,omitempty"`
}

type wireMsg struct {
	Type        string  `json:"type"`
	TaskID      string  `json:"task_id,omitempty"`
	Command     string  `json:"command,omitempty"`
	ExitCode    int     `json:"exit_code,omitempty"`
	Stdout      string  `json:"stdout,omitempty"`
	Stderr      string  `json:"stderr,omitempty"`
	Error       string  `json:"error,omitempty"`
	Name        string  `json:"name,omitempty"`
	Agent       string  `json:"agent,omitempty"`
	Runner      string  `json:"runner,omitempty"`
	Prompt      string  `json:"prompt,omitempty"`
	Cwd         string  `json:"cwd,omitempty"`
	ParentJobID string  `json:"parent_job_id,omitempty"`
	JobID       string  `json:"job_id,omitempty"`
	Status      string  `json:"status,omitempty"`
	PID         int     `json:"pid,omitempty"`
	Output      string  `json:"output,omitempty"`
	StartedAt   string  `json:"started_at,omitempty"`
	FinishedAt  string  `json:"finished_at,omitempty"`
	TailBytes   int     `json:"tail_bytes,omitempty"`
	MaxBytes    int     `json:"max_bytes,omitempty"`
	Truncated   bool    `json:"truncated,omitempty"`
	TotalBytes  int     `json:"total_bytes,omitempty"`
	Jobs        []AIJob `json:"jobs,omitempty"`
	Html        string  `json:"html,omitempty"`
}

func JobTerminal(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func New(auth Authenticator, touch Touch) *Hub {
	return &Hub{
		Auth:    auth,
		Touch:   touch,
		agents:  make(map[string]*agent),
		pending: make(map[string]*pendingTask),
		waiters: make(map[string][]chan AIJob),
	}
}

// Online returns whether a machine is connected.
func (h *Hub) Online(machineID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.agents[machineID]
	return ok
}

// OnlineNames for a user.
func (h *Hub) OnlineNames(userID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for _, a := range h.agents {
		if a.userID == userID {
			out = append(out, a.name)
		}
	}
	return out
}

func (h *Hub) findAgent(userID, machineName string) *agent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ag := range h.agents {
		if ag.userID == userID && ag.name == machineName {
			return ag
		}
	}
	return nil
}

// rpc sends a request to a machine and waits for a matching task_id reply.
func (h *Hub) rpc(ctx context.Context, userID, machineName string, req wireMsg, timeout time.Duration) (*wireMsg, error) {
	a := h.findAgent(userID, machineName)
	if a == nil {
		return nil, fmt.Errorf("machine %q is offline or unknown", machineName)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	taskID := uuid.NewString()
	req.TaskID = taskID
	ch := make(chan wireMsg, 1)
	h.mu.Lock()
	h.pending[taskID] = &pendingTask{machineID: a.machineID, ch: ch}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, taskID)
		h.mu.Unlock()
	}()

	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	a.writeMu.Lock()
	err = a.conn.WriteMessage(websocket.TextMessage, raw)
	a.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("timeout after %s", timeout)
	case res := <-ch:
		return &res, nil
	}
}

// RunBash sends a command to a machine owned by userID.
func (h *Hub) RunBash(ctx context.Context, userID, machineName, command string, timeout time.Duration) (*Result, error) {
	res, err := h.rpc(ctx, userID, machineName, wireMsg{Type: "bash", Command: command}, timeout)
	if err != nil {
		return nil, err
	}
	return &Result{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr, Error: res.Error}, nil
}

// StartAI launches a long-running job on the machine using a command template.
// commandTmpl may contain {{prompt}}; the agent shell-quotes and injects the prompt.
// runnerID is stored for status display (e.g. "claude", "grok", custom id).
// Returns as soon as the process has been spawned (does not wait for completion).
func (h *Hub) StartAI(ctx context.Context, userID, machineName, runnerID, commandTmpl, prompt, cwd, parentJobID string) (*AIStartResult, error) {
	runnerID = strings.TrimSpace(runnerID)
	commandTmpl = strings.TrimSpace(commandTmpl)
	prompt = strings.TrimSpace(prompt)
	if commandTmpl == "" {
		return nil, fmt.Errorf("command required")
	}
	if prompt == "" {
		return nil, fmt.Errorf("prompt required")
	}
	if runnerID == "" {
		runnerID = "custom"
	}
	res, err := h.rpc(ctx, userID, machineName, wireMsg{
		Type:        "ai_start",
		Agent:       runnerID,
		Runner:      runnerID,
		Command:     commandTmpl,
		Prompt:      prompt,
		Cwd:         strings.TrimSpace(cwd),
		ParentJobID: strings.TrimSpace(parentJobID),
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Error != "" && res.JobID == "" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return &AIStartResult{
		JobID:       res.JobID,
		Agent:       runnerID,
		Runner:      runnerID,
		ParentJobID: strings.TrimSpace(parentJobID),
		Status:      res.Status,
		PID:         res.PID,
		Error:       res.Error,
	}, nil
}

// AIStatus returns status (and log tail) for a job. Empty jobID lists recent jobs.
func (h *Hub) AIStatus(ctx context.Context, userID, machineName, jobID string, tailBytes int) (*AIJob, []AIJob, error) {
	if tailBytes <= 0 {
		tailBytes = 12_000
	}
	if tailBytes > 100_000 {
		tailBytes = 100_000
	}
	res, err := h.rpc(ctx, userID, machineName, wireMsg{
		Type:      "ai_status",
		JobID:     strings.TrimSpace(jobID),
		TailBytes: tailBytes,
	}, 20*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if res.Error != "" && res.JobID == "" && len(res.Jobs) == 0 {
		return nil, nil, fmt.Errorf("%s", res.Error)
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, res.Jobs, nil
	}
	return jobFromWire(res), nil, nil
}

func jobFromWire(res *wireMsg) *AIJob {
	if res == nil {
		return &AIJob{Status: "unknown"}
	}
	runner := res.Runner
	if runner == "" {
		runner = res.Agent
	}
	return &AIJob{
		JobID:       res.JobID,
		Agent:       res.Agent,
		Runner:      runner,
		ParentJobID: res.ParentJobID,
		Status:      res.Status,
		ExitCode:    res.ExitCode,
		PID:         res.PID,
		Cwd:         res.Cwd,
		Prompt:      res.Prompt,
		Output:      res.Output,
		Error:       res.Error,
		StartedAt:   res.StartedAt,
		FinishedAt:  res.FinishedAt,
		Truncated:   res.Truncated,
		TotalBytes:  res.TotalBytes,
	}
}

func waiterKey(machineID, jobID string) string {
	return machineID + "\x00" + jobID
}

func (h *Hub) addJobWaiter(machineID, jobID string) chan AIJob {
	ch := make(chan AIJob, 1)
	h.waitMu.Lock()
	h.waiters[waiterKey(machineID, jobID)] = append(h.waiters[waiterKey(machineID, jobID)], ch)
	h.waitMu.Unlock()
	return ch
}

func (h *Hub) removeJobWaiter(machineID, jobID string, ch chan AIJob) {
	h.waitMu.Lock()
	defer h.waitMu.Unlock()
	k := waiterKey(machineID, jobID)
	list := h.waiters[k]
	out := list[:0]
	for _, c := range list {
		if c != ch {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(h.waiters, k)
	} else {
		h.waiters[k] = out
	}
}

func (h *Hub) signalJobWaiters(machineID string, job AIJob) {
	h.waitMu.Lock()
	list := append([]chan AIJob(nil), h.waiters[waiterKey(machineID, job.JobID)]...)
	h.waitMu.Unlock()
	for _, ch := range list {
		select {
		case ch <- job:
		default:
		}
	}
}

// CancelAI asks the agent to kill a running job's process group.
func (h *Hub) CancelAI(ctx context.Context, userID, machineName, jobID string) (*AIJob, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("job_id required")
	}
	res, err := h.rpc(ctx, userID, machineName, wireMsg{Type: "ai_cancel", JobID: jobID}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Error != "" && res.JobID == "" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	if res.Error != "" && res.Status == "unknown" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return jobFromWire(res), nil
}

// AILog returns the job transcript from the start of output.log (capped).
func (h *Hub) AILog(ctx context.Context, userID, machineName, jobID string, maxBytes int) (*AIJob, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("job_id required")
	}
	if maxBytes <= 0 {
		maxBytes = 500_000
	}
	if maxBytes > 1_048_576 {
		maxBytes = 1_048_576
	}
	res, err := h.rpc(ctx, userID, machineName, wireMsg{
		Type:     "ai_log",
		JobID:    jobID,
		MaxBytes: maxBytes,
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Error != "" && (res.JobID == "" || res.Status == "unknown") {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return jobFromWire(res), nil
}

const (
	defaultWatchWait = 90 * time.Second
	maxWatchWait     = 300 * time.Second
	watchPollEvery   = 1500 * time.Millisecond
)

// WatchAI waits until a job is terminal or wait elapses. Uses agent completion
// events when available and polls status as a fallback.
func (h *Hub) WatchAI(ctx context.Context, userID, machineName, jobID string, wait time.Duration, tailBytes int) (*AIJob, bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, false, fmt.Errorf("job_id required")
	}
	if wait <= 0 {
		wait = defaultWatchWait
	}
	if wait > maxWatchWait {
		wait = maxWatchWait
	}
	if tailBytes <= 0 {
		tailBytes = 12_000
	}
	if tailBytes > 100_000 {
		tailBytes = 100_000
	}

	a := h.findAgent(userID, machineName)
	if a == nil {
		return nil, false, fmt.Errorf("machine %q is offline or unknown", machineName)
	}
	ch := h.addJobWaiter(a.machineID, jobID)
	defer h.removeJobWaiter(a.machineID, jobID, ch)

	deadline := time.Now().Add(wait)
	for {
		job, _, err := h.AIStatus(ctx, userID, machineName, jobID, tailBytes)
		if err != nil {
			return nil, false, err
		}
		if JobTerminal(job.Status) {
			return job, false, nil
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return job, true, nil
		}
		poll := watchPollEvery
		if poll > remain {
			poll = remain
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job, true, ctx.Err()
		case <-ch:
			timer.Stop()
			// Re-fetch so the caller gets a log tail, not just the event stub.
			continue
		case <-timer.C:
		}
	}
}

const maxDisplayHTML = 1 << 20 // 1 MiB

// ShowHTML pushes a static HTML document to a machine's local display server.
func (h *Hub) ShowHTML(ctx context.Context, userID, machineName, html string) error {
	if len(html) > maxDisplayHTML {
		return fmt.Errorf("html too large (%d bytes, max %d)", len(html), maxDisplayHTML)
	}
	res, err := h.rpc(ctx, userID, machineName, wireMsg{Type: "display", Html: html}, 30*time.Second)
	if err != nil {
		return err
	}
	if res.Error != "" {
		return fmt.Errorf("%s", res.Error)
	}
	return nil
}

// HandleWS is the /agent/ws endpoint.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(tok, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(tok, "Bearer "))
	}
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	machineID, userID, name, err := h.Auth(r.Context(), tok)
	if err != nil || machineID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(2 << 20)
	ag := &agent{machineID: machineID, userID: userID, name: name, conn: c}

	h.mu.Lock()
	if old, ok := h.agents[machineID]; ok {
		_ = old.conn.Close()
	}
	h.agents[machineID] = ag
	h.mu.Unlock()
	if h.Touch != nil {
		h.Touch(r.Context(), machineID)
	}
	log.Printf("agent connected machine=%s name=%s user=%s", machineID, name, userID)

	defer func() {
		h.mu.Lock()
		if cur, ok := h.agents[machineID]; ok && cur == ag {
			delete(h.agents, machineID)
		}
		h.mu.Unlock()
		_ = c.Close()
		log.Printf("agent disconnected machine=%s", machineID)
	}()

	_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
	c.SetPongHandler(func(string) error {
		_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			ag.writeMu.Lock()
			err := ag.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
			ag.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
		var msg wireMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "hello", "ping":
			if h.Touch != nil {
				h.Touch(r.Context(), machineID)
			}
			if msg.Type == "ping" {
				ag.writeMu.Lock()
				_ = ag.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong"}`))
				ag.writeMu.Unlock()
			}
		case "bash_result", "ai_start_result", "ai_status_result", "ai_cancel_result", "ai_log_result", "display_result":
			h.mu.Lock()
			pt := h.pending[msg.TaskID]
			// Only the agent that owns the task may complete it.
			if pt != nil && pt.machineID != machineID {
				pt = nil
			}
			h.mu.Unlock()
			if pt != nil {
				pt.ch <- msg
			}
		case "ai_done":
			job := jobFromWire(&msg)
			if job.JobID != "" {
				h.signalJobWaiters(machineID, *job)
			}
			if h.OnJobEvent != nil && job.JobID != "" {
				h.OnJobEvent(userID, name, *job)
			}
		}
	}
}
