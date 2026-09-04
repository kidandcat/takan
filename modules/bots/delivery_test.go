package bots

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/store"
)

func deliveryIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["deliveries"].([]any)
	ids := make([]string, 0, len(raw))
	for _, r := range raw {
		d, _ := r.(map[string]any)
		id, _ := d["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestDeliveriesFetchAckLifecycle(t *testing.T) {
	f := newFixture(t)

	w, out := f.do(t, "GET", "/api/bots/deliveries", f.token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("empty outbox: %d %s", w.Code, w.Body.String())
	}
	if len(deliveryIDs(t, out)) != 0 {
		t.Fatalf("expected empty outbox, got %v", out)
	}

	created, err := f.st.EnqueueBotDelivery(f.ctx(), f.bot.ID, f.user.ID,
		store.BotDeliveryAIJobResult, "ai_job_result:job-1", []byte(`{"job_id":"job-1","status":"done"}`))
	if err != nil || !created {
		t.Fatalf("enqueue: %v created=%v", err, created)
	}
	// Same dedupe key while still queued: no duplicate.
	again, err := f.st.EnqueueBotDelivery(f.ctx(), f.bot.ID, f.user.ID,
		store.BotDeliveryAIJobResult, "ai_job_result:job-1", []byte(`{"job_id":"job-1","status":"done"}`))
	if err != nil || again {
		t.Fatalf("dedupe: %v created=%v", err, again)
	}

	w, out = f.do(t, "GET", "/api/bots/deliveries", f.token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", w.Code, w.Body.String())
	}
	ids := deliveryIDs(t, out)
	if len(ids) != 1 {
		t.Fatalf("expected 1 delivery, got %v", out)
	}
	list, _ := out["deliveries"].([]any)
	d, _ := list[0].(map[string]any)
	if d["type"] != store.BotDeliveryAIJobResult {
		t.Fatalf("type: %v", d)
	}
	payload, _ := d["payload"].(map[string]any)
	if payload["job_id"] != "job-1" {
		t.Fatalf("payload: %v", d)
	}

	// Still in flight (visibility timeout), so a second poll returns nothing.
	if _, out2 := f.do(t, "GET", "/api/bots/deliveries", f.token, ""); len(deliveryIDs(t, out2)) != 0 {
		t.Fatalf("in-flight delivery handed out twice: %v", out2)
	}

	body, _ := json.Marshal(map[string]any{"ids": ids})
	w, out = f.do(t, "POST", "/api/bots/deliveries/ack", f.token, string(body))
	if w.Code != http.StatusOK || out["acked"] != float64(1) || out["pending"] != float64(0) {
		t.Fatalf("ack: %d %v", w.Code, out)
	}
	// Acking twice is harmless.
	if _, out2 := f.do(t, "POST", "/api/bots/deliveries/ack", f.token, string(body)); out2["acked"] != float64(0) {
		t.Fatalf("re-ack should be a no-op: %v", out2)
	}
	if n, _ := f.st.CountBotDeliveries(f.ctx(), f.bot.ID); n != 0 {
		t.Fatalf("pending after ack: %d", n)
	}
}

func TestDeliveriesAreTenantScoped(t *testing.T) {
	f := newFixture(t)
	other, err := f.st.CreateUserOpts(f.ctx(), "other-bots@example.com", "password2",
		store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	otherBot, otherToken, err := f.st.CreateBot(f.ctx(), other.ID, "other-bot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetModuleEnabled(f.ctx(), other.ID, "bots", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.EnqueueBotDelivery(f.ctx(), f.bot.ID, f.user.ID,
		store.BotDeliveryAIJobResult, "", []byte(`{"job_id":"mine"}`)); err != nil {
		t.Fatal(err)
	}
	if _, out := f.do(t, "GET", "/api/bots/deliveries", otherToken, ""); len(deliveryIDs(t, out)) != 0 {
		t.Fatalf("bot %s saw another bot's outbox: %v", otherBot.Name, out)
	}
}

func TestDeliveriesLongPollWakesOnEnqueue(t *testing.T) {
	f := newFixture(t)
	go func() {
		time.Sleep(80 * time.Millisecond)
		if _, err := f.st.EnqueueBotDelivery(f.ctx(), f.bot.ID, f.user.ID,
			store.BotDeliveryAIJobResult, "", []byte(`{"job_id":"late"}`)); err == nil {
			f.srv.Watch.NotifyDeliveries(f.bot.ID)
		}
	}()
	start := time.Now()
	w, out := f.do(t, "GET", "/api/bots/deliveries?wait=5", f.token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("long poll: %d", w.Code)
	}
	if len(deliveryIDs(t, out)) != 1 {
		t.Fatalf("long poll returned %v", out)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("long poll did not wake early: %s", time.Since(start))
	}
}

func TestJobDeliveryEnqueuesForOwningBot(t *testing.T) {
	f := newFixture(t)
	d := &JobDelivery{Store: f.st, Watch: f.srv.Watch}

	// Attributed at launch, with the Telegram chat that asked.
	if err := f.st.RecordBotJob(f.ctx(), "job-42", f.bot.ID, f.user.ID, "282611642", "vps2"); err != nil {
		t.Fatal(err)
	}
	job := agenthub.AIJob{
		JobID: "job-42", Runner: "grok", Status: "done", ExitCode: 0,
		Output: "all good", FinishedAt: "2026-09-04T10:00:00Z",
	}
	if err := d.deliver(f.ctx(), f.user.ID, "vps2", job); err != nil {
		t.Fatal(err)
	}
	list, err := f.st.ListBotDeliveries(f.ctx(), f.bot.ID, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("deliveries: %v %v", err, list)
	}
	var res AIJobResult
	if err := json.Unmarshal([]byte(list[0].Payload), &res); err != nil {
		t.Fatal(err)
	}
	if res.JobID != "job-42" || res.Status != "done" || res.Machine != "vps2" ||
		res.RequestedChatID != "282611642" || res.OutputTail != "all good" {
		t.Fatalf("payload: %+v", res)
	}

	// A job whose owner is not a registered bot delivers nowhere.
	stray := agenthub.AIJob{JobID: "job-99", Status: "done", Owner: "NotABot"}
	if err := d.deliver(f.ctx(), f.user.ID, "vps2", stray); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.st.CountBotDeliveries(f.ctx(), f.bot.ID); n != 1 {
		t.Fatalf("unknown owner should not enqueue, pending=%d", n)
	}
}

func TestJobDeliveryFallsBackToOwnerName(t *testing.T) {
	f := newFixture(t)
	d := &JobDelivery{Store: f.st, Watch: f.srv.Watch}
	// No bot_jobs row: a job launched before this module, carrying the owner name.
	job := agenthub.AIJob{JobID: "legacy-job", Status: "failed", ExitCode: 2, Owner: "test-bot"}
	if err := d.deliver(f.ctx(), f.user.ID, "vps2", job); err != nil {
		t.Fatal(err)
	}
	list, _ := f.st.ListBotDeliveries(f.ctx(), f.bot.ID, 0)
	if len(list) != 1 {
		t.Fatalf("expected fallback delivery, got %v", list)
	}
	var res AIJobResult
	if err := json.Unmarshal([]byte(list[0].Payload), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.ExitCode != 2 || res.RequestedChatID != "" {
		t.Fatalf("payload: %+v", res)
	}
}

func TestJobDeliveryIgnoresRunningJobs(t *testing.T) {
	f := newFixture(t)
	d := &JobDelivery{Store: f.st, Watch: f.srv.Watch}
	d.OnJobEvent(f.user.ID, "vps2", agenthub.AIJob{JobID: "job-run", Status: "running", Owner: "test-bot"})
	time.Sleep(50 * time.Millisecond)
	if n, _ := f.st.CountBotDeliveries(f.ctx(), f.bot.ID); n != 0 {
		t.Fatalf("running job enqueued a delivery: %d", n)
	}
}

func TestOutputTailIsTruncated(t *testing.T) {
	truncated := false
	long := strings.Repeat("x", OutputTailBytes+500)
	got := tailOf(long, OutputTailBytes, &truncated)
	if !truncated || !strings.HasPrefix(got, "…") || len(got) != OutputTailBytes+len("…") {
		t.Fatalf("tailOf len=%d truncated=%v", len(got), truncated)
	}
	if short := tailOf("hi", OutputTailBytes, &truncated); short != "hi" {
		t.Fatalf("short tail changed: %q", short)
	}
}

func TestJobDeliverySkippedWhenModuleDisabled(t *testing.T) {
	f := newFixture(t)
	if err := f.st.SetModuleEnabled(f.ctx(), f.user.ID, "bots", false); err != nil {
		t.Fatal(err)
	}
	d := &JobDelivery{Store: f.st, Watch: f.srv.Watch}
	job := agenthub.AIJob{JobID: "job-off", Status: "done", Owner: "test-bot"}
	if err := d.deliver(f.ctx(), f.user.ID, "vps2", job); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.st.CountBotDeliveries(f.ctx(), f.bot.ID); n != 0 {
		t.Fatalf("disabled module still queued %d deliveries", n)
	}
}
