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
	"github.com/robfig/cron/v3"
)

// ScheduleLocation is the timezone every schedule is interpreted in. Jobs must
// not follow the server's local time, which is UTC on the VPS.
const ScheduleLocation = "Europe/Madrid"

// missedGrace is how late a one-shot job may still fire after downtime.
const missedGrace = 12 * time.Hour

// tickInterval is how often the scheduler looks for due jobs.
const tickInterval = 10 * time.Second

// Job types.
const (
	JobMessage = "message"
	JobAgent   = "agent"
)

// cronParser accepts standard 5-field expressions plus @daily-style descriptors.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Job is one reminder (message) or routine (agent run).
type Job struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is JobMessage or JobAgent.
	Type string `json:"type"`
	// Payload is the literal text for a message job, or the prompt for an agent job.
	Payload string `json:"payload"`
	// Cron is a 5-field expression evaluated in Europe/Madrid. Empty for one-shots.
	Cron string `json:"cron,omitempty"`
	// At is the absolute trigger time of a one-shot job.
	At time.Time `json:"at,omitempty"`
	// ChatID targets a specific chat; zero means the owner DM.
	ChatID int64 `json:"chat_id,omitempty"`

	NextRun    time.Time `json:"next_run"`
	LastRun    time.Time `json:"last_run,omitempty"`
	LastStatus string    `json:"last_status,omitempty"`
	Runs       int64     `json:"runs"`
	CreatedAt  time.Time `json:"created_at"`
}

// Recurring reports whether the job repeats on a cron schedule.
func (j *Job) Recurring() bool { return strings.TrimSpace(j.Cron) != "" }

// Validate checks a job submitted through the API.
func (j *Job) Validate() error {
	switch j.Type {
	case JobMessage, JobAgent:
	case "":
		return fmt.Errorf("type is required (%q or %q)", JobMessage, JobAgent)
	default:
		return fmt.Errorf("unknown type %q (want %q or %q)", j.Type, JobMessage, JobAgent)
	}
	if strings.TrimSpace(j.Payload) == "" {
		return fmt.Errorf("payload is required")
	}
	if j.Recurring() {
		if _, err := cronParser.Parse(j.Cron); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", j.Cron, err)
		}
		return nil
	}
	if j.At.IsZero() {
		return fmt.Errorf("either cron or at is required")
	}
	return nil
}

// JobStore persists jobs to the hub database, cached in memory so the ten-second
// tick does not hit SQLite for every job on every pass.
type JobStore struct {
	st     *store.Store
	userID string
	ctx    context.Context
	mu     sync.Mutex
	jobs   map[string]*Job
}

// NewJobStore loads a user's jobs from the database.
func NewJobStore(ctx context.Context, st *store.Store, userID string) (*JobStore, error) {
	s := &JobStore{st: st, userID: userID, ctx: ctx, jobs: map[string]*Job{}}
	rows, err := st.ListAssistantJobs(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		j := jobFromRow(r)
		s.jobs[j.ID] = &j
	}
	return s, nil
}

func jobFromRow(r store.AssistantJob) Job {
	j := Job{
		ID: r.ID, Name: r.Name, Type: r.Type, Payload: r.Payload, Cron: r.Cron,
		LastStatus: r.LastStatus, Runs: r.Runs, CreatedAt: r.CreatedAt,
	}
	if r.At != nil {
		j.At = *r.At
	}
	if r.NextRun != nil {
		j.NextRun = *r.NextRun
	}
	if r.LastRun != nil {
		j.LastRun = *r.LastRun
	}
	if r.ChatID != "" {
		if id, err := strconv.ParseInt(r.ChatID, 10, 64); err == nil {
			j.ChatID = id
		}
	}
	return j
}

func rowOfJob(j *Job) store.AssistantJob {
	row := store.AssistantJob{
		ID: j.ID, Type: j.Type, Name: j.Name, Payload: j.Payload, Cron: j.Cron,
		LastStatus: j.LastStatus, Runs: j.Runs, CreatedAt: j.CreatedAt,
	}
	if j.ChatID != 0 {
		row.ChatID = strconv.FormatInt(j.ChatID, 10)
	}
	for _, pair := range []struct {
		src time.Time
		dst **time.Time
	}{{j.At, &row.At}, {j.NextRun, &row.NextRun}, {j.LastRun, &row.LastRun}} {
		if !pair.src.IsZero() {
			t := pair.src
			*pair.dst = &t
		}
	}
	return row
}

// persistLocked writes one job; callers must hold the mutex.
func (s *JobStore) persistLocked(j *Job) error {
	return s.st.SaveAssistantJob(context.WithoutCancel(s.ctx), s.userID, rowOfJob(j))
}

// List returns all jobs sorted by next run time.
func (s *JobStore) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *JobStore) listLocked() []Job {
	out := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		out = append(out, *job)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].NextRun.Before(out[k].NextRun) })
	return out
}

// Get returns a copy of one job.
func (s *JobStore) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

