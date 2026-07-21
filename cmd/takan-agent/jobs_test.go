package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellQuoteAndExpand(t *testing.T) {
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Fatalf("quote: %q", got)
	}
	got := expandPromptTemplate("claude -p {{prompt}}", "hello world")
	if !strings.Contains(got, "'hello world'") {
		t.Fatalf("expand: %q", got)
	}
	got = expandPromptTemplate("mycli --task", "x")
	if !strings.HasSuffix(got, " 'x'") {
		t.Fatalf("append: %q", got)
	}
}

func TestJobManagerStartAndStatus(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	jm, err := newJobManager()
	if err != nil {
		t.Fatal(err)
	}
	// Use /bin/echo via a template so we don't depend on claude/grok.
	meta, err := jm.start("echo", "echo started; sleep 0.2; echo done # {{prompt}}", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.JobID == "" || meta.Status != "running" {
		t.Fatalf("meta: %+v", meta)
	}

	deadline := time.Now().Add(3 * time.Second)
	var status, out string
	for time.Now().Before(deadline) {
		m, o, err := jm.status(meta.JobID, 4096)
		if err != nil {
			t.Fatal(err)
		}
		status, out = m.Status, o
		if status == "done" || status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "done" {
		t.Fatalf("status=%s out=%q", status, out)
	}
	if !strings.Contains(out, "done") && !strings.Contains(out, "started") {
		t.Fatalf("output: %q", out)
	}
	if len(jm.list()) == 0 {
		t.Fatal("expected jobs in list")
	}
	// ensure job dir under temp home
	if _, err := os.Stat(filepath.Join(tmp, ".takan", "jobs", meta.JobID, "meta.json")); err != nil {
		t.Fatal(err)
	}
}
