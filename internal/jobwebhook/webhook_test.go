package jobwebhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kidandcat/takan/internal/agenthub"
)

type countingTransport struct {
	mu sync.Mutex
	n  int
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.n++
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func TestNotifyNoURLNoRequest(t *testing.T) {
	rt := &countingTransport{}
	hook := Client{
		URL:  "",
		HTTP: &http.Client{Transport: rt, Timeout: time.Second},
	}
	seen := make(chan struct{}, 1)
	_, c := startHub(t, func(_ string, machine string, job agenthub.AIJob) {
		hook.Notify(PayloadFromJob(machine, job))
		seen <- struct{}{}
	})
	sendAIDone(t, c)
	waitSeen(t, seen)
	rt.mu.Lock()
	n := rt.n
	rt.mu.Unlock()
	if n != 0 {
		t.Fatalf("requests=%d, want 0 when webhook URL is empty", n)
	}
}

func TestNotifyURLSetPostsOnceOnAIDone(t *testing.T) {
	type rec struct {
		method string
		header http.Header
		body   []byte
	}
	var mu sync.Mutex
	var recs []rec
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		recs = append(recs, rec{r.Method, r.Header.Clone(), b})
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(hookSrv.Close)

	hook := Client{URL: hookSrv.URL, Secret: "sender-key-1"}
	seen := make(chan struct{}, 1)
	_, c := startHub(t, func(_ string, machine string, job agenthub.AIJob) {
		hook.Notify(PayloadFromJob(machine, job))
		seen <- struct{}{}
	})
	sendAIDone(t, c)
	waitSeen(t, seen)

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 1 {
		t.Fatalf("posts=%d, want 1", len(recs))
	}
	got := recs[0]
	if got.method != http.MethodPost {
		t.Fatalf("method=%s", got.method)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	if got.header.Get("Authorization") != "Bearer sender-key-1" {
		t.Fatalf("authorization=%q", got.header.Get("Authorization"))
	}
	if got.header.Get("X-Webhook-Key") != "sender-key-1" {
		t.Fatalf("x-webhook-key=%q", got.header.Get("X-Webhook-Key"))
	}
	if got.header.Get("X-Grok-Webhook-Secret") != "sender-key-1" {
		t.Fatalf("x-grok-webhook-secret=%q", got.header.Get("X-Grok-Webhook-Secret"))
	}
	var p Payload
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	want := Payload{
		Machine:     "mac",
		JobID:       "job-1",
		Status:      "done",
		ExitCode:    0,
		Runner:      "grok",
		ParentJobID: "parent-9",
		FinishedAt:  "2026-08-22T10:00:00Z",
		Owner:       "Minerva",
	}
	if p != want {
		t.Fatalf("body=%+v want=%+v", p, want)
	}
}

func startHub(t *testing.T, on agenthub.JobEventHandler) (*agenthub.Hub, *websocket.Conn) {
	t.Helper()
	h := agenthub.New(func(_ context.Context, _ string) (string, string, string, error) {
		return "mid", "user-1", "mac", nil
	}, nil)
	h.OnJobEvent = on
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	t.Cleanup(srv.Close)
	c := dialAgent(t, srv.URL)
	t.Cleanup(func() { _ = c.Close() })
	waitOnline(t, h, "mid")
	return h, c
}

func dialAgent(t *testing.T, httpURL string) *websocket.Conn {
	t.Helper()
	ws := "ws" + strings.TrimPrefix(httpURL, "http") + "?token=tok"
	c, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitOnline(t *testing.T, h *agenthub.Hub, machineID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Online(machineID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent never registered")
}

func waitSeen(t *testing.T, seen <-chan struct{}) {
	t.Helper()
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("OnJobEvent not fired")
	}
}

func sendAIDone(t *testing.T, c *websocket.Conn) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":          "ai_done",
		"job_id":        "job-1",
		"agent":         "grok",
		"runner":        "grok",
		"status":        "done",
		"exit_code":     0,
		"parent_job_id": "parent-9",
		"finished_at":   "2026-08-22T10:00:00Z",
		"owner":         "Minerva",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatal(err)
	}
}
