package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

// appMux builds the public app routes for one assistant.
func appMux(a *Assistant) *http.ServeMux {
	mux := http.NewServeMux()
	a.AppRoutes(mux)
	return mux
}

func TestAppAuthAndHealth(t *testing.T) {
	mux := appMux(newTestAssistant(t))

	health := httptest.NewRecorder()
	mux.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health should be public, got %d", health.Code)
	}
	if !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected health body: %s", health.Body.String())
	}

	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", denied.Code)
	}

	wrong := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	mux.ServeHTTP(wrong, req)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with a bad token, got %d", wrong.Code)
	}
}

func TestAppChannelIsOffWithoutAToken(t *testing.T) {
	a := newTestAssistant(t)
	a.AppToken = ""
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	appMux(a).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unconfigured app channel must fail closed, got %d", rec.Code)
	}
}

func TestAppPostListReset(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot
	started := make(chan string, 1)
	finished := make(chan struct{})
	b.startHook = stubStart(func(spec RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		started <- spec.Prompt
		defer close(finished)
		return &AgentResult{Stdout: "ack", Duration: time.Millisecond}, nil
	})
	mux := appMux(a)

	body, _ := json.Marshal(map[string]string{"text": "hello from the phone"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post: got %d %s", rec.Code, rec.Body.String())
	}

	select {
	case prompt := <-started:
		if prompt != "hello from the phone" {
			t.Fatalf("agent prompt = %q", prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent was not started")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not finish")
	}
	waitHistoryRole(t, b, RoleAssistant)

	list := httptest.NewRecorder()
	lreq := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	lreq.Header.Set("Authorization", "Bearer test-token")
	mux.ServeHTTP(list, lreq)
	if list.Code != http.StatusOK {
		t.Fatalf("list: got %d %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "hello from the phone") {
		t.Fatalf("history missing the user message: %s", list.Body.String())
	}

	reset := httptest.NewRecorder()
	rreq := httptest.NewRequest(http.MethodPost, "/v1/reset", nil)
	rreq.Header.Set("Authorization", "Bearer test-token")
	mux.ServeHTTP(reset, rreq)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset: got %d %s", reset.Code, reset.Body.String())
	}
	if b.state.Chat(ownerChat).ConversationStarted {
		t.Fatal("reset should clear the conversation flag")
	}
}

// TestAppListPagesBackwards covers the app scrolling into old history: without
// `before` it could only ever see the newest window.
func TestAppListPagesBackwards(t *testing.T) {
	a := newTestAssistant(t)
	mux := appMux(a)

	var ids []string
	for i := 0; i < 6; i++ {
		stored, err := a.Bot.history.Append(HistoryMessage{
			Role: RoleUser, Text: string(rune('a' + i)), Source: SourceApp,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, stored.ID)
	}

	get := func(query string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/messages"+query, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s: %d %s", query, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	newest := get("?limit=2")
	if newest["oldest"] != ids[4] || newest["newest"] != ids[5] {
		t.Fatalf("the newest window should be the last two, got %v", newest)
	}
	if newest["has_more"] != true {
		t.Fatalf("older messages exist, so has_more must be true: %v", newest)
	}

	older := get("?before=" + ids[4] + "&limit=2")
	msgs, _ := older["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 older messages, got %v", older)
	}
	if older["oldest"] != ids[2] || older["newest"] != ids[3] {
		t.Fatalf("before must walk backwards, got %v", older)
	}

	first := get("?before=" + ids[0])
	if list, _ := first["messages"].([]any); len(list) != 0 {
		t.Fatalf("nothing precedes the oldest message, got %v", first)
	}

	both := httptest.NewRecorder()
	bothReq := httptest.NewRequest(http.MethodGet, "/v1/messages?after=x&before=y", nil)
	bothReq.Header.Set("Authorization", "Bearer test-token")
	mux.ServeHTTP(both, bothReq)
	if both.Code != http.StatusBadRequest {
		t.Fatalf("after and before are mutually exclusive, got %d", both.Code)
	}
}

func TestAppMultipartSavesInboxFile(t *testing.T) {
	a := newTestAssistant(t)
	b := a.Bot
	started := make(chan string, 1)
	finished := make(chan struct{})
	b.startHook = stubStart(func(spec RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		started <- spec.Prompt
		defer close(finished)
		return &AgentResult{Stdout: "got it", Duration: time.Millisecond}, nil
	})
	mux := appMux(a)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", "see this photo")
	part, err := mw.CreateFormFile("photo", "chart.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("fake-png")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &buf)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post: got %d %s", rec.Code, rec.Body.String())
	}

	var prompt string
	select {
	case prompt = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent was not started")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not finish")
	}
	if !strings.Contains(prompt, "see this photo") {
		t.Fatalf("prompt missing caption: %s", prompt)
	}
	if !strings.Contains(prompt, b.inboxDir()) {
		t.Fatalf("prompt should include the inbox path:\n%s", prompt)
	}

	entries, err := os.ReadDir(b.inboxDir())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "chart.png") {
			found = true
			data, _ := os.ReadFile(filepath.Join(b.inboxDir(), e.Name()))
			if string(data) != "fake-png" {
				t.Fatalf("saved bytes = %q", data)
			}
		}
	}
	if !found {
		t.Fatal("photo was not saved to the inbox")
	}
}

