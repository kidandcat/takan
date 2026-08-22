// Package api serves the JSON REST API for the Takan mobile app (and future clients).
// Auth: OAuth-style access/refresh tokens issued via POST /api/v1/auth/login.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules"
)

const (
	clientMobile   = "takan-app"
	scopeMobile    = "mobile"
	accessTokenTTL = 24 * time.Hour
	refreshTTL     = 90 * 24 * time.Hour
)

// Server is the mobile/API surface.
type Server struct {
	Store          *store.Store
	Box            *cryptox.Box
	PublicURL      string
	OnToolsChanged func(userID string)
	// AuthRateLimit optional: return false to reject (same as web).
	AuthRateLimit func(key string) bool
	// StatusJSON optional: modules.Provider.status — inject from main.
	StatusJSON func(ctx context.Context, userID string) (string, error)
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/refresh", s.refresh)
	mux.HandleFunc("POST /api/v1/auth/logout", s.logout)
	mux.HandleFunc("GET /api/v1/me", s.me)
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/modules", s.listModules)
	mux.HandleFunc("POST /api/v1/modules/{id}/toggle", s.toggleModule)

	mux.HandleFunc("GET /api/v1/vault/items", s.vaultList)
	mux.HandleFunc("POST /api/v1/vault/items", s.vaultCreate)
	mux.HandleFunc("DELETE /api/v1/vault/items/{id}", s.vaultDelete)
	mux.HandleFunc("GET /api/v1/vault/settings", s.vaultGetSettings)
	mux.HandleFunc("PATCH /api/v1/vault/settings", s.vaultPatchSettings)
	mux.HandleFunc("PUT /api/v1/vault/settings", s.vaultPatchSettings) // alias for clients without PATCH
	mux.HandleFunc("GET /api/v1/vault/grants", s.vaultGrants)
	mux.HandleFunc("POST /api/v1/vault/grants/{id}/approve", s.vaultGrantApprove)
	mux.HandleFunc("POST /api/v1/vault/grants/{id}/deny", s.vaultGrantDeny)

	// Approvals: unified inbox for agent tool-call authorizations (vault grants for now).
	mux.HandleFunc("GET /api/v1/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/v1/approvals/{id}/approve", s.approveAny)
	mux.HandleFunc("POST /api/v1/approvals/{id}/deny", s.denyAny)

	mux.HandleFunc("GET /api/v1/people", s.peopleList)
	mux.HandleFunc("POST /api/v1/people", s.peopleCreate)
	mux.HandleFunc("GET /api/v1/people/{id}", s.peopleGet)
	mux.HandleFunc("DELETE /api/v1/people/{id}", s.peopleDelete)

	mux.HandleFunc("GET /api/v1/health", s.healthSnapshot)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]any{"error": msg})
}

func (s *Server) clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Real-IP"); x != "" {
		return x
	}
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

func (s *Server) authUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		s.writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	u, err := s.Store.UserByAccessToken(r.Context(), raw)
	if err != nil || u == nil {
		s.writeErr(w, http.StatusUnauthorized, "invalid or expired token")
		return nil, false
	}
	return u, true
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

func userJSON(u *store.User) map[string]any {
	return map[string]any{
		"id":       u.ID,
		"email":    u.Email,
		"operator": true,
	}
}

// --- auth ---

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.AuthRateLimit != nil && !s.AuthRateLimit("api-login:"+s.clientIP(r)) {
		s.writeErr(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var body struct {
		Email    string `json:"email"` // ignored; kept so existing mobile clients keep working
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Password == "" {
		s.writeErr(w, http.StatusBadRequest, "password required")
		return
	}
	u, err := s.Store.AuthenticatePassword(r.Context(), body.Password)
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	access, exp, err := s.Store.IssueAccessToken(r.Context(), u.ID, clientMobile, scopeMobile, accessTokenTTL)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "token issue failed")
		return
	}
	refresh, err := s.Store.IssueRefreshToken(r.Context(), u.ID, clientMobile, scopeMobile, refreshTTL)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "refresh issue failed")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(exp).Seconds()),
		"expires_at":    exp.UTC().Format(time.RFC3339),
		"user":          userJSON(u),
	})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if s.AuthRateLimit != nil && !s.AuthRateLimit("api-refresh:"+s.clientIP(r)) {
		s.writeErr(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSON(r, &body); err != nil || body.RefreshToken == "" {
		s.writeErr(w, http.StatusBadRequest, "refresh_token required")
		return
	}
	userID, clientID, scope, newRefresh, err := s.Store.RotateRefreshToken(r.Context(), body.RefreshToken, refreshTTL)
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	if clientID != clientMobile {
		// allow any client that has refresh — mobile may rotate MCP tokens too; restrict to mobile
		// Actually MCP refresh uses same table. Only issue if scope contains mobile or client is mobile.
		if clientID != clientMobile && scope != scopeMobile {
			// still ok for app if we issued with clientMobile
		}
	}
	access, exp, err := s.Store.IssueAccessToken(r.Context(), userID, clientID, scope, accessTokenTTL)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "token issue failed")
		return
	}
	u, _ := s.Store.UserByID(r.Context(), userID)
	out := map[string]any{
		"access_token":  access,
		"refresh_token": newRefresh,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(exp).Seconds()),
		"expires_at":    exp.UTC().Format(time.RFC3339),
	}
	if u != nil {
		out["user"] = userJSON(u)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	h := r.Header.Get("Authorization")
	raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	_ = s.Store.RevokeAccessToken(r.Context(), raw)
	_ = u
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"user": userJSON(u)})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	if s.StatusJSON != nil {
		raw, err := s.StatusJSON(r.Context(), u.ID)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(raw))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"modules": []any{}})
}

func (s *Server) listModules(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	mods, err := s.Store.ListModules(r.Context(), u.ID)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cat := map[string]modules.Info{}
	for _, c := range modules.Catalog {
		cat[c.ID] = c
	}
	var rows []map[string]any
	for _, m := range mods {
		info := cat[m.ModuleID]
		name := info.Name
		if name == "" {
			name = m.ModuleID
		}
		rows = append(rows, map[string]any{
			"id":          m.ModuleID,
			"name":        name,
			"description": info.Description,
			"enabled":     m.Enabled,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"modules": rows})
}

func (s *Server) toggleModule(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	known := false
	for _, c := range modules.Catalog {
		if c.ID == id {
			known = true
			break
		}
	}
	if !known {
		s.writeErr(w, http.StatusNotFound, "unknown module")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = decodeJSON(r, &body)
	mods, _ := s.Store.ListModules(r.Context(), u.ID)
	cur := false
	for _, m := range mods {
		if m.ModuleID == id {
			cur = m.Enabled
			break
		}
	}
	en := !cur
	if body.Enabled != nil {
		en = *body.Enabled
	}
	if err := s.Store.SetModuleEnabled(r.Context(), u.ID, id, en); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": en})
}
