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

type jobMeta struct {
	JobID      string `json:"job_id"`
	Agent      string `json:"agent"`
	Prompt     string `json:"prompt"`
	Cwd        string `json:"cwd,omitempty"`
	Status     string `json:"status"` // running | done | failed
	PID        int    `json:"pid,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type jobManager struct {
	mu   sync.Mutex
	root string
	// live PIDs for jobs started by this process
	cmds map[string]*exec.Cmd
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

func (m *jobManager) start(agent, prompt, cwd string, autoApprove bool) (jobMeta, error) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent != "claude" && agent != "grok" {
		return jobMeta{}, fmt.Errorf(`agent must be "claude" or "grok"`)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return jobMeta{}, fmt.Errorf("prompt required")
	}
	if cwd != "" {
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			return jobMeta{}, fmt.Errorf("cwd %q is not a directory", cwd)
		}
	}

	bin, err := resolveAIBinary(agent)
	if err != nil {
		return jobMeta{}, err
	}

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

	args := buildAIArgs(agent, prompt, autoApprove)
	cmd := exec.Command(bin, args...)
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
		JobID:     id,
		Agent:     agent,
		Prompt:    prompt,
		Cwd:       cwd,
		Status:    "running",
		StartedAt: started,
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
		return meta, fmt.Errorf("start %s: %w", agent, err)
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
		// re-read meta in case something else touched it
		meta2, _ := readMeta(dir)
		if meta2.JobID == "" {
			meta2 = meta
		}
		meta2.Status = status
		meta2.ExitCode = exitCode
		meta2.Error = errMsg
		meta2.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		_ = writeMeta(dir, meta2)

		m.mu.Lock()
		delete(m.cmds, id)
		m.mu.Unlock()
		m.pruneOld()
	}()

	return meta, nil
}

func (m *jobManager) status(jobID string, tailBytes int) (jobMeta, string, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return jobMeta{}, "", fmt.Errorf("job_id required")
	}
	// prevent path traversal
	if strings.Contains(jobID, "/") || strings.Contains(jobID, "..") || strings.Contains(jobID, "\\") {
		return jobMeta{}, "", fmt.Errorf("invalid job_id")
	}
	dir := m.jobDir(jobID)
	meta, err := readMeta(dir)
	if err != nil {
		return jobMeta{}, "", fmt.Errorf("unknown job %q", jobID)
	}
	// refresh running status from OS if we still track the process, or by PID
	if meta.Status == "running" && meta.PID > 0 {
		if !pidAlive(meta.PID) {
			// process gone but Wait not observed (e.g. agent restarted)
			meta.Status = "done"
			if meta.FinishedAt == "" {
				meta.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			}
			// try to recover exit code is hard; leave 0 if clean death without wait
			_ = writeMeta(dir, meta)
		}
	}
	if tailBytes <= 0 {
		tailBytes = 12_000
	}
	out := tailFile(filepath.Join(dir, "output.log"), tailBytes)
	return meta, out, nil
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
		meta, err := readMeta(m.jobDir(e.Name()))
		if err != nil {
			continue
		}
		if meta.Status == "running" && meta.PID > 0 && !pidAlive(meta.PID) {
			meta.Status = "done"
			if meta.FinishedAt == "" {
				meta.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			}
			_ = writeMeta(m.jobDir(e.Name()), meta)
		}
		// trim prompt in list views
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
	// Signal 0 checks existence without killing.
	err := syscall.Kill(pid, 0)
	return err == nil
}

func buildAIArgs(agent, prompt string, autoApprove bool) []string {
	switch agent {
	case "claude":
		// -p/--print is a boolean flag; prompt is positional.
		args := []string{"-p"}
		if autoApprove {
			args = append(args, "--dangerously-skip-permissions")
		}
		args = append(args, prompt)
		return args
	case "grok":
		// -p/--single takes the prompt value.
		args := []string{}
		if autoApprove {
			args = append(args, "--always-approve")
		}
		args = append(args, "-p", prompt)
		return args
	default:
		return []string{"-p", prompt}
	}
}

func resolveAIBinary(agent string) (string, error) {
	// Prefer PATH, then common install locations.
	if p, err := exec.LookPath(agent); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	switch agent {
	case "claude":
		candidates = []string{
			filepath.Join(home, ".local", "bin", "claude"),
			"/usr/local/bin/claude",
			"/opt/homebrew/bin/claude",
		}
	case "grok":
		candidates = []string{
			filepath.Join(home, ".grok", "bin", "grok"),
			filepath.Join(home, ".local", "bin", "grok"),
			"/usr/local/bin/grok",
			"/opt/homebrew/bin/grok",
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s binary not found in PATH or common locations", agent)
}

func enrichedEnv() []string {
	env := os.Environ()
	home, _ := os.UserHomeDir()
	// Ensure typical user bin dirs are on PATH (launchd/systemd often have a minimal PATH).
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
	// Replace PATH in env
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
