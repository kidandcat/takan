package assistant

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxAppUploadBytes = 32 << 20
	sseHeartbeat      = 15 * time.Second
)

// AppRoutes wires the authenticated phone-app channel. Caddy reverse-proxies
// only /v1/* to the app host; everything else there 404s.
func (a *Assistant) AppRoutes(mux *http.ServeMux) {
	b := a.Bot
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		uptime := time.Since(b.startedAt)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"channel":        "app",
			"started_at":     b.startedAt.UTC().Format(time.RFC3339),
			"uptime_seconds": int64(uptime.Seconds()),
		})
	})

	mux.HandleFunc("GET /v1/messages", a.requireAppAuth(b.handleAppList))
	mux.HandleFunc("POST /v1/messages", a.requireAppAuth(b.handleAppPost))
	mux.HandleFunc("GET /v1/events", a.requireAppAuth(b.handleAppEvents))
	mux.HandleFunc("POST /v1/reset", a.requireAppAuth(b.handleAppReset))
	mux.HandleFunc("POST /v1/push/register", a.requireAppAuth(b.handleAppPushRegister))
}

// requireAppAuth rejects requests that do not carry the app bearer token.
func (a *Assistant) requireAppAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := a.AppToken
		if token == "" {
			writeError(w, http.StatusServiceUnavailable, "app channel is not configured")
			return
		}
		got, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// handleAppList pages the conversation. after=<id> catches up forwards from a
// known message, before=<id> scrolls back through older history; with neither,
// it returns the newest window.
// bearerToken extracts the credential from an Authorization header. The scheme
// is case-insensitive per RFC 7235, but it is required: accepting a bare token
// means any header value at all is treated as a credential.
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}

func (b *Bot) handleAppList(w http.ResponseWriter, r *http.Request) {
	after := r.URL.Query().Get("after")
	before := r.URL.Query().Get("before")
	if after != "" && before != "" {
		writeError(w, http.StatusBadRequest, "after and before are mutually exclusive")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	// An unknown cursor silently fell back to the newest or oldest window, so a
	// client resuming from a message that has aged out of the log would jump
	// somewhere else in the conversation and look like it had lost its place.
	for _, cursor := range []string{after, before} {
		if cursor == "" {
			continue
		}
		known, err := b.history.Has(cursor)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read the conversation")
			return
		}
		if !known {
			writeError(w, http.StatusNotFound, "unknown cursor")
			return
		}
	}

	messages := b.history.List(after, before, limit)
	out := map[string]any{"messages": publicMessages(messages)}
	// Tell the app whether scrolling further back is worth a round trip.
	if len(messages) > 0 {
		out["oldest"] = messages[0].ID
		out["newest"] = messages[len(messages)-1].ID
		out["has_more"] = len(b.history.List("", messages[0].ID, 1)) > 0
	}
	writeJSON(w, http.StatusOK, out)
}

func (b *Bot) handleAppPost(w http.ResponseWriter, r *http.Request) {
	in, err := parseAppInbound(r, b.inboxDir())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(in.Text) == "" && len(in.Files) == 0 && in.VoicePath == "" {
		writeError(w, http.StatusBadRequest, "text or an attachment is required")
		return
	}

	stored, err := b.history.Append(HistoryMessage{
		Role:   RoleUser,
		Text:   in.Text,
		Files:  attachmentsFromInbound(in),
		Source: SourceApp,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist message")
		return
	}
	in.ID = stored.ID
	b.events.Broadcast(AppEvent{Type: "message", Message: &stored})
	b.enqueueApp(r.Context(), in)

	writeJSON(w, http.StatusAccepted, publicMessage(stored))
}

func (b *Bot) handleAppReset(w http.ResponseWriter, r *http.Request) {
	chatID := b.ownerTelegram
	if err := b.state.ResetConversation(chatID); err != nil {
		log.Printf("app: failed to reset conversation: %v", err)
		writeError(w, http.StatusInternalServerError, "could not reset the conversation")
		return
	}
	note, err := b.history.Append(HistoryMessage{
		Role:   RoleSystem,
		Text:   "Starting a fresh conversation.",
		Source: SourceApp,
	})
	if err != nil {
		log.Printf("app: failed to persist reset: %v", err)
	} else {
		b.events.Broadcast(AppEvent{Type: "message", Message: &note})
	}
	log.Printf("conversation reset for chat %d (app)", chatID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": publicMessage(note)})
}

// handleAppPushRegister records the device's FCM token so the assistant can
// reach the phone when the app is closed and no SSE client is listening.
func (b *Bot) handleAppPushRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if err := b.state.RegisterPushToken(token, platform); err != nil {
		log.Printf("app: failed to register push token: %v", err)
		writeError(w, http.StatusInternalServerError, "could not register the device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "push_enabled": b.push != nil})
}

func (b *Bot) handleAppEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := b.events.Subscribe()
	defer b.events.Unsubscribe(ch)

	if busy, elapsed := b.ChatBusy(b.ownerTelegram); busy {
		writeSSE(w, flusher, typingEvent(time.Now().Add(-elapsed)))
		// The indicator alone says only "wait"; the steps say what for. Replay
		// them so an app that connects mid-run is level with one that was
		// listening from the start.
		for _, ev := range b.ProgressSnapshot(b.ownerTelegram) {
			writeSSE(w, flusher, AppEvent{Type: EventProgress, Progress: &ev})
		}
	}

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, flusher, ev)
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleAppNotify mirrors a message into the app channel only, without touching
// Telegram. It exists for a caller that has already delivered to the chat
// itself; ordinary proactive messages go to /internal/send instead.
func (b *Bot) handleAppNotify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" && req.File == "" {
		writeError(w, http.StatusBadRequest, "text or file is required")
		return
	}
	if _, err := b.Emit(r.Context(), Outbound{
		Text: text, File: req.File, Source: SourceSend, SkipTelegram: true,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist message")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev AppEvent) {
	payload, err := json.Marshal(publicEvent(ev))
	if err != nil {
		return
	}
	id := ""
	if ev.Message != nil {
		id = ev.Message.ID
	}
	var b strings.Builder
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev.Type, payload)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return
	}
	flusher.Flush()
}

