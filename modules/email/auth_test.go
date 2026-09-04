package email

import (
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

func TestUnverifiedSenderDetectsResendRejections(t *testing.T) {
	retryable := []string{
		`resend send 403: {"statusCode":403,"message":"The takan.es domain is not verified.","name":"validation_error"}`,
		`resend send 422: The domain is not verified`,
		"resend send 422: Invalid `from` field",
	}
	for _, m := range retryable {
		if !unverifiedSender(errString(m)) {
			t.Fatalf("should fall through to the next sender: %s", m)
		}
	}
	fatal := []string{
		`resend send 401: {"message":"API key is invalid"}`,
		`resend send 429: {"message":"Too many requests"}`,
		"post https://api.resend.com/emails: dial tcp: lookup failed",
	}
	for _, m := range fatal {
		if unverifiedSender(errString(m)) {
			t.Fatalf("must not retry other senders: %s", m)
		}
	}
	if unverifiedSender(nil) {
		t.Fatal("nil is not a rejection")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestAuthDomainsPrefersEnabledButKeepsAll(t *testing.T) {
	got := authDomains([]store.EmailDomain{
		{Name: "Disabled.example", Enabled: false},
		{Name: "enabled.example", Enabled: true},
		{Name: "enabled.example", Enabled: true}, // duplicate
	})
	want := []string{"enabled.example", "disabled.example"}
	if len(got) != len(want) {
		t.Fatalf("domains: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("domains: %v want %v", got, want)
		}
	}
}

func TestLoginCodeBodyCarriesCodeAndTTLOnly(t *testing.T) {
	body := loginCodeBody("123456", 10*time.Minute)
	if !strings.Contains(body, "123456") || !strings.Contains(body, "10 minutes") {
		t.Fatalf("body: %s", body)
	}
	// No links: nothing for a mail client to rewrite or a phisher to imitate.
	if strings.Contains(body, "http") {
		t.Fatalf("login mail must not contain links: %s", body)
	}
	if d := loginCodeBody("123456", 0); !strings.Contains(d, "10 minutes") {
		t.Fatalf("zero ttl should fall back to the default: %s", d)
	}
}
