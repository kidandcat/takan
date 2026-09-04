package tg

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// theToken is a realistic BotFather credential shape. It must never appear in
// anything the client returns.
const theToken = "7712345678:AAH-s3cr3t-bot-token-value-xyz"

// failingTransport fails every request the way a DNS or dial error does, which
// is when net/http builds a *url.Error containing the full request URL.
type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: lookup api.telegram.org: no such host")
}

func clientWithFailingTransport(t *testing.T) *Client {
	t.Helper()
	c := New(theToken)
	c.client = &http.Client{Transport: failingTransport{}}
	c.pollClient = &http.Client{Transport: failingTransport{}}
	return c
}

// TestTransportErrorsNeverCarryTheToken is the regression test for a credential
// leak: the Bot API puts the token in the URL path, so *url.Error carries it,
// and those errors are published — a failed poll reaches the panel, takan_status
// and /health, and a failed download is delivered to Telegram and stored in the
// conversation history.
func TestTransportErrorsNeverCarryTheToken(t *testing.T) {
	c := clientWithFailingTransport(t)
	ctx := context.Background()

	var errs []error
	_, err := c.GetMe(ctx)
	errs = append(errs, err)
	_, err = c.GetUpdates(ctx, 0, 0)
	errs = append(errs, err)
	errs = append(errs, c.SendText(ctx, 1, "hola"))
	errs = append(errs, c.SendLongText(ctx, 1, "hola"))
	_, err = c.SendMessage(ctx, 1, "hola", "plain")
	errs = append(errs, err)
	errs = append(errs, c.SendChatAction(ctx, 1, "typing"))
	_, err = c.GetFile(ctx, "file-id")
	errs = append(errs, err)
	_, err = c.GetWebhookInfo(ctx)
	errs = append(errs, err)
	errs = append(errs, c.DeleteWebhook(ctx))
	_, err = c.DiscoverChats(ctx)
	errs = append(errs, err)
	errs = append(errs, c.DownloadFile(ctx, "documents/file.pdf", filepath.Join(t.TempDir(), "out.bin")))

	upload := filepath.Join(t.TempDir(), "chart.png")
	if err := os.WriteFile(upload, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	errs = append(errs, c.SendFile(ctx, 1, upload, "caption"))

	for i, err := range errs {
		if err == nil {
			t.Fatalf("call %d should have failed", i)
		}
		if strings.Contains(err.Error(), theToken) {
			t.Fatalf("call %d leaked the bot token: %s", i, err)
		}
		// Wrapping it again must not resurrect it either.
		if wrapped := fmt.Errorf("context: %w", err); strings.Contains(wrapped.Error(), theToken) {
			t.Fatalf("call %d leaked the bot token once wrapped: %s", i, wrapped)
		}
	}
}

func TestScrubRedactsAndKeepsUnwrapping(t *testing.T) {
	c := New(theToken)

	sentinel := errors.New("boom")
	raw := fmt.Errorf("Post \"https://api.telegram.org/bot%s/sendMessage\": %w", theToken, sentinel)

	scrubbed := c.scrub(raw)
	if strings.Contains(scrubbed.Error(), theToken) {
		t.Fatalf("token survived the scrub: %s", scrubbed)
	}
	if !strings.Contains(scrubbed.Error(), tokenPlaceholder) {
		t.Fatalf("expected the redaction marker, got: %s", scrubbed)
	}
	// Sentinel checks must keep working, e.g. context.Canceled on shutdown.
	if !errors.Is(scrubbed, sentinel) {
		t.Fatal("scrubbing must not break errors.Is")
	}

	// An error with no token in it is returned untouched, so the common path
	// allocates nothing.
	clean := errors.New("nothing secret here")
	if got := c.scrub(clean); got != clean {
		t.Fatalf("a clean error should pass through unchanged, got %v", got)
	}
	if got := c.scrub(nil); got != nil {
		t.Fatalf("nil must stay nil, got %v", got)
	}
}

// TestAPIErrorsStayTypedThroughScrubbing: the 409 self-heal depends on
// recognising an *APIError, and it must survive the scrubbing path.
func TestAPIErrorsStayTypedThroughScrubbing(t *testing.T) {
	c := New(theToken)
	wrapped := c.scrub(fmt.Errorf("telegram getUpdates: %w",
		&APIError{Method: "getUpdates", Code: 409, Description: "Conflict"}))

	var apiErr *APIError
	if !AsAPIError(wrapped, &apiErr) || apiErr.Code != 409 {
		t.Fatalf("a wrapped APIError must stay recognisable, got %v", wrapped)
	}
}
