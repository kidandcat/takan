package web

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	asst "github.com/kidandcat/takan/internal/assistant"
	"github.com/kidandcat/takan/internal/store"
)

// AssistantView is the slice of the running assistant the panel needs. It is an
// interface so the web package does not depend on a live Telegram connection —
// and so the panel still renders when the assistant failed to start.
type AssistantView interface {
	Status(ctx context.Context) asst.Status
	Jobs() []asst.Job
	Tasks() []asst.Task
	KillTask(id string) (bool, error)
	DeleteJob(id string) (bool, error)
	RunJob(ctx context.Context, id string) error
	Workdir() string
}

// assistantView is the rendered state of the Assistant page.
type assistantView struct {
	Running       bool
	Enabled       bool
	BotUsername   string
	OwnerTelegram string
	Workdir       string
	PollHealthy   bool
	LastPollOK    string
	LastPollError string
	Messages      int
	RunningTasks  int
	ScheduledJobs int
	// Agent runner settings, as edited in the modal.
	Command            string
	Args               string
	NewSessionArgs     string
	ResumeArgs         string
	ForkArgs           string
	WorkdirSetting     string
	SoftTimeout        string
	TaskTimeout        string
	MaxConcurrentTasks int
	UnsetEnv           string
	PollTimeout        string
	TypingInterval     string
	RespondToAll       bool
	Progress           bool

	Chats   []assistantChatView
	Jobs    []assistantJobView
	TaskRow []assistantTaskView
	Devices []assistantDeviceView
}

type assistantChatView struct {
	ChatID, Kind, Title, Session, LastRun string
	Runs                                  int64
	IsOwner, Group                        bool
}

type assistantJobView struct {
	ID, Type, Name, Schedule, NextRun, LastRun, LastStatus, ChatID string
	Runs                                                           int64
}

type assistantTaskView struct {
	ID, Title, State, Duration, Started, Error string
	Running, Promoted                          bool
}

type assistantDeviceView struct {
	Token, Masked, Platform, Updated string
}

