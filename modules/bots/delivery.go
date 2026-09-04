package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/store"
)

// OutputTailBytes is how much of the job transcript travels with a result
// delivery. The daemon can always fetch the full log with machine_ai_log.
const OutputTailBytes = 4000

// AIJobResult is the payload of an ai_job_result delivery. The daemon forwards
// it to RequestedChatID (when set) or to its default operator chat.
type AIJobResult struct {
	JobID           string `json:"job_id"`
	Machine         string `json:"machine,omitempty"`
	Runner          string `json:"runner,omitempty"`
	Status          string `json:"status"`
	ExitCode        int    `json:"exit_code"`
	ParentJobID     string `json:"parent_job_id,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	OutputTail      string `json:"output_tail,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
	Error           string `json:"error,omitempty"`
	RequestedChatID string `json:"requested_chat_id,omitempty"`
}

// TailFetcher returns the log tail of a finished job (best-effort).
type TailFetcher func(ctx context.Context, userID, machine, jobID string, maxBytes int) (string, bool)

// JobDelivery enqueues machine AI job results into the owning bot's outbox.
// It replaces the old Grok Bot webhook: nothing is POSTed anywhere, the daemon
// pulls the delivery from /api/bots/deliveries and acks it.
type JobDelivery struct {
	Store *store.Store
	Watch *Watcher
	// Tail optional: fetches the transcript tail to include in the payload.
	Tail TailFetcher
}

// OnJobEvent is wired to agenthub.Hub.OnJobEvent. Non-terminal events and jobs
// that belong to no bot are ignored. Safe to call from the agent WS goroutine:
// the (possibly slow) tail fetch happens on its own goroutine.
func (d *JobDelivery) OnJobEvent(userID, machineName string, job agenthub.AIJob) {
	if d == nil || d.Store == nil || job.JobID == "" || !agenthub.JobTerminal(job.Status) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := d.deliver(ctx, userID, machineName, job); err != nil {
			log.Printf("bots: ai job delivery %s: %v", job.JobID, err)
		}
	}()
}

func (d *JobDelivery) deliver(ctx context.Context, userID, machineName string, job agenthub.AIJob) error {
	// A disabled module means no daemon may poll, so queueing would only pile up.
	if on, err := d.Store.ModuleEnabled(ctx, userID, "bots"); err != nil || !on {
		return err
	}
	bot, chatID, err := d.resolve(ctx, userID, job)
	if err != nil || bot == nil {
		return err
	}
	machine := machineName
	runner := job.Runner
	if runner == "" {
		runner = job.Agent
	}
	res := AIJobResult{
		JobID: job.JobID, Machine: machine, Runner: runner, Status: job.Status,
		ExitCode: job.ExitCode, ParentJobID: job.ParentJobID, FinishedAt: job.FinishedAt,
		Error: job.Error, RequestedChatID: chatID,
	}
	res.OutputTail, res.Truncated = job.Output, job.Truncated
	if res.OutputTail == "" && d.Tail != nil && machine != "" {
		res.OutputTail, res.Truncated = d.Tail(ctx, userID, machine, job.JobID, OutputTailBytes)
	}
	res.OutputTail = tailOf(res.OutputTail, OutputTailBytes, &res.Truncated)

	payload, err := json.Marshal(res)
	if err != nil {
		return err
	}
	created, err := d.Store.EnqueueBotDelivery(ctx, bot.ID, bot.UserID,
		store.BotDeliveryAIJobResult, store.BotDeliveryAIJobResult+":"+job.JobID, payload)
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if created {
		d.Watch.NotifyDeliveries(bot.ID)
	}
	return nil
}

// resolve finds the bot a job belongs to: the attribution recorded at launch
// first, then the free-text owner as a fallback (jobs launched before this
// module, or by a client that passed an owner name directly).
func (d *JobDelivery) resolve(ctx context.Context, userID string, job agenthub.AIJob) (*store.Bot, string, error) {
	if link, err := d.Store.BotJobByID(ctx, job.JobID); err == nil && link != nil {
		bot, err := d.Store.BotByID(ctx, link.UserID, link.BotID)
		if err != nil {
			if NotFound(err) {
				return nil, "", nil // bot removed while the job was running
			}
			return nil, "", err
		}
		return bot, link.ChatID, nil
	} else if err != nil && !NotFound(err) {
		return nil, "", err
	}
	owner := strings.TrimSpace(job.Owner)
	if owner == "" {
		return nil, "", nil
	}
	bot, err := d.Store.BotByUserAndName(ctx, userID, owner)
	if err != nil {
		if NotFound(err) {
			return nil, "", nil // owner is not a registered bot: nothing to deliver
		}
		return nil, "", err
	}
	return bot, "", nil
}

// tailOf keeps the last n bytes of s, marking truncated when it had to cut.
func tailOf(s string, n int, truncated *bool) string {
	if len(s) <= n {
		return s
	}
	if truncated != nil {
		*truncated = true
	}
	return "…" + s[len(s)-n:]
}

// HubTail adapts agenthub to TailFetcher.
func HubTail(hub interface {
	AIStatus(ctx context.Context, userID, machineName, jobID string, tailBytes int) (*agenthub.AIJob, []agenthub.AIJob, error)
}) TailFetcher {
	return func(ctx context.Context, userID, machine, jobID string, maxBytes int) (string, bool) {
		job, _, err := hub.AIStatus(ctx, userID, machine, jobID, maxBytes)
		if err != nil || job == nil {
			return "", false
		}
		return job.Output, job.Truncated
	}
}