// TestPublicMessageHidesTheInboxPath: the app gets names and kinds, never a
// server-side filesystem location.
func TestPublicMessageHidesTheInboxPath(t *testing.T) {
	m := HistoryMessage{
		ID: "1", Role: RoleUser, Text: "x",
		Files: []Attachment{{Name: "chart.png", Kind: "image", Path: "/opt/atlas/data/inbox/1/chart.png"}},
	}
	pub := publicMessage(m)
	if pub.Files[0].Path != "" {
		t.Fatalf("the inbox path must not be exposed: %+v", pub.Files[0])
	}
	if pub.Files[0].Name != "chart.png" || pub.Files[0].Kind != "image" {
		t.Fatalf("name and kind must survive: %+v", pub.Files[0])
	}
	raw, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/opt/atlas") {
		t.Fatalf("serialised message leaked a path: %s", raw)
	}
}

func TestAppSSETypingAndDone(t *testing.T) {
	a := newTestAssistant(t)
	release := make(chan struct{})
	a.Bot.startHook = stubStart(func(_ RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		<-release
		return &AgentResult{Stdout: "the map is drawn", Duration: 10 * time.Millisecond}, nil
	})

	server := httptest.NewServer(appMux(a))
	defer server.Close()

	sseReq, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/events", nil)
	sseReq.Header.Set("Authorization", "Bearer test-token")
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("sse status %d", sseResp.StatusCode)
	}

	postBody, _ := json.Marshal(map[string]string{"text": "draw the coast"})
	preq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", bytes.NewReader(postBody))
	preq.Header.Set("Authorization", "Bearer test-token")
	preq.Header.Set("Content-Type", "application/json")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusAccepted {
		t.Fatalf("post status %d", presp.StatusCode)
	}

	reader := bufio.NewReader(sseResp.Body)
	gotTyping, gotDone, released := false, false, false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(gotTyping && gotDone) {
		line, err := readSSEEvent(reader, time.Until(deadline))
		if err != nil {
			break
		}
		if strings.Contains(line, "event: typing") {
			gotTyping = true
			if !released {
				close(release)
				released = true
			}
		}
		if strings.Contains(line, "event: done") && strings.Contains(line, "the map is drawn") {
			gotDone = true
		}
	}
	if !gotTyping || !gotDone {
		t.Fatalf("sse events incomplete: typing=%t done=%t", gotTyping, gotDone)
	}
}

func readSSEEvent(r *bufio.Reader, d time.Duration) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{b.String(), err}
				return
			}
			b.WriteString(line)
			if line == "\n" {
				ch <- result{b.String(), nil}
				return
			}
		}
	}()
	select {
	case got := <-ch:
		return got.s, got.err
	case <-time.After(d):
		return "", context.DeadlineExceeded
	}
}