// fillAssistant renders the Assistant page state. Everything on it is read-only
// except the runner settings: chats need no approval, so there is nothing to
// decide here.
func (s *Server) fillAssistant(ctx context.Context, u *store.User, data *pageData) {
	opts, err := asst.LoadOptions(ctx, s.Store, u.ID)
	if err != nil {
		opts = asst.DefaultOptions()
	}
	v := assistantView{
		Enabled:            opts.Enabled,
		RespondToAll:       opts.RespondToAll,
		Progress:           opts.Agent.ProgressEnabled(),
		Command:            opts.Agent.Command,
		Args:               strings.Join(opts.Agent.Args, "\n"),
		NewSessionArgs:     strings.Join(opts.Agent.NewSessionArgs, "\n"),
		ResumeArgs:         strings.Join(opts.Agent.ResumeArgs, "\n"),
		ForkArgs:           strings.Join(opts.Agent.ForkArgs, "\n"),
		WorkdirSetting:     opts.Agent.Workdir,
		SoftTimeout:        opts.Agent.SoftTimeout,
		TaskTimeout:        opts.Agent.TaskTimeout,
		MaxConcurrentTasks: opts.Agent.MaxConcurrentTasks,
		UnsetEnv:           strings.Join(opts.Agent.UnsetEnv, "\n"),
		PollTimeout:        opts.Telegram.PollTimeout,
		TypingInterval:     opts.Telegram.TypingInterval,
	}

	if s.Assistant != nil {
		v.Running = true
		st := s.Assistant.Status(ctx)
		v.BotUsername = st.BotUsername
		v.OwnerTelegram = strconv.FormatInt(st.OwnerTelegram, 10)
		v.Workdir = s.Assistant.Workdir()
		v.PollHealthy = st.PollHealthy
		v.LastPollError = st.LastPollError
		v.Messages = st.Messages
		v.RunningTasks = st.RunningTasks
		v.ScheduledJobs = st.ScheduledJobs
		if !st.LastPollOK.IsZero() {
			v.LastPollOK = st.LastPollOK.UTC().Format("2006-01-02 15:04")
		}

		for _, j := range s.Assistant.Jobs() {
			row := assistantJobView{
				ID: j.ID, Type: j.Type, Name: j.Name, LastStatus: j.LastStatus, Runs: j.Runs,
				Schedule: j.Cron,
			}
			if row.Schedule == "" {
				row.Schedule = "once"
			}
			if !j.NextRun.IsZero() {
				row.NextRun = j.NextRun.Format("2006-01-02 15:04")
			}
			if !j.LastRun.IsZero() {
				row.LastRun = j.LastRun.Format("2006-01-02 15:04")
			}
			if j.ChatID != 0 {
				row.ChatID = strconv.FormatInt(j.ChatID, 10)
			}
			v.Jobs = append(v.Jobs, row)
		}
		for _, t := range s.Assistant.Tasks() {
			v.TaskRow = append(v.TaskRow, assistantTaskView{
				ID: t.ID, Title: t.Title, State: t.State,
				Duration: t.Duration().Truncate(1e9).String(),
				Started:  t.StartedAt.Format("2006-01-02 15:04"),
				Error:    t.Error, Running: t.Running(), Promoted: t.Promoted,
			})
		}
	}

	if chats, err := s.Store.ListAssistantChats(ctx, u.ID); err == nil {
		for _, c := range chats {
			row := assistantChatView{
				ChatID: c.ChatID, Kind: c.Kind, Title: c.Title, Runs: c.Runs,
				Group:   c.Kind == "group",
				IsOwner: c.ChatID == v.OwnerTelegram,
			}
			if c.SessionID != "" {
				row.Session = shortID(c.SessionID)
			}
			if c.LastRunAt != nil {
				row.LastRun = c.LastRunAt.UTC().Format("2006-01-02 15:04")
			}
			v.Chats = append(v.Chats, row)
		}
	}
	if devices, err := s.Store.ListPushDevices(ctx, u.ID); err == nil {
		for _, d := range devices {
			v.Devices = append(v.Devices, assistantDeviceView{
				Token:    d.Token,
				Masked:   maskToken(d.Token),
				Platform: d.Platform,
				Updated:  d.UpdatedAt.UTC().Format("2006-01-02 15:04"),
			})
		}
	}
	data.Assistant = v
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// maskToken renders a device token recognisably without printing it.
func maskToken(t string) string {
	if len(t) <= 10 {
		return strings.Repeat("•", len(t))
	}
	return t[:6] + "…" + t[len(t)-4:]
}

func (s *Server) redirectAssistant(w http.ResponseWriter, r *http.Request, flash string) {
	url := "/dashboard/assistant"
	if flash != "" {
		url += "?flash=" + urlQuery(flash)
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// saveAssistant writes the runner settings back to the panel row. It takes
// effect on the next restart for the poll loop; new runs pick it up immediately.
func (s *Server) saveAssistant(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	// A form that will not parse yields empty values for every field, which
	// would silently disable the assistant and blank its runner config.
	if err := r.ParseForm(); err != nil {
		s.redirectAssistant(w, r, "error: could not read the form: "+err.Error())
		return
	}
	opts, err := asst.LoadOptions(r.Context(), s.Store, u.ID)
	if err != nil {
		opts = asst.DefaultOptions()
	}
	opts.Enabled = checked(r, "enabled")
	opts.RespondToAll = checked(r, "respond_to_all")
	// Progress picks the runner's --output-format, so SaveOptions rewrites the
	// argument list to match rather than trusting whatever the textarea said.
	progress := checked(r, "progress")
	opts.Agent.Progress = &progress
	opts.Agent.Command = strings.TrimSpace(r.FormValue("command"))
	opts.Agent.Args = splitLines(r.FormValue("args"))
	opts.Agent.NewSessionArgs = splitLines(r.FormValue("new_session_args"))
	opts.Agent.ResumeArgs = splitLines(r.FormValue("resume_args"))
	opts.Agent.ForkArgs = splitLines(r.FormValue("fork_args"))
	opts.Agent.Workdir = strings.TrimSpace(r.FormValue("workdir"))
	opts.Agent.SoftTimeout = strings.TrimSpace(r.FormValue("soft_timeout"))
	opts.Agent.TaskTimeout = strings.TrimSpace(r.FormValue("task_timeout"))
	opts.Agent.UnsetEnv = splitLines(r.FormValue("unset_env"))
	opts.Telegram.PollTimeout = strings.TrimSpace(r.FormValue("poll_timeout"))
	opts.Telegram.TypingInterval = strings.TrimSpace(r.FormValue("typing_interval"))
	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_concurrent_tasks"))); err == nil {
		opts.Agent.MaxConcurrentTasks = n
	}

	if err := asst.SaveOptions(r.Context(), s.Store, u.ID, opts); err != nil {
		s.redirectAssistant(w, r, "error: "+err.Error())
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, asst.ModuleID, true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.redirectAssistant(w, r, "Assistant settings saved — restart to apply the polling and runner changes")
}

func (s *Server) forgetAssistantChat(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.ForgetAssistantChat(r.Context(), u.ID, r.PathValue("chat")); err != nil {
		s.redirectAssistant(w, r, "error: "+err.Error())
		return
	}
	s.redirectAssistant(w, r, "Chat forgotten — the next message starts a fresh session")
}

func (s *Server) deleteAssistantJob(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if s.Assistant == nil {
		// The assistant is not running, so its in-memory job store cannot be
		// told. Delete the row directly; a restart picks up the change.
		if err := s.Store.DeleteAssistantJob(r.Context(), u.ID, r.PathValue("id")); err != nil {
			s.redirectAssistant(w, r, "error: "+err.Error())
			return
		}
		s.redirectAssistant(w, r, "Job deleted")
		return
	}
	if _, err := s.Assistant.DeleteJob(r.PathValue("id")); err != nil {
		s.redirectAssistant(w, r, "error: "+err.Error())
		return
	}
	s.redirectAssistant(w, r, "Job deleted")
}

func (s *Server) runAssistantJob(w http.ResponseWriter, r *http.Request) {
	if u := s.requireUser(w, r); u == nil {
		return
	}
	if s.Assistant == nil {
		s.redirectAssistant(w, r, "error: the assistant is not running")
		return
	}
	// An agent job can take minutes; fire and forget, the result arrives in
	// Telegram. Log a failure: the operator clicked a button and would otherwise
	// see nothing at all.
	go func(id string) {
		if err := s.Assistant.RunJob(context.Background(), id); err != nil {
			log.Printf("assistant: manual run of job %s failed: %v", id, err)
		}
	}(r.PathValue("id"))
	s.redirectAssistant(w, r, "Job fired — the result will arrive in Telegram")
}

func (s *Server) killAssistantTask(w http.ResponseWriter, r *http.Request) {
	if u := s.requireUser(w, r); u == nil {
		return
	}
	if s.Assistant == nil {
		s.redirectAssistant(w, r, "error: the assistant is not running")
		return
	}
	killed, err := s.Assistant.KillTask(r.PathValue("id"))
	if err != nil {
		s.redirectAssistant(w, r, "error: "+err.Error())
		return
	}
	if !killed {
		s.redirectAssistant(w, r, "That task was not running")
		return
	}
	s.redirectAssistant(w, r, "Task killed")
}

func (s *Server) deleteAssistantDevice(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.DeletePushDevice(r.Context(), u.ID, r.PathValue("token")); err != nil {
		s.redirectAssistant(w, r, "error: "+err.Error())
		return
	}
	s.redirectAssistant(w, r, "Device removed")
}

// splitLines turns a textarea into a trimmed list, dropping blank lines.
func splitLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// checked reads an HTML checkbox.
func checked(r *http.Request, name string) bool {
	v := r.FormValue(name)
	return v == "1" || v == "on" || v == "true"
}
