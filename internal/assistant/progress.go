package assistant

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kidandcat/takan/internal/tg"
)

// Progress event kinds. They are the whole vocabulary the app and the Telegram
// renderer see; the runner's own line types are mapped onto them.
const (
	// ProgressToolStart is a tool the agent has just decided to run.
	ProgressToolStart = "tool_start"
	// ProgressToolEnd is that tool finishing, successfully or not.
	ProgressToolEnd = "tool_end"
	// ProgressThinking marks the first reasoning token of a turn. It is emitted
	// once per run, not per delta: the deltas are noise.
	ProgressThinking = "thinking"
	// ProgressEnd terminates the run's progress stream.
	ProgressEnd = "end"
)

// Tool statuses carried by a ProgressEvent.
const (
	ProgressRunning   = "in_progress"
	ProgressCompleted = "completed"
	ProgressFailed    = "failed"
)

const (
	// progressDetailRunes caps the redacted one-line summary of a tool call.
	// Long enough to recognise the command, short enough that nothing
	// interesting can hide past the cut.
	progressDetailRunes = 80
	// progressSteps is how many tool steps the Telegram message shows.
	progressSteps = 4
	// progressRingSize bounds the per-run buffer replayed to an app that
	// connects mid-run.
	progressRingSize = 64
)

// ProgressEvent is one step of a run, as shown on Telegram and on the app's SSE
// channel.
//
// It is deliberately NOT the runner's own event: no tool output, no rawInput, no
// usage blob ever reaches this struct. Detail is a redacted one-liner built by
// scrubDetail. See §"Redaction" below.
type ProgressEvent struct {
	Kind   string `json:"kind"`
	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail,omitempty"`
	Status string `json:"status,omitempty"`
	// CallID ties a tool_end back to its tool_start so the renderer can update
	// a step in place instead of printing it twice.
	CallID  string        `json:"call_id,omitempty"`
	Elapsed time.Duration `json:"-"`
	Seq     int           `json:"seq"`
}

// MarshalJSON renders Elapsed as whole milliseconds. time.Duration would
// otherwise reach the app as a nanosecond count, which no client wants to
// divide by a magic number.
func (e ProgressEvent) MarshalJSON() ([]byte, error) {
	type alias ProgressEvent
	return json.Marshal(struct {
		alias
		ElapsedMS int64 `json:"elapsed_ms"`
	}{alias(e), e.Elapsed.Milliseconds()})
}

// Redaction. A tool call's arguments are model-authored and routinely contain
// the operator's own secrets (curl -H "Authorization: Bearer …", a psql URL, an
// API key echoed into a file). The progress message is posted to Telegram and
// mirrored to the phone, so nothing reaches it unscrubbed — and tool OUTPUT is
// never forwarded at all, because that is where file contents and credentials
// actually live.
var (
	// ansiPattern strips the colour codes the runner's tools emit.
	ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	// secretPattern drops the value of anything named like a credential,
	// keeping the label so the line still reads. The optional "Bearer " is
	// swallowed too: "Authorization: Bearer <jwt>" must not stop at the scheme
	// and leave the token behind.
	secretPattern = regexp.MustCompile(`(?i)(token|secret|key|password|passwd|bearer|authorization|api[_-]?key)\s*[=:]\s*(bearer\s+)?\S+`)
	// bearerPattern catches the scheme used without a header name.
	bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	// blobPattern drops long opaque runs: base64, hex digests, bare tokens.
	// "/" is deliberately outside the class so an ordinary path is not eaten.
	blobPattern = regexp.MustCompile(`[A-Za-z0-9+=_-]{24,}`)
)

// redactedMarker is what a scrubbed value reads as.
const redactedMarker = "<redacted>"

// scrubDetail turns a raw argument string into something safe to publish: ANSI
// stripped, credentials and opaque blobs removed, whitespace collapsed, capped.
func scrubDetail(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	s = secretPattern.ReplaceAllString(s, "$1="+redactedMarker)
	s = bearerPattern.ReplaceAllString(s, "bearer "+redactedMarker)
	s = blobPattern.ReplaceAllString(s, redactedMarker)
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > progressDetailRunes {
		s = strings.TrimRight(tg.TruncateRunes(s, progressDetailRunes-1), " ") + "…"
	}
	return s
}

