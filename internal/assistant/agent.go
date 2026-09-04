package assistant

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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

	mu  sync.Mutex
	res *AgentResult
	err error
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
	cmd := exec.Command(a.opts.Command, a.buildArgs(spec)...)
	cmd.Dir = a.workdir
	cmd.Env = a.childEnv()
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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

		waitErr := cmd.Wait()
		close(exited)

		res := &AgentResult{
			Stdout:   strings.TrimSpace(stdout.String()),
			Stderr:   strings.TrimSpace(stderr.String()),
			Duration: time.Since(start),
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