// Add validates, schedules and persists a new job.
func (s *JobStore) Add(job *Job, loc *time.Location) (Job, error) {
	if err := job.Validate(); err != nil {
		return Job{}, err
	}
	job.ID = newJobID()
	job.CreatedAt = time.Now().In(loc)
	if job.Name == "" {
		job.Name = tg.TruncateRunes(strings.TrimSpace(job.Payload), 40)
	}

	next, err := nextRun(job, time.Now().In(loc), loc)
	if err != nil {
		return Job{}, err
	}
	job.NextRun = next

	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
	if err := s.persistLocked(job); err != nil {
		delete(s.jobs, job.ID)
		return Job{}, err
	}
	return *job, nil
}

// Delete removes a job by id.
func (s *JobStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return false, nil
	}
	delete(s.jobs, id)
	return true, s.st.DeleteAssistantJob(context.WithoutCancel(s.ctx), s.userID, id)
}

// Due returns the jobs whose next run has arrived.
func (s *JobStore) Due(now time.Time) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Job
	for _, job := range s.jobs {
		if !job.NextRun.IsZero() && !job.NextRun.After(now) {
			due = append(due, *job)
		}
	}
	sort.Slice(due, func(i, k int) bool { return due[i].NextRun.Before(due[k].NextRun) })
	return due
}

// Advance is called the moment a job is picked up for execution: a recurring
// job moves to its next occurrence, a one-shot is removed. Doing this before
// the run means a slow job is never fired twice, and a one-shot never repeats.
func (s *JobStore) Advance(id string, loc *time.Location) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil
	}
	now := time.Now().In(loc)
	job.LastRun = now
	job.Runs++

	if !job.Recurring() {
		delete(s.jobs, id)
		return s.st.DeleteAssistantJob(context.WithoutCancel(s.ctx), s.userID, id)
	}
	next, err := nextRun(job, now, loc)
	if err != nil {
		return err
	}
	job.NextRun = next
	return s.persistLocked(job)
}

// Reschedule recomputes next run times at startup and reports the one-shot jobs
// that were missed while the process was down but are still inside the grace
// window.
func (s *JobStore) Reschedule(loc *time.Location) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().In(loc)

	var missed []Job
	var firstErr error
	for id, job := range s.jobs {
		if job.Recurring() {
			next, err := nextRun(job, now, loc)
			if err != nil {
				log.Printf("scheduler: dropping job %s with invalid cron %q: %v", id, job.Cron, err)
				delete(s.jobs, id)
				if delErr := s.st.DeleteAssistantJob(context.WithoutCancel(s.ctx), s.userID, id); delErr != nil && firstErr == nil {
					firstErr = delErr
				}
				continue
			}
			job.NextRun = next
			if err := s.persistLocked(job); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if job.NextRun.After(now) {
			continue
		}
		if now.Sub(job.NextRun) > missedGrace {
			log.Printf("scheduler: dropping one-shot %s (%q), missed by %s", id, job.Name, now.Sub(job.NextRun).Truncate(time.Minute))
			delete(s.jobs, id)
			if delErr := s.st.DeleteAssistantJob(context.WithoutCancel(s.ctx), s.userID, id); delErr != nil && firstErr == nil {
				firstErr = delErr
			}
			continue
		}
		missed = append(missed, *job)
	}
	return missed, firstErr
}

// recordStatus stores the outcome of a job's run.
func (s *JobStore) recordStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil
	}
	job.LastStatus = status
	return s.persistLocked(job)
}

// nextRun computes the next trigger for a job after the given time.
func nextRun(job *Job, after time.Time, loc *time.Location) (time.Time, error) {
	if job.Recurring() {
		schedule, err := cronParser.Parse(job.Cron)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", job.Cron, err)
		}
		return schedule.Next(after.In(loc)), nil
	}
	return job.At.In(loc), nil
}

// Scheduler fires jobs at their scheduled time. It never depends on the model
// to remember anything: the daemon owns the clock.
type Scheduler struct {
	store *JobStore
	// out is the assistant's single outbound choke point: a reminder that only
	// reached Telegram would leave a hole in the phone app's history.
	out  Emitter
	loc  *time.Location
	opts Options
	// workdir is the agent workspace; routines run in workdir/routines/<id>.
	workdir string
	home    string
	// ownerChat is the default delivery target.
	ownerChat int64
	// tasks records rate-limit hits so /usage can report them.
	tasks *TaskManager

	// fire serialises job execution so two agent runs never overlap.
	fire chan fireRequest
}

// fireRequest asks the scheduler worker to run one job.
type fireRequest struct {
	job  Job
	late bool
	done chan error
}

