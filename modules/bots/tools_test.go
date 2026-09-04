package bots

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/mcp"
	"github.com/kidandcat/takan/internal/store"
)

func toolMap(t *testing.T, list []mcp.RegisteredTool) map[string]mcp.RegisteredTool {
	t.Helper()
	out := map[string]mcp.RegisteredTool{}
	for _, tl := range list {
		out[tl.Tool.Name] = tl
	}
	return out
}

func TestBotsToolsApproveDenyFlow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.st.ReportBotChat(ctx, f.bot.ID, f.user.ID, store.BotChat{
		ChatID: "-100777", Type: "supergroup", Title: "Casa", FirstMessage: "hola",
	}); err != nil {
		t.Fatal(err)
	}

	tools := toolMap(t, Factory(f.st, f.srv.Watch)(ctx, f.user.ID))
	for _, name := range []string{"bots_list", "bots_chats", "bots_approve", "bots_deny"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing tool %s", name)
		}
	}

	out, err := tools["bots_list"].Handler(ctx, f.user.ID, nil)
	if err != nil || !strings.Contains(out, "test-bot") {
		t.Fatalf("bots_list: %v %s", err, out)
	}

	out, err = tools["bots_chats"].Handler(ctx, f.user.ID, map[string]any{"status": "pending"})
	if err != nil || !strings.Contains(out, "Casa") || !strings.Contains(out, "group") {
		t.Fatalf("bots_chats: %v %s", err, out)
	}

	if _, err := tools["bots_chats"].Handler(ctx, f.user.ID, map[string]any{"status": "nope"}); err == nil {
		t.Fatal("invalid status should error")
	}
	if _, err := tools["bots_approve"].Handler(ctx, f.user.ID, map[string]any{"bot": "ghost", "chat_id": "1"}); err == nil {
		t.Fatal("unknown bot should error")
	}
	if _, err := tools["bots_approve"].Handler(ctx, f.user.ID, map[string]any{"bot": "test-bot", "chat_id": "404"}); err == nil {
		t.Fatal("unknown chat should error")
	}

	out, err = tools["bots_approve"].Handler(ctx, f.user.ID, map[string]any{"bot": "test-bot", "chat_id": "-100777"})
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res["status"] != store.BotChatApproved || res["type"] != store.BotChatGroup {
		t.Fatalf("approve result: %v", res)
	}

	if _, err := tools["bots_deny"].Handler(ctx, f.user.ID, map[string]any{"bot": "test-bot", "chat_id": "-100777"}); err != nil {
		t.Fatal(err)
	}
	c, err := f.st.BotChat(ctx, f.bot.ID, "-100777")
	if err != nil || c.Status != store.BotChatDenied || c.DecidedBy != "mcp" {
		t.Fatalf("deny: %v %+v", err, c)
	}
}
