package main

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const defaultBashTimeout = 5 * time.Minute

// bashSession runs at most one hub bash command at a time so the WebSocket
// read loop never blocks. A new command cancels the previous one (supersede).
// Parent should be the connection lifetime so a hub disconnect kills the group.
type bashSession struct {
	timeout time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	gen    uint64
}

func (s *bashSession) start(parent context.Context, command string, done func(wireMsg)) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultBashTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)

	s.mu.Lock()
	prev := s.cancel
	s.gen++
	gen := s.gen
	s.cancel = cancel
	s.mu.Unlock()
	if prev != nil {
		log.Printf("bash: cancelling previous command")
		prev()
	}

	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			if s.gen == gen {
				s.cancel = nil
			}
			s.mu.Unlock()
		}()
		res := runBash(ctx, command)
		if done != nil {
			done(res)
		}
	}()
}

func (s *bashSession) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// runBash runs command under bash -lc. ctx cancellation/timeout kills the
// process group (bash -lc leaves children that CommandContext would otherwise orphan).
func runBash(ctx context.Context, command string) wireMsg {
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		killProcessGroupNow(cmd.Process.Pid)
		return nil
	}
	// If a grandchild outside the group keeps pipes open, do not hang Wait forever.
	cmd.WaitDelay = time.Second

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
