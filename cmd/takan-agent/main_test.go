package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestResolveHubURL(t *testing.T) {
	if _, err := resolveHubURL(""); err == nil {
		t.Fatal("empty url should fail")
	}
	if _, err := resolveHubURL("   "); err == nil {
		t.Fatal("whitespace url should fail")
	}
	got, err := resolveHubURL(" https://hub.example/ ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://hub.example" {
		t.Fatalf("got %q", got)
	}
}

func TestLivenessAction(t *testing.T) {
	stale := 90 * time.Second
	hang := 3 * time.Minute
	cases := []struct {
		idle        time.Duration
		close, exit bool
	}{
		{0, false, false},
		{30 * time.Second, false, false},
		{90 * time.Second, false, false},
		{90*time.Second + time.Millisecond, true, false},
		{2 * time.Minute, true, false},
		{3 * time.Minute, true, false},
		{3*time.Minute + time.Millisecond, true, true},
	}
	for _, tc := range cases {
		closeConn, exitProc := livenessAction(tc.idle, stale, hang)
		if closeConn != tc.close || exitProc != tc.exit {
			t.Fatalf("idle=%s close=%v exit=%v want close=%v exit=%v",
				tc.idle, closeConn, exitProc, tc.close, tc.exit)
		}
	}
}

func TestRunOnceWatchdogClosesSilentConn(t *testing.T) {
	tm := connTiming{
		readWait:      time.Hour,
		writeWait:     time.Second,
		pingEvery:     time.Hour,
		watchdogTick:  40 * time.Millisecond,
		staleAfter:    150 * time.Millisecond,
		hangExitAfter: time.Hour,
	}
	srv := silentWSServer(t)
	jobs := testJobs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := runOnce(ctx, srv.URL, "tok", jobs, tm, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected watchdog to close the socket")
	}
	if ctx.Err() != nil {
		t.Fatalf("context fired before watchdog: %v (runOnce=%v)", ctx.Err(), err)
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("watchdog timing %s err=%v", elapsed, err)
	}
}

func TestRunOnceWatchdogIgnoresOutboundPings(t *testing.T) {
	tm := connTiming{
		readWait:      time.Hour,
		writeWait:     time.Second,
		pingEvery:     30 * time.Millisecond,
		watchdogTick:  40 * time.Millisecond,
		staleAfter:    180 * time.Millisecond,
		hangExitAfter: time.Hour,
	}
	srv := silentWSServer(t)
	jobs := testJobs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := runOnce(ctx, srv.URL, "tok", jobs, tm, nil)
	if err == nil {
		t.Fatal("expected watchdog despite outbound pings")
	}
	if ctx.Err() != nil {
		t.Fatalf("context fired before watchdog: %v (runOnce=%v)", ctx.Err(), err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("watchdog too slow: %s err=%v", elapsed, err)
	}
}

func TestRunOnceStaysUpWhenHubPings(t *testing.T) {
	tm := connTiming{
		readWait:      2 * time.Second,
		writeWait:     time.Second,
		pingEvery:     time.Hour,
		watchdogTick:  40 * time.Millisecond,
		staleAfter:    200 * time.Millisecond,
		hangExitAfter: time.Hour,
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		go func() {
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}()
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for range tick.C {
			deadline := time.Now().Add(time.Second)
			if err := c.WriteControl(websocket.PingMessage, []byte("ping"), deadline); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	jobs := testJobs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	err := runOnce(ctx, srv.URL, "tok", jobs, tm, nil)
	if err == nil {
		t.Fatal("expected cancel")
	}
	if ctx.Err() == nil {
		t.Fatalf("watchdog closed a live connection: %v", err)
	}
}

func TestJobWireSetsType(t *testing.T) {
	got := jobWire("ai_done", jobMeta{JobID: "j1", Status: "done", Owner: "Minerva"}, "", 0, false)
	if got.Type != "ai_done" {
		t.Fatalf("Type=%q want ai_done", got.Type)
	}
	if got.JobID != "j1" || got.Status != "done" || got.Owner != "Minerva" {
		t.Fatalf("meta: %+v", got)
	}
	empty := jobWire("", jobMeta{JobID: "j2", Status: "running"}, "", 0, false)
	if empty.Type != "" || empty.JobID != "j2" {
		t.Fatalf("empty type: %+v", empty)
	}
}

func TestRunOnceEmitsAIDoneOnJobFinish(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	got := make(chan wireMsg, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		started := false
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			var msg wireMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			got <- msg
			if msg.Type == "hello" && !started {
				started = true
				_ = c.WriteJSON(wireMsg{
					Type:    "ai_start",
					TaskID:  "t-done",
					Runner:  "echo",
					Command: "echo finished # {{prompt}}",
					Prompt:  "p",
					Owner:   "Minerva",
				})
			}
		}
	}))
	t.Cleanup(srv.Close)

	jobs := testJobs(t)
	tm := connTiming{
		readWait:      time.Hour,
		writeWait:     time.Second,
		pingEvery:     time.Hour,
		watchdogTick:  time.Hour,
		staleAfter:    time.Hour,
		hangExitAfter: time.Hour,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = runOnce(ctx, srv.URL, "tok", jobs, tm, nil) }()

	deadline := time.After(4 * time.Second)
	var startID string
	for {
		select {
		case <-deadline:
			t.Fatal("finished job did not emit type ai_done")
		case msg := <-got:
			if msg.Type == "ai_start_result" {
				startID = msg.JobID
			}
			if msg.Type != "ai_done" {
				continue
			}
			if msg.JobID == "" {
				t.Fatal("ai_done missing job_id")
			}
			if startID != "" && msg.JobID != startID {
				t.Fatalf("ai_done job_id=%q start=%q", msg.JobID, startID)
			}
			if msg.Status != "done" {
				t.Fatalf("ai_done status=%q", msg.Status)
			}
			if msg.Owner != "Minerva" {
				t.Fatalf("ai_done owner=%q", msg.Owner)
			}
			cancel()
			return
		}
	}
}

func testJobs(t *testing.T) *jobManager {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	jm, err := newJobManager()
	if err != nil {
		t.Fatal(err)
	}
	return jm
}

// silentWSServer upgrades, drains client frames, and never sends. Handler returns
// when the peer closes so httptest.Server.Close does not hang.
func silentWSServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
