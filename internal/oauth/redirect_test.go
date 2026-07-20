package oauth

import "testing"

func TestRedirectAllowlist(t *testing.T) {
	c := NewRedirectChecker(nil)
	ok := []string{
		"https://grok.com/api/plugins/oauth/callback",
		"https://x.ai/callback",
		"http://127.0.0.1:1234/cb",
		"http://localhost/cb",
		"https://claude.ai/api/mcp/auth_callback",
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
		"",
	}
	for _, u := range bad {
		if err := c.ValidateRedirectURI(u); err == nil {
			t.Errorf("%s: expected error", u)
		}
	}
	// Extra host from env
	c2 := NewRedirectChecker([]string{"myapp.dev"})
	if err := c2.ValidateRedirectURI("https://app.myapp.dev/cb"); err != nil {
		t.Fatal(err)
	}
}
