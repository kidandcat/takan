package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.telegram.org"

type botUser struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
	First    string `json:"first_name"`
}

type sendResult struct {
	MessageID int64  `json:"message_id"`
	ChatID    int64  `json:"chat,omitempty"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
}

type discoveredChat struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
	First    string `json:"first_name,omitempty"`
	Last     string `json:"last_name,omitempty"`
}

func apiDo(ctx context.Context, token, method string, body any) ([]byte, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("bot token required")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := fmt.Sprintf("%s/bot%s/%s", apiBase, token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("telegram %s: invalid json (%d): %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !envelope.OK {
		desc := strings.TrimSpace(envelope.Description)
		if desc == "" {
			desc = strings.TrimSpace(string(raw))
		}
		return nil, fmt.Errorf("telegram %s: %s", method, desc)
	}
	return envelope.Result, nil
}

// GetMe validates the bot token and returns the bot username (without @).
func GetMe(ctx context.Context, token string) (botUser, error) {
	raw, err := apiDo(ctx, token, "getMe", map[string]any{})
	if err != nil {
		return botUser{}, err
	}
	var u botUser
	if err := json.Unmarshal(raw, &u); err != nil {
		return botUser{}, err
	}
	if u.Username == "" && u.First == "" {
		return botUser{}, fmt.Errorf("telegram getMe: empty bot profile")
	}
	return u, nil
}

// SendMessage posts text to chatID. parseMode may be "", "HTML", or "Markdown" / "MarkdownV2".
func SendMessage(ctx context.Context, token, chatID, text, parseMode string) (messageID int64, err error) {
	chatID = strings.TrimSpace(chatID)
	text = strings.TrimSpace(text)
	if chatID == "" {
		return 0, fmt.Errorf("chat_id required")
	}
	if text == "" {
		return 0, fmt.Errorf("text required")
	}
	// Telegram hard limit is 4096 characters.
	if len([]rune(text)) > 4096 {
		return 0, fmt.Errorf("text exceeds Telegram 4096 character limit (%d runes)", len([]rune(text)))
	}
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	parseMode = strings.TrimSpace(parseMode)
	switch strings.ToLower(parseMode) {
	case "", "plain", "none", "text":
		// plain
	case "html":
		payload["parse_mode"] = "HTML"
	case "markdown", "md":
		payload["parse_mode"] = "Markdown"
	case "markdownv2", "mdv2":
		payload["parse_mode"] = "MarkdownV2"
	default:
		return 0, fmt.Errorf("parse_mode must be empty, HTML, Markdown, or MarkdownV2 (got %q)", parseMode)
	}
	raw, err := apiDo(ctx, token, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &msg)
	return msg.MessageID, nil
}

// DiscoverChats calls getUpdates once and returns unique chats that recently messaged the bot.
// Note: this consumes updates for the bot token; avoid if another poller shares the same bot.
func DiscoverChats(ctx context.Context, token string) ([]discoveredChat, error) {
	// offset=-1 with limit=1 is sometimes used to skip backlog; we want recent history.
	// Use getUpdates without long poll; limit 100.
	raw, err := apiDo(ctx, token, "getUpdates", map[string]any{
		"limit":      100,
		"timeout":    0,
		"allowed_updates": []string{"message", "channel_post", "my_chat_member"},
	})
	if err != nil {
		return nil, err
	}
	var updates []struct {
		Message *struct {
			Chat struct {
				ID        int64  `json:"id"`
				Type      string `json:"type"`
				Title     string `json:"title"`
				Username  string `json:"username"`
				FirstName string `json:"first_name"`
				LastName  string `json:"last_name"`
			} `json:"chat"`
		} `json:"message"`
		ChannelPost *struct {
			Chat struct {
				ID       int64  `json:"id"`
				Type     string `json:"type"`
				Title    string `json:"title"`
				Username string `json:"username"`
			} `json:"chat"`
		} `json:"channel_post"`
		MyChatMember *struct {
			Chat struct {
				ID        int64  `json:"id"`
				Type      string `json:"type"`
				Title     string `json:"title"`
				Username  string `json:"username"`
				FirstName string `json:"first_name"`
				LastName  string `json:"last_name"`
			} `json:"chat"`
		} `json:"my_chat_member"`
	}
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []discoveredChat
	add := func(id int64, typ, title, username, first, last string) {
		sid := fmt.Sprintf("%d", id)
		if id == 0 || seen[sid] {
			return
		}
		seen[sid] = true
		out = append(out, discoveredChat{
			ID: sid, Type: typ, Title: title, Username: username, First: first, Last: last,
		})
	}
	for _, u := range updates {
		if u.Message != nil {
			c := u.Message.Chat
			add(c.ID, c.Type, c.Title, c.Username, c.FirstName, c.LastName)
		}
		if u.ChannelPost != nil {
			c := u.ChannelPost.Chat
			add(c.ID, c.Type, c.Title, c.Username, "", "")
		}
		if u.MyChatMember != nil {
			c := u.MyChatMember.Chat
			add(c.ID, c.Type, c.Title, c.Username, c.FirstName, c.LastName)
		}
	}
	return out, nil
}

// FormatChatLabel builds a short human label for a discovered chat.
func FormatChatLabel(c discoveredChat) string {
	if c.Title != "" {
		return c.Title
	}
	name := strings.TrimSpace(c.First + " " + c.Last)
	if name != "" {
		if c.Username != "" {
			return name + " (@" + c.Username + ")"
		}
		return name
	}
	if c.Username != "" {
		return "@" + c.Username
	}
	if c.Type != "" {
		return c.Type + " " + c.ID
	}
	return c.ID
}
