package email

import (
	"context"
	"fmt"
	"log"
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
		apiKey, senders, err := authMailConfig(ctx, st, box, envKey, from)
		if err != nil {
			return "", err
		}
		return sendLoginMail(ctx, apiKey, senders, to, loginCodeBody(code, ttl))
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

// sendLoginMail tries each candidate From until Resend accepts one.
//
// Takan caches domain status when the panel last refreshed it, so a domain can
// be "verified" in the database and rejected by Resend today (DNS drifted, moved
// registrar, …). Login is how the operator gets back in, so one stale domain
// must not be a lockout: an unverified-sender rejection falls through to the
// next candidate. A rejected send delivers nothing, so at most one mail arrives.
func sendLoginMail(ctx context.Context, apiKey string, senders []string, to, body string) (string, error) {
	const subject = "Takan login code"
	var lastErr error
	for _, sender := range senders {
		id, err := sendResend(ctx, apiKey, sender, to, subject, body, "")
		if err == nil {
			// Sender and Resend id only — the code itself is never logged.
			log.Printf("login code accepted by Resend (from=%s id=%s)", sender, id)
			return id, nil
		}
		lastErr = err
		if !unverifiedSender(err) {
			return "", err
		}
		log.Printf("login mail: sender %s rejected as unverified, trying the next domain", sender)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no sender address available")
	}
	return "", lastErr
}

// unverifiedSender reports whether Resend refused the From address itself
// (as opposed to a transport, auth or quota failure, which retrying cannot fix).
func unverifiedSender(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not verified") ||
		strings.Contains(msg, "domain is not") ||
		strings.Contains(msg, "invalid `from`")
}

// authMailConfig resolves the Resend key and the candidate From addresses for
// auth mail: environment first (works on a fresh instance), then the owner's
// saved Email settings (ignoring the module toggle, which must never lock the
// panel). Candidates are ordered explicit-config-first.
func authMailConfig(ctx context.Context, st *store.Store, box *cryptox.Box, envKey, from string) (apiKey string, senders []string, err error) {
	apiKey = strings.TrimSpace(envKey)

	var domains []store.EmailDomain
	if owner, oerr := st.Owner(ctx); oerr == nil && owner != nil {
		keyEnc, d, ok, gerr := st.GetEmailSettings(ctx, owner.ID)
		if gerr != nil {
			return "", nil, gerr
		}
		domains = d
		if apiKey == "" && ok && strings.TrimSpace(keyEnc) != "" {
			if box == nil {
				return "", nil, fmt.Errorf("no encryption key available to read the stored Resend key")
			}
			apiKey, err = box.Open(keyEnc)
			if err != nil {
				return "", nil, fmt.Errorf("decrypt api key: %w", err)
			}
		}
	}
	if apiKey == "" {
		return "", nil, fmt.Errorf("no Resend API key: set TAKAN_RESEND_API_KEY or save one in panel → Email")
	}

	seen := map[string]bool{}
	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[strings.ToLower(addr)] {
			return
		}
		seen[strings.ToLower(addr)] = true
		senders = append(senders, addr)
	}
	add(from)
	for _, d := range authDomains(domains) {
		add(authSenderLocalPart + "@" + d)
	}
	if len(senders) == 0 {
		return "", nil, fmt.Errorf("no sender address: set TAKAN_AUTH_EMAIL_FROM")
	}
	return apiKey, senders, nil
}

// authDomains lists candidate sending domains, enabled ones first (a disabled
// toggle must not lock the operator out of the panel).
func authDomains(domains []store.EmailDomain) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range store.EnabledEmailDomains(domains) {
		if n := normalizeDomain(d); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, d := range domains {
		if n := normalizeDomain(d.Name); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
