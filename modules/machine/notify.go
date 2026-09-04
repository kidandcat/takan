package machine

import "github.com/kidandcat/takan/internal/agenthub"

// JobNotification is the body of notifications/takan/machine_ai_job, pushed on
// open MCP SSE streams when a machine AI job reaches a terminal status.
type JobNotification struct {
	Machine     string `json:"machine"`
	JobID       string `json:"job_id"`
	Status      string `json:"status"`
	ExitCode    int    `json:"exit_code"`
	Runner      string `json:"runner"`
	ParentJobID string `json:"parent_job_id"`
	FinishedAt  string `json:"finished_at"`
	Owner       string `json:"owner"`
}

// NotificationFromJob maps a terminal AI job onto the notification fields.
func NotificationFromJob(machine string, job agenthub.AIJob) JobNotification {
	runner := job.Runner
	if runner == "" {
		runner = job.Agent
	}
	return JobNotification{
		Machine:     machine,
		JobID:       job.JobID,
		Status:      job.Status,
		ExitCode:    job.ExitCode,
		Runner:      runner,
		ParentJobID: job.ParentJobID,
		FinishedAt:  job.FinishedAt,
		Owner:       job.Owner,
	}
}
