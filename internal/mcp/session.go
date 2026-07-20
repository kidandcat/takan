package mcp

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session is a Streamable HTTP MCP session for one user/client.
type Session struct {
	ID        string
	UserID    string
	CreatedAt time.Time

	mu      sync.Mutex
	streams map[chan []byte]struct{} // open SSE writers
}

// SessionHub tracks active MCP sessions for server-push notifications.
type SessionHub struct {
	mu       sync.RWMutex
	sessions map[string]*Session // id -> session
}

func NewSessionHub() *SessionHub {
	return &SessionHub{sessions: make(map[string]*Session)}
}

func (h *SessionHub) Create(userID string) *Session {
	s := &Session{
		ID:        uuid.NewString(),
		UserID:    userID,
		CreatedAt: time.Now().UTC(),
		streams:   make(map[chan []byte]struct{}),
	}
	h.mu.Lock()
	h.sessions[s.ID] = s
	h.mu.Unlock()
	return s
}

func (h *SessionHub) Get(id string) *Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessions[id]
}

func (h *SessionHub) Delete(id string) {
	h.mu.Lock()
	s := h.sessions[id]
	delete(h.sessions, id)
	h.mu.Unlock()
	if s != nil {
		s.closeAll()
	}
}

// NotifyToolsChanged sends notifications/tools/list_changed to all SSE
// streams belonging to sessions of userID (MCP Streamable HTTP).
func (h *SessionHub) NotifyToolsChanged(userID string) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/tools/list_changed",
	})
	h.mu.RLock()
	var targets []*Session
	for _, s := range h.sessions {
		if s.UserID == userID {
			targets = append(targets, s)
		}
	}
	h.mu.RUnlock()
	for _, s := range targets {
		n := s.broadcast(payload)
		if n > 0 {
			log.Printf("mcp: tools/list_changed user=%s session=%s streams=%d", userID, s.ID, n)
		}
	}
}

func (s *Session) addStream() chan []byte {
	ch := make(chan []byte, 8)
	s.mu.Lock()
	s.streams[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Session) removeStream(ch chan []byte) {
	s.mu.Lock()
	delete(s.streams, ch)
	s.mu.Unlock()
	// drain
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func (s *Session) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.streams {
		close(ch)
		delete(s.streams, ch)
	}
}

func (s *Session) broadcast(msg []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for ch := range s.streams {
		select {
		case ch <- msg:
			n++
		default:
			// slow consumer — drop
		}
	}
	return n
}

// writeSSE writes one SSE data event.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, id string, data []byte) error {
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
