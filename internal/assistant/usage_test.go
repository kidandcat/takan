package assistant

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSession lays out one grok session directory the way the CLI does:
// <home>/.grok/sessions/<url-encoded cwd>/<session id>/{summary.json,updates.jsonl}.
//
// The usage objects in updates.jsonl are cumulative, so the last one is the
// session's total — that is the whole reason the reader only needs the tail.
func writeSession(t *testing.T, home, cwd, id string, when time.Time, records []usageCounters) {
	t.Helper()
	dir := filepath.Join(home, ".grok", "sessions", cwd, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	summary := map[string]any{
		"info":           map[string]string{"id": id, "cwd": "/tmp/" + cwd},
		"last_active_at": when.UTC().Format(time.RFC3339Nano),
		"updated_at":     when.UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var lines strings.Builder
	// Noise the reader must skip over on its way back to the last usage record.
	lines.WriteString(`{"jsonrpc":"2.0","method":"noise","params":{"update":{"text":"hello"}}}` + "\n")
	for _, rec := range records {
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "update",
			"params":  map[string]any{"update": map[string]any{"usage": rec}},
		})
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(payload)
		lines.WriteByte('\n')
	}
	lines.WriteString(`{"jsonrpc":"2.0","method":"done","params":{"update":{}}}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
}

func counters(in, out, cached, calls, ticks int64, model string) usageCounters {
	u := usageCounters{
		InputTokens: in, OutputTokens: out, CachedReadTokens: cached,
		TotalTokens: in + out, ModelCalls: calls, CostUsdTicks: ticks,
	}
	if model != "" {
		u.ModelUsage = map[string]usageCounters{model: u}
	}
	return u
}

func TestGrokUsageBucketsByLastActivity(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)

	// Cumulative records: only the last one counts.
	writeSession(t, home, "proj-a", "s-today", now.Add(-2*time.Hour), []usageCounters{
		counters(100, 10, 50, 1, 1_000_000_000, "grok-4.6"),
		counters(1000, 100, 500, 10, 2_000_000_000, "grok-4.6"),
	})
	writeSession(t, home, "proj-b", "s-week", now.Add(-3*24*time.Hour), []usageCounters{
		counters(2000, 200, 1000, 20, 3_000_000_000, "grok-4.6-build"),
	})
	writeSession(t, home, "proj-c", "s-month", now.Add(-20*24*time.Hour), []usageCounters{
		counters(4000, 400, 2000, 40, 4_000_000_000, "grok-4.6"),
	})
	// Outside the window: never read.
	writeSession(t, home, "proj-d", "s-ancient", now.Add(-90*24*time.Hour), []usageCounters{
		counters(9_000_000, 9_000_000, 0, 900, 90_000_000_000, "grok-4.6"),
	})

	reporter := NewGrokUsage(home)
	reporter.now = func() time.Time { return now }
	report, err := reporter.Report()
	if err != nil {
		t.Fatal(err)
	}

	if report.Today.Sessions != 1 || report.Today.ModelCalls != 10 {
		t.Fatalf("today should hold only the last cumulative record: %+v", report.Today)
	}
	if report.Today.InputTokens != 1000 {
		t.Fatalf("cumulative records must not be summed within a session: %+v", report.Today)
	}
	if report.Week.Sessions != 2 || report.Week.ModelCalls != 30 {
		t.Fatalf("the 7d bucket should include today: %+v", report.Week)
	}
	if report.Month.Sessions != 3 || report.Month.ModelCalls != 70 {
		t.Fatalf("the 30d bucket should include the other two: %+v", report.Month)
	}
	if report.Month.CostTicks != 9_000_000_000 {
		t.Fatalf("cost should be summed across sessions: %d", report.Month.CostTicks)
	}
	if got := report.Month.CostUSD(); got < 8.99 || got > 9.01 {
		t.Fatalf("ticks are nano-USD, so 9e9 is $9: got %f", got)
	}
	if len(report.TodayByModel) != 1 || report.TodayByModel[0].Model != "grok-4.6" {
		t.Fatalf("today should break down per model: %+v", report.TodayByModel)
	}
}

func TestGrokUsageWithoutASessionTree(t *testing.T) {
	// A runner that has never been used, or is not grok at all.
	_, err := NewGrokUsage(t.TempDir()).Report()
	if err != ErrUsageUnsupported {
		t.Fatalf("a missing session tree means unsupported, got %v", err)
	}
}

func TestGrokUsageToleratesBrokenSessions(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	writeSession(t, home, "good", "s1", now, []usageCounters{counters(10, 1, 0, 1, 1_000_000, "grok-4.6")})

	// A session directory with no updates.jsonl at all: skipped, not fatal.
	empty := filepath.Join(home, ".grok", "sessions", "empty", "s2")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	// A session whose updates.jsonl is truncated garbage.
	broken := filepath.Join(home, ".grok", "sessions", "broken", "s3")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "updates.jsonl"), []byte(`{"usage":`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := NewGrokUsage(home).Report()
	if err != nil {
		t.Fatalf("a partial report beats no report: %v", err)
	}
	if report.Month.Sessions != 1 || report.Month.ModelCalls != 1 {
		t.Fatalf("the readable session must still be counted: %+v", report.Month)
	}
}

// TestLastUsageRecordReadsOnlyTheTail: these files reach tens of megabytes and
// /usage runs from a chat message, so the reader must not slurp the whole file.
func TestLastUsageRecordReadsOnlyTheTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "updates.jsonl")

	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, `{"method":"noise","params":{"update":{"text":"%s"}}}`+"\n", strings.Repeat("x", 200))
	}
	last, err := json.Marshal(map[string]any{
		"params": map[string]any{"update": map[string]any{
			"usage": counters(5_748_302, 35_893, 5_232_896, 61, 6_666_450_600, "grok-4.6-build"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Write(last)
	b.WriteByte('\n')
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := lastUsageRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ModelCalls != 61 || got.InputTokens != 5_748_302 {
		t.Fatalf("the last cumulative record was not found: %+v", got)
	}
}

func TestLastUsageRecordOnAFileWithNoUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	if err := os.WriteFile(path, []byte(`{"method":"noise"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := lastUsageRecord(path)
	if err != nil || got != nil {
		t.Fatalf("a session with no usage yet is not an error: %+v err=%v", got, err)
	}
	missing, err := lastUsageRecord(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || missing != nil {
		t.Fatalf("a missing file is not an error: %+v err=%v", missing, err)
	}
}

// TestUsageSummaryNeverFails: /usage is a status command. If it errors out
// instead of answering, it is useless exactly when it is needed.
func TestUsageSummaryNeverFails(t *testing.T) {
	b := newTestBot(t)

	// No reporter at all (a runner that is not grok).
	b.SetUsage(nil)
	if got := b.UsageSummary(); !strings.Contains(got, "no disponible") {
		t.Fatalf("an unsupported runner should say so: %q", got)
	}

	// A reporter whose tree is missing.
	b.SetUsage(NewGrokUsage(t.TempDir()))
	if got := b.UsageSummary(); !strings.Contains(got, "no disponible") {
		t.Fatalf("a missing tree should say so: %q", got)
	}

	// A reporter that fails outright.
	b.SetUsage(failingUsage{})
	got := b.UsageSummary()
	if !strings.Contains(got, "No he podido leer el consumo") {
		t.Fatalf("a failing reporter should degrade, not error: %q", got)
	}
	// The assistant's own counters are reported either way.
	if !strings.Contains(got, InstanceName) || !strings.Contains(got, "rate limit") {
		t.Fatalf("the local counters should always be there: %q", got)
	}
	if !strings.Contains(got, grokUsageURL) {
		t.Fatalf("subscription limits are not on disk, so /usage links to them: %q", got)
	}
}

type failingUsage struct{}

func (failingUsage) Runner() string { return "grok" }
func (failingUsage) Report() (UsageReport, error) {
	return UsageReport{}, fmt.Errorf("permission denied")
}

func TestUsageSummaryRendersTheBuckets(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	writeSession(t, home, "proj", "s1", now, []usageCounters{
		counters(1_500_000, 20_000, 1_400_000, 42, 2_450_000_000, "grok-4.6"),
	})

	b := newTestBot(t)
	b.SetUsage(NewGrokUsage(home))
	got := b.UsageSummary()

	for _, want := range []string{"hoy", "7 días", "30 días", "grok-4.6", "1.5M", "$2.45 est."} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestHumanCount(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{999, "999"}, {1500, "1.5k"}, {2_400_000, "2.4M"}} {
		if got := humanCount(tc.in); got != tc.want {
			t.Fatalf("humanCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
