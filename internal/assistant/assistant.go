package assistant

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/internal/tg"
)

// Config is everything the assistant needs from the process environment. The
// runner knobs live in the panel (Options); this is the secret and path layer.
type Config struct {
	// OwnerID is the hub users.id every row belongs to.
	OwnerID string
	// OwnerTelegram is the owner's Telegram user id. Required: without it the
	// assistant cannot tell the owner from a stranger and refuses to start.
	OwnerTelegram int64
	// DataDir holds the inbox and the agent workspace.
	DataDir string
	// AgentHome is the HOME handed to the CLI agent (where ~/.grok lives).
	AgentHome string
	// TelegramBotToken bootstraps the credential. When set it wins and is
	// sealed into assistant_meta; when empty the sealed copy is used.
	TelegramBotToken string
	GroqAPIKey       string
	// AppToken guards /v1/*. Empty disables the phone app channel.
	AppToken string
	// FirebaseServiceAccount enables push. Empty leaves it off.
	FirebaseServiceAccount string
	// LegacyDir is the standalone daemon's data directory, imported once.
	LegacyDir string
}

// Assistant is the personal assistant running inside the hub process.
type Assistant struct {
	Opts  Options
	Store *store.Store
	Box   *cryptox.Box

	OwnerID       string
	OwnerTelegram int64
	// AppToken guards the /v1 app channel.
	AppToken string

	Bot *Bot
	// TaskMgr owns background tasks; Sched owns reminders and routines.
	TaskMgr *TaskManager
	Sched   *Scheduler
	// Hub is the machine-agent hub, used to fetch job tails.
	Hub *agenthub.Hub

	dataDir string
	workdir string
	home    string
	ctx     context.Context
}

// New builds the assistant from the process configuration and the panel's
// runner options. It fails closed: no owner id, or no Telegram credential, and
// the caller gets an error rather than a half-live assistant.
func New(ctx context.Context, st *store.Store, box *cryptox.Box, hub *agenthub.Hub, cfg Config) (*Assistant, error) {
	if strings.TrimSpace(cfg.OwnerID) == "" {
		return nil, fmt.Errorf("assistant: owner user id is required")
	}
	if cfg.OwnerTelegram == 0 {
		return nil, fmt.Errorf("assistant: OWNER_TELEGRAM_ID is not set — the assistant cannot tell the owner from a stranger")
	}

	// The runner configuration moved from config.toml to the panel; seed it once
	// from the file when the panel row is still empty.
	if err := ImportLegacyTOML(ctx, st, cfg.OwnerID, cfg.LegacyDir); err != nil {
		log.Printf("assistant: legacy config import: %v", err)
	}
	opts, err := LoadOptions(ctx, st, cfg.OwnerID)
	if err != nil {
		return nil, err
	}

	token, err := resolveBotToken(ctx, st, box, cfg)
	if err != nil {
		return nil, err
	}

	workdir := strings.TrimSpace(opts.Agent.Workdir)
	if workdir == "" {
		workdir = filepath.Join(cfg.DataDir, "workspace")
	}
	home := strings.TrimSpace(cfg.AgentHome)
	if home == "" {
		home = os.Getenv("HOME")
	}
	for _, dir := range []string{workdir, filepath.Join(cfg.DataDir, "inbox")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	client := tg.New(token)
	push, err := NewPushSender(ctx, cfg.FirebaseServiceAccount)
	if err != nil {
		return nil, err
	}

	a := &Assistant{
		Opts: opts, Store: st, Box: box, Hub: hub,
		OwnerID: cfg.OwnerID, OwnerTelegram: cfg.OwnerTelegram, AppToken: cfg.AppToken,
		dataDir: cfg.DataDir, workdir: workdir, home: home, ctx: ctx,
	}

	a.Bot = &Bot{
		opts:          opts,
		tg:            client,
		state:         NewStateStore(ctx, st, cfg.OwnerID),
		stt:           NewTranscriber(cfg.GroqAPIKey),
		push:          push,
		ownerTelegram: cfg.OwnerTelegram,
		dataDir:       cfg.DataDir,
		workdir:       workdir,
		home:          home,
		runners:       map[int64]*chatRunner{},
		history:       NewHistory(ctx, st, cfg.OwnerID),
		events:        NewEventBus(),
		startedAt:     time.Now(),
		ready:         make(chan struct{}),
	}
	a.Bot.SetRunContext(ctx)
	if opts.Agent.Command == "grok" {
		a.Bot.SetUsage(NewGrokUsage(home))
	}

	// Everything the assistant says leaves through the bot's Emit, so a reminder
	// or a task result lands in the app history as well as in Telegram.
	a.TaskMgr, err = NewTaskManager(ctx, st, cfg.OwnerID, opts, workdir, home, a.Bot, cfg.OwnerTelegram)
	if err != nil {
		return nil, err
	}
	a.Bot.SetTasks(a.TaskMgr)

	a.Sched, err = NewScheduler(ctx, st, cfg.OwnerID, opts, workdir, home, a.Bot, cfg.OwnerTelegram)
	if err != nil {
		return nil, err
	}
	a.Sched.SetTasks(a.TaskMgr)

	if err := WriteAgentsGuide(workdir, filepath.Join(cfg.DataDir, "inbox")); err != nil {
		log.Printf("assistant: could not write AGENTS.md: %v", err)
	}
	return a, nil
}

// resolveBotToken picks the Telegram credential: the environment wins, else the
// copy sealed in assistant_meta, else a clear failure. A first boot with an env
// token seals it so a later restart works without it.
func resolveBotToken(ctx context.Context, st *store.Store, box *cryptox.Box, cfg Config) (string, error) {
	if token := strings.TrimSpace(cfg.TelegramBotToken); token != "" {
		sealed, err := box.Seal(token)
		if err != nil {
			return "", fmt.Errorf("seal telegram token: %w", err)
		}
		if err := st.SetAssistantMeta(ctx, cfg.OwnerID, store.MetaBotTokenEnc, sealed); err != nil {
			log.Printf("assistant: could not persist the telegram token: %v", err)
		}
		return token, nil
	}
	sealed, err := st.AssistantMeta(ctx, cfg.OwnerID, store.MetaBotTokenEnc)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sealed) == "" {
		return "", fmt.Errorf("assistant: no Telegram credential — set TELEGRAM_BOT_TOKEN once so it can be sealed into the database")
	}
	token, err := box.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("assistant: stored Telegram token could not be decrypted (wrong session key?): %w", err)
	}
	return token, nil
}

