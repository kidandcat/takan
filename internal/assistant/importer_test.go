package assistant

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kidandcat/takan/internal/store"
)

// legacyFixture copies testdata/legacy into a writable directory. The fixtures
// keep the shapes the standalone daemon wrote in production, with every message
// body and token replaced by a placeholder.
func legacyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir("testdata/legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata/legacy", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestImportLegacyJSON(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()
	dir := legacyFixture(t)

	// A dead pid must not be reported as still running.
	processAlive = func(int) bool { return false }
	t.Cleanup(func() { processAlive = defaultProcessAlive })

	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, dir); err != nil {
		t.Fatal(err)
	}

	// state.json → offset, chats, push devices
	offset, err := a.Store.AssistantMeta(ctx, a.OwnerID, store.MetaTelegramOffset)
	if err != nil {
		t.Fatal(err)
	}
	if offset != "246853685" {
		t.Fatalf("the update offset must carry over, got %q", offset)
	}
	chats, err := a.Store.ListAssistantChats(ctx, a.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("expected both chats, got %+v", chats)
	}
	owner, _ := a.Store.AssistantChatState(ctx, a.OwnerID, "282611642")
	if owner.SessionID != "69b79e79-3e04-4e17-ad88-ccaf19ac9752" {
		t.Fatalf("the live session must carry over: %+v", owner)
	}
	if owner.ForkFrom == "" {
		t.Fatalf("a pending fork must carry over: %+v", owner)
	}
	devices, err := a.Store.ListPushDevices(ctx, a.OwnerID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("push devices: %+v err=%v", devices, err)
	}

	// app-messages.json → history, attachments intact
	messages := a.Bot.history.List("", "", 100)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	if messages[0].ID != "26c5ebb91d1cc73c" {
		t.Fatalf("ids must be preserved so the app's cursor still resolves: %+v", messages[0])
	}
	if len(messages[2].Files) != 1 || messages[2].Files[0].Name != "chart.png" {
		t.Fatalf("attachments must survive the import: %+v", messages[2])
	}

	// jobs.json → reminders and routines
	jobs, err := a.Store.ListAssistantJobs(ctx, a.OwnerID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %+v err=%v", jobs, err)
	}
	byID := map[string]store.AssistantJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if got := byID["a1b2c3d4"]; got.Cron != "0 9 * * 1" || got.Runs != 12 {
		t.Fatalf("recurring job lost detail: %+v", got)
	}
	if got := byID["e5f6a7b8"]; got.ChatID != "-1002233445566" || got.At == nil {
		t.Fatalf("one-shot job lost its target or its time: %+v", got)
	}

	// tasks.json → a dead running task becomes orphaned, not a phantom
	tasks, err := a.Store.ListAssistantTasks(ctx, a.OwnerID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks: %+v err=%v", tasks, err)
	}
	byTaskID := map[string]store.AssistantTask{}
	for _, task := range tasks {
		byTaskID[task.ID] = task
	}
	stale := byTaskID["51be991d"]
	if stale.State != TaskOrphaned {
		t.Fatalf("a task whose pid is gone must be orphaned, got %q", stale.State)
	}
	if stale.FinishedAt == nil {
		t.Fatal("an orphaned task needs a finish time or it reports a growing duration forever")
	}
	if !stale.Promoted || stale.ChatID != "282611642" {
		t.Fatalf("promotion and delivery target must survive: %+v", stale)
	}
	if byTaskID["7c8d9e0f"].State != TaskDone {
		t.Fatalf("a finished task keeps its state: %+v", byTaskID["7c8d9e0f"])
	}

	// Files are renamed, never deleted: the originals stay recoverable.
	for _, name := range []string{"state.json", "app-messages.json", "jobs.json", "tasks.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been renamed after import", name)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".imported")); err != nil {
			t.Fatalf("%s.imported is missing: %v", name, err)
		}
	}
	// chats.json held the retired approval whitelist.
	if _, err := os.Stat(filepath.Join(dir, "chats.json.obsolete")); err != nil {
		t.Fatalf("chats.json should be retired, not imported: %v", err)
	}
}

