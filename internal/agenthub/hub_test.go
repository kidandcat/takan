package agenthub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHubAIFollowProtocol(t *testing.T) {
	h := New(func(ctx context.Context, token string) (string, string, string, error) {
		return "mid-1", "user-1", "mac", nil
	}, nil)

	var events []AIJob
	var evMu sync.Mutex
	h.OnJobEvent = func(userID, machineName string, job AIJob) {
		evMu.Lock()
		events = append(events, job)
		evMu.Unlock()
		if userID != "user-1" || machineName != "mac" {
			t.Errorf("event ids user=%s machine=%s", userID, machineName)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	t.Cleanup(srv.Close)

	c := dialAgent(t, srv.URL)
	defer c.Close()

	var mu sync.Mutex
	var writeMu sync.Mutex
	writeJSON := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = c.WriteJSON(v)
	}
	jobs := map[string]AIJob{}

	go func() {
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req wireMsg
			if json.Unmarshal(raw, &req) != nil {
				continue
			}
			switch req.Type {
			case "ai_start":
				job := AIJob{
					JobID:       "job-follow-1",
					Agent:       req.Runner,
					Runner:      req.Runner,
					Status:      "running",
					PID:         4242,
					Prompt:      req.Prompt,
					Cwd:         req.Cwd,
					ParentJobID: req.ParentJobID,
					StartedAt:   "2026-08-21T00:00:00Z",
				}
				mu.Lock()
				jobs[job.JobID] = job
				mu.Unlock()
				writeJSON(wireMsg{
					Type: "ai_start_result", TaskID: req.TaskID,
					JobID: job.JobID, Agent: job.Agent, Runner: job.Runner,
					Status: job.Status, PID: job.PID, ParentJobID: job.ParentJobID,
				})
			case "ai_status":
				if req.JobID == "" {
					mu.Lock()
					var list []AIJob
					for _, j := range jobs {
						list = append(list, j)
					}
					mu.Unlock()
					writeJSON(wireMsg{Type: "ai_status_result", TaskID: req.TaskID, Jobs: list, Status: "ok"})
					continue
				}
				mu.Lock()
				j, ok := jobs[req.JobID]
				mu.Unlock()
				if !ok {
					writeJSON(wireMsg{Type: "ai_status_result", TaskID: req.TaskID, JobID: req.JobID, Status: "unknown", Error: "unknown job"})
					continue
				}
				j.Output = "tail-log"
				writeJSON(jobResult("ai_status_result", req.TaskID, j))
			case "ai_cancel":
				mu.Lock()
				j, ok := jobs[req.JobID]
				if ok && !JobTerminal(j.Status) {
					j.Status = "cancelled"
					j.FinishedAt = "2026-08-21T00:00:01Z"
					jobs[req.JobID] = j
				}
				mu.Unlock()
				if !ok {
					writeJSON(wireMsg{Type: "ai_cancel_result", TaskID: req.TaskID, JobID: req.JobID, Status: "unknown", Error: "unknown job"})
					continue
				}
				writeJSON(jobResult("ai_cancel_result", req.TaskID, j))
			case "ai_log":
				mu.Lock()
				j, ok := jobs[req.JobID]
				mu.Unlock()
				if !ok {
					writeJSON(wireMsg{Type: "ai_log_result", TaskID: req.TaskID, JobID: req.JobID, Status: "unknown", Error: "unknown job"})
					continue
				}
				j.Output = "full-transcript-line-1\nfull-transcript-line-2\n"
				writeJSON(wireMsg{
					Type: "ai_log_result", TaskID: req.TaskID,
					JobID: j.JobID, Agent: j.Agent, Runner: j.Runner, Status: j.Status,
					ParentJobID: j.ParentJobID, Output: j.Output, TotalBytes: len(j.Output),
				})
			}
		}
	}()

	waitOnline(t, h, "mid-1")
	ctx := context.Background()

	started, err := h.StartAI(ctx, "user-1", "mac", "grok", "grok --always-approve -p {{prompt}}", "do work", "/tmp", "parent-9")
	if err != nil {
		t.Fatal(err)
	}
	if started.JobID != "job-follow-1" || started.ParentJobID != "parent-9" || started.Status != "running" {
		t.Fatalf("start: %+v", started)
	}

	job, list, err := h.AIStatus(ctx, "user-1", "mac", started.JobID, 1000)
	if err != nil || job.Output != "tail-log" || job.ParentJobID != "parent-9" {
		t.Fatalf("status: %+v list=%v err=%v", job, list, err)
	}

	lg, err := h.AILog(ctx, "user-1", "mac", started.JobID, 2000)
	if err != nil || !strings.Contains(lg.Output, "full-transcript") || lg.TotalBytes == 0 {
		t.Fatalf("log: %+v err=%v", lg, err)
	}

	watchDone := make(chan struct {
		job      *AIJob
		timedOut bool
		err      error
	}, 1)
	go func() {
		j, to, err := h.WatchAI(ctx, "user-1", "mac", started.JobID, 3*time.Second, 1000)
		watchDone <- struct {
			job      *AIJob
			timedOut bool
			err      error
		}{j, to, err}
	}()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	j := jobs[started.JobID]
	j.Status = "done"
	j.FinishedAt = "2026-08-21T00:00:02Z"
	jobs[started.JobID] = j
	mu.Unlock()
	writeJSON(wireMsg{
		Type: "ai_done", JobID: started.JobID, Agent: "grok", Runner: "grok",
		Status: "done", ParentJobID: "parent-9", FinishedAt: "2026-08-21T00:00:02Z",
	})

	select {
	case got := <-watchDone:
		if got.err != nil || got.timedOut || got.job == nil || got.job.Status != "done" {
			t.Fatalf("watch: job=%+v timeout=%v err=%v", got.job, got.timedOut, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not return after ai_done")
	}

	cancelled, err := h.CancelAI(ctx, "user-1", "mac", "missing")
	if err == nil {
		t.Fatalf("expected unknown cancel, got %+v", cancelled)
	}

	evMu.Lock()
	n := len(events)
	evMu.Unlock()
	if n == 0 {
		t.Fatal("expected OnJobEvent from ai_done")
	}
}

func TestHubWatchTimeoutStillRunning(t *testing.T) {
	h := New(func(ctx context.Context, token string) (string, string, string, error) {
		return "mid-2", "user-2", "box", nil
	}, nil)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	t.Cleanup(srv.Close)
	c := dialAgent(t, srv.URL)
	defer c.Close()

	go func() {
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req wireMsg
			if json.Unmarshal(raw, &req) != nil {
				continue
			}
			if req.Type == "ai_status" {
				_ = c.WriteJSON(wireMsg{
					Type: "ai_status_result", TaskID: req.TaskID,
					JobID: req.JobID, Status: "running", Agent: "grok", Runner: "grok", PID: 7,
				})
			}
		}
	}()
	waitOnline(t, h, "mid-2")

	job, timedOut, err := h.WatchAI(context.Background(), "user-2", "box", "job-x", 200*time.Millisecond, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut || job.Status != "running" {
		t.Fatalf("want running+timeout, got %+v timeout=%v", job, timedOut)
	}
}

func jobResult(typ, taskID string, j AIJob) wireMsg {
	return wireMsg{
		Type: typ, TaskID: taskID,
		JobID: j.JobID, Agent: j.Agent, Runner: j.Runner, Status: j.Status,
		PID: j.PID, Prompt: j.Prompt, Cwd: j.Cwd, ParentJobID: j.ParentJobID,
		Output: j.Output, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		Error: j.Error, ExitCode: j.ExitCode,
	}
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

func waitOnline(t *testing.T, h *Hub, machineID string) {
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
