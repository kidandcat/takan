package bots

import (
	"context"
	"sync"
	"time"
)

// Watch topics: a daemon long-polls the whitelist and the outbox separately,
// so a chat decision does not wake a delivery poll (and vice versa).
const (
	TopicChats      = "chats"
	TopicDeliveries = "deliveries"
)

// Watcher wakes long-polling daemons when something they follow changes.
// In-process only: a bot that misses a wake-up still converges on its next
// poll (chats via the updated_since cursor, deliveries because they stay
// queued until acked), so no durability is needed here.
type Watcher struct {
	mu       sync.Mutex
	channels map[string][]chan struct{} // botID+topic -> waiters
}

func NewWatcher() *Watcher {
	return &Watcher{channels: make(map[string][]chan struct{})}
}

// Wait blocks until the topic changes for that bot, the timeout elapses, or
// ctx ends. Reports whether a change was signalled.
func (w *Watcher) Wait(ctx context.Context, botID, topic string, timeout time.Duration) bool {
	if w == nil || botID == "" || timeout <= 0 {
		return false
	}
	key := watchKey(botID, topic)
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.channels[key] = append(w.channels[key], ch)
	w.mu.Unlock()
	defer w.remove(key, ch)

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// NotifyChats wakes whitelist waiters (chat reported, approved, denied, forgotten).
func (w *Watcher) NotifyChats(botID string) { w.notify(botID, TopicChats) }

// NotifyDeliveries wakes outbox waiters (a delivery was queued for the bot).
func (w *Watcher) NotifyDeliveries(botID string) { w.notify(botID, TopicDeliveries) }

func (w *Watcher) notify(botID, topic string) {
	if w == nil || botID == "" {
		return
	}
	key := watchKey(botID, topic)
	w.mu.Lock()
	list := append([]chan struct{}(nil), w.channels[key]...)
	w.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func watchKey(botID, topic string) string { return botID + "\x00" + topic }

func (w *Watcher) remove(key string, ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.channels[key]
	out := list[:0]
	for _, c := range list {
		if c != ch {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(w.channels, key)
	} else {
		w.channels[key] = out
	}
}