// TestImportLegacyJSONIsIdempotent is what makes the prod cutover safe to retry:
// a second boot must not duplicate anything or resurrect finished work.
func TestImportLegacyJSONIsIdempotent(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()
	processAlive = func(int) bool { return false }
	t.Cleanup(func() { processAlive = defaultProcessAlive })

	first := legacyFixture(t)
	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, first); err != nil {
		t.Fatal(err)
	}
	// Simulate a rerun against an untouched copy of the same data.
	second := legacyFixture(t)
	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, second); err != nil {
		t.Fatal(err)
	}

	if got := len(a.Bot.history.List("", "", 100)); got != 3 {
		t.Fatalf("messages were duplicated: %d", got)
	}
	jobs, _ := a.Store.ListAssistantJobs(ctx, a.OwnerID)
	if len(jobs) != 2 {
		t.Fatalf("jobs were duplicated: %d", len(jobs))
	}
	tasks, _ := a.Store.ListAssistantTasks(ctx, a.OwnerID)
	if len(tasks) != 2 {
		t.Fatalf("tasks were duplicated: %d", len(tasks))
	}
	chats, _ := a.Store.ListAssistantChats(ctx, a.OwnerID)
	if len(chats) != 2 {
		t.Fatalf("chats were duplicated: %d", len(chats))
	}
}

// TestImportNeverRewindsTheOffset: replaying an old state.json over a running
// instance would re-deliver updates the assistant already answered.
func TestImportNeverRewindsTheOffset(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()
	if err := a.Store.SetAssistantMeta(ctx, a.OwnerID, store.MetaTelegramOffset, "999999999"); err != nil {
		t.Fatal(err)
	}
	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, legacyFixture(t)); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Store.AssistantMeta(ctx, a.OwnerID, store.MetaTelegramOffset)
	if got != "999999999" {
		t.Fatalf("a newer offset must win, got %q", got)
	}
}

// TestImportKeepsALiveTask: a task whose pid is still alive was adopted by the
// new process, so it must not be marked orphaned.
func TestImportKeepsALiveTask(t *testing.T) {
	a := newTestAssistant(t)
	processAlive = func(int) bool { return true }
	t.Cleanup(func() { processAlive = defaultProcessAlive })

	if err := ImportLegacyJSON(context.Background(), a.Store, a.OwnerID, legacyFixture(t)); err != nil {
		t.Fatal(err)
	}
	tasks, _ := a.Store.ListAssistantTasks(context.Background(), a.OwnerID)
	for _, task := range tasks {
		if task.ID == "51be991d" && task.State != TaskRunning {
			t.Fatalf("a task with a live pid must stay running, got %q", task.State)
		}
	}
}

func TestImportLegacyTOML(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()
	dir := legacyFixture(t)

	if err := ImportLegacyTOML(ctx, a.Store, a.OwnerID, dir); err != nil {
		t.Fatal(err)
	}
	opts, err := LoadOptions(ctx, a.Store, a.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Agent.SoftTimeout != "90s" || opts.Agent.TaskTimeout != "2h" {
		t.Fatalf("timeouts did not carry over: %+v", opts.Agent)
	}
	if opts.Telegram.PollTimeout != "45s" || opts.Telegram.TypingInterval != "6s" {
		t.Fatalf("telegram tuning did not carry over: %+v", opts.Telegram)
	}
	if len(opts.Agent.Args) == 0 || opts.Agent.Args[1] != promptPlaceholder {
		t.Fatalf("argument template did not carry over: %+v", opts.Agent.Args)
	}
	// Defaults still fill in what the old file never had.
	if opts.Agent.MaxConcurrentTasks != 3 {
		t.Fatalf("new settings must get their default: %d", opts.Agent.MaxConcurrentTasks)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml.imported")); err != nil {
		t.Fatalf("config.toml should be renamed after import: %v", err)
	}
}

// TestImportLegacyTOMLDoesNotClobberThePanel: once the operator has edited the
// settings in the panel, a leftover config.toml must not overwrite them.
func TestImportLegacyTOMLDoesNotClobberThePanel(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()

	edited := DefaultOptions()
	edited.Agent.SoftTimeout = "30s"
	if err := SaveOptions(ctx, a.Store, a.OwnerID, edited); err != nil {
		t.Fatal(err)
	}
	if err := ImportLegacyTOML(ctx, a.Store, a.OwnerID, legacyFixture(t)); err != nil {
		t.Fatal(err)
	}
	opts, _ := LoadOptions(ctx, a.Store, a.OwnerID)
	if opts.Agent.SoftTimeout != "30s" {
		t.Fatalf("the panel value must win, got %q", opts.Agent.SoftTimeout)
	}
}

func TestImportIsANoOpWithoutALegacyDir(t *testing.T) {
	a := newTestAssistant(t)
	ctx := context.Background()
	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, ""); err != nil {
		t.Fatal(err)
	}
	if err := ImportLegacyJSON(ctx, a.Store, a.OwnerID, filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("a missing directory is not an error: %v", err)
	}
	if err := ImportLegacyTOML(ctx, a.Store, a.OwnerID, ""); err != nil {
		t.Fatal(err)
	}
}