func publicEvent(ev AppEvent) AppEvent {
	if ev.Message != nil {
		msg := publicMessage(*ev.Message)
		ev.Message = &msg
	}
	return ev
}

func publicMessages(in []HistoryMessage) []HistoryMessage {
	out := make([]HistoryMessage, len(in))
	for i, m := range in {
		out[i] = publicMessage(m)
	}
	return out
}

// publicMessage strips the server-side inbox path: the app gets names and
// kinds, never a filesystem location.
func publicMessage(m HistoryMessage) HistoryMessage {
	if len(m.Files) == 0 {
		return m
	}
	files := make([]Attachment, len(m.Files))
	for i, f := range m.Files {
		files[i] = Attachment{Name: f.Name, Kind: f.Kind}
	}
	m.Files = files
	return m
}

func parseAppInbound(r *http.Request, inboxDir string) (*appInbound, error) {
	ctype := r.Header.Get("Content-Type")
	media, _, _ := mime.ParseMediaType(ctype) // safe-ignore: a malformed Content-Type falls through to the prefix check below
	if media == "application/json" || (media == "" && !strings.HasPrefix(ctype, "multipart/")) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&req); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		return &appInbound{Text: strings.TrimSpace(req.Text)}, nil
	}

	if err := r.ParseMultipartForm(maxAppUploadBytes); err != nil {
		return nil, fmt.Errorf("invalid multipart body: %w", err)
	}

	in := &appInbound{Text: strings.TrimSpace(r.FormValue("text"))}
	if r.MultipartForm == nil {
		return in, nil
	}
	for field, files := range r.MultipartForm.File {
		kind := "document"
		switch strings.ToLower(field) {
		case "voice", "audio":
			kind = "voice"
		case "photo", "image", "images":
			kind = "photo"
		}
		for _, fh := range files {
			path, err := saveFormFile(fh, inboxDir, kind)
			if err != nil {
				return nil, err
			}
			if kind == "voice" && in.VoicePath == "" {
				in.VoicePath = path
				continue
			}
			in.Files = append(in.Files, path)
		}
	}
	return in, nil
}

func saveFormFile(fh *multipart.FileHeader, inboxDir, kind string) (string, error) {
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()

	name := fh.Filename
	if name == "" {
		name = kind + extForMime(fh.Header.Get("Content-Type"), "")
		if !strings.Contains(name, ".") {
			name = kind + ".bin"
		}
	}
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return "", err
	}
	dst := inboxPath(inboxDir, name)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(src, maxAppUploadBytes)); err != nil {
		return "", err
	}
	return dst, nil
}

func attachmentsFromInbound(in *appInbound) []Attachment {
	var out []Attachment
	if in.VoicePath != "" {
		out = append(out, Attachment{Name: filepath.Base(in.VoicePath), Kind: "audio", Path: in.VoicePath})
	}
	for _, path := range in.Files {
		out = append(out, Attachment{Name: filepath.Base(path), Kind: kindForName(path), Path: path})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent, so a write failure here can only be a
	// dead client; there is nothing left to report to.
	_ = json.NewEncoder(w).Encode(payload) // safe-ignore: response already committed
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
