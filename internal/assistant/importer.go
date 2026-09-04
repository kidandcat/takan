package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// legacyState mirrors state.json from the standalone daemon.
type legacyState struct {
	Offset int64 `json:"offset"`
	Chats  map[string]struct {
		ConversationStarted bool      `json:"conversation_started"`
		SessionID           string    `json:"session_id"`
		ForkFrom            string    `json:"fork_from"`
		LastRunAt           time.Time `json:"last_run_at"`
		Runs                int64     `json:"runs"`
	} `json:"chats"`
	Push map[string]struct {
		Platform  string    `json:"platform"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"push"`
}

// legacyMessage mirrors one entry of app-messages.json.
type legacyMessage struct {
	ID        string       `json:"id"`
	Role      string       `json:"role"`
	Text      string       `json:"text"`
	Files     []Attachment `json:"files"`
	Source    string       `json:"source"`
	CreatedAt time.Time    `json:"created_at"`
}

// legacyJob mirrors one entry of jobs.json.
type legacyJob struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Payload    string    `json:"payload"`
	Cron       string    `json:"cron"`
	At         time.Time `json:"at"`
	ChatID     int64     `json:"chat_id"`
	NextRun    time.Time `json:"next_run"`
	LastRun    time.Time `json:"last_run"`
	LastStatus string    `json:"last_status"`
	Runs       int64     `json:"runs"`
	CreatedAt  time.Time `json:"created_at"`
}

// legacyTask mirrors one entry of tasks.json.
type legacyTask struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Prompt     string    `json:"prompt"`
	State      string    `json:"state"`
	PID        int       `json:"pid"`
	Dir        string    `json:"dir"`
	OutputPath string    `json:"output_path"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error"`
	SessionID  string    `json:"session_id"`
	Promoted   bool      `json:"promoted"`
	ChatID     int64     `json:"chat_id"`
}

// processAlive is a seam over kill(pid, 0), so the importer can be tested
// without spawning processes.
var processAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM)
}

