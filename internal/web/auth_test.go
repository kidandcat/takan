package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// mailbox captures the codes the server would have emailed, so tests can read
// them without the code ever being logged or exposed by a handler.
type mailbox struct {
	mu    sync.Mutex
	to    []string
	codes []string
}

func (m *mailbox) send(_ context.Context, to, code string, _ time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to = append(m.to, to)
	m.codes = append(m.codes, code)
	return "resend-id", nil
}

func (m *mailbox) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.codes) == 0 {
		return ""
	}
	return m.codes[len(m.codes)-1]
}

func (m *mailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.codes)
}

func testWeb(t *testing.T) (*Server, *store.Store, http.Handler) {
	s, st, h, _ := testWebMail(t)
	return s, st, h
}

func testWebMail(t *testing.T) (*Server, *store.Store, http.Handler, *mailbox) {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(st, nil, nil, "http://example.test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mb := &mailbox{}
	s.OwnerEmail = "owner@example.com"
	s.SendLoginCode = mb.send
	mux := http.NewServeMux()
	s.Routes(mux)
	return s, st, mux, mb
}

func postForm(h http.Handler, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return doReq(h, req)
}

func doReq(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func TestRegisterAndInvitesGone(t *testing.T) {
	_, _, h := testWeb(t)
	for _, path := range []string{"/register", "/dashboard/invites", "/dashboard/admin"} {
		rec := doReq(h, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s GET: %d", path, rec.Code)
		}
	}
	rec := doReq(h, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader("email=a@b.c&password=password1&invite=x")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /register: %d", rec.Code)
	}
}

func TestCodeLoginBootstrapsOwner(t *testing.T) {
	_, st, h, mb := testWebMail(t)

	home := doReq(h, httptest.NewRequest(http.MethodGet, "/", nil))
	hb, _ := io.ReadAll(home.Body)
	if !strings.Contains(string(hb), "Claim instance") {
		t.Fatalf("home should offer setup, got %s", hb[:min(len(hb), 300)])
	}

	// Step 1: the login page offers "Send code" and masks the destination.
	rec := doReq(h, httptest.NewRequest(http.MethodGet, "/login", nil))
	page := rec.Body.String()
	if !strings.Contains(page, "Send code") || !strings.Contains(page, "o***r@example.com") {
		t.Fatalf("login page: %s", page)
	}
	if strings.Contains(page, "password") {
		t.Fatal("login page must not ask for a password")
	}

	// Step 2: request the code.
	rec = postForm(h, "/login/send", url.Values{})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Code sent") {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if mb.count() != 1 || mb.to[0] != "owner@example.com" {
		t.Fatalf("mailbox: %v", mb.to)
	}
	code := mb.last()
	if len(code) != store.LoginCodeLen {
		t.Fatalf("code length: %q", code)
	}
	if strings.Contains(rec.Body.String(), code) {
		t.Fatal("the code must never be rendered in the response")
	}

	// Step 3: a wrong code is rejected without creating anything.
	bad := postForm(h, "/login", url.Values{"code": {"000000"}})
	if bad.Code != http.StatusOK || !strings.Contains(bad.Body.String(), "Invalid or expired code") {
		t.Fatalf("wrong code: %d %s", bad.Code, bad.Body.String())
	}
	if n, _ := st.UserCount(context.Background()); n != 0 {
		t.Fatalf("a failed code must not bootstrap an owner (users=%d)", n)
	}

	// Step 4: the right code signs in and creates the owner.
	rec = postForm(h, "/login", url.Values{"code": {code}})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	tok := cookieNamed(rec, "takan_session")
	if tok == "" {
		t.Fatal("expected session cookie")
	}
	if !rec.Result().Cookies()[0].HttpOnly {
		t.Fatal("session cookie must be httpOnly")
	}
	owner, err := st.Owner(context.Background())
	if err != nil || owner.Email != "owner@example.com" {
		t.Fatalf("owner: %+v err=%v", owner, err)
	}

	dash := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dash.AddCookie(&http.Cookie{Name: "takan_session", Value: tok})
	got := doReq(h, dash)
	if got.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", got.Code)
	}
	html, _ := io.ReadAll(got.Body)
	if !strings.Contains(string(html), "Operator") {
		t.Fatal("expected operator chip")
	}
}