func TestAppAndTelegramShareTheRunner(t *testing.T) {
	b := newTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.SetRunContext(ctx)

	// Hold the runner busy so the backlog is still there when we inspect it.
	release := make(chan struct{})
	b.startHook = stubStart(func(_ RunSpec, _ <-chan struct{}) (*AgentResult, error) {
		<-release
		return &AgentResult{Stdout: "ok", Duration: time.Millisecond}, nil
	})

	b.enqueue(ctx, &tg.Message{
		MessageID: 1, Chat: tg.Chat{ID: ownerChat, Type: "private"},
		From: &tg.User{ID: ownerChat}, Text: "from telegram",
	})
	b.enqueueApp(ctx, &appInbound{Text: "from the app"})

	b.runnersMu.Lock()
	r := b.runners[ownerChat]
	b.runnersMu.Unlock()
	if r == nil {
		t.Fatal("expected a shared runner for the owner chat")
	}
	r.mu.Lock()
	n := len(r.pending)
	r.mu.Unlock()
	if n != 2 {
		t.Fatalf("expected both channels on the same backlog, got %d pending", n)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		replied := false
		for _, m := range b.history.List("", "", 100) {
			if m.Role == RoleAssistant {
				replied = true
			}
		}
		if busy, _ := r.busy(); replied && !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the turn never completed")
}

func waitHistoryRole(t *testing.T, b *Bot, role string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range b.history.List("", "", 50) {
			if m.Role == role {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a %s message in history", role)
}

func TestPushRegisterStoresAndDedupes(t *testing.T) {
	a := newTestAssistant(t)
	mux := appMux(a)

	register := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/push/register", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	unauth := httptest.NewRecorder()
	mux.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/v1/push/register", strings.NewReader(`{"token":"x"}`)))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("push register must require the app token, got %d", unauth.Code)
	}
	if rec := register(`{"token":"","platform":"android"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an empty token should be rejected, got %d", rec.Code)
	}

	rec := register(`{"token":"device-a","platform":"android"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register failed: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK          bool `json:"ok"`
		PushEnabled bool `json:"push_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if out.PushEnabled {
		t.Fatal("push should report disabled without a service account")
	}

	// Re-registering the same device must refresh it, not duplicate it.
	register(`{"token":"device-a","platform":"ios"}`)
	register(`{"token":"device-b","platform":"ios"}`)
	if got := len(a.Bot.state.PushTokens()); got != 2 {
		t.Fatalf("expected 2 device tokens, got %d", got)
	}
	if err := a.Bot.state.DeletePushToken("device-a"); err != nil {
		t.Fatal(err)
	}
	if got := a.Bot.state.PushTokens(); len(got) != 1 || got[0] != "device-b" {
		t.Fatalf("expected only device-b to remain, got %v", got)
	}
}

func TestEventBusSubscribersGatePush(t *testing.T) {
	h := NewEventBus()
	if h.Subscribers() != 0 {
		t.Fatalf("a fresh bus has no subscribers, got %d", h.Subscribers())
	}
	ch := h.Subscribe()
	if h.Subscribers() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", h.Subscribers())
	}
	h.Unsubscribe(ch)
	if h.Subscribers() != 0 {
		t.Fatalf("expected 0 subscribers after unsubscribe, got %d", h.Subscribers())
	}

	// pushOutbound must stay a no-op when push is unconfigured, whether or not
	// anyone is listening, and must not panic on a nil message.
	b := newTestBot(t)
	b.pushOutbound(nil)
	b.pushOutbound(&HistoryMessage{ID: "1", Role: RoleAssistant, Text: "hi"})
}

func TestPushTokensSurviveAReload(t *testing.T) {
	a := newTestAssistant(t)
	if err := a.Bot.state.RegisterPushToken("device-a", "android"); err != nil {
		t.Fatal(err)
	}
	reloaded := NewStateStore(context.Background(), a.Store, a.OwnerID)
	if got := reloaded.PushTokens(); len(got) != 1 || got[0] != "device-a" {
		t.Fatalf("push tokens did not survive a reload: %v", got)
	}
}
