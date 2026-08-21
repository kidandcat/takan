package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidandcat/takan/internal/store"
)

func TestLoginPasswordOnly(t *testing.T) {
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
	_, err = st.CreateUserOpts(ctx, "guest@example.com", "guest-pass-1", store.CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{Store: st, PublicURL: "http://example.test"}
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

	rec := postJSON("/api/v1/auth/login", map[string]string{"password": "instance-secret"})
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
	if user["id"] != owner.ID {
		t.Fatalf("user: %+v", user)
	}
	if user["operator"] != true {
		t.Fatal("expected operator")
	}

	// email field ignored; extra-user password rejected
	rec = postJSON("/api/v1/auth/login", map[string]string{
		"email": "guest@example.com", "password": "guest-pass-1",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest password: %d %s", rec.Code, rec.Body.String())
	}

	rec = postJSON("/api/v1/auth/login", map[string]string{
		"email": "guest@example.com", "password": "instance-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner password with extra email: %d %s", rec.Code, rec.Body.String())
	}

	inv := httptest.NewRequest(http.MethodGet, "/api/v1/invites", nil)
	irec := httptest.NewRecorder()
	mux.ServeHTTP(irec, inv)
	if irec.Code != http.StatusNotFound && irec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("invites should be gone, got %d", irec.Code)
	}
}
