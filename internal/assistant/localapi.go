package assistant

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// LocalRoutes wires the loopback API: health for systemd and Gatus, the
// scheduler and task JSON APIs the CLIs drive, and the app fan-out helper.
// It listens on loopback only, so no authentication is required.
func (a *Assistant) LocalRoutes(mux *http.ServeMux) {
	a.registerJobRoutes(mux)
	a.registerTaskRoutes(mux)
	mux.HandleFunc("POST /internal/app-notify", a.Bot.handleAppNotify)
	mux.HandleFunc("POST /internal/send", a.handleSend)
	mux.HandleFunc("GET /health", a.handleLocalHealth)
}

// handleSend is what atlas-send drives. Routing it through the hub means the
// helper binary needs no Telegram credential of its own: the token stays in one
// process, sealed in the database.
func (a *Assistant) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string `json:"text"`
		File   string `json:"file"`
		ChatID int64  `json:"chat_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" && req.File == "" {
		writeError(w, http.StatusBadRequest, "text or file is required")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()

	// Emit is the one exit for outbound messages: it sends to Telegram and
	// mirrors the owner's copy into the app history in the same step.
	if _, err := a.Bot.Emit(ctx, Outbound{
		ChatID: req.ChatID, Text: text, File: req.File, Source: SourceSend,
	}); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"sent": true})
}

func (a *Assistant) handleLocalHealth(w http.ResponseWriter, r *http.Request) {
	b, sched, tasks := a.Bot, a.Sched, a.TaskMgr
	uptime := time.Since(b.startedAt)
	payload := map[string]any{
		"status":         "ok",
		"started_at":     b.startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds": int64(uptime.Seconds()),
		"uptime":         uptime.Truncate(time.Second).String(),
		"runs":           b.processedRuns.Load(),
		"instance":       InstanceName,
		"scheduled_jobs": len(sched.Store().List()),
		"running_tasks":  countRunningTasks(tasks),
		"bot":            b.Username(),
	}
	if last := b.lastUpdateAt.Load(); last > 0 {
		payload["last_update_at"] = time.Unix(last, 0).UTC().Format(time.RFC3339)
		payload["seconds_since_last_update"] = int64(time.Since(time.Unix(last, 0)).Seconds())
	}

	// A daemon that is up but cannot poll is not healthy: it silently receives
	// nothing. Report that as 503 so Gatus pages.
	healthy, lastOK, lastErr := b.PollHealth()
	if !lastOK.IsZero() {
		payload["last_poll_ok_at"] = lastOK.UTC().Format(time.RFC3339)
	}
	status := http.StatusOK
	if reason := b.StoppedReason(); reason != "" {
		// The loop exited and will not come back: page now, do not wait out the
		// stall threshold.
		status = http.StatusServiceUnavailable
		payload["status"] = "stopped"
		payload["error"] = "the assistant stopped: " + reason
	} else if !a.Opts.Enabled {
		// Deliberately off: healthy, but say so rather than pretending to poll.
		payload["status"] = "disabled"
	} else if !healthy {
		status = http.StatusServiceUnavailable
		payload["status"] = "degraded"
		payload["error"] = "getUpdates has been failing for over " + pollStallThreshold.String()
		if lastErr != "" {
			payload["last_poll_error"] = lastErr
		}
	}

	writeJSON(w, status, payload)
}

// registerJobRoutes wires the scheduler's JSON API.
func (a *Assistant) registerJobRoutes(mux *http.ServeMux) {
	sched := a.Sched
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"timezone": ScheduleLocation,
			"now":      time.Now().In(sched.Location()).Format(time.RFC3339),
			"jobs":     sched.Store().List(),
		})
	})

	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		var job Job
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&job); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		created, err := sched.Store().Add(&job, sched.Location())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, created)
	})

	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, ok := sched.Store().Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	})

	mux.HandleFunc("DELETE /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		removed, err := sched.Store().Delete(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !removed {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
	})

	mux.HandleFunc("POST /jobs/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		// Run detached: an agent job can take minutes, well past any client timeout.
		id := r.PathValue("id")
		if _, ok := sched.Store().Get(id); !ok {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		go func() {
			if err := sched.RunNow(context.Background(), id); err != nil {
				log.Printf("scheduler: manual run of job %s failed: %v", id, err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"started": id})
	})
}

// registerTaskRoutes wires the background task API.
func (a *Assistant) registerTaskRoutes(mux *http.ServeMux) {
	tasks := a.TaskMgr
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks.List()})
	})

	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
			Title  string `json:"title"`
			ChatID int64  `json:"chat_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		// Returns as soon as the task is spawned; the result arrives by Telegram.
		task, err := tasks.Run(req.Prompt, req.Title, req.ChatID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	})

	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		task, ok := tasks.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, task)
	})

	mux.HandleFunc("DELETE /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		killed, err := tasks.Kill(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "killed": killed})
	})
}
