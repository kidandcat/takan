package oauth

import "testing"

func TestRedirectAllowlist(t *testing.T) {
	c := NewRedirectChecker(nil)
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
	}
	for _, u := range ok {
		if err := c.ValidateRedirectURI(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"https://evil.example/steal",
		"http://evil.com/x",
		"ftp://localhost/x",
		"cursor://evil.example/cb",
		"cursor://evil/cb",
		"cursor://cursor.com/oauth/callback",
		"cursor://localhost/callback",
		"http://anysphere.cursor-mcp/callback",
		"",
	}
	for _, u := range bad {
		if err := c.ValidateRedirectURI(u); err == nil {
			t.Errorf("%s: expected error", u)
		}
	}
	if err := c.ValidateRedirectURI("ftp://localhost/x"); err == nil || err.Error() != "redirect_uri scheme must be http or https" {
		t.Errorf("ftp://: want scheme error, got %v", err)
	}
	// Extra host from env
	c2 := NewRedirectChecker([]string{"myapp.dev"})
	if err := c2.ValidateRedirectURI("https://app.myapp.dev/cb"); err != nil {
		t.Fatal(err)
	}
}
