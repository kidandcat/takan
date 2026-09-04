// Package bots manages Telegram assistant bot instances (e.g. Atlas) that run as
// daemons on machines. Takan owns the chat whitelist and the approval flow; the
// Telegram bot token never leaves the bot's machine.
package bots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// Notifier sends a plain-text message to the operator (Telegram module).
// Errors are logged by the caller and never block the daemon request.
type Notifier func(ctx context.Context, userID, text string) error

// Server exposes the bot daemon API under /api/bots/*.
// Auth is the per-bot token issued in the panel (Authorization: Bearer …).
type Server struct {
	Store *store.Store
	// Notify optional: called once when a chat first shows up as pending.
	Notify Notifier
	// Watch optional: long-poll support for GET /api/bots/chats?wait=…
	Watch *Watcher
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/bots/register", s.register)
	mux.HandleFunc("POST /api/bots/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /api/bots/chats", s.listChats)
	mux.HandleFunc("POST /api/bots/chats/pending", s.reportPending)
	mux.HandleFunc("GET /api/bots/deliveries", s.listDeliveries)
	mux.HandleFunc("POST /api/bots/deliveries/ack", s.ackDeliveries)
}

const (
	maxWaitSeconds     = 60
	defaultWaitSeconds = 0
)

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]any{"error": msg})
}

// authBot resolves the bot token and records the call as a heartbeat.
func (s *Server) authBot(w http.ResponseWriter, r *http.Request) (*store.Bot, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	raw := ""
	if strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if raw == "" {
		s.writeErr(w, http.StatusUnauthorized, "missing bearer bot token")
		return nil, false
	}
	b, err := s.Store.BotByToken(r.Context(), raw)
	if err != nil || b == nil {
		s.writeErr(w, http.StatusUnauthorized, "invalid bot token")
		return nil, false
	}
	enabled, err := s.Store.ModuleEnabled(r.Context(), b.UserID, "bots")
	if err == nil && !enabled {
		s.writeErr(w, http.StatusForbidden, "bots module is disabled in the Takan panel")
		return nil, false
	}
	_ = s.Store.TouchBot(r.Context(), b.ID)
	return b, true
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

type botView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BotUsername string `json:"bot_username,omitempty"`
	Machine     string `json:"machine,omitempty"`
	Version     string `json:"version,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"`
}

func viewBot(b *store.Bot) botView {
	v := botView{
		ID: b.ID, Name: b.Name, BotUsername: b.BotUsername,
		Machine: b.MachineName, Version: b.Version,
	}
	if b.LastSeen != nil {
		v.LastSeen = b.LastSeen.UTC().Format(time.RFC3339)
	}
	return v
}

type chatView struct {
	ChatID    string `json:"chat_id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	Status    string `json:"status"`
	Snippet   string `json:"first_message,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
	UpdatedAt string `json:"updated_at"`
	CreatedAt string `json:"created_at"`
}

