package assistant

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/tg"
)

// jobTailBytes is how much of a finished job's log is quoted back into the chat.
const jobTailBytes = 4000

// jobDeliveryTimeout bounds one delivery attempt, including the tail fetch.
const jobDeliveryTimeout = 20 * time.Second

// jobSweepInterval is how often undelivered job results are retried.
const jobSweepInterval = 60 * time.Second

// jobRetryWindow is how long a result stays worth retrying. Past that the run
// is stale enough that a surprise message would confuse more than it helps.
const jobRetryWindow = time.Hour

// OnJobEvent delivers a finished machine_ai_run back to the chat that asked for
// it. It replaces the bot outbox: job_chats holds the routing, delivered_at is
// both the dedupe guard and the retry queue.
//
// It runs off the agent WebSocket goroutine, so it never blocks the hub.
func (a *Assistant) OnJobEvent(userID, machineName string, job agenthub.AIJob) {
	if !agenthub.JobTerminal(job.Status) || userID != a.OwnerID {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), jobDeliveryTimeout)
		defer cancel()
		if err := a.deliverJobResult(ctx, machineName, job); err != nil {
			log.Printf("assistant: job %s delivery failed (will retry): %v", job.JobID, err)
		}
	}()
}

// deliverJobResult sends one job's outcome and stamps it delivered.
func (a *Assistant) deliverJobResult(ctx context.Context, machineName string, job agenthub.AIJob) error {
	link, err := a.Store.JobChatByID(ctx, job.JobID)
	if err != nil {
		return err
	}
	if link != nil && link.DeliveredAt != nil {
		return nil // already sent; a duplicate ai_done event
	}

	target := a.OwnerTelegram
	if link != nil && link.ChatID != "" {
		if id, err := strconv.ParseInt(link.ChatID, 10, 64); err == nil && id != 0 {
			target = id
		}
	}
	if machineName == "" && link != nil {
		machineName = link.Machine
	}

	// A terminal event does not always carry the output; fetch the tail when it
	// does not, so the reply is useful rather than a bare "done".
	if strings.TrimSpace(job.Output) == "" && machineName != "" && a.Hub != nil {
		if full, _, err := a.Hub.AIStatus(ctx, a.OwnerID, machineName, job.JobID, jobTailBytes); err == nil && full != nil {
			job.Output = full.Output
			job.Truncated = full.Truncated
			if job.ExitCode == 0 {
				job.ExitCode = full.ExitCode
			}
		}
	}

	// Emit mirrors the owner's copy into the durable history, so the phone app
	// shows the same result the chat just received.
	if _, err := a.Bot.Emit(ctx, Outbound{
		ChatID: target, Text: FormatJobResult(machineName, job), Source: SourceJob,
	}); err != nil {
		return err
	}
	if link != nil {
		return a.Store.MarkJobChatDelivered(ctx, job.JobID)
	}
	return nil
}

// sweepJobResults re-delivers results whose first attempt failed. It preserves
// the old outbox's at-least-once property without a deliveries table.
func (a *Assistant) sweepJobResults(ctx context.Context) {
	ticker := time.NewTicker(jobSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pending, err := a.Store.PendingJobChats(ctx, a.OwnerID, time.Now().Add(-jobRetryWindow))
		if err != nil {
			log.Printf("assistant: job sweeper: %v", err)
			continue
		}
		for _, link := range pending {
			if a.Hub == nil || link.Machine == "" {
				continue
			}
			attempt, cancel := context.WithTimeout(ctx, jobDeliveryTimeout)
			job, _, err := a.Hub.AIStatus(attempt, a.OwnerID, link.Machine, link.JobID, jobTailBytes)
			if err != nil || job == nil || !agenthub.JobTerminal(job.Status) {
				cancel()
				continue
			}
			if err := a.deliverJobResult(attempt, link.Machine, *job); err != nil {
				log.Printf("assistant: job %s still undelivered: %v", link.JobID, err)
			}
			cancel()
		}
	}
}

// FormatJobResult renders a finished AI job for a chat message.
func FormatJobResult(machine string, job agenthub.AIJob) string {
	icon := map[string]string{"done": "✅", "failed": "❌", "cancelled": "🛑"}[job.Status]
	if icon == "" {
		icon = "ℹ️"
	}
	header := fmt.Sprintf("%s Job %s — %s", icon, job.JobID, job.Status)

	runner := job.Runner
	if runner == "" {
		runner = job.Agent
	}
	var details []string
	if runner != "" && machine != "" {
		details = append(details, fmt.Sprintf("%s en %s", runner, machine))
	} else if machine != "" {
		details = append(details, machine)
	}
	if job.Status == "failed" && job.ExitCode != 0 {
		details = append(details, fmt.Sprintf("exit %d", job.ExitCode))
	}
	if len(details) > 0 {
		header += " (" + strings.Join(details, ", ") + ")"
	}

	body := strings.TrimSpace(job.Output)
	if body == "" {
		body = "(sin salida)"
	} else if job.Truncated {
		body = "…" + body
	}
	return header + "\n\n" + tg.TruncateRunes(body, jobTailBytes)
}
