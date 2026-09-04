package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxMessageRunes is the hard limit the Telegram Bot API enforces on a text
// message.
const MaxMessageRunes = 4096

// DefaultAPIBase is Telegram's own Bot API host.
const DefaultAPIBase = "https://api.telegram.org"

// Client is a minimal Bot API client built on the standard library.
type Client struct {
	token string
	// base is the API host. It is overridable for a self-hosted Bot API server,
	// and so tests never reach the real Telegram.
	base   string
	client *http.Client
	// pollClient uses a longer timeout because getUpdates blocks server-side.
	pollClient *http.Client
}

// New builds a client for the given bot token.
func New(token string) *Client { return NewWithBase(token, DefaultAPIBase) }

// NewWithBase builds a client against a specific Bot API host.
func NewWithBase(token, base string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultAPIBase
	}
	return &Client{
		token:      strings.TrimSpace(token),
		base:       base,
		client:     &http.Client{Timeout: 60 * time.Second},
		pollClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// apiResponse is the envelope every Bot API method returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

func (c *Client) endpoint(method string) string {
	return c.base + "/bot" + c.token + "/" + method
}

// tokenPlaceholder is what a redacted bot token reads as.
const tokenPlaceholder = "«bot-token»"

// scrubbedError hides the bot token in an error message.
//
// The Bot API puts the credential in the URL path, so every *url.Error from
// http.Client carries it verbatim. Those errors do not stay in the process:
// a failed getUpdates lands in the panel, in takan_status and in /health, and a
// failed download is delivered to Telegram and stored in the conversation. One
// transient DNS failure would publish the bot token to all of them.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }

// Unwrap keeps errors.Is/As working for sentinels like context.Canceled. Only
// Error() is redacted, because only Error() is what callers store and display.
func (e *scrubbedError) Unwrap() error { return e.err }

// scrub redacts the bot token anywhere in an error's message. Every path that
// can produce an error mentioning a request URL must pass through here.
func (c *Client) scrub(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, c.token) {
		return err
	}
	return &scrubbedError{msg: strings.ReplaceAll(msg, c.token, tokenPlaceholder), err: err}
}

// call posts a JSON payload to a Bot API method and decodes result into out.
func (c *Client) call(ctx context.Context, client *http.Client, method string, payload any, out any) error {
	if c.token == "" {
		return fmt.Errorf("telegram %s: bot token is not configured", method)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), bytes.NewReader(body))
	if err != nil {
		return c.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(client, req, method, out)
}

func (c *Client) do(client *http.Client, req *http.Request, method string, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return c.scrub(fmt.Errorf("telegram %s: %w", method, err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read body: %w", method, err)
	}
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: decode body (status %d): %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return &APIError{Method: method, Code: env.ErrorCode, Description: env.Description}
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}

// GetMe returns the bot's own account, used at startup to learn the username.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var user User
	if err := c.call(ctx, c.client, "getMe", map[string]any{}, &user); err != nil {
		return nil, err
	}
	if user.Username == "" && user.FirstName == "" {
		return nil, fmt.Errorf("telegram getMe: empty bot profile")
	}
	return &user, nil
}

// GetUpdates long-polls for new updates starting at offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message"},
	}
	var updates []Update
	if err := c.call(ctx, c.pollClient, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendChatAction shows the "typing…" indicator in the chat.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return c.call(ctx, c.client, "sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  action,
	}, nil)
}

// SendText sends one chunk, trying Markdown first and falling back to plain
// text when Telegram rejects the markup. GFM tables are rewritten first so
// they stay readable on a phone.
func (c *Client) SendText(ctx context.Context, chatID int64, text string) error {
	return c.sendChunks(ctx, chatID, RewriteTablesForTelegram(text))
}

// SendLongText splits text into API-sized chunks and sends them in order.
func (c *Client) SendLongText(ctx context.Context, chatID int64, text string) error {
	return c.sendChunks(ctx, chatID, RewriteTablesForTelegram(text))
}

