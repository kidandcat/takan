package mcp

import (
	"encoding/json"
	"testing"
)

func TestSessionHubNotify(t *testing.T) {
	h := NewSessionHub()
	s := h.Create("user-a")
	ch := s.addStream()
	t.Cleanup(func() { s.removeStream(ch) })

	n := h.Notify("user-a", "notifications/takan/machine_ai_job", map[string]any{
		"job_id": "j1", "status": "done",
	})
	if n != 1 {
		t.Fatalf("streams=%d", n)
	}
	select {
	case raw := <-ch:
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		if msg["method"] != "notifications/takan/machine_ai_job" {
			t.Fatalf("method=%v", msg["method"])
		}
		params, _ := msg["params"].(map[string]any)
		if params["job_id"] != "j1" || params["status"] != "done" {
			t.Fatalf("params=%v", params)
		}
	default:
		t.Fatal("expected notification on SSE stream")
	}

	if n := h.Notify("other", "notifications/takan/machine_ai_job", nil); n != 0 {
		t.Fatalf("other user streams=%d", n)
	}
}
