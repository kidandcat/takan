package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// TestLoginByEmailCode covers the mobile API: send-code then exchange it.
func TestLoginByEmailCode(t *testing.T) {
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
	if _, err = st.CreateUser(ctx, "guest@example.com", "guest-pass-1"); err != nil {
		t.Fatal(err)
	}

	var sent []string
	s := &Server{
		Store: st, PublicURL: "http://example.test",
		OwnerEmail: "owner@example.com",
		SendLoginCode: func(_ context.Context, to, code string, _ time.Duration) (string, error) {
			sent = append(sent, code)
			if to != "owner@example.com" {
				t.Errorf("unexpected destination %q", to)
			}
			return "resend-id", nil
		},
	}
	mux := http.NewServeMux()
	s.Routes(mux)

	postJSON := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// A password is no longer accepted.
	if rec := postJSON("/api/v1/auth/login", map[string]string{"password": "whatever"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("password login must be rejected: %d %s", rec.Code, rec.Body.String())
	}

	rec := postJSON("/api/v1/auth/send-code", map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("send-code: %d %s", rec.Code, rec.Body.String())
	}
	if len(sent) != 1 {
		t.Fatalf("codes sent: %d", len(sent))
	}
	if strings.Contains(rec.Body.String(), sent[0]) {
		t.Fatal("the code must never appear in the response")
	}

	if rec := postJSON("/api/v1/auth/login", map[string]string{"code": "000000"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", rec.Code)
	}

	rec = postJSON("/api/v1/auth/login", map[string]string{"email": "guest@example.com", "code": sent[0]})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["access_token"] == "" {
		t.Fatal("missing access_token")
	}
	user, _ := out["user"].(map[string]any)
	if user["id"] != owner.ID || user["operator"] != true {
		t.Fatalf("user: %+v", user)
	}
	// Single use.
	if rec := postJSON("/api/v1/auth/login", map[string]string{"code": sent[0]}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code: %d", rec.Code)
	}

	inv := httptest.NewRequest(http.MethodGet, "/api/v1/invites", nil)
	irec := httptest.NewRecorder()
	mux.ServeHTTP(irec, inv)
	if irec.Code != http.StatusNotFound && irec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("invites should be gone, got %d", irec.Code)
	}
}

// TestLoginFailsClosedWithoutOwnerEmail: no configured address, no login.
func TestAPILoginFailsClosed(t *testing.T) {
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st, PublicURL: "http://example.test"}
	mux := http.NewServeMux()
	s.Routes(mux)

	post := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/api/v1/auth/send-code", map[string]string{}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("send-code without owner email: %d", rec.Code)
	}
	if rec := post("/api/v1/auth/login", map[string]string{"code": "123456"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login without owner email: %d", rec.Code)
	}
}
