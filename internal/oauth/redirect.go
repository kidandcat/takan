package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateRedirectURI returns nil if raw is a non-empty parseable absolute URI.
// Any scheme is accepted (http, https, cursor://, RFC 8252 private-use URIs
// like com.example.app:/path with no host). This hub is personal/single-tenant
// and does not apply a redirect host or scheme allowlist.
func ValidateRedirectURI(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("redirect_uri required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("invalid redirect_uri")
	}
	return nil
}
