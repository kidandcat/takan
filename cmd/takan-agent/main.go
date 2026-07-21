// takan-agent connects outbound to a Takan hub and runs bash / AI jobs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type wireMsg struct {
	Type        string    `json:"type"`
	TaskID      string    `json:"task_id,omitempty"`
	Command     string    `json:"command,omitempty"`
	ExitCode    int       `json:"exit_code,omitempty"`
	Stdout      string    `json:"stdout,omitempty"`
	Stderr      string    `json:"stderr,omitempty"`
	Error       string    `json:"error,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	Prompt      string    `json:"prompt,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	AutoApprove bool      `json:"auto_approve"`
	JobID       string    `json:"job_id,omitempty"`
	Status      string    `json:"status,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Output      string    `json:"output,omitempty"`
	StartedAt   string    `json:"started_at,omitempty"`
	FinishedAt  string    `json:"finished_at,omitempty"`
	TailBytes   int       `json:"tail_bytes,omitempty"`
	Jobs        []jobMeta `json:"jobs,omitempty"`
}

func main() {
	baseURL := flag.String("url", env("TAKAN_URL", "https://takan.es"), "Takan hub base URL")
	token := flag.String("token", env("TAKAN_AGENT_TOKEN", ""), "Agent token from panel")
	name := flag.String("name", env("TAKAN_AGENT_NAME", ""), "Machine name (informational)")
	flag.Parse()
	if *token == "" {
		log.Fatal("--token or TAKAN_AGENT_TOKEN required")
	}
	_ = name

	jobs, err := newJobManager()
	if err != nil {
		log.Fatalf("jobs dir: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for ctx.Err() == nil {
		err := runOnce(ctx, *baseURL, *token, jobs)
		if ctx.Err() != nil {
			return
		}
		log.Printf("disconnected: %v — reconnect in 5s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func runOnce(ctx context.Context, base, token string, jobs *jobManager) error {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/agent/ws"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	c, _, err := d.DialContext(ctx, u.String(), hdr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Close()
	log.Printf("connected to %s", base)

	// Unblock ReadMessage promptly on SIGTERM/SIGINT.
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()

	_ = c.WriteJSON(wireMsg{Type: "hello"})

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
		_, raw, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var msg wireMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "bash":
			res := runBash(ctx, msg.Command)
			res.Type = "bash_result"
			res.TaskID = msg.TaskID
			if err := c.WriteJSON(res); err != nil {
				return err
			}
		case "ai_start":
			res := handleAIStart(jobs, msg)
			res.Type = "ai_start_result"
			res.TaskID = msg.TaskID
			if err := c.WriteJSON(res); err != nil {
				return err
			}
		case "ai_status":
			res := handleAIStatus(jobs, msg)
			res.Type = "ai_status_result"
			res.TaskID = msg.TaskID
			if err := c.WriteJSON(res); err != nil {
				return err
			}
		case "pong":
		}
	}
}

func handleAIStart(jobs *jobManager, msg wireMsg) wireMsg {
	meta, err := jobs.start(msg.Agent, msg.Prompt, msg.Cwd, msg.AutoApprove)
	if err != nil && meta.JobID == "" {
		return wireMsg{Error: err.Error(), Status: "failed"}
	}
	if err != nil {
		return wireMsg{
			JobID:  meta.JobID,
			Agent:  meta.Agent,
			Status: meta.Status,
			PID:    meta.PID,
			Error:  err.Error(),
		}
	}
	log.Printf("ai job started id=%s agent=%s pid=%d", meta.JobID, meta.Agent, meta.PID)
	return wireMsg{
		JobID:     meta.JobID,
		Agent:     meta.Agent,
		Status:    meta.Status,
		PID:       meta.PID,
		StartedAt: meta.StartedAt,
	}
}

func handleAIStatus(jobs *jobManager, msg wireMsg) wireMsg {
	jobID := strings.TrimSpace(msg.JobID)
	if jobID == "" {
		return wireMsg{Jobs: jobs.list(), Status: "ok"}
	}
	meta, out, err := jobs.status(jobID, msg.TailBytes)
	if err != nil {
		return wireMsg{Error: err.Error(), JobID: jobID, Status: "unknown"}
	}
	return wireMsg{
		JobID:      meta.JobID,
		Agent:      meta.Agent,
		Status:     meta.Status,
		ExitCode:   meta.ExitCode,
		PID:        meta.PID,
		Cwd:        meta.Cwd,
		Prompt:     meta.Prompt,
		Output:     out,
		Error:      meta.Error,
		StartedAt:  meta.StartedAt,
		FinishedAt: meta.FinishedAt,
	}
}

func runBash(parent context.Context, command string) wireMsg {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := wireMsg{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Error = err.Error()
			res.ExitCode = -1
		}
	}
	return res
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
