// Package jobwebhook POSTs machine AI job terminal events to an optional
// Grok Bot webhook routine (best-effort; never fails the job).
package jobwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
)

const timeout = 5 * time.Second

// Payload is the JSON body sent to the webhook. Field names match the MCP
// notification notifications/takan/machine_ai_job.
type Payload struct {
	Machine     string `json:"machine"`
	JobID       string `json:"job_id"`
	Status      string `json:"status"`
	ExitCode    int    `json:"exit_code"`
	Runner      string `json:"runner"`
	ParentJobID string `json:"parent_job_id"`
	FinishedAt  string `json:"finished_at"`
	Owner       string `json:"owner"`
}

// Client POSTs job events to URL when it is non-empty.
type Client struct {
	URL    string
	Secret string
	HTTP   *http.Client
}

var defaultHTTP = &http.Client{Timeout: timeout}

// PayloadFromJob maps a terminal AI job to the MCP / webhook fields.
func PayloadFromJob(machine string, job agenthub.AIJob) Payload {
	runner := job.Runner
	if runner == "" {
		runner = job.Agent
	}
	return Payload{
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

// Notify POSTs p as JSON. No-op when URL is empty. Errors are logged only.
func (c Client) Notify(p Payload) {
	url := strings.TrimSpace(c.URL)
	if url == "" {
		return
	}
	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("job webhook: marshal: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("job webhook: request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := strings.TrimSpace(c.Secret); secret != "" {
		// Cursor / Grok webhook triggers document Authorization: Bearer <sender key>.
		// Grok Bot routine UI shows URL + sender key but not the header name, so the
		// same key is also sent as X-Webhook-Key and X-Grok-Webhook-Secret.
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-Webhook-Key", secret)
		req.Header.Set("X-Grok-Webhook-Secret", secret)
	}
	client := c.HTTP
	if client == nil {
		client = defaultHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("job webhook: post: %v", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("job webhook: status %d", resp.StatusCode)
	}
}
