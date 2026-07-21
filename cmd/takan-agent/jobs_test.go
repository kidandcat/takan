package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildAIArgs(t *testing.T) {
	claude := buildAIArgs("claude", "fix it", true)
	// claude -p [--dangerously-skip-permissions] <prompt>
	if claude[0] != "-p" {
		t.Fatalf("claude args: %v", claude)
	}
	if claude[len(claude)-1] != "fix it" {
		t.Fatalf("claude prompt last: %v", claude)
	}
	found := false
	for _, a := range claude {
		if a == "--dangerously-skip-permissions" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected --dangerously-skip-permissions")
	}

	claudeNo := buildAIArgs("claude", "x", false)
	if len(claudeNo) != 2 || claudeNo[0] != "-p" || claudeNo[1] != "x" {
		t.Fatalf("claude no-approve: %v", claudeNo)
	}

	grok := buildAIArgs("grok", "hi", true)
	// grok [--always-approve] -p <prompt>
	if grok[len(grok)-2] != "-p" || grok[len(grok)-1] != "hi" {
		t.Fatalf("grok args: %v", grok)
	}
	found = false
	for _, a := range grok {
		if a == "--always-approve" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected --always-approve for grok")
	}
}

func TestJobManagerStartAndStatus(t *testing.T) {
	// Use a throwaway home so we don't pollute ~/.takan/jobs
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// also USERPROFILE for portability (unused on mac)
	t.Setenv("USERPROFILE", tmp)

	jm, err := newJobManager()
	if err != nil {
		t.Fatal(err)
	}
	// start a short-lived shell stand-in by pointing "claude" at /bin/echo via PATH trick:
	// resolveAIBinary looks for claude; install a fake binary.
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\necho started\nsleep 0.3\necho done\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	meta, err := jm.start("claude", "hello world", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.JobID == "" || meta.Status != "running" {
		t.Fatalf("meta: %+v", meta)
	}

	// wait for completion
	deadline := time.Now().Add(3 * time.Second)
	var status string
	var out string
	for time.Now().Before(deadline) {
		m, o, err := jm.status(meta.JobID, 4096)
		if err != nil {
			t.Fatal(err)
		}
		status = m.Status
		out = o
		if status == "done" || status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("status=%s out=%q", status, out)
	}
	if !strings.Contains(out, "done") && !strings.Contains(out, "started") {
		// fake script may have written both
		t.Fatalf("output missing expected text: %q", out)
	}

	list := jm.list()
	if len(list) == 0 {
		t.Fatal("expected at least one job in list")
	}
}
