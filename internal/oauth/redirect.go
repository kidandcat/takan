package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultRedirectAllowlists are host suffixes / exact hosts accepted for http(s) OAuth redirects.
// Clients (Grok, Claude, Cursor, local dev) must match one of these.
// Cursor / Grok Bot MCP also uses the custom scheme cursor:// — see isCursorMCPHost.
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

// cursorMCPHost is the custom-scheme OAuth callback host used by Cursor / Grok Bot
// MCP: cursor://anysphere.cursor-mcp/oauth/callback (also /oauth/<app>/callback).
// Exact host cursor-mcp and any subdomain (e.g. anysphere.cursor-mcp) are allowed.
const cursorMCPHost = "cursor-mcp"

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

// ValidateRedirectURI returns nil if uri is an allowed redirect.
// http(s) must match the host allowlist (http only on loopback).
// The cursor:// scheme is accepted only for cursor-mcp (and subdomains),
// used by Cursor / Grok Bot MCP — not arbitrary custom schemes or cursor://evil.
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
	host := strings.ToLower(u.Hostname())
	if scheme == "cursor" {
		if isCursorMCPHost(host) {
			return nil
		}
		return fmt.Errorf("redirect_uri host %q not allowed", host)
	}
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("redirect_uri scheme must be http or https")
	}
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

func isCursorMCPHost(host string) bool {
	return host == cursorMCPHost || strings.HasSuffix(host, "."+cursorMCPHost)
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return strings.HasPrefix(host, "127.")
	}
}