// Run starts the background workers and then the Telegram poll loop, blocking
// until the context ends.
func (a *Assistant) Run(ctx context.Context) error {
	a.ctx = ctx
	a.Bot.SetRunContext(ctx)

	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, a.dataDir); err != nil {
		log.Printf("assistant: legacy state import: %v", err)
	}

	a.TaskMgr.Start(ctx)
	go a.Sched.Run(ctx)
	go a.sweepJobResults(ctx)

	if !a.Opts.Enabled {
		log.Printf("assistant: disabled in the panel; the app channel and the CLIs stay available")
		<-ctx.Done()
		return nil
	}
	log.Printf("assistant: starting (data dir %s, workspace %s, soft budget %s, hard cap %s)",
		a.dataDir, a.workdir, a.Opts.SoftTimeout(), a.Opts.TaskTimeout())
	return a.Bot.Run(ctx)
}

// Notify sends an operator message to the owner's Telegram DM. It replaces the
// notifier the Telegram channel layer used to provide.
func (a *Assistant) Notify(ctx context.Context, userID, text string) error {
	if userID != "" && userID != a.OwnerID {
		return fmt.Errorf("assistant: %s is not the operator", userID)
	}
	return a.Bot.Emit(ctx, Outbound{Text: text, Source: SourceSend})
}

// SendTelegram delivers a message to a chat, defaulting to the owner DM. It
// backs the telegram_send MCP tool.
func (a *Assistant) SendTelegram(ctx context.Context, chatID int64, text, parseMode string) (int64, error) {
	if chatID == 0 {
		chatID = a.OwnerTelegram
	}
	return a.Bot.tg.SendMessage(ctx, chatID, text, parseMode)
}

// Status is the panel and takan_status view of the assistant.
type Status struct {
	Enabled       bool
	BotUsername   string
	OwnerTelegram int64
	KnownChats    int
	RunningTasks  int
	ScheduledJobs int
	PushDevices   int
	Messages      int
	PollHealthy   bool
	LastPollOK    time.Time
	LastPollError string
}

// Status snapshots the assistant for the panel and the status tool.
func (a *Assistant) Status(ctx context.Context) Status {
	s := Status{
		Enabled:       a.Opts.Enabled,
		BotUsername:   a.Bot.Username(),
		OwnerTelegram: a.OwnerTelegram,
		RunningTasks:  countRunningTasks(a.TaskMgr),
		ScheduledJobs: len(a.Sched.Store().List()),
	}
	s.PollHealthy, s.LastPollOK, s.LastPollError = a.Bot.PollHealth()
	if chats, err := a.Store.ListAssistantChats(ctx, a.OwnerID); err == nil {
		s.KnownChats = len(chats)
	}
	if devices, err := a.Store.ListPushDevices(ctx, a.OwnerID); err == nil {
		s.PushDevices = len(devices)
	}
	if n, err := a.Store.CountAssistantMessages(ctx, a.OwnerID); err == nil {
		s.Messages = n
	}
	return s
}

// Workdir is the agent workspace, exposed for the panel.
func (a *Assistant) Workdir() string { return a.workdir }

// Jobs lists the scheduled reminders and routines, for the panel.
func (a *Assistant) Jobs() []Job { return a.Sched.Store().List() }

// Tasks lists the background tasks, newest first, for the panel.
func (a *Assistant) Tasks() []Task { return a.TaskMgr.List() }

// KillTask stops a running background task.
func (a *Assistant) KillTask(id string) (bool, error) { return a.TaskMgr.Kill(id) }

// DeleteJob removes a scheduled job.
func (a *Assistant) DeleteJob(id string) (bool, error) { return a.Sched.Store().Delete(id) }

// RunJob fires a scheduled job once, now.
func (a *Assistant) RunJob(ctx context.Context, id string) error { return a.Sched.RunNow(ctx, id) }
