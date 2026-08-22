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
	meta, err := jm.start("echo", "echo started; sleep 0.2; echo done # {{prompt}}", "hello", "", "", "Minerva")
	if err != nil {
		t.Fatal(err)
	}
	if meta.JobID == "" || meta.Status != "running" || meta.Owner != "Minerva" {
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
	done, _, err := jm.status(meta.JobID, 256)
	if err != nil {
		t.Fatal(err)
	}
	if done.Owner != "Minerva" {
		t.Fatalf("owner not persisted: %+v", done)
	}
	if !strings.Contains(out, "done") && !strings.Contains(out, "started") {
		t.Fatalf("output: %q", out)
	}
	listed := jm.list()
	if len(listed) == 0 {
		t.Fatal("expected jobs in list")
	}
	found := false
	for _, j := range listed {
		if j.JobID == meta.JobID && j.Owner == "Minerva" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("list missing owner: %+v", listed)
	}
	// ensure job dir under temp home
	if _, err := os.Stat(filepath.Join(tmp, ".takan", "jobs", meta.JobID, "meta.json")); err != nil {
		t.Fatal(err)
	}
}

func TestJobManagerCancelAndStatus(t *testing.T) {
	jm := testJobs(t)
	meta, err := jm.start("sleep", "sleep 30 # {{prompt}}", "hold", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != "running" || meta.PID <= 0 {
		t.Fatalf("meta: %+v", meta)
	}

	got, err := jm.cancel(meta.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "cancelled" {
		t.Fatalf("cancel status=%s", got.Status)
	}
	if got.FinishedAt == "" {
		t.Fatal("expected finished_at")
	}

	deadline := time.Now().Add(3 * time.Second)
	var last jobMeta
	for time.Now().Before(deadline) {
		m, _, err := jm.status(meta.JobID, 256)
		if err != nil {
			t.Fatal(err)
		}
		last = m
		if m.Status == "cancelled" && (m.PID <= 0 || !pidAlive(m.PID)) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if last.Status != "cancelled" {
		t.Fatalf("Wait() overwrote cancel: %+v", last)
	}

	again, err := jm.cancel(meta.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "cancelled" {
		t.Fatalf("second cancel: %+v", again)
	}
}

func TestJobManagerCancelAlreadyDone(t *testing.T) {
	jm := testJobs(t)
	meta, err := jm.start("echo", "echo already-done # {{prompt}}", "x", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, jm, meta.JobID, "done", 3*time.Second)
	got, err := jm.cancel(meta.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("must not rewrite done → cancelled: %+v", got)
	}
}

func TestJobManagerCancelUnknown(t *testing.T) {
	jm := testJobs(t)
	if _, err := jm.cancel("no-such-job"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := jm.cancel("../etc"); err == nil {
		t.Fatal("expected invalid job_id")
	}
}

func TestJobManagerReadLogAndParent(t *testing.T) {
	jm := testJobs(t)
	line := strings.Repeat("LINE", 80)
	meta, err := jm.start("echo", "echo "+line+" # {{prompt}}", "prompt-text", "", "parent-abc", "Menta")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ParentJobID != "parent-abc" || meta.Owner != "Menta" {
		t.Fatalf("parent/owner: %+v", meta)
	}
	waitStatus(t, jm, meta.JobID, "done", 3*time.Second)

	m, out, total, truncated, err := jm.readLog(meta.JobID, 40)
	if err != nil {
		t.Fatal(err)
	}
	if m.ParentJobID != "parent-abc" || m.Owner != "Menta" {
		t.Fatalf("parent/owner on read: %+v", m)
	}
	if total <= 40 {
		t.Fatalf("expected a larger log, total=%d out=%q", total, out)
	}
	if !truncated || len(out) != 40 {
		t.Fatalf("truncated=%v len=%d total=%d", truncated, len(out), total)
	}
	if !strings.Contains(out, "LINE") {
		t.Fatalf("output from start: %q", out)
	}

	full, all, tot2, trunc2, err := jm.readLog(meta.JobID, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	if trunc2 || len(all) != tot2 || full.JobID != meta.JobID {
		t.Fatalf("full log trunc=%v len=%d total=%d", trunc2, len(all), tot2)
	}

	st, tail, err := jm.status(meta.JobID, 12)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "done" || !strings.Contains(all, tail) {
		t.Fatalf("status tail %q not in full log", tail)
	}
}

func TestHandleAICancelStatusLog(t *testing.T) {
	jm := testJobs(t)
	started := handleAIStart(jm, wireMsg{
		Runner: "sleep", Command: "sleep 30 # {{prompt}}", Prompt: "hold",
	})
	if started.JobID == "" || started.Status != "running" {
		t.Fatalf("start: %+v", started)
	}
	cancelled := handleAICancel(jm, wireMsg{JobID: started.JobID})
	if cancelled.Status != "cancelled" || cancelled.JobID != started.JobID {
		t.Fatalf("cancel: %+v", cancelled)
	}
	st := handleAIStatus(jm, wireMsg{JobID: started.JobID, TailBytes: 100})
	if st.Status != "cancelled" {
		t.Fatalf("status after cancel: %+v", st)
	}

	echo := handleAIStart(jm, wireMsg{
		Runner: "echo", Command: "echo transcript-ok # {{prompt}}", Prompt: "p", ParentJobID: "from-parent", Owner: "Games",
	})
	waitStatus(t, jm, echo.JobID, "done", 3*time.Second)
	lg := handleAILog(jm, wireMsg{JobID: echo.JobID, MaxBytes: 2000})
	if lg.ParentJobID != "from-parent" || lg.Owner != "Games" || !strings.Contains(lg.Output, "transcript-ok") {
		t.Fatalf("log: %+v", lg)
	}
	stEcho := handleAIStatus(jm, wireMsg{JobID: echo.JobID, TailBytes: 100})
	if stEcho.Owner != "Games" {
		t.Fatalf("status owner: %+v", stEcho)
	}
	listed := handleAIStatus(jm, wireMsg{})
	if len(listed.Jobs) == 0 {
		t.Fatal("expected jobs in list")
	}
}

func waitStatus(t *testing.T, jm *jobManager, id, want string, d time.Duration) jobMeta {
	t.Helper()
	deadline := time.Now().Add(d)
	var last jobMeta
	for time.Now().Before(deadline) {
		m, _, err := jm.status(id, 256)
		if err != nil {
			t.Fatal(err)
		}
		last = m
		if m.Status == want {
			return m
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("job %s status=%s want=%s", id, last.Status, want)
	return last
}