func TestCodeIsSingleUseAndSupersededByReissue(t *testing.T) {
	_, _, h, mb := testWebMail(t)

	postForm(h, "/login/send", url.Values{})
	first := mb.last()
	rec := postForm(h, "/login", url.Values{"code": {first}})
	if rec.Code != http.StatusFound {
		t.Fatalf("first use: %d", rec.Code)
	}
	// Replaying the same code fails.
	again := postForm(h, "/login", url.Values{"code": {first}})
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), "Invalid or expired code") {
		t.Fatalf("replay should fail: %d", again.Code)
	}

	// Issuing a new code invalidates the previous one.
	postForm(h, "/login/send", url.Values{})
	older := mb.last()
	postForm(h, "/login/send", url.Values{})
	newest := mb.last()
	if older == newest {
		t.Fatal("expected a fresh code")
	}
	if rec := postForm(h, "/login", url.Values{"code": {older}}); rec.Code != http.StatusOK {
		t.Fatal("superseded code must not sign in")
	}
	if rec := postForm(h, "/login", url.Values{"code": {newest}}); rec.Code != http.StatusFound {
		t.Fatalf("newest code should work: %d", rec.Code)
	}
}

func TestSendCodeIsRateLimited(t *testing.T) {
	s, _, h, mb := testWebMail(t)
	// Each send consults two keys (per-IP and global), so a budget of 4 calls
	// allows exactly two requests.
	n := 0
	s.LoginCodeRateLimit = func(string) bool {
		n++
		return n <= 4
	}
	for i := 0; i < 2; i++ {
		if rec := postForm(h, "/login/send", url.Values{}); !strings.Contains(rec.Body.String(), "Code sent") {
			t.Fatalf("send %d was throttled early: %s", i, rec.Body.String())
		}
	}
	rec := postForm(h, "/login/send", url.Values{})
	if !strings.Contains(rec.Body.String(), "Too many code requests") {
		t.Fatalf("expected throttling: %s", rec.Body.String())
	}
	if mb.count() != 2 {
		t.Fatalf("throttled request still sent mail: %d", mb.count())
	}
}

func TestLoginFailsClosedWithoutOwnerEmail(t *testing.T) {
	s, _, h, mb := testWebMail(t)
	s.OwnerEmail = ""

	rec := postForm(h, "/login/send", url.Values{})
	if !strings.Contains(rec.Body.String(), "TAKAN_OWNER_EMAIL") {
		t.Fatalf("expected fail-closed message: %s", rec.Body.String())
	}
	if mb.count() != 0 {
		t.Fatal("no mail may be sent without a configured owner")
	}
	if rec := postForm(h, "/login", url.Values{"code": {"123456"}}); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "TAKAN_OWNER_EMAIL") {
		t.Fatalf("verify must fail closed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPasswordRoutesAreGone(t *testing.T) {
	_, st, h, _ := testWebMail(t)
	owner, err := st.BootstrapOwner(context.Background(), "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateWebSession(context.Background(), owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := postForm(h, "/dashboard/instance/password", url.Values{
		"current": {"old-secret1"}, "new": {"new-secret1"}, "confirm": {"new-secret1"},
	}, &http.Cookie{Name: "takan_session", Value: sess})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("password change route must be gone: %d", rec.Code)
	}
	// The Instance page no longer offers a password form.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/instance", nil)
	req.AddCookie(&http.Cookie{Name: "takan_session", Value: sess})
	page := doReq(h, req).Body.String()
	if strings.Contains(page, "Change password") || strings.Contains(page, `name="current"`) {
		t.Fatal("instance page still renders the password form")
	}
}

func TestSessionSurvivesAndOAuthRedirectsToLogin(t *testing.T) {
	_, st, h, mb := testWebMail(t)
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// A session created before the code flow keeps working.
	sess, err := st.CreateWebSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "takan_session", Value: sess})
	if rec := doReq(h, req); rec.Code != http.StatusOK {
		t.Fatalf("pre-existing session must stay valid: %d", rec.Code)
	}

	// A next= target is honoured after verification and stays on this host.
	postForm(h, "/login/send", url.Values{})
	rec := postForm(h, "/login", url.Values{"code": {mb.last()}, "next": {"/dashboard/instance"}})
	if rec.Header().Get("Location") != "/dashboard/instance" {
		t.Fatalf("next: %s", rec.Header().Get("Location"))
	}
	postForm(h, "/login/send", url.Values{})
	rec = postForm(h, "/login", url.Values{"code": {mb.last()}, "next": {"https://evil.example/"}})
	if rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("off-host next must be dropped: %s", rec.Header().Get("Location"))
	}
}

func TestExtraUserSessionRejected(t *testing.T) {
	_, st, h := testWeb(t)
	ctx := context.Background()
	if _, err := st.BootstrapOwner(ctx, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	extra, err := st.CreateUserOpts(ctx, "guest@example.com", "guest-pass-1", store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateWebSession(ctx, extra.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "takan_session", Value: sess})
	rec := doReq(h, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("extra user cookie must not unlock panel: %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
}
