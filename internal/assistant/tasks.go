package assistant

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/internal/tg"
)

// Task states.
const (
	TaskRunning  = "running"
	TaskDone     = "done"
	TaskFailed   = "failed"
	TaskKilled   = "killed"
	TaskTimeout  = "timeout"
	TaskOrphaned = "orphaned"
)

// taskOutputLimit is how much of a task's output is pushed to Telegram. The
// full output always stays in the task directory.
const taskOutputLimit = 3000

// Task is one long-running agent run, detached from the conversation.
type Task struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
	State  string `json:"state"`
	PID    int    `json:"pid,omitempty"`
	Dir    string `json:"dir"`
	// OutputPath is the file holding the complete stdout of the run.
	OutputPath string    `json:"output_path,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
	// SessionID is the agent session the task runs in.
	SessionID string `json:"session_id,omitempty"`
	// Promoted marks a task that began as a conversational reply and was moved
	// to the background because it crossed the soft budget.
	Promoted bool `json:"promoted,omitempty"`
	// ChatID is where the result is delivered; zero means the owner DM.
	ChatID int64 `json:"chat_id,omitempty"`
}

// Running reports whether the task is still executing.
func (t *Task) Running() bool { return t.State == TaskRunning }

// Duration is how long the task ran, or has been running.
func (t *Task) Duration() time.Duration {
	if t.FinishedAt.IsZero() {
		return time.Since(t.StartedAt)
	}
	return t.FinishedAt.Sub(t.StartedAt)
}

// Summary is the one-line form used by /tasks and atlas-task list.
func (t *Task) Summary() string {
	icon := map[string]string{
		TaskRunning: "⏳", TaskDone: "✅", TaskFailed: "❌",
		TaskKilled: "🛑", TaskTimeout: "⌛", TaskOrphaned: "⚠️",
	}[t.State]
	return fmt.Sprintf("%s %s [%s] %s (%s)", icon, t.ID, t.State, t.Title, t.Duration().Truncate(time.Second))
}

// TaskManager runs background tasks and reports their results.
type TaskManager struct {
	st      *store.Store
	userID  string
	opts    Options
	home    string
	workdir string
	// out is the assistant's single outbound choke point, so a task result
	// reaches the app history as well as Telegram.
	out Emitter
	// host builds a task's progress display. Nil disables it, which is what a
	// manager built without a bot gets.
	host progressHost
	// ownerChat is the default delivery target.
	ownerChat int64

	mu      sync.Mutex
	tasks   map[string]*Task
	cancels map[string]context.CancelFunc
	// progress holds each running task's single editable message.
	progress map[string]*progressTracker
	// ctx is the daemon lifetime; tasks are spawned from it.
	ctx context.Context
	// rateLimited counts runs refused upstream, for /usage.
	rateLimited []time.Time
}

// NewTaskManager loads persisted tasks from the database.
func NewTaskManager(ctx context.Context, st *store.Store, userID string, opts Options,
	workdir, home string, out Emitter, ownerChat int64) (*TaskManager, error) {
	m := &TaskManager{
		st: st, userID: userID, opts: opts, home: home, workdir: workdir,
		out: out, ownerChat: ownerChat,
		tasks: map[string]*Task{}, cancels: map[string]context.CancelFunc{},
		progress: map[string]*progressTracker{},
		ctx:      ctx,
	}
	rows, err := st.ListAssistantTasks(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t := taskFromRow(r)
		m.tasks[t.ID] = &t
	}
	return m, nil
}

func taskFromRow(r store.AssistantTask) Task {
	t := Task{
		ID: r.ID, Title: r.Title, Prompt: r.Prompt, State: r.State, PID: r.PID,
		Dir: r.Dir, OutputPath: r.OutputPath, StartedAt: r.StartedAt,
		Error: r.Error, SessionID: r.SessionID, Promoted: r.Promoted,
	}
	if r.FinishedAt != nil {
		t.FinishedAt = *r.FinishedAt
	}
	if r.ChatID != "" {
		if id, err := strconv.ParseInt(r.ChatID, 10, 64); err == nil {
			t.ChatID = id
		}
	}
	return t
}

func (m *TaskManager) rowOf(t *Task) store.AssistantTask {
	row := store.AssistantTask{
		ID: t.ID, Title: t.Title, Prompt: t.Prompt, State: t.State, PID: t.PID,
		SessionID: t.SessionID, Dir: t.Dir, OutputPath: t.OutputPath,
		Promoted: t.Promoted, StartedAt: t.StartedAt, Error: t.Error,
	}
	if t.ChatID != 0 {
		row.ChatID = strconv.FormatInt(t.ChatID, 10)
	}
	if !t.FinishedAt.IsZero() {
		finished := t.FinishedAt
		row.FinishedAt = &finished
	}
	return row
}

// progressHost builds the single editable message a run reports through. Only
// *Bot implements it; the task manager holds it so a background task gets the
// same one-message-per-run treatment as a conversational turn.
type progressHost interface {
	NewProgress(chatID int64, telegram bool, started time.Time) *progressTracker
}

// SetProgressHost wires the live progress display. Without it tasks still run;
// they just report once, at the end, as they always did.
func (m *TaskManager) SetProgressHost(h progressHost) { m.host = h }

// newProgress starts a task's display, or returns nil when there is no host.
func (m *TaskManager) newProgress(id string, chatID int64, started time.Time, header string) *progressTracker {
	if m.host == nil {
		return nil
	}
	p := m.host.NewProgress(chatID, true, started)
	p.SetHeader(header)
	m.mu.Lock()
	m.progress[id] = p
	m.mu.Unlock()
	return p
}

// takeProgress removes and returns a task's display.
func (m *TaskManager) takeProgress(id string) *progressTracker {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.progress[id]
	delete(m.progress, id)
	return p
}

// taskHeader is the line that stays pinned above a running task's steps, so its
// id is readable at every moment rather than only in the final message.
func taskHeader(id, title string) string {
	return fmt.Sprintf("⏳ Tarea %s — %s", id, tg.TruncateRunes(strings.TrimSpace(title), 60))
}

// announce delivers text to the owner on every channel.
func (m *TaskManager) announce(text string) { m.announceTo(0, text) }

// announceTo delivers text to a specific chat, or the owner when zero.
func (m *TaskManager) announceTo(chatID int64, text string) {
	if m.out == nil {
		log.Printf("tasks: no delivery channel available for: %s", tg.TruncateRunes(text, 200))
		return
	}
	if _, err := m.out.Emit(context.Background(), Outbound{
		ChatID: chatID, Text: text, Source: SourceSend,
	}); err != nil {
		log.Printf("tasks: failed to deliver to chat %d: %v", chatID, err)
	}
}

// Start binds the manager to the daemon's lifetime and reconciles tasks that
// were running when the process last stopped.
func (m *TaskManager) Start(ctx context.Context) {
	m.ctx = ctx

	m.mu.Lock()
	var orphaned []string
	for _, t := range m.tasks {
		if t.Running() {
			// The child died with the old process. Re-attaching would be
			// guesswork, so mark it lost and say so.
			t.State = TaskOrphaned
			t.FinishedAt = time.Now()
			t.Error = "the hub restarted while this task was running"
			m.persistLocked(t)
			orphaned = append(orphaned, fmt.Sprintf("%s (%s)", t.ID, t.Title))
		}
	}
	m.mu.Unlock()

	if len(orphaned) > 0 {
		log.Printf("tasks: %d task(s) orphaned by the restart: %s", len(orphaned), strings.Join(orphaned, ", "))
		go m.announce(fmt.Sprintf("⚠️ %s se ha reiniciado y ha perdido %d tarea(s) en background:\n• %s\n\nNo se han reanudado; relánzalas si aún las necesitas.",
			InstanceName, len(orphaned), strings.Join(orphaned, "\n• ")))
	}
}

// persistLocked writes one task; callers must hold the mutex.
func (m *TaskManager) persistLocked(t *Task) {
	if err := m.st.SaveAssistantTask(context.WithoutCancel(m.ctx), m.userID, m.rowOf(t)); err != nil {
		log.Printf("tasks: failed to persist %s: %v", t.ID, err)
	}
}

// runningLocked counts executing tasks; callers must hold the mutex.
func (m *TaskManager) runningLocked() int {
	n := 0
	for _, t := range m.tasks {
		if t.Running() {
			n++
		}
	}
	return n
}

// Run spawns a new background task and returns immediately.
func (m *TaskManager) Run(prompt, title string, chatID int64) (Task, error) {
	if strings.TrimSpace(prompt) == "" {
		return Task{}, fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(title) == "" {
		title = tg.TruncateRunes(strings.TrimSpace(prompt), 60)
	}

	m.mu.Lock()
	if max := m.opts.Agent.MaxConcurrentTasks; max > 0 && m.runningLocked() >= max {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("too many background tasks running (%d); wait for one to finish or kill it", max)
	}
	m.mu.Unlock()

	id := newJobID()
	dir := filepath.Join(m.workdir, "tasks", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Task{}, fmt.Errorf("create task dir: %w", err)
	}

	sessionID := newSessionID()
	task := &Task{
		ID:         id,
		Title:      title,
		Prompt:     prompt,
		State:      TaskRunning,
		Dir:        dir,
		OutputPath: filepath.Join(dir, "output.txt"),
		StartedAt:  time.Now(),
		SessionID:  sessionID,
		ChatID:     chatID,
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		log.Printf("tasks: could not write prompt.txt for %s: %v", id, err)
	}

	ctx, cancel := context.WithCancel(m.ctx)

	m.mu.Lock()
	m.tasks[id] = task
	m.cancels[id] = cancel
	m.persistLocked(task)
	snapshot := *task
	m.mu.Unlock()

	log.Printf("tasks: started %s (%q), timeout %s, dir %s", id, title, m.opts.TaskTimeout(), dir)
	progress := m.newProgress(id, chatID, task.StartedAt, taskHeader(id, title))
	go m.execute(ctx, id, dir, prompt, sessionID, progress)

	return snapshot, nil
}

// execute runs the task's agent and delivers the outcome.
func (m *TaskManager) execute(ctx context.Context, id, dir, prompt, sessionID string, progress *progressTracker) {
	// A task always gets a fresh session in its own directory, so it never
	// resumes (or disturbs) the conversation the owner is having in Telegram.
	agent := NewAgent(m.opts.Agent, dir, m.home, m.opts.TaskTimeout())

	handle, startErr := agent.Start(ctx, RunSpec{
		Prompt: prompt, SessionID: sessionID, Mode: RunNew, OnProgress: progress.Add,
	})
	if startErr != nil {
		m.finalize(id, dir, nil, startErr)
		return
	}
	m.setPID(id, handle.PID())
	<-handle.Done()
	res, err := handle.Result()
	m.finalize(id, dir, res, err)
}

// setPID records the child's pid for a task.
func (m *TaskManager) setPID(id string, pid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		t.PID = pid
		m.persistLocked(t)
	}
}

// Adopt takes ownership of a run that is ALREADY EXECUTING, turning it into a
// background task. This is how a conversational reply that overruns the soft
// budget stops blocking the chat: the process keeps going untouched, only its
// bookkeeping and its delivery channel change.
// progress is the display the conversational turn already started; it is taken
// over rather than replaced, so the message the chat is watching becomes the
// task's message. It may be nil.
func (m *TaskManager) Adopt(h *RunHandle, title string, chatID int64, progress *progressTracker) (Task, error) {
	id := newJobID()
	dir := filepath.Join(m.workdir, "tasks", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Task{}, fmt.Errorf("create task dir: %w", err)
	}
	if strings.TrimSpace(title) == "" {
		title = tg.TruncateRunes(strings.TrimSpace(h.Prompt()), 60)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(h.Prompt()), 0o644); err != nil {
		log.Printf("tasks: could not write prompt.txt for %s: %v", id, err)
	}

	task := &Task{
		ID:    id,
		Title: title,
		// The task inherits the run's original start time, so its hard cap and
		// its reported duration both count from when the owner actually asked.
		Prompt:     h.Prompt(),
		State:      TaskRunning,
		PID:        h.PID(),
		Dir:        dir,
		OutputPath: filepath.Join(dir, "output.txt"),
		StartedAt:  h.StartedAt(),
		SessionID:  h.SessionID(),
		Promoted:   true,
		ChatID:     chatID,
	}

	m.mu.Lock()
	m.tasks[id] = task
	m.cancels[id] = h.Cancel
	if progress != nil {
		m.progress[id] = progress
	}
	m.persistLocked(task)
	snapshot := *task
	m.mu.Unlock()

	log.Printf("tasks: adopted running process %d as task %s (%q), session %s", h.PID(), id, title, h.SessionID())

	go func() {
		<-h.Done()
		res, runErr := h.Result()
		m.finalize(id, dir, res, runErr)
	}()

	return snapshot, nil
}

// finalize records a finished run, persists its output and reports it.
func (m *TaskManager) finalize(id, dir string, res *AgentResult, err error) {
	state, errMsg := TaskDone, ""
	switch {
	case res != nil && res.Cancelled:
		state, errMsg = TaskKilled, "killed on request"
	case res != nil && res.TimedOut:
		state, errMsg = TaskTimeout, fmt.Sprintf("timed out after %s", m.opts.TaskTimeout())
	case err != nil:
		state, errMsg = TaskFailed, tg.TruncateRunes(err.Error(), 300)
	}

	if res != nil && res.Stdout != "" {
		if writeErr := os.WriteFile(filepath.Join(dir, "output.txt"), []byte(res.Stdout), 0o644); writeErr != nil {
			log.Printf("tasks: could not write output for %s: %v", id, writeErr)
		}
	}
	if res != nil && res.Stderr != "" {
		if writeErr := os.WriteFile(filepath.Join(dir, "stderr.txt"), []byte(res.Stderr), 0o644); writeErr != nil {
			log.Printf("tasks: could not write stderr for %s: %v", id, writeErr)
		}
	}
	if res != nil && res.RateLimited {
		m.NoteRateLimited()
	}

	m.mu.Lock()
	task, ok := m.tasks[id]
	var snapshot Task
	if ok {
		task.State = state
		task.Error = errMsg
		task.FinishedAt = time.Now()
		task.PID = 0
		m.persistLocked(task)
		snapshot = *task
	}
	delete(m.cancels, id)
	m.mu.Unlock()

	if !ok {
		return
	}
	log.Printf("tasks: %s finished in %s with state %s", id, snapshot.Duration().Truncate(time.Second), state)
	m.report(&snapshot, res)
}

// report pushes a finished task's outcome to the chat that asked for it.
//
// A task is one message in the chat from beginning to end: the progress display
// is edited in place while it runs and then edited into this result. Only a
// result too long for a single Telegram message costs a second one, and even
// then the first is edited into the header rather than left showing steps.
func (m *TaskManager) report(task *Task, res *AgentResult) {
	icon := map[string]string{
		TaskDone: "✅", TaskFailed: "❌", TaskKilled: "🛑", TaskTimeout: "⌛",
	}[task.State]
	header := fmt.Sprintf("%s Tarea %s — %s (%s)",
		icon, task.ID, task.Title, task.Duration().Truncate(time.Second))
	if task.Promoted {
		// Make it obvious which message this answers.
		header += fmt.Sprintf("\n↩️ Responde a: «%s»", tg.TruncateRunes(strings.TrimSpace(task.Prompt), 160))
	}

	var body string
	switch {
	case res != nil && res.Stdout != "":
		body = res.Stdout
		if len([]rune(body)) > taskOutputLimit {
			body = tg.TruncateRunes(body, taskOutputLimit) +
				fmt.Sprintf("\n\n… truncado. Salida completa: %s", task.OutputPath)
		}
	case task.Error != "":
		body = task.Error
	default:
		body = "(sin salida)"
	}

	full := header + "\n\n" + body
	ctx := context.WithoutCancel(m.ctx)
	progress := m.takeProgress(task.ID)
	switch {
	case progress.FinishWith(ctx, full):
		return
	case progress.FinishHeader(ctx, fmt.Sprintf("%s Tarea %s — %s (%s) · resultado abajo ↓",
		icon, task.ID, tg.TruncateRunes(strings.TrimSpace(task.Title), 60),
		task.Duration().Truncate(time.Second))):
		// Too long to live in one message: the progress message becomes a
		// one-line "done" and the result follows through the chunked send.
	default:
		progress.Discard(ctx)
	}
	m.announceTo(task.ChatID, full)
}

// List returns every task, newest first.
func (m *TaskManager) List() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.After(out[k].StartedAt) })
	return out
}

// ForkSafety reports whether a session may be branched off, and why not.
//
// Measured against grok on vps2: forking a cleanly finished session takes ~5s
// and preserves context, but forking one whose run is still writing, or whose
// run was killed mid-write, produces no output and hangs until it is killed.
// So only a session whose task reached TaskDone is safe to fork.
func (m *TaskManager) ForkSafety(sessionID string) (safe bool, blockingTask, reason string) {
	if sessionID == "" {
		return false, "", "no session"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.SessionID != sessionID {
			continue
		}
		switch {
		case t.Running():
			return false, t.ID, "still running"
		case t.State != TaskDone:
			return false, t.ID, "did not finish cleanly (" + t.State + ")"
		}
		return true, t.ID, ""
	}
	// No task owns it: it is an ordinary finished conversational session.
	return true, "", ""
}

// Get returns one task.
func (m *TaskManager) Get(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *t, true
}

// Kill stops a running task. It reports whether a task was actually killed.
func (m *TaskManager) Kill(id string) (bool, error) {
	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("task %s not found", id)
	}
	if !task.Running() {
		m.mu.Unlock()
		return false, nil
	}
	cancel := m.cancels[id]
	m.mu.Unlock()

	if cancel == nil {
		return false, nil
	}
	log.Printf("tasks: killing %s on request", id)
	cancel()
	return true, nil
}

// Prune drops finished tasks older than the retention window.
func (m *TaskManager) Prune(olderThan time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	for id, t := range m.tasks {
		if !t.Running() && !t.FinishedAt.IsZero() && t.FinishedAt.Before(cutoff) {
			delete(m.tasks, id)
			delete(m.progress, id)
			if err := m.st.DeleteAssistantTask(context.WithoutCancel(m.ctx), m.userID, id); err != nil {
				log.Printf("tasks: failed to delete %s: %v", id, err)
			}
			removed++
		}
	}
	return removed
}

// NoteRateLimited records an upstream refusal, for the /usage report.
func (m *TaskManager) NoteRateLimited() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	kept := m.rateLimited[:0]
	for _, t := range m.rateLimited {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	m.rateLimited = append(kept, time.Now())
}

// RateLimitedLast24h counts upstream refusals seen in the last day.
func (m *TaskManager) RateLimitedLast24h() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	n := 0
	for _, t := range m.rateLimited {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// countRunningTasks reports how many tasks are currently executing.
func countRunningTasks(m *TaskManager) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningLocked()
}
