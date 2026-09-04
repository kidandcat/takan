package bots

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/store"
)

// TestAgentJobEventReachesBotOutbox wires the pieces the way cmd/takan does:
// agent WebSocket -> hub.OnJobEvent -> JobDelivery -> bot outbox -> daemon API.
func TestAgentJobEventReachesBotOutbox(t *testing.T) {
	f := newFixture(t)

	hub := agenthub.New(func(_ context.Context, _ string) (string, string, string, error) {
		return "machine-1", f.user.ID, "vps2", nil
	}, nil)
	delivery := &JobDelivery{Store: f.st, Watch: f.srv.Watch}
	hub.OnJobEvent = delivery.OnJobEvent

	agentSrv := httptest.NewServer(http.HandlerFunc(hub.HandleWS))
	t.Cleanup(agentSrv.Close)
	ws := "ws" + strings.TrimPrefix(agentSrv.URL, "http") + "?token=agent-token"
	conn, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	waitFor(t, func() bool { return hub.Online("machine-1") }, "agent never registered")

	// The job was launched by the bot on behalf of a Telegram chat.
	if err := f.st.RecordBotJob(f.ctx(), "job-e2e", f.bot.ID, f.user.ID, "282611642", "vps2"); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		"type": "ai_done", "job_id": "job-e2e", "agent": "grok", "runner": "grok",
		"status": "done", "exit_code": 0, "finished_at": "2026-09-04T10:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		n, _ := f.st.CountBotDeliveries(f.ctx(), f.bot.ID)
		return n == 1
	}, "job event never reached the outbox")

	w, out := f.do(t, "GET", "/api/bots/deliveries", f.token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("fetch: %d %s", w.Code, w.Body.String())
	}
	ids := deliveryIDs(t, out)
	if len(ids) != 1 {
		t.Fatalf("deliveries: %v", out)
	}
	list, _ := out["deliveries"].([]any)
	d, _ := list[0].(map[string]any)
	payload, _ := d["payload"].(map[string]any)
	if d["type"] != store.BotDeliveryAIJobResult || payload["job_id"] != "job-e2e" ||
		payload["status"] != "done" || payload["requested_chat_id"] != "282611642" {
		t.Fatalf("payload: %v", d)
	}

	body, _ := json.Marshal(map[string]any{"ids": ids})
	if _, out := f.do(t, "POST", "/api/bots/deliveries/ack", f.token, string(body)); out["pending"] != float64(0) {
		t.Fatalf("ack: %v", out)
	}
	if _, out := f.do(t, "GET", "/api/bots/deliveries", f.token, ""); len(deliveryIDs(t, out)) != 0 {
		t.Fatalf("acked delivery came back: %v", out)
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
