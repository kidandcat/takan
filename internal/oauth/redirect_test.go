package oauth

import "testing"

func TestValidateRedirectURI(t *testing.T) {
	ok := []string{
		"https://grok.com/api/plugins/oauth/callback",
		"https://x.ai/callback",
		"http://127.0.0.1:1234/cb",
		"http://localhost/cb",
		"http://localhost:8787/callback",
		"https://claude.ai/api/mcp/auth_callback",
		"https://www.cursor.com/agents/mcp/oauth/callback",
		"cursor://anysphere.cursor-mcp/oauth/callback",
		"cursor://anysphere.cursor-mcp/oauth/takan/callback",
		"cursor://preview.cursor-mcp/oauth/callback",
		// previously rejected: any scheme / any host
		"ftp://localhost/x",
		"cursor://evil.example/cb",
		"cursor://evil/cb",
		"cursor://cursor.com/oauth/callback",
		"cursor://localhost/callback",
		"http://anysphere.cursor-mcp/callback",
		"http://evil.com/x",
		"https://evil.example/steal",
		// RFC 8252 private-use URI (no host)
		"com.example.app:/oauth/callback",
	}
	for _, u := range ok {
		if err := ValidateRedirectURI(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"",
		"   ",
		"not-a-uri",
		"/relative/path",
		"garbage%%%",
	}
	for _, u := range bad {
		if err := ValidateRedirectURI(u); err == nil {
			t.Errorf("%q: expected error", u)
		}
	}
}
