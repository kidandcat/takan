package assistant

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long a cancelled agent has to exit after SIGTERM before the
// whole process group is SIGKILLed.
const killGrace = 10 * time.Second

const (
	// streamLineLimit bounds one NDJSON line. A tool that dumps a whole file
	// into its update produces a line nothing needs to read; past this it is
	// discarded, but the pipe keeps draining so the child never blocks.
	streamLineLimit = 8 << 20
	// streamRawLimit bounds the verbatim copy kept for the fallback below.
	streamRawLimit = 4 << 20
)

// Run modes. Sessions are addressed by explicit id rather than "continue the
// most recent session in this directory", which silently breaks when any other
// run touches the same working directory.
const (
	// RunNew starts a brand new session with a generated id.
	RunNew = "new"
	// RunResume continues an existing session by id.
	RunResume = "resume"
	// RunFork branches an existing session into a new id, which is how the
	// conversation continues while a promoted run still owns the parent.
	RunFork = "fork"
)

// childEnvAllow is the entire environment the CLI agent inherits.
//
// The assistant lives in the hub process, whose environment holds the session
// key, the Resend key, the AWS backup credentials and the app token. Inheriting
// os.Environ() would hand every one of them to a subprocess that runs arbitrary
// model-authored commands, so the child gets an allowlist instead.
var childEnvAllow = []string{
	"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TERM", "TZ",
	"TMPDIR", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
}

// RunSpec describes one agent invocation.
type RunSpec struct {
	Prompt string
	// SessionID is the session being created (new/fork) or resumed.
	SessionID string
	// ParentSession is the session forked from, when Mode is RunFork.
	ParentSession string
	Mode          string
	// OnProgress receives redacted progress events as the run streams them. It
	// is called from the stdout reader, so it must not block: whatever it feeds
	// has to be buffered, or the child stalls on a full pipe.
	OnProgress func(ProgressEvent)
}

// AgentResult is the outcome of one CLI agent run.
type AgentResult struct {
	Stdout   string
	Stderr   string
	Duration time.Duration
	TimedOut bool
	// Cancelled is true when the run was stopped on purpose (/cancel, shutdown).
	Cancelled bool
	// RateLimited is true when the runner refused because of an upstream quota
	// or overload. /usage counts these.
	RateLimited bool
}

// Agent runs the configured CLI coding agent to turn a prompt into a reply.
type Agent struct {
	opts AgentOpts
	// workdir overrides opts.Workdir for tasks and routines, which each run in
	// their own directory.
	workdir string
	// home is the HOME handed to the child, so the agent finds its credentials
	// even when the hub runs as a different user.
	home string
	// timeout bounds a single run.
	timeout time.Duration
}

// NewAgent builds a runner. workdir and home are resolved by the caller.
func NewAgent(opts AgentOpts, workdir, home string, timeout time.Duration) *Agent {
	return &Agent{opts: opts, workdir: workdir, home: home, timeout: timeout}
}

// RunHandle is a started agent process. It exists so a run can outlive the
// conversation turn that began it: when a run passes the soft budget, its
// handle is transferred to the task manager and the chat is freed.
type RunHandle struct {
	spec      RunSpec
	startedAt time.Time
	pid       int
	done      chan struct{}
	cancel    context.CancelFunc
	// progress is the run's bounded step history, filled by the stdout reader
	// and replayed to an app that connects mid-run.
	progress *progressRing

	mu  sync.Mutex
	res *AgentResult
	err error
}

// Progress is the run's step history so far.
func (h *RunHandle) Progress() []ProgressEvent {
	if h.progress == nil {
		return nil
	}
	return h.progress.snapshot()
}

// Done is closed when the run finishes.
func (h *RunHandle) Done() <-chan struct{} { return h.done }

// Result returns the outcome; only valid once Done is closed.
func (h *RunHandle) Result() (*AgentResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.res, h.err
}

// Cancel stops the run and its whole process group.
func (h *RunHandle) Cancel() { h.cancel() }

// PID is the child's process id.
func (h *RunHandle) PID() int { return h.pid }

// StartedAt is when the process was launched.
func (h *RunHandle) StartedAt() time.Time { return h.startedAt }

// SessionID is the session this run writes to.
func (h *RunHandle) SessionID() string { return h.spec.SessionID }

// Prompt is the prompt the run was given.
func (h *RunHandle) Prompt() string { return h.spec.Prompt }

