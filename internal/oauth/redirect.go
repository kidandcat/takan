package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultRedirectAllowlists are host suffixes / exact hosts accepted for OAuth redirects.
// Clients (Grok, Claude, Cursor, local dev) must match one of these.
var DefaultRedirectAllowlists = []string{
	"grok.com",
	"x.ai",
	"claude.ai",
	"anthropic.com",
	"cursor.com",
	"cursor.sh",
	"127.0.0.1",
	"localhost",
	"[::1]",
}

// RedirectChecker validates redirect_uri against an allowlist.
type RedirectChecker struct {
	// Hosts is a list of allowed hostnames (exact or parent for subdomains).
	Hosts []string
}

// NewRedirectChecker merges defaults with extra hosts (e.g. from env).
func NewRedirectChecker(extra []string) *RedirectChecker {
	seen := map[string]bool{}
	var hosts []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimPrefix(h, ".")
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	for _, h := range DefaultRedirectAllowlists {
		add(h)
	}
	for _, h := range extra {
		// allow full URLs or hosts
		if strings.Contains(h, "://") {
			if u, err := url.Parse(h); err == nil {
				add(u.Hostname())
				continue
			}
		}
		add(h)
	}
	return &RedirectChecker{Hosts: hosts}
}

// ValidateRedirectURI returns nil if uri is an allowed http(s) redirect.
func (c *RedirectChecker) ValidateRedirectURI(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("redirect_uri required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid redirect_uri")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("redirect_uri scheme must be http or https")
	}
	host := strings.ToLower(u.Hostname())
	// http only for loopback
	if scheme == "http" && !isLoopbackHost(host) {
		return fmt.Errorf("http redirect_uri only allowed for localhost")
	}
	if c == nil || len(c.Hosts) == 0 {
		return fmt.Errorf("redirect allowlist empty")
	}
	for _, allowed := range c.Hosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("redirect_uri host %q not allowed", host)
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return strings.HasPrefix(host, "127.")
	}
}