// detailKeys are the rawInput fields worth showing, in order of preference.
// Anything not on this list is ignored: an unknown tool contributes its name
// and nothing else, which is the safe default.
var detailKeys = []string{"command", "file_path", "path", "pattern", "query", "url", "target_file", "description"}

// detailFrom picks a one-line summary out of a tool's raw input.
func detailFrom(raw map[string]json.RawMessage) string {
	for _, key := range detailKeys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil || strings.TrimSpace(s) == "" {
			continue
		}
		return scrubDetail(s)
	}
	return ""
}

// streamLine is the subset of the runner's NDJSON schema this code reads.
// Everything else on the line — tool output, usage counters, the usage
// signature blob — is dropped on the floor by not being declared here.
type streamLine struct {
	Type string `json:"type"`
	// Data carries a text or thought delta.
	Data string `json:"data"`
	// Tool call fields.
	ToolCallID string                     `json:"toolCallId"`
	ToolName   string                     `json:"toolName"`
	Title      string                     `json:"title"`
	Status     string                     `json:"status"`
	RawInput   map[string]json.RawMessage `json:"rawInput"`
	RawOutput  *struct {
		ExitCode *int `json:"exit_code"`
	} `json:"rawOutput"`
}

// knownStreamTypes is how the parser recognises the runner's schema. Seeing one
// of these is what licenses reconstructing the answer from the stream; a stdout
// with none of them is treated as plain output instead, so an upgrade that
// changes the format loses the progress display, never the answer.
var knownStreamTypes = map[string]bool{
	"text": true, "thought": true, "tool_call": true, "tool_call_update": true,
	"usage": true, "end": true, "available_commands": true, "error": true,
}

// parseStreamLine decodes one NDJSON line. It returns the text delta to append
// to the answer, an optional progress event, and whether the line was
// recognised as the runner's schema at all.
func parseStreamLine(line []byte) (text string, ev *ProgressEvent, known bool) {
	var parsed streamLine
	if err := json.Unmarshal(line, &parsed); err != nil {
		return "", nil, false
	}
	if !knownStreamTypes[parsed.Type] {
		return "", nil, false
	}

	switch parsed.Type {
	case "text":
		return parsed.Data, nil, true

	case "thought":
		// One marker per run is enough; the caller dedupes on Kind.
		return "", &ProgressEvent{Kind: ProgressThinking}, true

	case "tool_call":
		return "", &ProgressEvent{
			Kind:   ProgressToolStart,
			Tool:   toolNameOf(parsed),
			Detail: detailFrom(parsed.RawInput),
			Status: ProgressRunning,
			CallID: parsed.ToolCallID,
		}, true

	case "tool_call_update":
		// Only the terminal update is interesting. "in_progress" and the null
		// status are chatter, and the update's content is the tool's OUTPUT,
		// which must never be forwarded.
		status := strings.ToLower(strings.TrimSpace(parsed.Status))
		if status != "completed" && status != "failed" && status != "error" {
			return "", nil, true
		}
		outcome := ProgressCompleted
		if status != "completed" {
			outcome = ProgressFailed
		}
		if parsed.RawOutput != nil && parsed.RawOutput.ExitCode != nil && *parsed.RawOutput.ExitCode != 0 {
			outcome = ProgressFailed
		}
		return "", &ProgressEvent{
			Kind:   ProgressToolEnd,
			Tool:   toolNameOf(parsed),
			Status: outcome,
			CallID: parsed.ToolCallID,
		}, true

	case "end":
		return "", &ProgressEvent{Kind: ProgressEnd}, true
	}
	return "", nil, true
}

// toolNameOf prefers the explicit tool name and falls back to the title.
func toolNameOf(parsed streamLine) string {
	if name := strings.TrimSpace(parsed.ToolName); name != "" {
		return name
	}
	return strings.TrimSpace(parsed.Title)
}