// Finished reports whether the run is already complete.
func (h *RunHandle) Finished() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// buildArgs renders the configured argument templates for a run.
func (a *Agent) buildArgs(spec RunSpec) []string {
	replace := func(s string) string {
		s = strings.ReplaceAll(s, promptPlaceholder, spec.Prompt)
		s = strings.ReplaceAll(s, sessionPlaceholder, spec.SessionID)
		s = strings.ReplaceAll(s, parentPlaceholder, spec.ParentSession)
		return s
	}

	var extra []string
	switch spec.Mode {
	case RunFork:
		extra = a.opts.ForkArgs
	case RunResume:
		extra = a.opts.ResumeArgs
	default:
		extra = a.opts.NewSessionArgs
	}

	args := make([]string, 0, len(a.opts.Args)+len(extra))
	for _, arg := range a.opts.Args {
		args = append(args, replace(arg))
	}
	for _, arg := range extra {
		args = append(args, replace(arg))
	}
	return args
}

// Start launches the agent and returns immediately with a handle.
func (a *Agent) Start(ctx context.Context, spec RunSpec) (*RunHandle, error) {
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)

	if err := os.MkdirAll(a.workdir, 0o755); err != nil {
		cancel()
		return nil, fmt.Errorf("create agent workdir: %w", err)
	}

	// The child is put in its own process group so that cancelling a run kills
	// the whole tree: the agent spawns tools and subagents that would otherwise
	// survive a kill aimed at the leader alone.
	args := a.buildArgs(spec)
	cmd := exec.Command(a.opts.Command, args...)
	cmd.Dir = a.workdir
	cmd.Env = a.childEnv()
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stderr = &stderr

	// With the streaming format stdout is NDJSON, read line by line so progress
	// is live; the answer is then reconstructed from its text deltas. With any
	// other format stdout is the answer itself and is buffered as before.
	streaming := outputFormatOf(args) == OutputStreaming
	var pipe io.ReadCloser
	if streaming {
		var err error
		if pipe, err = cmd.StdoutPipe(); err != nil {
			cancel()
			return nil, fmt.Errorf("agent stdout pipe: %w", err)
		}
	} else {
		cmd.Stdout = &stdout
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("agent failed to start: %w", err)
	}

	handle := &RunHandle{
		spec:      spec,
		startedAt: start,
		pid:       cmd.Process.Pid,
		done:      make(chan struct{}),
		cancel:    cancel,
		progress:  &progressRing{},
	}

	// The reader must finish before Wait closes the pipe under it.
	var stream streamResult
	scanDone := make(chan struct{})
	if streaming {
		go func() {
			defer close(scanDone)
			stream = scanStream(pipe, handle.progress, spec.OnProgress, start)
		}()
	} else {
		close(scanDone)
	}

	go func() {
		defer cancel()
		defer close(handle.done)

		// Terminate the process group when cancelled or timed out.
		exited := make(chan struct{})
		go func() {
			select {
			case <-exited:
				return
			case <-runCtx.Done():
			}
			killGroup(cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(killGrace):
				killGroup(cmd.Process.Pid, syscall.SIGKILL)
			}
		}()

		<-scanDone
		waitErr := cmd.Wait()
		close(exited)

		res := &AgentResult{
			Stdout:   strings.TrimSpace(stdout.String()),
			Stderr:   strings.TrimSpace(stderr.String()),
			Duration: time.Since(start),
		}
		if streaming {
			res.Stdout = stream.answer()
		}
		res.RateLimited = looksRateLimited(res.Stderr)
		var err error
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			res.TimedOut = true
			err = fmt.Errorf("agent timed out after %s", a.timeout)
		case errors.Is(runCtx.Err(), context.Canceled):
			res.Cancelled = true
			err = context.Canceled
		case waitErr != nil:
			err = fmt.Errorf("agent exited with error: %w", waitErr)
		case res.Stdout == "":
			err = errors.New("agent produced no output")
		}

		handle.mu.Lock()
		handle.res, handle.err = res, err
		handle.mu.Unlock()
	}()

	return handle, nil
}

// Run starts the agent and blocks until it finishes.
func (a *Agent) Run(ctx context.Context, spec RunSpec) (*AgentResult, error) {
	handle, err := a.Start(ctx, spec)
	if err != nil {
		return &AgentResult{}, err
	}
	<-handle.Done()
	return handle.Result()
}

// streamResult is what reading a run's NDJSON stdout produced.
type streamResult struct {
	// text is the answer, rebuilt by concatenating the runner's text deltas.
	text string
	// raw is a capped verbatim copy of the lines that did NOT parse as the
	// known schema. It is the fallback answer.
	raw string
	// known is true once at least one line matched the runner's schema.
	known bool
	// lines and events are counted for the log line only.
	lines, events int
}

// answer is the reply to deliver.
//
// The reply now depends on parsing a format the runner does not promise to keep
// stable, so the failure mode is chosen deliberately: if nothing on stdout
// looked like the streaming schema, the raw output is delivered verbatim. A
// runner upgrade that changes the format therefore costs the progress display,
// never the answer.
func (s streamResult) answer() string {
	if s.known {
		return strings.TrimSpace(stripANSI(s.text))
	}
	if raw := strings.TrimSpace(stripANSI(s.raw)); raw != "" {
		log.Printf("agent: stdout did not match the %s schema (%d lines); delivering it verbatim", OutputStreaming, s.lines)
		return raw
	}
	return ""
}

