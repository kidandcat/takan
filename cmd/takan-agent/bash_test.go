package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunBashSuccessAndNonZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok := runBash(ctx, "echo hello-stdout; echo hello-stderr >&2")
	if ok.ExitCode != 0 || ok.Error != "" {
		t.Fatalf("success: %+v", ok)
	}
	if !strings.Contains(ok.Stdout, "hello-stdout") {
		t.Fatalf("stdout: %q", ok.Stdout)
	}
	if !strings.Contains(ok.Stderr, "hello-stderr") {
		t.Fatalf("stderr: %q", ok.Stderr)
	}

	fail := runBash(ctx, "echo failed-stderr >&2; exit 7")
	if fail.ExitCode != 7 {
		t.Fatalf("exit 7: %+v", fail)
	}
	if fail.Error != "" {
		t.Fatalf("non-zero should use exit_code, not error: %+v", fail)
	}
	if !strings.Contains(fail.Stderr, "failed-stderr") {
		t.Fatalf("stderr on fail: %q", fail.Stderr)
	}
}

func TestRunBashTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	start := time.Now()
	done := make(chan wireMsg, 1)
	go func() { done <- runBash(ctx, sleepGroupCommand(pidFile)) }()
	pid := waitPIDFile(t, pidFile, 2*time.Second)

	var res wireMsg
	select {
	case res = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runBash did not return after timeout")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("timeout too slow: %s res=%+v", elapsed, res)
	}
	if res.ExitCode == 0 {
		t.Fatalf("expected killed command, got %+v", res)
	}
	waitDead(t, pid, 2*time.Second)
}

func TestRunBashParentCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan wireMsg, 1)
	go func() { done <- runBash(ctx, sleepGroupCommand(pidFile)) }()

	pid := waitPIDFile(t, pidFile, 2*time.Second)
	if !pidAlive(pid) {
		t.Fatal("child not running")
	}
	cancel()

	select {
	case res := <-done:
		if res.ExitCode == 0 {
			t.Fatalf("expected cancel error, got %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runBash did not return after cancel")
	}
	waitDead(t, pid, 2*time.Second)
}

func TestBashSessionSupersedeKillsPrevious(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := &bashSession{timeout: time.Minute}
	defer s.stop()
	got := make(chan wireMsg, 2)
	s.start(parent, sleepGroupCommand(pidFile), func(m wireMsg) { got <- m })

	pid := waitPIDFile(t, pidFile, 2*time.Second)
	if !pidAlive(pid) {
		t.Fatal("child not running")
	}
	s.start(parent, "echo SECOND", func(m wireMsg) { got <- m })
	waitDead(t, pid, 2*time.Second)

	deadline := time.After(3 * time.Second)
	sawSecond := false
	for !sawSecond {
		select {
		case <-deadline:
			t.Fatal("did not get second bash result")
		case m := <-got:
			if strings.Contains(m.Stdout, "SECOND") && m.ExitCode == 0 {
				sawSecond = true
			}
		}
	}
}

func TestBashSessionStopKillsRunning(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := &bashSession{timeout: time.Minute}
	got := make(chan wireMsg, 1)
	s.start(parent, sleepGroupCommand(pidFile), func(m wireMsg) { got <- m })
	pid := waitPIDFile(t, pidFile, 2*time.Second)
	s.stop()
	waitDead(t, pid, 2*time.Second)
	select {
	case res := <-got:
		if res.ExitCode == 0 {
			t.Fatalf("stop should fail the command: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no result after stop")
	}
}

func sleepGroupCommand(pidFile string) string {
	// Background sleep is a child of bash -lc; killing only bash would leak it.
	return fmt.Sprintf("/bin/sleep 30 & echo $! > %q; wait", pidFile)
}

func waitPIDFile(t *testing.T, path string, d time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = strings.TrimSpace(string(b))
			pid, err := strconv.Atoi(last)
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s not ready (last %q)", path, last)
	return 0
}

func waitDead(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive", pid)
}
