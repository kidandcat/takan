package assistant

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

// grokUsageURL is where the subscription limits live. The CLI does not expose
// them — its own /usage just opens this page — so the report links to it.
const grokUsageURL = "https://grok.com/?_s=usage"

// costTicksPerUSD converts costUsdTicks to dollars. The runner reports nano-USD
// (2454069000 ≈ $2.45), which is an inference from observed values rather than
// a documented unit, so it lives in one constant and the report says "est.".
const costTicksPerUSD = 1e9

// usageWindow is how far back sessions are scanned. Older session directories
// are skipped by mtime before anything is read.
const usageWindow = 30 * 24 * time.Hour

// usageTailLimit bounds how much of an updates.jsonl is read looking for the
// last usage record. These files reach tens of megabytes; the record we want is
// always at the end.
const usageTailLimit = 8 << 20

// ErrUsageUnsupported is returned by a reporter that cannot introspect its
// runner. The caller turns it into a plain "not available" reply.
var ErrUsageUnsupported = errors.New("usage reporting is not available for this runner")

// UsageReporter reads a CLI agent's own accounting. Implementations are
// read-only and must tolerate missing, partial or concurrently written files.
type UsageReporter interface {
	// Runner names the agent this reporter understands, for the "not available"
	// message.
	Runner() string
	// Report returns consumption bucketed into today / 7d / 30d.
	Report() (UsageReport, error)
}

// UsageTotals is one accumulated set of counters.
type UsageTotals struct {
	Sessions     int
	ModelCalls   int64
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	TotalTokens  int64
	CostTicks    int64
}

// CostUSD is the estimated spend for these counters.
func (t UsageTotals) CostUSD() float64 { return float64(t.CostTicks) / costTicksPerUSD }

func (t *UsageTotals) add(u usageCounters) {
	t.ModelCalls += u.ModelCalls
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CachedTokens += u.CachedReadTokens
	t.TotalTokens += u.TotalTokens
	t.CostTicks += u.CostUsdTicks
}

// ModelUsage is one model's slice of a bucket.
type ModelUsage struct {
	Model string
	UsageTotals
}

// UsageReport is what /usage renders.
type UsageReport struct {
	Runner string
	Today  UsageTotals
	Week   UsageTotals
	Month  UsageTotals
	// TodayByModel breaks today down per model, biggest first.
	TodayByModel []ModelUsage
	// Scanned is how many session directories were considered.
	Scanned int
	// Skipped counts sessions whose files could not be read. A partial report is
	// better than no report.
	Skipped int
}

