package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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