// ImportLegacyJSON moves the standalone daemon's JSON state into SQLite.
//
// It is idempotent (inserts ignore existing ids), non-destructive (each file is
// renamed to <name>.imported on success, never deleted) and tolerant: a missing
// or unreadable file is skipped with a log line rather than failing the boot.
func ImportLegacyJSON(ctx context.Context, st *store.Store, userID, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	var problems []string
	for _, step := range []struct {
		file string
		run  func(context.Context, *store.Store, string, []byte) (int, error)
	}{
		{"state.json", importState},
		{"app-messages.json", importMessages},
		{"jobs.json", importJobs},
		{"tasks.json", importTasks},
	} {
		path := filepath.Join(dir, step.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", step.file, err))
			}
			continue
		}
		n, err := step.run(ctx, st, userID, raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", step.file, err))
			continue
		}
		log.Printf("assistant: imported %d row(s) from %s", n, path)
		if err := os.Rename(path, path+".imported"); err != nil {
			problems = append(problems, fmt.Sprintf("rename %s: %v", step.file, err))
		}
	}
	// chats.json held the old approval whitelist. There is no whitelist any
	// more, so it is retired rather than imported.
	if path := filepath.Join(dir, "chats.json"); fileExists(path) {
		if err := os.Rename(path, path+".obsolete"); err != nil {
			problems = append(problems, fmt.Sprintf("retire chats.json: %v", err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("legacy import: %s", strings.Join(problems, "; "))
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func importState(ctx context.Context, st *store.Store, userID string, raw []byte) (int, error) {
	var state legacyState
	if err := json.Unmarshal(raw, &state); err != nil {
		return 0, err
	}
	n := 0
	if state.Offset > 0 {
		// The offset is the one value that must not be re-imported over a newer
		// one: rewinding it would replay updates the assistant already answered.
		current, err := st.AssistantMeta(ctx, userID, store.MetaTelegramOffset)
		if err != nil {
			return 0, err
		}
		existing, _ := strconv.ParseInt(current, 10, 64)
		if state.Offset > existing {
			if err := st.SetAssistantMeta(ctx, userID, store.MetaTelegramOffset,
				strconv.FormatInt(state.Offset, 10)); err != nil {
				return 0, err
			}
			n++
		}
	}
	for chatID, chat := range state.Chats {
		existing, err := st.AssistantChatState(ctx, userID, chatID)
		if err != nil {
			return n, err
		}
		if existing.SessionID != "" || existing.Runs > 0 {
			continue // already migrated, or live
		}
		if err := st.SeeAssistantChat(ctx, userID, chatID, "", ""); err != nil {
			return n, err
		}
		if chat.SessionID != "" {
			if err := st.MarkConversationStarted(ctx, userID, chatID, chat.SessionID); err != nil {
				return n, err
			}
			if chat.ForkFrom != "" {
				if err := st.MarkConversationPromoted(ctx, userID, chatID, chat.ForkFrom); err != nil {
					return n, err
				}
			}
			if !chat.ConversationStarted {
				if err := st.ResetAssistantConversation(ctx, userID, chatID); err != nil {
					return n, err
				}
			}
		}
		n++
	}
	for token, reg := range state.Push {
		if err := st.RegisterPushDevice(ctx, userID, token, reg.Platform); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func importMessages(ctx context.Context, st *store.Store, userID string, raw []byte) (int, error) {
	var messages []legacyMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return 0, err
	}
	n := 0
	for _, m := range messages {
		if m.ID == "" {
			m.ID = newMessageID()
		}
		files := "[]"
		if len(m.Files) > 0 {
			if encoded, err := json.Marshal(m.Files); err == nil {
				files = string(encoded)
			}
		}
		if err := st.AppendAssistantMessage(ctx, userID, store.AssistantMessage{
			ID: m.ID, Role: m.Role, Text: m.Text, FilesJSON: files,
			Source: m.Source, CreatedAt: m.CreatedAt,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func importJobs(ctx context.Context, st *store.Store, userID string, raw []byte) (int, error) {
	var jobs []legacyJob
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return 0, err
	}
	existing := map[string]bool{}
	rows, err := st.ListAssistantJobs(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		existing[r.ID] = true
	}

	n := 0
	for _, j := range jobs {
		if j.ID == "" || existing[j.ID] {
			continue
		}
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
		if err := st.SaveAssistantJob(ctx, userID, row); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func importTasks(ctx context.Context, st *store.Store, userID string, raw []byte) (int, error) {
	var tasks []legacyTask
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return 0, err
	}
	existing := map[string]bool{}
	rows, err := st.ListAssistantTasks(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		existing[r.ID] = true
	}

	n := 0
	for _, t := range tasks {
		if t.ID == "" || existing[t.ID] {
			continue
		}
		state, errMsg, finished := t.State, t.Error, t.FinishedAt
		// A task the old daemon believed was running is only still running if
		// its pid is: otherwise it died with the process that owned it.
		if state == TaskRunning && !processAlive(t.PID) {
			state = TaskOrphaned
			errMsg = "the daemon was replaced while this task was running"
			if finished.IsZero() {
				finished = time.Now().UTC()
			}
		}
		row := store.AssistantTask{
			ID: t.ID, Title: t.Title, Prompt: t.Prompt, State: state, PID: t.PID,
			SessionID: t.SessionID, Dir: t.Dir, OutputPath: t.OutputPath,
			Promoted: t.Promoted, StartedAt: t.StartedAt, Error: errMsg,
		}
		if t.ChatID != 0 {
			row.ChatID = strconv.FormatInt(t.ChatID, 10)
		}
		if !finished.IsZero() {
			row.FinishedAt = &finished
		}
		if err := st.SaveAssistantTask(ctx, userID, row); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
