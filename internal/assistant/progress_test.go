package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// streamFixture is a real capture of `grok --output-format streaming-json`
// (grok 1.0.13) answering "list the files in this directory with ls -la, then
// count the lines of notes.txt, then say done" in a throwaway session, next to
// the answer the same run would have printed with --output-format plain.
func streamFixture(t *testing.T) (ndjson []byte, answer string) {
	t.Helper()
	ndjson, err := os.ReadFile(filepath.Join("testdata", "grok-streaming.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "grok-streaming.answer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return ndjson, strings.TrimSpace(string(raw))
}

// TestStreamRebuildsTheAnswerFromTheDeltas is the regression test for the
// riskiest part of the change: the reply is no longer stdout, it is the
// concatenation of the stream's text deltas. If this breaks, every answer
// breaks.
func TestStreamRebuildsTheAnswerFromTheDeltas(t *testing.T) {
	ndjson, answer := streamFixture(t)

	var got []ProgressEvent
	ring := &progressRing{}
	res := scanStream(bytes.NewReader(ndjson), ring, func(ev ProgressEvent) { got = append(got, ev) }, time.Now())

	if !res.known {
		t.Fatal("the fixture is the runner's own format; it must be recognised as such")
	}
	if res.answer() != answer {
		t.Fatalf("the rebuilt answer does not match the plain one.\n got: %q\nwant: %q", res.answer(), answer)
	}

	starts, ends, thinking := 0, 0, 0
	for _, ev := range got {
		switch ev.Kind {
		case ProgressToolStart:
			starts++
		case ProgressToolEnd:
			ends++
		case ProgressThinking:
			thinking++
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("the run made two tool calls; got %d starts and %d ends: %+v", starts, ends, got)
	}
	if thinking != 1 {
		t.Fatalf("the reasoning marker is emitted once per run, not once per token; got %d", thinking)
	}
	if len(ring.snapshot()) != len(got) {
		t.Fatalf("every published event must also be buffered for replay: %d vs %d", len(ring.snapshot()), len(got))
	}

	// The command lines are what the operator sees; the tool OUTPUT never is.
	details := map[string]bool{}
	for _, ev := range got {
		if ev.Detail != "" {
			details[ev.Detail] = true
		}
	}
	for _, want := range []string{"ls -la", "wc -l notes.txt"} {
		if !details[want] {
			t.Fatalf("expected the redacted command %q among the details: %v", want, details)
		}
	}
	for _, ev := range got {
		for _, leak := range []string{"drwxr", "total 48", "3 notes.txt", "\x1b["} {
			if strings.Contains(ev.Detail, leak) {
				t.Fatalf("tool output leaked into a progress detail (%q): %+v", leak, ev)
			}
		}
	}

	// The usage signature is a large opaque blob with no business leaving the
	// process; it is dropped by never being decoded.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("signature")) {
		t.Fatalf("the usage signature must never reach a progress event: %s", encoded)
	}
}

// TestStreamFallsBackWhenTheSchemaIsUnknown is the other half of the promise: a
// runner upgrade that changes the format costs the progress display, never the
// answer.
func TestStreamFallsBackWhenTheSchemaIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, want string
	}{
		{"plain text", "here is the answer\nsecond line", "here is the answer\nsecond line"},
		{"not json at all", "{{{ not json", "{{{ not json"},
		{"json of another shape", `{"kind":"reply","body":"hola"}`, `{"kind":"reply","body":"hola"}`},
		{"ansi noise", "\x1b[1mbold answer\x1b[0m", "bold answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := scanStream(strings.NewReader(tc.stdout), &progressRing{}, nil, time.Now())
			if res.known {
				t.Fatalf("%q is not the streaming schema", tc.stdout)
			}
			if got := res.answer(); got != tc.want {
				t.Fatalf("the answer must survive verbatim: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStreamPrefersTheSchemaOverStrayLines covers the mixed case: a warning
// printed before the stream starts must not become the answer.
func TestStreamPrefersTheSchemaOverStrayLines(t *testing.T) {
	stdout := strings.Join([]string{
		"warning: something the runner printed first",
		`{"type":"text","data":"the real "}`,
		`{"type":"text","data":"answer"}`,
		`{"type":"end","stopReason":"end_turn"}`,
	}, "\n")
	res := scanStream(strings.NewReader(stdout), &progressRing{}, nil, time.Now())
	if !res.known || res.answer() != "the real answer" {
		t.Fatalf("known=%t answer=%q", res.known, res.answer())
	}
}

// TestStreamDrainsAMonsterLine: a tool dumping a whole file into its update
// must not stop the reader, or the child blocks on a full pipe and the run
// never ends.
func TestStreamDrainsAMonsterLine(t *testing.T) {
	huge := strings.Repeat("x", streamLineLimit+1024)
	stdout := strings.Join([]string{
		`{"type":"text","data":"before "}`,
		`{"type":"tool_call_update","status":"completed","content":"` + huge + `"}`,
		`{"type":"text","data":"after"}`,
	}, "\n")
	res := scanStream(strings.NewReader(stdout), &progressRing{}, nil, time.Now())
	if res.answer() != "before after" {
		t.Fatalf("the deltas either side of the monster line must survive: %q", res.answer())
	}
}

// TestScrubDetailRedacts pins the rule that keeps the operator's own secrets out
// of a message posted to Telegram and mirrored to his phone.
func TestScrubDetailRedacts(t *testing.T) {
	for _, tc := range []struct{ name, in, wantOut, wantGone string }{
		{"api key assignment", "export API_KEY=xai-abc123", "API_KEY=" + redactedMarker, "xai-abc123"},
		{"bearer header", `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9"`, redactedMarker, "eyJhbGciOiJIUzI1NiJ9"},
		{"password flag", "psql --password=hunter2forever", redactedMarker, "hunter2forever"},
		{"token colon", "token: 0123456789abcdef", redactedMarker, "0123456789abcdef"},
		{"long base64", "echo c2VjcmV0LXZhbHVlLXRoYXQtaXMtbG9uZw==", redactedMarker, "c2VjcmV0LXZhbHVl"},
		{"long hex digest", "sha 5f4dcc3b5aa765d61d8327deb882cf99aa", redactedMarker, "5f4dcc3b5aa765d61d8327deb882cf99aa"},
		{"ansi", "\x1b[31mls -la\x1b[0m", "ls -la", "\x1b["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubDetail(tc.in)
			if !strings.Contains(got, tc.wantOut) {
				t.Fatalf("scrubDetail(%q) = %q, expected it to contain %q", tc.in, got, tc.wantOut)
			}
			if strings.Contains(got, tc.wantGone) {
				t.Fatalf("scrubDetail(%q) = %q, still leaks %q", tc.in, got, tc.wantGone)
			}
		})
	}

	// Ordinary commands and paths survive: a scrubber that eats everything is
	// the same as having no progress display.
	for _, keep := range []string{"ls -la", "go test ./internal/assistant/...", "grep -n func Emit"} {
		if got := scrubDetail(keep); got != keep {
			t.Fatalf("scrubDetail(%q) = %q, an ordinary command must survive", keep, got)
		}
	}

	// Nothing past the cap can hide behind a long argument list.
	long := scrubDetail(strings.Repeat("ab ", 200))
	if len([]rune(long)) > progressDetailRunes {
		t.Fatalf("detail is capped at %d runes, got %d", progressDetailRunes, len([]rune(long)))
	}
}

// TestProgressNeverForwardsToolOutput is the rule stated as a test: a tool
// call's OUTPUT is where file contents and credentials actually live, so it is
// not merely redacted, it is never read.
func TestProgressNeverForwardsToolOutput(t *testing.T) {
	line := []byte(`{"type":"tool_call_update","toolCallId":"c1","status":"completed","toolName":"read_file",
	 "content":[{"type":"content","content":{"type":"text","text":"AWS_SECRET_ACCESS_KEY=very-secret"}}]}`)
	text, ev, known := parseStreamLine(line)
	if !known || text != "" || ev == nil {
		t.Fatalf("unexpected parse: text=%q ev=%+v known=%t", text, ev, known)
	}
	if ev.Detail != "" {
		t.Fatalf("a tool update must contribute no detail at all, got %q", ev.Detail)
	}
}

func TestParseStreamLineMarksAFailedTool(t *testing.T) {
	_, ev, _ := parseStreamLine([]byte(`{"type":"tool_call_update","toolCallId":"c1","status":"completed",
	 "toolName":"run_terminal_command","rawOutput":{"exit_code":2}}`))
	if ev == nil || ev.Status != ProgressFailed {
		t.Fatalf("a non-zero exit is a failed step, got %+v", ev)
	}
}

func TestRenderProgressShowsTheLastStepsAndElapsed(t *testing.T) {
	var events []ProgressEvent
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("c%d", i)
		events = append(events,
			ProgressEvent{Kind: ProgressToolStart, Tool: "run_terminal_command", Detail: fmt.Sprintf("step %d", i), Status: ProgressRunning, CallID: id},
			ProgressEvent{Kind: ProgressToolEnd, Tool: "run_terminal_command", Status: ProgressCompleted, CallID: id},
		)
	}
	events = append(events, ProgressEvent{Kind: ProgressToolStart, Tool: "grep", Detail: "func Emit", Status: ProgressRunning, CallID: "c7"})

	body := renderProgress("⏳ Tarea abc — algo", events, 83*time.Second)
	lines := strings.Split(body, "\n")
	// header + progressSteps steps + elapsed
	if len(lines) != progressSteps+2 {
		t.Fatalf("expected a header, %d steps and the elapsed line:\n%s", progressSteps, body)
	}
	if lines[0] != "⏳ Tarea abc — algo" {
		t.Fatalf("the header must stay pinned on top:\n%s", body)
	}
	if strings.Contains(body, "step 1") || strings.Contains(body, "step 3") {
		t.Fatalf("only the last %d steps are shown:\n%s", progressSteps, body)
	}
	if !strings.Contains(body, "✅ bash: step 6") {
		t.Fatalf("a finished step must be ticked and its tool name shortened:\n%s", body)
	}
	if !strings.Contains(body, "⚙️ grep: func Emit") {
		t.Fatalf("the step in flight must be shown as running:\n%s", body)
	}
	if lines[len(lines)-1] != "⏱ 1m 23s" {
		t.Fatalf("the elapsed line is wrong:\n%s", body)
	}

	// A run that has not called a tool yet has nothing to show, which is what
	// keeps a short answer from ever producing a message.
	if got := renderProgress("", nil, time.Second); got != "" {
		t.Fatalf("a run with no steps must render nothing, got %q", got)
	}
}

// TestAgentStartStreamsAndRebuilds runs the real Start path over a stub runner
// that replays the fixture, so the StdoutPipe wiring is covered end to end.
func TestAgentStartStreamsAndRebuilds(t *testing.T) {
	ndjson, answer := streamFixture(t)
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.ndjson")
	if err := os.WriteFile(fixture, ndjson, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-runner")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec cat "+fixture+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := DefaultOptions()
	opts.Agent.Command = script
	if got := outputFormatOf(opts.Agent.Args); got != OutputStreaming {
		t.Fatalf("progress defaults to on, so the runner must be asked for %s, got %q", OutputStreaming, got)
	}

	var steps []ProgressEvent
	agent := NewAgent(opts.Agent, dir, dir, 30*time.Second)
	res, err := agent.Run(context.Background(), RunSpec{
		Prompt: "hi", SessionID: "s1", Mode: RunNew,
		OnProgress: func(ev ProgressEvent) { steps = append(steps, ev) },
	})
	if err != nil {
		t.Fatalf("run failed: %v (stderr %q)", err, res.Stderr)
	}
	if res.Stdout != answer {
		t.Fatalf("Start must deliver the rebuilt answer, got %q", res.Stdout)
	}
	if len(steps) == 0 {
		t.Fatal("no progress reached the callback")
	}
}

// TestPlainOutputStillWorks keeps the escape hatch honest: turning progress off
// puts the runner back on --output-format plain and stdout back to verbatim.
func TestPlainOutputStillWorks(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-runner")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'the plain answer'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	off := false
	opts := Options{Agent: AgentOpts{Command: script, Progress: &off}}
	opts.applyDefaults()
	if got := outputFormatOf(opts.Agent.Args); got != OutputPlain {
		t.Fatalf("progress off must select %s, got %q", OutputPlain, got)
	}

	res, err := NewAgent(opts.Agent, dir, dir, 30*time.Second).Run(context.Background(),
		RunSpec{Prompt: "hi", SessionID: "s1", Mode: RunNew})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Stdout != "the plain answer" {
		t.Fatalf("plain stdout must pass through untouched, got %q", res.Stdout)
	}
}

// TestStoredPlainArgsAreUpgraded: an instance configured before this existed
// has "--output-format plain" saved in the panel. Leaving it would silently
// disable progress on exactly the machine that has been running longest.
func TestStoredPlainArgsAreUpgraded(t *testing.T) {
	o := Options{Agent: AgentOpts{
		Command: "grok",
		Args:    []string{"--single", promptPlaceholder, "--output-format", "plain", "--always-approve"},
	}}
	o.applyDefaults()
	if got := outputFormatOf(o.Agent.Args); got != OutputStreaming {
		t.Fatalf("a stored plain configuration must be upgraded, got %q (%v)", got, o.Agent.Args)
	}

	// The flag is added when it was never there at all.
	o = Options{Agent: AgentOpts{Command: "grok", Args: []string{"--single", promptPlaceholder}}}
	o.applyDefaults()
	if got := outputFormatOf(o.Agent.Args); got != OutputStreaming {
		t.Fatalf("the flag must be appended when missing, got %q (%v)", got, o.Agent.Args)
	}
}