// usageCounters is the runner's usage object. It is cumulative per session, so
// the LAST record in a session's updates.jsonl is that session's total.
type usageCounters struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	TotalTokens       int64 `json:"totalTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CacheCreationToks int64 `json:"cacheCreationTokens"`
	ReasoningTokens   int64 `json:"reasoningTokens"`
	ModelCalls        int64 `json:"modelCalls"`
	APIDurationMs     int64 `json:"apiDurationMs"`
	CostUsdTicks      int64 `json:"costUsdTicks"`

	ModelUsage map[string]usageCounters `json:"modelUsage"`
}

// updateLine is the one shape we care about in updates.jsonl.
type updateLine struct {
	Params struct {
		Update struct {
			Usage *usageCounters `json:"usage"`
		} `json:"update"`
	} `json:"params"`
}

// sessionSummary is the subset of summary.json used to date a session.
type sessionSummary struct {
	UpdatedAt    string `json:"updated_at"`
	LastActiveAt string `json:"last_active_at"`
	CreatedAt    string `json:"created_at"`
	CurrentModel string `json:"current_model_id"`
}

// GrokUsage reads the grok CLI's per-session accounting from disk.
//
// The CLI has no usage subcommand: its own /usage just opens the web page. But
// every session directory under <home>/.grok/sessions/<url-encoded cwd>/<id>/
// carries an updates.jsonl whose usage objects are cumulative, so the last one
// is that session's total.
type GrokUsage struct {
	// home is the agent's HOME (ATLAS_AGENT_HOME, default $HOME).
	home string
	// now is injectable so tests can pin the bucket boundaries.
	now func() time.Time
}

// NewGrokUsage builds a reporter rooted at the agent's home directory.
func NewGrokUsage(home string) *GrokUsage {
	return &GrokUsage{home: home, now: time.Now}
}

// Runner names the agent this reporter understands.
func (g *GrokUsage) Runner() string { return "grok" }

// sessionsDir is where the CLI keeps its session directories.
func (g *GrokUsage) sessionsDir() string { return filepath.Join(g.home, ".grok", "sessions") }

// Report scans the session tree and buckets it by last activity in Madrid time.
func (g *GrokUsage) Report() (UsageReport, error) {
	report := UsageReport{Runner: g.Runner()}
	root := g.sessionsDir()
	cwdDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return report, ErrUsageUnsupported
		}
		return report, err
	}

	loc, err := time.LoadLocation(ScheduleLocation)
	if err != nil {
		loc = time.UTC
	}
	now := g.now().In(loc)
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	weekAgo := now.Add(-7 * 24 * time.Hour)
	monthAgo := now.Add(-usageWindow)

	todayModels := map[string]*UsageTotals{}

	for _, cwdDir := range cwdDirs {
		if !cwdDir.IsDir() {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(root, cwdDir.Name()))
		if err != nil {
			report.Skipped++
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			dir := filepath.Join(root, cwdDir.Name(), session.Name())

			// Cheap gate first: an old directory is never opened.
			info, err := session.Info()
			if err != nil {
				report.Skipped++
				continue
			}
			when := info.ModTime().In(loc)
			if when.Before(monthAgo) {
				continue
			}
			summary := readSessionSummary(dir)
			if t, ok := summary.when(loc); ok {
				when = t
			}
			if when.Before(monthAgo) {
				continue
			}

			usage, err := lastUsageRecord(filepath.Join(dir, "updates.jsonl"))
			if err != nil || usage == nil {
				if err != nil {
					report.Skipped++
				}
				continue
			}
			report.Scanned++

			report.Month.Sessions++
			report.Month.add(*usage)
			if !when.Before(weekAgo) {
				report.Week.Sessions++
				report.Week.add(*usage)
			}
			if !when.Before(startOfToday) {
				report.Today.Sessions++
				report.Today.add(*usage)
				for model, mu := range usage.ModelUsage {
					row, ok := todayModels[model]
					if !ok {
						row = &UsageTotals{}
						todayModels[model] = row
					}
					row.Sessions++
					row.add(mu)
				}
			}
		}
	}

	for model, totals := range todayModels {
		report.TodayByModel = append(report.TodayByModel, ModelUsage{Model: model, UsageTotals: *totals})
	}
	sort.Slice(report.TodayByModel, func(i, j int) bool {
		if report.TodayByModel[i].TotalTokens != report.TodayByModel[j].TotalTokens {
			return report.TodayByModel[i].TotalTokens > report.TodayByModel[j].TotalTokens
		}
		return report.TodayByModel[i].Model < report.TodayByModel[j].Model
	})
	return report, nil
}

// when resolves a session's last activity from its summary, newest field first.
func (s sessionSummary) when(loc *time.Location) (time.Time, bool) {
	for _, raw := range []string{s.LastActiveAt, s.UpdatedAt, s.CreatedAt} {
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t.In(loc), true
		}
	}
	return time.Time{}, false
}

// readSessionSummary reads summary.json, tolerating a missing or broken file.
func readSessionSummary(dir string) sessionSummary {
	var s sessionSummary
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

// lastUsageRecord returns the final cumulative usage object of an updates.jsonl,
// reading only the tail of the file. A missing file is not an error: a session
// can exist before it has produced any usage.
func lastUsageRecord(path string) (*usageCounters, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}

	for tail := int64(256 << 10); ; tail *= 4 {
		if tail > usageTailLimit {
			tail = usageTailLimit
		}
		offset := size - tail
		atStart := false
		if offset <= 0 {
			offset, atStart = 0, true
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		buf, err := io.ReadAll(io.LimitReader(f, size-offset))
		if err != nil {
			return nil, err
		}
		lines := bytes.Split(buf, []byte("\n"))
		if !atStart && len(lines) > 0 {
			// The first line is almost certainly truncated mid-JSON.
			lines = lines[1:]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			line := bytes.TrimSpace(lines[i])
			if len(line) == 0 || !bytes.Contains(line, []byte(`"usage"`)) {
				continue
			}
			var parsed updateLine
			if err := json.Unmarshal(line, &parsed); err != nil {
				continue
			}
			if parsed.Params.Update.Usage != nil {
				return parsed.Params.Update.Usage, nil
			}
		}
		if atStart || tail >= usageTailLimit {
			return nil, nil
		}
	}
}

// UsageSummary renders /usage: the runner's own accounting plus the assistant's
// counters. It never fails — a broken or unsupported runner produces a line
// saying so, because a status command that errors out is useless.
func (b *Bot) UsageSummary() string {
	var lines []string

	if b.usage == nil {
		lines = append(lines, fmt.Sprintf("Consumo no disponible para %q.", b.opts.Agent.Command))
	} else {
		report, err := b.usage.Report()
		switch {
		case errors.Is(err, ErrUsageUnsupported):
			lines = append(lines, fmt.Sprintf("Consumo no disponible para %q.", b.opts.Agent.Command))
		case err != nil:
			lines = append(lines, "No he podido leer el consumo del agente: "+tg.TruncateRunes(err.Error(), 200))
		default:
			lines = append(lines, formatUsageReport(report)...)
		}
	}

	lines = append(lines, "", fmt.Sprintf("%s: %d tarea(s) en background · %d turnos hoy · %d error(es) de rate limit en 24h",
		InstanceName, countRunningTasks(b.tasks), b.RunsToday(), b.rateLimitedLast24h()))
	lines = append(lines, "Límites de la suscripción: "+grokUsageURL+" (el CLI no los expone).")
	return strings.Join(lines, "\n")
}

func (b *Bot) rateLimitedLast24h() int {
	if b.tasks == nil {
		return 0
	}
	return b.tasks.RateLimitedLast24h()
}

// formatUsageReport turns a report into the lines /usage prints.
func formatUsageReport(r UsageReport) []string {
	lines := []string{fmt.Sprintf("Consumo de %s", r.Runner)}
	for _, bucket := range []struct {
		label  string
		totals UsageTotals
	}{
		{"hoy", r.Today},
		{"7 días", r.Week},
		{"30 días", r.Month},
	} {
		lines = append(lines, fmt.Sprintf("• %s: %d sesiones · %d llamadas · in %s / out %s (cache %s) · ~%s",
			bucket.label, bucket.totals.Sessions, bucket.totals.ModelCalls,
			humanCount(bucket.totals.InputTokens), humanCount(bucket.totals.OutputTokens),
			humanCount(bucket.totals.CachedTokens), humanUSD(bucket.totals.CostUSD())))
	}
	if len(r.TodayByModel) > 0 {
		lines = append(lines, "", "Hoy por modelo:")
		for _, m := range r.TodayByModel {
			lines = append(lines, fmt.Sprintf("• %s: %d llamadas · %s tokens · ~%s",
				m.Model, m.ModelCalls, humanCount(m.TotalTokens), humanUSD(m.CostUSD())))
		}
	}
	if r.Skipped > 0 {
		lines = append(lines, fmt.Sprintf("(%d sesión(es) ilegibles, omitidas)", r.Skipped))
	}
	return lines
}

// humanCount renders a token count compactly.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// humanUSD renders an estimated cost. It is explicitly an estimate: the tick
// unit is inferred, not documented.
func humanUSD(v float64) string { return fmt.Sprintf("$%.2f est.", v) }
