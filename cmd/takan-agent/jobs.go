package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const maxTrackedJobs = 50

const promptPlaceholder = "{{prompt}}"

type jobMeta struct {
	JobID       string `json:"job_id"`
	Agent       string `json:"agent"` // runner id
	Command     string `json:"command,omitempty"`
	Prompt      string `json:"prompt"`
	Cwd         string `json:"cwd,omitempty"`
	ParentJobID string `json:"parent_job_id,omitempty"`
	Owner       string `json:"owner,omitempty"` // Grok Bot that launched the job
	Status      string `json:"status"`          // running | done | failed | cancelled
	PID         int    `json:"pid,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

type jobManager struct {
	mu   sync.Mutex
	root string
	// live PIDs for jobs started by this process
	cmds map[string]*exec.Cmd
	// notify is invoked (without holding mu) when a job reaches a terminal status.
	notify func(jobMeta)
}

func newJobManager() (*jobManager, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	root := filepath.Join(home, ".takan", "jobs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &jobManager{root: root, cmds: make(map[string]*exec.Cmd)}, nil
}

func (m *jobManager) jobDir(id string) string {
	return filepath.Join(m.root, id)
}

func (m *jobManager) setNotify(fn func(jobMeta)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

func (m *jobManager) emit(meta jobMeta) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		fn(meta)
	}
}

func validJobID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job_id required")
	}
	if strings.Contains(id, "/") || strings.Contains(id, "..") || strings.Contains(id, "\\") {
		return fmt.Errorf("invalid job_id")
	}
	return nil
}

func jobTerminal(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// start launches a shell command template with the prompt injected.
// commandTmpl may include {{prompt}}; otherwise the quoted prompt is appended.
func (m *jobManager) start(runnerID, commandTmpl, prompt, cwd, parentJobID, owner string) (jobMeta, error) {
	runnerID = strings.TrimSpace(runnerID)
	if runnerID == "" {
		runnerID = "custom"
	}
	commandTmpl = strings.TrimSpace(commandTmpl)
	prompt = strings.TrimSpace(prompt)
	if commandTmpl == "" {
		return jobMeta{}, fmt.Errorf("command required")
	}
	if prompt == "" {
		return jobMeta{}, fmt.Errorf("prompt required")
	}
	if cwd != "" {
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			return jobMeta{}, fmt.Errorf("cwd %q is not a directory", cwd)
		}
	}

	shellCmd := expandPromptTemplate(commandTmpl, prompt)

	id := uuid.NewString()
	dir := m.jobDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return jobMeta{}, err
	}

	logPath := filepath.Join(dir, "output.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return jobMeta{}, err
	}

	cmd := exec.Command("bash", "-lc", shellCmd)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach from agent lifetime: child keeps running if agent restarts mid-job.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = enrichedEnv()

	started := time.Now().UTC().Format(time.RFC3339)
	meta := jobMeta{
		JobID:       id,
		Agent:       runnerID,
		Command:     commandTmpl,
		Prompt:      prompt,
		Cwd:         cwd,
		ParentJobID: strings.TrimSpace(parentJobID),
		Owner:       strings.TrimSpace(owner),
		Status:      "running",
		StartedAt:   started,
	}
	if err := writeMeta(dir, meta); err != nil {
		_ = logFile.Close()
		return jobMeta{}, err
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		meta.Status = "failed"
		meta.Error = err.Error()
		meta.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		_ = writeMeta(dir, meta)
		return meta, fmt.Errorf("start: %w", err)
	}
	meta.PID = cmd.Process.Pid
	_ = writeMeta(dir, meta)

	m.mu.Lock()
	m.cmds[id] = cmd
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		exitCode := 0
		status := "done"
		errMsg := ""
		if err != nil {
			status = "failed"
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
				errMsg = err.Error()
			}
		}
		m.mu.Lock()
		meta2, _ := readMeta(dir)
		if meta2.JobID == "" {
			meta2 = meta
		}
		if meta2.Status != "cancelled" {
			meta2.Status = status
			meta2.Error = errMsg
		}
		meta2.ExitCode = exitCode
		if meta2.FinishedAt == "" {
			meta2.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		}
		_ = writeMeta(dir, meta2)
		delete(m.cmds, id)
		m.mu.Unlock()
		m.emit(meta2)
		m.pruneOld()
	}()

	return meta, nil
}

func expandPromptTemplate(tmpl, prompt string) string {
	q := shellQuote(prompt)
	if strings.Contains(tmpl, promptPlaceholder) {
		return strings.ReplaceAll(tmpl, promptPlaceholder, q)
	}
	// No placeholder: append quoted prompt as final argument.
	return strings.TrimSpace(tmpl) + " " + q
}

// shellQuote wraps s in single quotes for bash -lc (safe for arbitrary text).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (m *jobManager) loadMeta(jobID string) (jobMeta, error) {
	jobID = strings.TrimSpace(jobID)
	if err := validJobID(jobID); err != nil {
		return jobMeta{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reapLocked(jobID)
}

// reapLocked refreshes a dead "running" job. Caller must hold m.mu.
func (m *jobManager) reapLocked(jobID string) (jobMeta, error) {
	dir := m.jobDir(jobID)
	meta, err := readMeta(dir)
	if err != nil {
		return jobMeta{}, fmt.Errorf("unknown job %q", jobID)
	}
	if meta.Status == "running" && meta.PID > 0 && !pidAlive(meta.PID) {
		meta.Status = "done"
		if meta.FinishedAt == "" {
			meta.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		}
		_ = writeMeta(dir, meta)
	}
	return meta, nil
}

func (m *jobManager) status(jobID string, tailBytes int) (jobMeta, string, error) {
	meta, err := m.loadMeta(jobID)
	if err != nil {
		return jobMeta{}, "", err
	}
	if tailBytes <= 0 {
		tailBytes = 12_000
	}
	out := tailFile(filepath.Join(m.jobDir(meta.JobID), "output.log"), tailBytes)
	return meta, out, nil
}

const maxLogBytes = 1_048_576

// readLog returns the transcript from the start of output.log (not a tail).
func (m *jobManager) readLog(jobID string, maxBytes int) (jobMeta, string, int, bool, error) {
	meta, err := m.loadMeta(jobID)
	if err != nil {
		return jobMeta{}, "", 0, false, err
	}
	if maxBytes <= 0 {
		maxBytes = 500_000
	}
	if maxBytes > maxLogBytes {
		maxBytes = maxLogBytes
	}
	b, err := os.ReadFile(filepath.Join(m.jobDir(meta.JobID), "output.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return meta, "", 0, false, nil
		}
		return meta, "", 0, false, err
	}
	total := len(b)
	truncated := false
	if total > maxBytes {
		b = b[:maxBytes]
		truncated = true
	}
	return meta, string(b), total, truncated, nil
}

// cancel persists status cancelled first, then kills the process group so
// Wait() cannot overwrite the terminal state with failed.
func (m *jobManager) cancel(jobID string) (jobMeta, error) {
	jobID = strings.TrimSpace(jobID)
	if err := validJobID(jobID); err != nil {
		return jobMeta{}, err
	}

	m.mu.Lock()
	meta, err := m.reapLocked(jobID)
	if err != nil {
		m.mu.Unlock()
		return jobMeta{}, err
	}
	if jobTerminal(meta.Status) {
		m.mu.Unlock()
		return meta, nil
	}
	pid := meta.PID
	if cmd := m.cmds[meta.JobID]; cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	meta.Status = "cancelled"
	meta.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeMeta(m.jobDir(meta.JobID), meta); err != nil {
		m.mu.Unlock()
		return meta, err
	}
	m.mu.Unlock()

	if pid > 0 {
		killProcessGroup(pid)
	}
	return meta, nil
}

func killProcessGroupNow(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	killProcessGroupNow(pid)
}

func (m *jobManager) list() []jobMeta {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil
	}
	var out []jobMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m.mu.Lock()
		meta, err := m.reapLocked(e.Name())
		m.mu.Unlock()
		if err != nil {
			continue
		}
		if len(meta.Prompt) > 200 {
			meta.Prompt = meta.Prompt[:200] + "…"
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt > out[j].StartedAt
	})
	if len(out) > maxTrackedJobs {
		out = out[:maxTrackedJobs]
	}
	return out
}

func (m *jobManager) pruneOld() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	type item struct {
		name string
		at   string
	}
	var all []item
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := readMeta(m.jobDir(e.Name()))
		if err != nil {
			all = append(all, item{name: e.Name(), at: ""})
			continue
		}
		all = append(all, item{name: e.Name(), at: meta.StartedAt})
	}
	if len(all) <= maxTrackedJobs {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at > all[j].at })
	for _, it := range all[maxTrackedJobs:] {
		meta, _ := readMeta(m.jobDir(it.name))
		if meta.Status == "running" {
			continue
		}
		_ = os.RemoveAll(m.jobDir(it.name))
	}
}

func writeMeta(dir string, meta jobMeta) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600)
}

func readMeta(dir string) (jobMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return jobMeta{}, err
	}
	var m jobMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return jobMeta{}, err
	}
	return m, nil
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func enrichedEnv() []string {
	env := os.Environ()
	home, _ := os.UserHomeDir()
	extra := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".grok", "bin"),
		"/usr/local/bin",
		"/opt/homebrew/bin",
	}
	path := os.Getenv("PATH")
	for _, d := range extra {
		if d != "" && !strings.Contains(path, d) {
			path = d + string(os.PathListSeparator) + path
		}
	}
	out := make([]string, 0, len(env)+1)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			out = append(out, "PATH="+path)
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		out = append(out, "PATH="+path)
	}
	return out
}