func (c *Client) sendChunks(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range SplitMessage(text, MaxMessageRunes) {
		if err := c.sendOne(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sendOne(ctx context.Context, chatID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "Markdown",
		"disable_web_page_preview": true,
	}
	err := c.call(ctx, c.client, "sendMessage", payload, nil)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !AsAPIError(err, &apiErr) {
		return err
	}
	delete(payload, "parse_mode")
	return c.call(ctx, c.client, "sendMessage", payload, nil)
}

// SendMessage posts text to chatID with an explicit parse mode and returns the
// message id. parseMode may be "", "HTML", "Markdown" or "MarkdownV2".
func (c *Client) SendMessage(ctx context.Context, chatID int64, text, parseMode string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("text required")
	}
	if len([]rune(text)) > MaxMessageRunes {
		return 0, fmt.Errorf("text exceeds Telegram %d character limit (%d runes)", MaxMessageRunes, len([]rune(text)))
	}
	payload := map[string]any{"chat_id": chatID, "text": text}
	switch strings.ToLower(strings.TrimSpace(parseMode)) {
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
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.call(ctx, c.client, "sendMessage", payload, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// GetWebhookInfo reports which webhook, if any, owns this bot's updates.
func (c *Client) GetWebhookInfo(ctx context.Context) (*WebhookInfo, error) {
	var info WebhookInfo
	if err := c.call(ctx, c.client, "getWebhookInfo", map[string]any{}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// DeleteWebhook removes the registered webhook so getUpdates works again.
// Pending updates are kept so nothing already queued is lost.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, c.client, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// GetFile resolves a file_id into a downloadable file path.
func (c *Client) GetFile(ctx context.Context, fileID string) (*File, error) {
	var file File
	if err := c.call(ctx, c.client, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return nil, err
	}
	if file.FilePath == "" {
		return nil, fmt.Errorf("telegram getFile: empty file_path for %s", fileID)
	}
	return &file, nil
}

// DownloadFile fetches a resolved file and writes it to dst.
func (c *Client) DownloadFile(ctx context.Context, filePath, dst string) error {
	rawURL := c.base + "/file/bot" + c.token + "/" + filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return c.scrub(err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return c.scrub(fmt.Errorf("download %s: %w", filepath.Base(filePath), err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %d", filepath.Base(filePath), resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// SendFile uploads a local file, picking sendPhoto or sendDocument by extension.
func (c *Client) SendFile(ctx context.Context, chatID int64, path, caption string) error {
	method, field := "sendDocument", "document"
	if isPhotoExt(filepath.Ext(path)) {
		method, field = "sendPhoto", "photo"
	}
	if err := c.upload(ctx, method, field, chatID, path, caption); err == nil {
		return nil
	} else if method == "sendDocument" {
		return err
	}
	// Telegram rejects photos that are too large or oddly proportioned; retry as
	// a document.
	return c.upload(ctx, "sendDocument", "document", chatID, path, caption)
}

func (c *Client) upload(ctx context.Context, method, field string, chatID int64, path, caption string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return err
	}
	if caption != "" {
		if err := mw.WriteField("caption", TruncateRunes(caption, 1024)); err != nil {
			return err
		}
	}
	part, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), &buf)
	if err != nil {
		return c.scrub(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.do(&http.Client{Timeout: 5 * time.Minute}, req, method, nil)
}

func isPhotoExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

// DiscoverChats calls getUpdates once and returns the unique chats that
// recently messaged the bot.
//
// getUpdates is exclusive: this consumes updates for the token, so it must not
// run while the assistant is long-polling the same bot.
func (c *Client) DiscoverChats(ctx context.Context) ([]DiscoveredChat, error) {
	var updates []struct {
		Message *struct {
			Chat discoveryChat `json:"chat"`
		} `json:"message"`
		ChannelPost *struct {
			Chat discoveryChat `json:"chat"`
		} `json:"channel_post"`
		MyChatMember *struct {
			Chat discoveryChat `json:"chat"`
		} `json:"my_chat_member"`
	}
	payload := map[string]any{
		"limit":           100,
		"timeout":         0,
		"allowed_updates": []string{"message", "channel_post", "my_chat_member"},
	}
	if err := c.call(ctx, c.client, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []DiscoveredChat
	add := func(ch discoveryChat) {
		sid := fmt.Sprintf("%d", ch.ID)
		if ch.ID == 0 || seen[sid] {
			return
		}
		seen[sid] = true
		out = append(out, DiscoveredChat{
			ID: sid, Type: ch.Type, Title: ch.Title,
			Username: ch.Username, First: ch.FirstName, Last: ch.LastName,
		})
	}
	for _, u := range updates {
		if u.Message != nil {
			add(u.Message.Chat)
		}
		if u.ChannelPost != nil {
			add(u.ChannelPost.Chat)
		}
		if u.MyChatMember != nil {
			add(u.MyChatMember.Chat)
		}
	}
	return out, nil
}

type discoveryChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}
