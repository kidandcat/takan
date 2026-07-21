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

// Hub is the agent WebSocket registry.
type Hub struct {
	Auth  Authenticator
	Touch Touch

	mu      sync.RWMutex
	agents  map[string]*agent // machineID -> conn
	pending map[string]*pendingTask
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

// AIJob is a long-running Claude/Grok headless task on a machine.
type AIJob struct {
	JobID      string `json:"job_id"`
	Agent      string `json:"agent"`
	Status     string `json:"status"` // running | done | failed | unknown
	ExitCode   int    `json:"exit_code,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// AIStartResult is returned when a headless AI job is launched.
type AIStartResult struct {
	JobID  string `json:"job_id"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
	PID    int    `json:"pid,omitempty"`
	Error  string `json:"error,omitempty"`
}

type wireMsg struct {
	Type        string   `json:"type"`
	TaskID      string   `json:"task_id,omitempty"`
	Command     string   `json:"command,omitempty"`
	ExitCode    int      `json:"exit_code,omitempty"`
	Stdout      string   `json:"stdout,omitempty"`
	Stderr      string   `json:"stderr,omitempty"`
	Error       string   `json:"error,omitempty"`
	Name        string   `json:"name,omitempty"`
	Agent       string   `json:"agent,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	AutoApprove bool     `json:"auto_approve"`
	JobID       string   `json:"job_id,omitempty"`
	Status      string   `json:"status,omitempty"`
	PID         int      `json:"pid,omitempty"`
	Output      string   `json:"output,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	FinishedAt  string   `json:"finished_at,omitempty"`
	TailBytes   int      `json:"tail_bytes,omitempty"`
	Jobs        []AIJob  `json:"jobs,omitempty"`
}

func New(auth Authenticator, touch Touch) *Hub {
	return &Hub{
		Auth:    auth,
		Touch:   touch,
		agents:  make(map[string]*agent),
		pending: make(map[string]*pendingTask),
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

// StartAI launches a long-running headless Claude/Grok job on the machine.
// Returns as soon as the process has been spawned (does not wait for completion).
func (h *Hub) StartAI(ctx context.Context, userID, machineName, agentName, prompt, cwd string, autoApprove bool) (*AIStartResult, error) {
	agentName = strings.ToLower(strings.TrimSpace(agentName))
	if agentName != "claude" && agentName != "grok" {
		return nil, fmt.Errorf(`agent must be "claude" or "grok"`)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt required")
	}
	res, err := h.rpc(ctx, userID, machineName, wireMsg{
		Type:        "ai_start",
		Agent:       agentName,
		Prompt:      prompt,
		Cwd:         strings.TrimSpace(cwd),
		AutoApprove: autoApprove,
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if res.Error != "" && res.JobID == "" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	return &AIStartResult{
		JobID:  res.JobID,
		Agent:  agentName,
		Status: res.Status,
		PID:    res.PID,
		Error:  res.Error,
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
	job := &AIJob{
		JobID:      res.JobID,
		Agent:      res.Agent,
		Status:     res.Status,
		ExitCode:   res.ExitCode,
		PID:        res.PID,
		Cwd:        res.Cwd,
		Prompt:     res.Prompt,
		Output:     res.Output,
		Error:      res.Error,
		StartedAt:  res.StartedAt,
		FinishedAt: res.FinishedAt,
	}
	return job, nil, nil
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
		case "bash_result", "ai_start_result", "ai_status_result":
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
		}
	}
}
