// takan-agent connects outbound to a Takan hub and runs bash commands.
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
	Type     string `json:"type"`
	TaskID   string `json:"task_id,omitempty"`
	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for ctx.Err() == nil {
		err := runOnce(ctx, *baseURL, *token)
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

func runOnce(ctx context.Context, base, token string) error {
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

	_ = c.WriteJSON(wireMsg{Type: "hello"})

	for {
		_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
		_, raw, err := c.ReadMessage()
		if err != nil {
			return err
		}
		var msg wireMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "bash":
			res := runBash(msg.Command)
			res.Type = "bash_result"
			res.TaskID = msg.TaskID
			if err := c.WriteJSON(res); err != nil {
				return err
			}
		case "pong":
		}
	}
}

func runBash(command string) wireMsg {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