// toolLabels shorten the runner's tool names for a phone-width line.
var toolLabels = map[string]string{
	"run_terminal_command":           "bash",
	"read_file":                      "read",
	"write":                          "write",
	"search_replace":                 "edit",
	"list_dir":                       "ls",
	"grep":                           "grep",
	"todo_write":                     "todo",
	"spawn_subagent":                 "agent",
	"web_search":                     "web",
	"web_fetch":                      "fetch",
	"get_command_or_subagent_output": "output",
}

func toolLabel(name string) string {
	if label, ok := toolLabels[name]; ok {
		return label
	}
	if name == "" {
		return "tool"
	}
	return name
}

// progressRing is a run's bounded event history: the source for the Telegram
// message and for the replay an app gets when it connects mid-run.
type progressRing struct {
	mu     sync.Mutex
	events []ProgressEvent
	seq    int
	// sawThinking dedupes the reasoning marker, which the runner emits once per
	// token.
	sawThinking bool
}

// add records an event and returns it with its sequence number filled in. It
// reports false when the event was dropped as a duplicate.
func (r *progressRing) add(ev ProgressEvent) (ProgressEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.Kind == ProgressThinking {
		if r.sawThinking {
			return ev, false
		}
		r.sawThinking = true
	}
	r.seq++
	ev.Seq = r.seq
	r.appendLocked(ev)
	return ev, true
}

// store records an event that already carries its sequence number, which is
// what the Telegram display and the app replay both consume.
func (r *progressRing) store(ev ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendLocked(ev)
}

func (r *progressRing) appendLocked(ev ProgressEvent) {
	r.events = append(r.events, ev)
	if len(r.events) > progressRingSize {
		r.events = append([]ProgressEvent(nil), r.events[len(r.events)-progressRingSize:]...)
	}
}

// snapshot copies the current buffer.
func (r *progressRing) snapshot() []ProgressEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProgressEvent(nil), r.events...)
}

// progressStep is one tool call as the Telegram message shows it.
type progressStep struct {
	Tool   string
	Detail string
	Status string
}

// progressStepsOf folds a run's events into one line per tool call, in the
// order the calls started. A tool_end updates its step rather than adding one.
func progressStepsOf(events []ProgressEvent) []progressStep {
	var steps []progressStep
	index := map[string]int{}
	for _, ev := range events {
		switch ev.Kind {
		case ProgressToolStart:
			index[ev.CallID] = len(steps)
			steps = append(steps, progressStep{Tool: ev.Tool, Detail: ev.Detail, Status: ev.Status})
		case ProgressToolEnd:
			at, ok := index[ev.CallID]
			if !ok {
				// The start aged out of the ring; show the end on its own.
				steps = append(steps, progressStep{Tool: ev.Tool, Status: ev.Status})
				continue
			}
			steps[at].Status = ev.Status
			if steps[at].Tool == "" {
				steps[at].Tool = ev.Tool
			}
		}
	}
	return steps
}

// stepGlyph is the leading icon for a step's status.
func stepGlyph(status string) string {
	switch status {
	case ProgressCompleted:
		return "✅"
	case ProgressFailed:
		return "❌"
	default:
		return "⚙️"
	}
}

// renderProgress builds the body of the single progress message: an optional
// header (a background task's identity), the last few steps, and the elapsed
// time. It returns "" when there is nothing worth showing yet.
func renderProgress(header string, events []ProgressEvent, elapsed time.Duration) string {
	steps := progressStepsOf(events)
	if len(steps) > progressSteps {
		steps = steps[len(steps)-progressSteps:]
	}

	var lines []string
	if header = strings.TrimSpace(header); header != "" {
		lines = append(lines, header)
	}
	for _, step := range steps {
		line := stepGlyph(step.Status) + " " + toolLabel(step.Tool)
		if step.Detail != "" {
			line += ": " + step.Detail
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	lines = append(lines, "⏱ "+formatElapsed(elapsed))
	return strings.Join(lines, "\n")
}

// formatElapsed renders a duration the way a person reads a wait.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Round(time.Second).Seconds())
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm %02ds", total/60, total%60)
}
