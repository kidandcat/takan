package email

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
)

// authSenderLocalPart is the default local part of the From address used for
// login codes when TAKAN_AUTH_EMAIL_FROM is not set.
const authSenderLocalPart = "login"

// LoginCodeSender sends a one-time login code to the operator.
// Signature matches web.Server.SendLoginCode.
type LoginCodeSender func(ctx context.Context, to, code string, ttl time.Duration) (string, error)

// LoginCodeFactory builds the sender used by the panel and mobile login flows.
//
// Login is infrastructure, not an optional integration, so this path
// deliberately does NOT check whether the Email module is enabled: it reads the
// stored Resend key directly. envKey / from (both optional, from
// TAKAN_RESEND_API_KEY and TAKAN_AUTH_EMAIL_FROM) take precedence and are what
// makes the very first sign-in possible, before any owner row or panel
// configuration exists.
func LoginCodeFactory(st *store.Store, box *cryptox.Box, envKey, from string) LoginCodeSender {
	return func(ctx context.Context, to, code string, ttl time.Duration) (string, error) {
		to = strings.TrimSpace(to)
		if to == "" {
			return "", fmt.Errorf("no destination address")
		}
		apiKey, sender, err := authMailConfig(ctx, st, box, envKey, from)
		if err != nil {
			return "", err
		}
		return sendResend(ctx, apiKey, sender, to, "Takan login code", loginCodeBody(code, ttl), "")
	}
}

// loginCodeBody is plain text on purpose: no links, no tracking, nothing that
// an inbox preview would mangle.
func loginCodeBody(code string, ttl time.Duration) string {
	mins := int(ttl.Minutes())
	if mins <= 0 {
		mins = int(store.LoginCodeTTL.Minutes())
	}
	return fmt.Sprintf(
		"Your Takan login code is:\n\n    %s\n\nIt expires in %d minutes and can be used once.\n"+
			"If you did not ask to sign in, ignore this message.\n", code, mins)
}

// authMailConfig resolves the Resend key and the From address for auth mail:
// environment first (works on a fresh instance), then the owner's saved Email
// settings (ignoring the module toggle, which must never lock the panel).
func authMailConfig(ctx context.Context, st *store.Store, box *cryptox.Box, envKey, from string) (apiKey, sender string, err error) {
	apiKey = strings.TrimSpace(envKey)
	sender = strings.TrimSpace(from)

	var domains []store.EmailDomain
	if owner, oerr := st.Owner(ctx); oerr == nil && owner != nil {
		keyEnc, d, ok, gerr := st.GetEmailSettings(ctx, owner.ID)
		if gerr != nil {
			return "", "", gerr
		}
		domains = d
		if apiKey == "" && ok && strings.TrimSpace(keyEnc) != "" {
			if box == nil {
				return "", "", fmt.Errorf("no encryption key available to read the stored Resend key")
			}
			apiKey, err = box.Open(keyEnc)
			if err != nil {
				return "", "", fmt.Errorf("decrypt api key: %w", err)
			}
		}
	}
	if apiKey == "" {
		return "", "", fmt.Errorf("no Resend API key: set TAKAN_RESEND_API_KEY or save one in panel → Email")
	}
	if sender == "" {
		domain := firstAuthDomain(domains)
		if domain == "" {
			return "", "", fmt.Errorf("no verified sender: set TAKAN_AUTH_EMAIL_FROM")
		}
		sender = authSenderLocalPart + "@" + domain
	}
	return apiKey, sender, nil
}

// firstAuthDomain prefers an enabled domain, falling back to any configured one
// (a disabled toggle must not lock the operator out of the panel).
func firstAuthDomain(domains []store.EmailDomain) string {
	if enabled := store.EnabledEmailDomains(domains); len(enabled) > 0 {
		return enabled[0]
	}
	for _, d := range domains {
		if n := normalizeDomain(d.Name); n != "" {
			return n
		}
	}
	return ""
}