func viewChat(c store.BotChat) chatView {
	return chatView{
		ChatID: c.ChatID, Type: c.Type, Title: c.Title, Username: c.Username,
		Status: c.Status, Snippet: c.FirstMessage, DecidedBy: c.DecidedBy,
		UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339),
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func viewChats(list []store.BotChat) []chatView {
	out := make([]chatView, 0, len(list))
	for _, c := range list {
		out = append(out, viewChat(c))
	}
	return out
}

// register is the idempotent announce call. The bot is created in the panel
// (that is where the token comes from); this only refreshes what the daemon
// knows about itself and returns the current whitelist.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	b, ok := s.authBot(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		BotUsername string `json:"bot_username"`
		Machine     string `json:"machine"`
		Version     string `json:"version"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := s.Store.UpdateBotIdentity(r.Context(), b.ID, in.BotUsername, in.Machine, in.Version); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	fresh, err := s.Store.BotByID(r.Context(), b.UserID, b.ID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	chats, err := s.Store.ListBotChats(r.Context(), b.ID, "", "")
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cursor, _ := s.Store.BotChatsUpdatedAt(r.Context(), b.ID)
	// The daemon keeps its configured name only as a label; Takan's name wins.
	if n := strings.TrimSpace(in.Name); n != "" && !strings.EqualFold(n, fresh.Name) {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"bot":    viewBot(fresh),
			"chats":  viewChats(chats),
			"cursor": cursor,
			"note":   fmt.Sprintf("this token belongs to bot %q; rename it in the Takan panel if %q is wrong", fresh.Name, n),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"bot":    viewBot(fresh),
		"chats":  viewChats(chats),
		"cursor": cursor,
	})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	b, ok := s.authBot(w, r)
	if !ok {
		return
	}
	cursor, _ := s.Store.BotChatsUpdatedAt(r.Context(), b.ID)
	pending, _ := s.Store.CountBotChats(r.Context(), b.ID, store.BotChatPending)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"bot":     b.Name,
		"cursor":  cursor,
		"pending": pending,
		"now":     time.Now().UTC().Format(time.RFC3339),
	})
}

// listChats returns the whitelist. Optional query params:
//
//	status=pending|approved|denied  filter
//	updated_since=RFC3339           only rows changed after that instant
//	wait=<seconds>                  long-poll: block until something changes
func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	b, ok := s.authBot(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	switch status {
	case "", store.BotChatPending, store.BotChatApproved, store.BotChatDenied:
	default:
		s.writeErr(w, http.StatusBadRequest, "status must be pending, approved or denied")
		return
	}
	since := strings.TrimSpace(q.Get("updated_since"))
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "updated_since must be RFC3339")
			return
		}
		since = t.UTC().Format(time.RFC3339)
	}
	wait, ok := s.waitParam(w, q.Get("wait"))
	if !ok {
		return
	}

	chats, err := s.Store.ListBotChats(r.Context(), b.ID, status, since)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(chats) == 0 && wait > 0 && s.Watch != nil {
		if s.Watch.Wait(r.Context(), b.ID, TopicChats, time.Duration(wait)*time.Second) {
			chats, err = s.Store.ListBotChats(r.Context(), b.ID, status, since)
			if err != nil {
				s.writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	cursor, _ := s.Store.BotChatsUpdatedAt(r.Context(), b.ID)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"bot":    b.Name,
		"chats":  viewChats(chats),
		"cursor": cursor,
		"hint":   "only status=approved chats may be served; re-poll with updated_since=cursor",
	})
}

// reportPending records an unknown chat that wrote to the bot. Idempotent per
// chat: repeating it never resets an approved/denied decision, and the operator
// is notified only the first time.
func (s *Server) reportPending(w http.ResponseWriter, r *http.Request) {
	b, ok := s.authBot(w, r)
	if !ok {
		return
	}
	var in struct {
		ChatID    string `json:"chat_id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Snippet   string `json:"first_message"`
		// Snippet2 accepts "snippet" as an alias so the daemon can use either name.
		Snippet2 string `json:"snippet"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(in.ChatID) == "" {
		s.writeErr(w, http.StatusBadRequest, "chat_id required")
		return
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = strings.TrimSpace(in.FirstName + " " + in.LastName)
	}
	snippet := strings.TrimSpace(in.Snippet)
	if snippet == "" {
		snippet = strings.TrimSpace(in.Snippet2)
	}
	chat, created, err := s.Store.ReportBotChat(r.Context(), b.ID, b.UserID, store.BotChat{
		ChatID: in.ChatID, Type: in.Type, Title: title,
		Username: in.Username, FirstMessage: snippet,
	})
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if created {
		s.notifyPending(r.Context(), b, *chat)
		s.Watch.NotifyChats(b.ID)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, map[string]any{
		"chat":    viewChat(*chat),
		"created": created,
		"hint":    "serve this chat only once status is approved",
	})
}

// deliveryView is one queued hub -> bot message. payload is the type-specific
// object (for ai_job_result: AIJobResult).
type deliveryView struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Attempts  int             `json:"attempts"`
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// listDeliveries hands out the bot's queued deliveries (oldest first) and marks
// them in flight. They stay queued until acked, so a daemon that crashes before
// forwarding gets them again after DeliveryRetryAfter. Query params:
//
//	limit=<n>       max deliveries in this response (default 20, max 50)
//	wait=<seconds>  long-poll: block until something is queued
func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	b, ok := s.authBot(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.writeErr(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = n
	}
	wait, ok := s.waitParam(w, q.Get("wait"))
	if !ok {
		return
	}

	list, err := s.Store.ListBotDeliveries(r.Context(), b.ID, limit)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(list) == 0 && wait > 0 && s.Watch != nil {
		if s.Watch.Wait(r.Context(), b.ID, TopicDeliveries, time.Duration(wait)*time.Second) {
			list, err = s.Store.ListBotDeliveries(r.Context(), b.ID, limit)
			if err != nil {
				s.writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	out := make([]deliveryView, 0, len(list))
	for _, d := range list {
		out = append(out, deliveryView{
			ID: d.ID, Type: d.Type, Attempts: d.Attempts,
			CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
			Payload:   json.RawMessage(d.Payload),
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"bot":        b.Name,
		"deliveries": out,
		"hint":       "ack every delivery you handled with POST /api/bots/deliveries/ack; unacked ones come back",
	})
}

// ackDeliveries removes deliveries from the outbox. Idempotent; unknown ids are
// ignored so a retried ack is harmless.
func (s *Server) ackDeliveries(w http.ResponseWriter, r *http.Request) {
	b, ok := s.authBot(w, r)
	if !ok {
		return
	}
	var in struct {
		IDs []string `json:"ids"`
		ID  string   `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids := in.IDs
	if id := strings.TrimSpace(in.ID); id != "" {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		s.writeErr(w, http.StatusBadRequest, "ids required")
		return
	}
	n, err := s.Store.AckBotDeliveries(r.Context(), b.ID, ids)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pending, _ := s.Store.CountBotDeliveries(r.Context(), b.ID)
	s.writeJSON(w, http.StatusOK, map[string]any{"acked": n, "pending": pending})
}

// waitParam parses the shared long-poll ?wait= parameter (seconds, capped).
func (s *Server) waitParam(w http.ResponseWriter, raw string) (int, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultWaitSeconds, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		s.writeErr(w, http.StatusBadRequest, "wait must be a positive number of seconds")
		return 0, false
	}
	return min(n, maxWaitSeconds), true
}

// notifyPending tells the operator about a new pending chat via the telegram module.
func (s *Server) notifyPending(ctx context.Context, b *store.Bot, c store.BotChat) {
	if s.Notify == nil {
		return
	}
	kind := "private chat"
	if c.Type == store.BotChatGroup {
		kind = "group"
	}
	text := fmt.Sprintf("Takan · bot %s\nNew %s waiting for approval: %s (chat %s)",
		b.Name, kind, c.Label(), c.ChatID)
	if c.FirstMessage != "" {
		text += "\n\n" + truncate(c.FirstMessage, 300)
	}
	text += "\n\nApprove or deny: takan.es/dashboard/bots"
	_ = s.Notify(ctx, b.UserID, text)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// NotFound reports whether err means "no such row" (shared by tools/panel).
func NotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