// stripANSI removes terminal colour codes, which reach stdout whenever a tool
// writes through a pty.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiPattern.ReplaceAllString(s, "")
}

// scanStream reads the runner's NDJSON stdout to EOF, rebuilding the answer and
// publishing progress as it goes. It always drains the pipe: returning early
// would leave the child blocked on a full buffer.
func scanStream(r io.Reader, ring *progressRing, onProgress func(ProgressEvent), start time.Time) streamResult {
	var out streamResult
	var text, raw strings.Builder

	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		line, truncated, err := readLimitedLine(reader, streamLineLimit)
		if len(bytes.TrimSpace(line)) > 0 && !truncated {
			out.lines++
			delta, ev, known := parseStreamLine(line)
			switch {
			case known:
				out.known = true
			case raw.Len() < streamRawLimit:
				// line still carries its newline; the fallback answer is the
				// stray output exactly as the runner printed it.
				raw.Write(line)
			}
			if delta != "" {
				text.WriteString(delta)
			}
			if ev != nil {
				ev.Elapsed = time.Since(start)
				if stored, ok := ring.add(*ev); ok {
					out.events++
					if onProgress != nil {
						onProgress(stored)
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("agent: reading the progress stream stopped early: %v", err)
			}
			break
		}
	}

	out.text, out.raw = text.String(), raw.String()
	return out
}

// readLimitedLine reads one newline-terminated line, discarding anything past
// limit bytes and reporting that it did. The overflow is still consumed, which
// is the point: the reader may never stop draining the pipe.
func readLimitedLine(r *bufio.Reader, limit int) (line []byte, truncated bool, err error) {
	for {
		chunk, readErr := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if len(line)+len(chunk) <= limit {
				line = append(line, chunk...)
			} else {
				truncated = true
			}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		return line, truncated, readErr
	}
}

// outputFormatOf reads the --output-format value out of a rendered argument
// list. The runner's format and this code's parser must agree, so the args are
// the single source of truth for both.
func outputFormatOf(args []string) string {
	for i, arg := range args {
		if arg == outputFormatFlag && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, outputFormatFlag+"="); ok {
			return value
		}
	}
	return ""
}

// killGroup signals the whole process group led by pid.
func killGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("failed to send %v to process group %d: %v", sig, pid, err)
	}
}

// rateLimitMarkers are what an upstream quota or overload looks like on stderr.
var rateLimitMarkers = []string{"rate limit", "rate_limit", "429", "503", "529", "too many requests", "overloaded"}

// looksRateLimited reports whether a failed run was refused upstream rather
// than broken locally. It only feeds the /usage counter, so a false positive
// costs nothing.
func looksRateLimited(stderr string) bool {
	if stderr == "" {
		return false
	}
	lower := strings.ToLower(stderr)
	for _, marker := range rateLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// childEnv builds the agent's environment from an allowlist, then applies the
// configured additions and removals. See childEnvAllow for why.
func (a *Agent) childEnv() []string {
	drop := make(map[string]bool, len(a.opts.UnsetEnv))
	for _, name := range a.opts.UnsetEnv {
		drop[strings.TrimSpace(name)] = true
	}

	env := make([]string, 0, len(childEnvAllow)+len(a.opts.Env))
	seen := map[string]bool{}
	for _, name := range childEnvAllow {
		if drop[name] {
			continue
		}
		value, ok := os.LookupEnv(name)
		if name == "HOME" && a.home != "" {
			value, ok = a.home, true
		}
		if !ok {
			continue
		}
		seen[name] = true
		env = append(env, name+"="+value)
	}
	// HOME is the one variable the agent cannot do without: it is where its
	// credentials live. Supply it even when the hub process has none.
	if !seen["HOME"] && !drop["HOME"] && a.home != "" {
		env = append(env, "HOME="+a.home)
	}
	for name, value := range a.opts.Env {
		if drop[name] {
			continue
		}
		env = append(env, name+"="+value)
	}
	return env
}

// Describe renders the effective command line for logging.
func (a *Agent) Describe(mode string) string {
	parts := append([]string{a.opts.Command}, a.buildArgs(RunSpec{
		Prompt: promptPlaceholder, SessionID: sessionPlaceholder, ParentSession: parentPlaceholder, Mode: mode,
	})...)
	return strings.Join(parts, " ")
}

// newSessionID returns a random UUIDv4, the identifier format the runner expects.
func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// A time-derived fallback still yields a unique, well-formed id.
		return fmt.Sprintf("%08x-0000-4000-8000-%012x", time.Now().Unix(), time.Now().UnixNano()&0xffffffffffff)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
