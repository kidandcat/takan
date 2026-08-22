// takan-agent connects outbound to a Takan hub and runs bash / AI jobs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// connTiming: readWait must exceed hub ping interval (30s) and match hub's read
// deadline (120s). Without resetting this on inbound protocol pings, idle agents
// disconnect every ~2 min.
//
// Watchdog is inbound-only: a half-open TCP write can succeed locally, so outbound
// pings must not count as liveness. The timer (not the socket deadline) is what
// recovers a Mac after sleep when kqueue never delivers the 120s read timeout.
type connTiming struct {
	readWait      time.Duration
	writeWait     time.Duration
	pingEvery     time.Duration
	watchdogTick  time.Duration
	staleAfter    time.Duration
	hangExitAfter time.Duration
}

func defaultConnTiming() connTiming {
	return connTiming{
		readWait:      120 * time.Second,
		writeWait:     10 * time.Second,
		pingEvery:     30 * time.Second,
		watchdogTick:  15 * time.Second,
		staleAfter:    90 * time.Second,
		hangExitAfter: 3 * time.Minute,
	}
}

var forceExit = os.Exit

// livenessAction decides whether a silent connection should be closed or the process
// should exit so launchd/systemd can restart it. Idle is time since last inbound.
func livenessAction(idle, staleAfter, hangExitAfter time.Duration) (closeConn, exitProc bool) {
	if idle > hangExitAfter {
		return true, true
	}
	if idle > staleAfter {
		return true, false
	}
	return false, false
}

type wireMsg struct {
	Type        string    `json:"type"`
	TaskID      string    `json:"task_id,omitempty"`
	Command     string    `json:"command,omitempty"`
	ExitCode    int       `json:"exit_code,omitempty"`
	Stdout      string    `json:"stdout,omitempty"`
	Stderr      string    `json:"stderr,omitempty"`
	Error       string    `json:"error,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	Runner      string    `json:"runner,omitempty"`
	Prompt      string    `json:"prompt,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	ParentJobID string    `json:"parent_job_id,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	JobID       string    `json:"job_id,omitempty"`
	Status      string    `json:"status,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Output      string    `json:"output,omitempty"`
	StartedAt   string    `json:"started_at,omitempty"`
	FinishedAt  string    `json:"finished_at,omitempty"`
	TailBytes   int       `json:"tail_bytes,omitempty"`
	MaxBytes    int       `json:"max_bytes,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
	TotalBytes  int       `json:"total_bytes,omitempty"`
	Jobs        []jobMeta `json:"jobs,omitempty"`
	Html        string    `json:"html,omitempty"`
}

