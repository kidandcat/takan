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
	ch        chan Result
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

type wireMsg struct {
	Type      string `json:"type"`
	TaskID    string `json:"task_id,omitempty"`
	Command   string `json:"command,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
	Name      string `json:"name,omitempty"`
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

// RunBash sends a command to a machine owned by userID.
func (h *Hub) RunBash(ctx context.Context, userID, machineName, command string, timeout time.Duration) (*Result, error) {
	h.mu.RLock()
	var a *agent
	for _, ag := range h.agents {
		if ag.userID == userID && ag.name == machineName {
			a = ag
			break
		}
	}
	h.mu.RUnlock()
	if a == nil {
		return nil, fmt.Errorf("machine %q is offline or unknown", machineName)
	}

	taskID := uuid.NewString()
	ch := make(chan Result, 1)
	h.mu.Lock()
	h.pending[taskID] = &pendingTask{machineID: a.machineID, ch: ch}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, taskID)
		h.mu.Unlock()
	}()

	msg, _ := json.Marshal(wireMsg{Type: "bash", TaskID: taskID, Command: command})
	a.writeMu.Lock()
	err := a.conn.WriteMessage(websocket.TextMessage, msg)
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
		case "bash_result":
			h.mu.Lock()
			pt := h.pending[msg.TaskID]
			// Only the agent that owns the task may complete it.
			if pt != nil && pt.machineID != machineID {
				pt = nil
			}
			h.mu.Unlock()
			if pt != nil {
				pt.ch <- Result{ExitCode: msg.ExitCode, Stdout: msg.Stdout, Stderr: msg.Stderr, Error: msg.Error}
			}
		}
	}
}
