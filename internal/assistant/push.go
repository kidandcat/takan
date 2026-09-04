package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/tg"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// fcmScope is the OAuth2 scope for the FCM HTTP v1 API.
const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// pushTitle is the notification title. The assistant is one correspondent, so
// the title never varies.
const pushTitle = InstanceName

// pushBodyLimit keeps a long reply from overflowing the notification shade.
const pushBodyLimit = 240

// PushSender delivers notifications through the FCM HTTP v1 API.
//
// It talks to the REST endpoint directly rather than pulling in the Firebase
// SDK: one authenticated POST per token is the whole protocol.
type PushSender struct {
	projectID string
	client    *http.Client
}

// NewPushSender builds a sender from a service account JSON blob, or returns
// nil when none is configured so push simply stays off.
func NewPushSender(ctx context.Context, serviceAccountJSON string) (*PushSender, error) {
	serviceAccountJSON = strings.TrimSpace(serviceAccountJSON)
	if serviceAccountJSON == "" {
		return nil, nil
	}
	creds, err := google.CredentialsFromJSON(ctx, []byte(serviceAccountJSON), fcmScope)
	if err != nil {
		return nil, fmt.Errorf("firebase service account: %w", err)
	}
	projectID := creds.ProjectID
	if projectID == "" {
		// CredentialsFromJSON only fills ProjectID for some key shapes.
		var raw struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.Unmarshal([]byte(serviceAccountJSON), &raw); err != nil {
			return nil, fmt.Errorf("firebase service account: %w", err)
		}
		projectID = raw.ProjectID
	}
	if projectID == "" {
		return nil, fmt.Errorf("firebase service account has no project_id")
	}
	return &PushSender{
		projectID: projectID,
		client:    oauth2.NewClient(ctx, creds.TokenSource),
	}, nil
}

// pushResult reports whether a token should be dropped from the registry.
type pushResult struct {
	dead bool
	err  error
}

// Send delivers one notification. A 404 or 400 means the token is gone or
// malformed, which is the caller's cue to forget it.
func (p *PushSender) Send(ctx context.Context, token, body string, data map[string]string) pushResult {
	payload := map[string]any{
		"message": map[string]any{
			"token": token,
			"notification": map[string]any{
				"title": pushTitle,
				"body":  body,
			},
			"data":    data,
			"android": map[string]any{"priority": "high"},
			"apns": map[string]any{
				"headers": map[string]any{"apns-priority": "10"},
				"payload": map[string]any{"aps": map[string]any{"sound": "default"}},
			},
		},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return pushResult{err: err}
	}

	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return pushResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return pushResult{err: fmt.Errorf("fcm send: %w", err)}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // safe-ignore: only enriches an error message
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return pushResult{}
	}
	dead := resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest
	return pushResult{
		dead: dead,
		err:  fmt.Errorf("fcm send: status %d: %s", resp.StatusCode, tg.TruncateRunes(string(respBody), 300)),
	}
}

// pushOutbound notifies the phone about a message it is not already watching.
//
// The app holds an SSE stream whenever it is in the foreground, so a live
// subscriber means the reply is already on screen and a notification would only
// duplicate it. Push is for the closed-app case.
func (b *Bot) pushOutbound(msg *HistoryMessage) {
	if b == nil || b.push == nil || msg == nil {
		return
	}
	if b.events.Subscribers() > 0 {
		return
	}
	body := strings.TrimSpace(msg.Text)
	if body == "" {
		if len(msg.Files) == 0 {
			return
		}
		body = "Sent you a file."
	}
	body = tg.TruncateRunes(body, pushBodyLimit)

	tokens := b.state.PushTokens()
	if len(tokens) == 0 {
		return
	}
	data := map[string]string{"message_id": msg.ID}

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(b.background()), 20*time.Second)
		defer cancel()
		for _, token := range tokens {
			res := b.push.Send(ctx, token, body, data)
			if res.dead {
				if err := b.state.DeletePushToken(token); err != nil {
					log.Printf("push: could not drop dead token: %v", err)
				}
				log.Printf("push: dropped a dead device token")
				continue
			}
			if res.err != nil {
				log.Printf("push: %v", res.err)
			}
		}
	}()
}