// NewScheduler builds the scheduler and loads its job store.
func NewScheduler(ctx context.Context, st *store.Store, userID string, opts Options,
	workdir, home string, out Emitter, ownerChat int64) (*Scheduler, error) {
	loc, err := time.LoadLocation(ScheduleLocation)
	if err != nil {
		return nil, fmt.Errorf("load timezone %s: %w", ScheduleLocation, err)
	}
	js, err := NewJobStore(ctx, st, userID)
	if err != nil {
		return nil, err
	}
	return &Scheduler{
		store: js, out: out, loc: loc, opts: opts,
		workdir: workdir, home: home, ownerChat: ownerChat,
		fire: make(chan fireRequest, 16),
	}, nil
}

// SetTasks wires the task manager so routine failures feed the /usage counters.
func (s *Scheduler) SetTasks(m *TaskManager) { s.tasks = m }

// Location exposes the schedule timezone.
func (s *Scheduler) Location() *time.Location { return s.loc }

// Store exposes the job store to the HTTP API and the panel.
func (s *Scheduler) Store() *JobStore { return s.store }

// Run recovers missed jobs, then ticks until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	missed, err := s.store.Reschedule(s.loc)
	if err != nil {
		log.Printf("scheduler: failed to reschedule jobs: %v", err)
	}
	log.Printf("scheduler: %d job(s) loaded, timezone %s", len(s.store.List()), ScheduleLocation)

	go s.worker(ctx)

	for _, job := range missed {
		log.Printf("scheduler: firing missed one-shot %s (%q), due %s", job.ID, job.Name, job.NextRun.Format(time.RFC3339))
		if err := s.store.Advance(job.ID, s.loc); err != nil {
			log.Printf("scheduler: failed to advance missed job %s: %v", job.ID, err)
		}
		s.enqueue(job, true)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, job := range s.store.Due(time.Now().In(s.loc)) {
				// Advance before running so the tick loop cannot re-enqueue a
				// job that is still executing.
				if err := s.store.Advance(job.ID, s.loc); err != nil {
					log.Printf("scheduler: failed to advance job %s: %v", job.ID, err)
				}
				s.enqueue(job, false)
			}
		}
	}
}

// enqueue hands a job to the serial worker without blocking the tick loop.
func (s *Scheduler) enqueue(job Job, late bool) {
	select {
	case s.fire <- fireRequest{job: job, late: late}:
	default:
		log.Printf("scheduler: fire queue full, skipping job %s (%q)", job.ID, job.Name)
	}
}

// RunNow executes a job immediately, for testing from atlas-sched.
func (s *Scheduler) RunNow(ctx context.Context, id string) error {
	job, ok := s.store.Get(id)
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	return s.execute(ctx, job, false)
}

// worker executes queued jobs one at a time.
func (s *Scheduler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.fire:
			err := s.execute(ctx, req.job, req.late)
			status := "ok"
			if err != nil {
				status = "error: " + tg.TruncateRunes(err.Error(), 200)
				log.Printf("scheduler: job %s (%q) failed: %v", req.job.ID, req.job.Name, err)
			}
			// No-op when the job was a one-shot and is already gone.
			if updErr := s.store.recordStatus(req.job.ID, status); updErr != nil {
				log.Printf("scheduler: failed to record status for %s: %v", req.job.ID, updErr)
			}
			if req.done != nil {
				req.done <- err
			}
		}
	}
}

// execute performs the job's action and delivers the result.
func (s *Scheduler) execute(ctx context.Context, job Job, late bool) error {
	prefix := ""
	if late {
		prefix = "[late] "
	}
	log.Printf("scheduler: running %s job %s (%q)", job.Type, job.ID, job.Name)

	target := s.target(job)

	switch job.Type {
	case JobMessage:
		return s.out.Emit(ctx, Outbound{ChatID: target, Text: prefix + job.Payload, Source: SourceSend})

	case JobAgent:
		// Routines run in their own directory with a fresh session so they never
		// pollute the conversation the owner is having in Telegram.
		dir := filepath.Join(s.workdir, "routines", job.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create routine workdir: %w", err)
		}
		res, err := NewAgent(s.opts.Agent, dir, s.home, s.opts.TaskTimeout()).Run(ctx,
			RunSpec{Prompt: job.Payload, SessionID: newSessionID(), Mode: RunNew})
		if err != nil {
			if res != nil && res.RateLimited && s.tasks != nil {
				s.tasks.NoteRateLimited()
			}
			log.Printf("scheduler: agent job %s stderr: %s", job.ID, tg.TruncateRunes(res.Stderr, 2000))
			return s.out.Emit(ctx, Outbound{ChatID: target, Source: SourceSend, Event: EventError,
				Text: fmt.Sprintf("%sLa rutina %q ha fallado: %s", prefix, job.Name, tg.TruncateRunes(err.Error(), 200))})
		}
		return s.out.Emit(ctx, Outbound{ChatID: target, Text: prefix + res.Stdout, Source: SourceSend})
	}
	return fmt.Errorf("unknown job type %q", job.Type)
}

// target resolves a job's destination chat, defaulting to the owner DM.
func (s *Scheduler) target(job Job) int64 {
	if job.ChatID != 0 {
		return job.ChatID
	}
	return s.ownerChat
}