func main() {
	baseURL := flag.String("url", env("TAKAN_URL", ""), "Takan hub base URL")
	token := flag.String("token", env("TAKAN_AGENT_TOKEN", ""), "Agent token from panel")
	name := flag.String("name", env("TAKAN_AGENT_NAME", ""), "Machine name (informational)")
	displayAddr := flag.String("display-addr", env("TAKAN_DISPLAY_ADDR", "127.0.0.1:8787"), "Local HTTP addr for display HTML (empty to disable)")
	flag.Parse()
	hubURL, err := resolveHubURL(*baseURL)
	if err != nil {
		log.Fatal(err)
	}
	if *token == "" {
		log.Fatal("--token or TAKAN_AGENT_TOKEN required")
	}
	_ = name

	jobs, err := newJobManager()
	if err != nil {
		log.Fatalf("jobs dir: %v", err)
	}

	var disp *displayServer
	if strings.TrimSpace(*displayAddr) != "" {
		disp = newDisplayServer(*displayAddr, defaultDisplayDir())
		if err := disp.start(); err != nil {
			log.Printf("display server: %v — display_show will fail on this host", err)
			disp = nil
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for ctx.Err() == nil {
		err := runOnce(ctx, hubURL, *token, jobs, defaultConnTiming(), disp)
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

func runOnce(ctx context.Context, base, token string, jobs *jobManager, tm connTiming, disp *displayServer) error {
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
	d := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext:   (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	}
	c, _, err := d.DialContext(ctx, u.String(), hdr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.SetReadLimit(2 << 20)
	defer c.Close()
	log.Printf("connected to %s", base)

	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()

	var lastIn atomic.Int64
	touch := func() { lastIn.Store(time.Now().UnixNano()) }
	touch()

	// Serialize writes: protocol pongs, app pings, and task results share the conn.
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(tm.writeWait))
		return c.WriteJSON(v)
	}

	// Hub sends WebSocket Ping frames every 30s. Those are control frames: they do
	// not make ReadMessage return, so a fixed SetReadDeadline before ReadMessage
	// expires after 120s of pure control traffic → i/o timeout + flapping offline.
	// Reset the deadline on every inbound ping (and reply with pong).
	c.SetPingHandler(func(appData string) error {
		touch()
		writeMu.Lock()
		err := c.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(tm.writeWait))
		writeMu.Unlock()
		if err == websocket.ErrCloseSent {
			return nil
		}
		if err != nil {
			return err
		}
		return c.SetReadDeadline(time.Now().Add(tm.readWait))
	})

	jobs.setNotify(func(meta jobMeta) {
		_ = writeJSON(jobWire("ai_done", meta, "", 0, false))
	})
	defer jobs.setNotify(nil)

	if err := writeJSON(wireMsg{Type: "hello"}); err != nil {
		return err
	}

	// App-level keepalive: hub Touch() + server read deadline need data messages
	// (protocol pings alone only keep the client deadline if SetPingHandler runs).
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		t := time.NewTicker(tm.pingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writeJSON(wireMsg{Type: "ping"}); err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(tm.watchdogTick)
		defer t.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				idle := time.Since(time.Unix(0, lastIn.Load()))
				closeConn, exitProc := livenessAction(idle, tm.staleAfter, tm.hangExitAfter)
				if !closeConn {
					continue
				}
				log.Printf("watchdog: no inbound for %s, closing socket", idle)
				_ = c.Close()
				if exitProc {
					log.Printf("watchdog: still hung after %s, exiting", idle)
					forceExit(2)
					return
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = c.SetReadDeadline(time.Now().Add(tm.readWait))
		_, raw, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		touch()
		_ = c.SetReadDeadline(time.Now().Add(tm.readWait))
		var msg wireMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "bash":
			res := runBash(ctx, msg.Command)
			res.Type = "bash_result"
			res.TaskID = msg.TaskID
			if err := writeJSON(res); err != nil {
				return err
			}
		case "ai_start":
			res := handleAIStart(jobs, msg)
			res.Type = "ai_start_result"
			res.TaskID = msg.TaskID
			if err := writeJSON(res); err != nil {
				return err
			}
		case "ai_status":
			res := handleAIStatus(jobs, msg)
			res.Type = "ai_status_result"
			res.TaskID = msg.TaskID
			if err := writeJSON(res); err != nil {
				return err
			}
		case "ai_cancel":
			res := handleAICancel(jobs, msg)
			res.Type = "ai_cancel_result"
			res.TaskID = msg.TaskID
			if err := writeJSON(res); err != nil {
				return err
			}
		case "ai_log":
			res := handleAILog(jobs, msg)
			res.Type = "ai_log_result"
			res.TaskID = msg.TaskID
			if err := writeJSON(res); err != nil {
				return err
			}
		case "display":
			res := wireMsg{Type: "display_result", TaskID: msg.TaskID, Status: "ok"}
			if disp == nil {
				res.Status = "failed"
				res.Error = "display server disabled on this agent"
			} else if err := disp.setHTML(msg.Html); err != nil {
				res.Status = "failed"
				res.Error = err.Error()
			}
			if err := writeJSON(res); err != nil {
				return err
			}
		case "pong", "ping":
			// Hub may echo pong for our app pings; ignore.
		}
	}
}

func handleAIStart(jobs *jobManager, msg wireMsg) wireMsg {
	runner := strings.TrimSpace(msg.Runner)
	if runner == "" {
		runner = strings.TrimSpace(msg.Agent)
	}
	cmdTmpl := strings.TrimSpace(msg.Command)
	// Backward compat: old hubs sent agent=claude|grok without command.
	if cmdTmpl == "" && (runner == "claude" || runner == "grok") {
		if runner == "claude" {
			cmdTmpl = "claude -p --dangerously-skip-permissions " + promptPlaceholder
		} else {
			cmdTmpl = "grok --always-approve -p " + promptPlaceholder
		}
	}
	meta, err := jobs.start(runner, cmdTmpl, msg.Prompt, msg.Cwd, msg.ParentJobID, msg.Owner)
	if err != nil && meta.JobID == "" {
		return wireMsg{Error: err.Error(), Status: "failed"}
	}
	if err != nil {
		out := jobWire("", meta, "", 0, false)
		out.Error = err.Error()
		return out
	}
	log.Printf("ai job started id=%s runner=%s pid=%d parent=%s owner=%s", meta.JobID, meta.Agent, meta.PID, meta.ParentJobID, meta.Owner)
	return jobWire("", meta, "", 0, false)
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
	return jobWire("", meta, out, 0, false)
}

func handleAICancel(jobs *jobManager, msg wireMsg) wireMsg {
	meta, err := jobs.cancel(strings.TrimSpace(msg.JobID))
	if err != nil {
		return wireMsg{Error: err.Error(), JobID: strings.TrimSpace(msg.JobID), Status: "unknown"}
	}
	log.Printf("ai job cancel id=%s status=%s", meta.JobID, meta.Status)
	return jobWire("", meta, "", 0, false)
}

func handleAILog(jobs *jobManager, msg wireMsg) wireMsg {
	meta, out, total, truncated, err := jobs.readLog(strings.TrimSpace(msg.JobID), msg.MaxBytes)
	if err != nil {
		return wireMsg{Error: err.Error(), JobID: strings.TrimSpace(msg.JobID), Status: "unknown"}
	}
	res := jobWire("", meta, out, total, truncated)
	return res
}

func jobWire(typ string, meta jobMeta, output string, total int, truncated bool) wireMsg {
	return wireMsg{
		Type:        typ,
		JobID:       meta.JobID,
		Agent:       meta.Agent,
		Runner:      meta.Agent,
		Status:      meta.Status,
		ExitCode:    meta.ExitCode,
		PID:         meta.PID,
		Cwd:         meta.Cwd,
		Prompt:      meta.Prompt,
		Command:     meta.Command,
		ParentJobID: meta.ParentJobID,
		Owner:       meta.Owner,
		Output:      output,
		Error:       meta.Error,
		StartedAt:   meta.StartedAt,
		FinishedAt:  meta.FinishedAt,
		TotalBytes:  total,
		Truncated:   truncated,
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

// resolveHubURL trims the hub base URL. Empty is fatal so a missing --url does
// not silently connect to some other operator's hosted instance.
func resolveHubURL(raw string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return "", fmt.Errorf("--url or TAKAN_URL required (your hub, e.g. http://127.0.0.1:8090)")
	}
	return u, nil
}
