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

func TestAuthorizeRedirectsToPanelLogin(t *testing.T) {
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Signed-in state is supplied by the panel; this server never sees credentials.
	var signedIn *store.User
	s := &Server{
		Store: st, PublicURL: "http://example.test",
		UserFromSession: func(*http.Request) *store.User { return signedIn },
	}
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
	if rec.Code != http.StatusFound {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("unauthenticated authorize must redirect to /login, got %d: %s", rec.Code, body)
	}
	loc0 := rec.Header().Get("Location")
	if !strings.HasPrefix(loc0, "/login?next=") {
		t.Fatalf("redirect target: %s", loc0)
	}
	next, err := url.QueryUnescape(strings.TrimPrefix(loc0, "/login?next="))
	if err != nil || !strings.HasPrefix(next, "/oauth/authorize?") {
		t.Fatalf("next should resume consent: %q err=%v", next, err)
	}

	// Once the panel session exists, consent renders and can be granted.
	signedIn = owner
	crec := httptest.NewRecorder()
	mux.ServeHTTP(crec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil))
	if body, _ := io.ReadAll(crec.Body); !strings.Contains(string(body), "Authorize Takan") {
		t.Fatalf("expected consent screen: %s", body)
	}

	form := url.Values{}
	for k, vs := range q {
		form[k] = append([]string{}, vs...)
	}
	form.Set("action", "allow")
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
	// A stray password POST authenticates nobody: it just bounces to /login.
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("expected redirect to /login, got %d %s", rec.Code, rec.Header().Get("Location"))
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
