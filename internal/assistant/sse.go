package assistant

import (
	"sync"
	"time"
)

// AppEvent is one SSE payload on GET /v1/events.
//
// Types: typing, token, done, error, message, queued.
type AppEvent struct {
	Type    string          `json:"type"`
	Message *HistoryMessage `json:"message,omitempty"`
	Token   string          `json:"token,omitempty"`
	Error   string          `json:"error,omitempty"`
	Since   string          `json:"since,omitempty"`
	Queued  int             `json:"queued,omitempty"`
}

// EventBus fans app-channel events out to connected SSE clients. It is named
// EventBus rather than Hub so it does not collide with agenthub.Hub.
type EventBus struct {
	mu   sync.Mutex
	subs map[chan AppEvent]struct{}
}

// NewEventBus builds an empty subscriber set.
func NewEventBus() *EventBus {
	return &EventBus{subs: map[chan AppEvent]struct{}{}}
}

const eventBuffer = 16

// Subscribe registers a client. The caller must Unsubscribe.
func (h *EventBus) Subscribe() chan AppEvent {
	ch := make(chan AppEvent, eventBuffer)
	if h == nil {
		return ch
	}
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe drops a client and closes its channel.
func (h *EventBus) Unsubscribe(ch chan AppEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// Subscribers reports how many SSE clients are connected. A live subscriber
// means the app is in the foreground and already showing the conversation, so
// push notifications can be skipped.
func (h *EventBus) Subscribers() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Broadcast delivers an event to every subscriber. Slow clients are skipped
// rather than blocking the agent run.
func (h *EventBus) Broadcast(ev AppEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Drop rather than stall. The client catches up via GET /v1/messages.
		}
	}
}

// typingEvent is the SSE payload sent while the agent is running.
func typingEvent(since time.Time) AppEvent {
	ev := AppEvent{Type: "typing"}
	if !since.IsZero() {
		ev.Since = since.UTC().Format(time.RFC3339)
	}
	return ev
}
