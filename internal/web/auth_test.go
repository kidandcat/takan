package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

func testWeb(t *testing.T) (*Server, *store.Store, http.Handler) {
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
	mux := http.NewServeMux()
	s.Routes(mux)
	return s, st, mux
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

func TestBootstrapThenUnlock(t *testing.T) {
	_, _, h := testWeb(t)

	home := doReq(h, httptest.NewRequest(http.MethodGet, "/", nil))
	body, _ := io.ReadAll(home.Body)
	if !strings.Contains(string(body), "Set instance password") {
		t.Fatalf("home should offer setup, got %s", body[:min(len(body), 400)])
	}

	form := url.Values{"password": {"instance-secret"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := doReq(h, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("bootstrap login: %d %s", rec.Code, rec.Body.String())
	}
	tok := cookieNamed(rec, "takan_session")
	if tok == "" {
		t.Fatal("expected session cookie")
	}
	c := rec.Result().Cookies()[0]
	if !c.HttpOnly {
		t.Fatal("session cookie must be httpOnly")
	}

	dash := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dash.AddCookie(&http.Cookie{Name: "takan_session", Value: tok})
	got := doReq(h, dash)
	if got.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", got.Code)
	}
	html, _ := io.ReadAll(got.Body)
	if strings.Contains(string(html), "Invites") || strings.Contains(string(html), "/register") {
		t.Fatal("panel must not expose invites/register")
	}
	if !strings.Contains(string(html), "Operator") {
		t.Fatal("expected operator chip")
	}

	// Wrong password
	bad := url.Values{"password": {"wrong-password"}}.Encode()
	breq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(bad))
	breq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	brec := doReq(h, breq)
	if brec.Code != http.StatusOK {
		t.Fatalf("bad password should re-render form, got %d", brec.Code)
	}
	if !strings.Contains(brec.Body.String(), "Invalid password") {
		t.Fatal("expected invalid password")
	}
}

func TestExtraUserSessionRejected(t *testing.T) {
	_, st, h := testWeb(t)
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner-pass-1")
	if err != nil {
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
	_ = owner
}

func TestChangePasswordLocksPanel(t *testing.T) {
	_, st, h := testWeb(t)
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "old-secret1")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateWebSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"current": {"old-secret1"},
		"new":     {"new-secret1"},
		"confirm": {"new-secret1"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/dashboard/instance/password", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "takan_session", Value: sess})
	rec := doReq(h, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body.String())
	}
	dash := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dash.AddCookie(&http.Cookie{Name: "takan_session", Value: sess})
	got := doReq(h, dash)
	if got.Header().Get("Location") != "/login" {
		t.Fatalf("old session should be dead, loc=%s code=%d", got.Header().Get("Location"), got.Code)
	}
}
