package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kidandcat/takan/internal/store"
)

func TestAuthorizePasswordOnly(t *testing.T) {
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "instance-secret")
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{Store: st, PublicURL: "http://example.test"}
	mux := http.NewServeMux()
	s.Routes(mux)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {PublicClientID},
		"redirect_uri":          {"https://grok.com/api/plugins/oauth/callback"},
		"code_challenge":        {"abc"},
		"code_challenge_method": {"S256"},
		"state":                 {"st"},
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil))
	body, _ := io.ReadAll(rec.Body)
	html := string(body)
	if strings.Contains(html, "email") || strings.Contains(html, "/register") {
		t.Fatalf("OAuth login must be password-only without register: %s", html)
	}
	if !strings.Contains(html, "Instance password") {
		t.Fatal("expected instance password field")
	}

	form := url.Values{}
	for k, vs := range q {
		form[k] = append([]string{}, vs...)
	}
	form.Set("action", "login")
	form.Set("password", "instance-secret")
	post := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	prec := httptest.NewRecorder()
	mux.ServeHTTP(prec, post)
	if prec.Code != http.StatusFound {
		t.Fatalf("authorize: %d %s", prec.Code, prec.Body.String())
	}
	loc := prec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatalf("missing code in %s", loc)
	}

	verifier := "pkce-verifier-value-aaaaaaaa"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Re-do with a real PKCE pair
	rawCode, err := randomCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAuthCode(ctx, rawCode, owner.ID, PublicClientID, "https://grok.com/api/plugins/oauth/callback", challenge, "S256", "mcp", AccessTokenTTL); err != nil {
		t.Fatal(err)
	}
	tokForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {rawCode},
		"redirect_uri":  {"https://grok.com/api/plugins/oauth/callback"},
		"client_id":     {PublicClientID},
		"code_verifier": {verifier},
	}
	tokReq := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokForm.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	mux.ServeHTTP(tokRec, tokReq)
	if tokRec.Code != http.StatusOK {
		t.Fatalf("token: %d %s", tokRec.Code, tokRec.Body.String())
	}
	if !strings.Contains(tokRec.Body.String(), "access_token") {
		t.Fatalf("token body: %s", tokRec.Body.String())
	}
}

func TestAuthorizeDoesNotBootstrap(t *testing.T) {
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st, PublicURL: "http://example.test"}
	mux := http.NewServeMux()
	s.Routes(mux)

	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {PublicClientID},
		"redirect_uri":          {"https://grok.com/api/plugins/oauth/callback"},
		"code_challenge":        {"abc"},
		"code_challenge_method": {"S256"},
		"action":                {"login"},
		"password":              {"instance-secret"},
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected login re-render, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Set the instance password in the panel first") {
		t.Fatalf("body: %s", rec.Body.String())
	}
	n, _ := st.UserCount(context.Background())
	if n != 0 {
		t.Fatal("OAuth must not bootstrap an owner")
	}
}

func TestRegisterClientDCR(t *testing.T) {
	s := &Server{PublicURL: "http://example.test"}
	mux := http.NewServeMux()
	s.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["https://grok.com/api/plugins/oauth/callback"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dcr: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"client_id":"takan"`) && !strings.Contains(rec.Body.String(), `"client_id": "takan"`) {
		t.Fatalf("dcr body: %s", rec.Body.String())
	}
}
