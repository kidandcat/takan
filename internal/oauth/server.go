// Package oauth implements a minimal OAuth 2.1 authorization server for MCP
// clients (Grok connectors, Claude, etc.): authorization code + PKCE (S256).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/kidandcat/takan/internal/store"
)

// PublicClientID is the fixed public client for Takan (PKCE only, no secret).
const PublicClientID = "takan"

// Server is the OAuth AS + resource metadata, co-hosted with the MCP resource.
type Server struct {
	Store     *store.Store
	PublicURL string // https://takan.es
	// UserFromSession returns the logged-in panel user for a request, if any.
	UserFromSession func(r *http.Request) *store.User
	// CreateSession logs the user in on the panel after authorize login.
	CreateSession func(ctx context.Context, userID string) (cookieToken string, err error)
	// SetSessionCookie writes the web session cookie.
	SetSessionCookie func(w http.ResponseWriter, token string)
}

func (s *Server) Routes(mux *http.ServeMux) {
	// RFC 9728 discovery variants (clients probe several of these).
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/{path...}", s.protectedResourceMetadata)
	mux.HandleFunc("GET /mcp/.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.asMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/{path...}", s.asMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.asMetadata)
	mux.HandleFunc("GET /oauth/authorize", s.authorizeGET)
	mux.HandleFunc("POST /oauth/authorize", s.authorizePOST)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("POST /oauth/register", s.registerClient)
}

func (s *Server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"resource":                 s.PublicURL + "/mcp",
		"authorization_servers":    []string{s.PublicURL},
		"scopes_supported":         []string{"mcp", "openid"},
		"bearer_methods_supported": []string{"header"},
		"resource_documentation":   s.PublicURL,
	})
}

func (s *Server) asMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.PublicURL
	writeJSON(w, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"mcp", "openid", "offline_access"},
	})
}

// Dynamic client registration — always returns the public PKCE client.
func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"client_id":                  PublicClientID,
		"client_id_issued_at":        time.Now().Unix(),
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"redirect_uris":              parseRedirectURIs(r),
	})
}

func parseRedirectURIs(r *http.Request) []string {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.RedirectURIs) == 0 {
		return []string{"https://grok.com/api/plugins/oauth/callback", "http://127.0.0.1/callback"}
	}
	return body.RedirectURIs
}

func (s *Server) authorizeGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := validateAuthorizeQuery(q); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	user := s.UserFromSession(r)
	if user == nil {
		s.renderLogin(w, q, "")
		return
	}
	s.renderConsent(w, q, user.Email, "")
}

func (s *Server) authorizePOST(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := url.Values{}
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"} {
		if v := r.FormValue(k); v != "" {
			q.Set(k, v)
		}
	}
	if err := validateAuthorizeQuery(q); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	action := r.FormValue("action")
	user := s.UserFromSession(r)

	if action == "login" || user == nil {
		u, err := s.Store.Authenticate(r.Context(), r.FormValue("email"), r.FormValue("password"))
		if err != nil {
			s.renderLogin(w, q, "Invalid email or password")
			return
		}
		tok, err := s.CreateSession(r.Context(), u.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.SetSessionCookie(w, tok)
		user = u
		action = "allow"
	}

	if action != "allow" {
		redir := q.Get("redirect_uri")
		u, _ := url.Parse(redir)
		qq := u.Query()
		qq.Set("error", "access_denied")
		if st := q.Get("state"); st != "" {
			qq.Set("state", st)
		}
		u.RawQuery = qq.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}

	rawCode, err := randomCode()
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	method := q.Get("code_challenge_method")
	if method == "" {
		method = "S256"
	}
	err = s.Store.SaveAuthCode(r.Context(), rawCode, user.ID, q.Get("client_id"), q.Get("redirect_uri"),
		q.Get("code_challenge"), method, q.Get("scope"), 10*time.Minute)
	if err != nil {
		log.Printf("oauth save code: %v", err)
		http.Error(w, "server error", 500)
		return
	}
	redir := q.Get("redirect_uri")
	u, err := url.Parse(redir)
	if err != nil {
		http.Error(w, "bad redirect_uri", 400)
		return
	}
	qq := u.Query()
	qq.Set("code", rawCode)
	if st := q.Get("state"); st != "" {
		qq.Set("state", st)
	}
	u.RawQuery = qq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func validateAuthorizeQuery(q url.Values) error {
	if q.Get("response_type") != "code" {
		return fmt.Errorf("response_type must be code")
	}
	if q.Get("client_id") == "" {
		return fmt.Errorf("client_id required")
	}
	if q.Get("redirect_uri") == "" {
		return fmt.Errorf("redirect_uri required")
	}
	if q.Get("code_challenge") == "" {
		return fmt.Errorf("code_challenge required (PKCE)")
	}
	m := q.Get("code_challenge_method")
	if m != "" && m != "S256" {
		return fmt.Errorf("only S256 PKCE supported")
	}
	return nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch r.FormValue("grant_type") {
	case "authorization_code":
		s.tokenAuthCode(w, r)
	case "refresh_token":
		s.tokenRefresh(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (s *Server) tokenAuthCode(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	redirectURI := r.FormValue("redirect_uri")
	clientID := r.FormValue("client_id")
	verifier := r.FormValue("code_verifier")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier required")
		return
	}
	userID, storedClient, storedRedirect, challenge, method, scope, err := s.Store.ConsumeAuthCode(r.Context(), code)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	if clientID != "" && clientID != storedClient {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if redirectURI != "" && redirectURI != storedRedirect {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if method == "" {
		method = "S256"
	}
	if !verifyPKCE(verifier, challenge, method) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "pkce verification failed")
		return
	}
	access, exp, err := s.Store.IssueAccessToken(r.Context(), userID, storedClient, scope, 30*24*time.Hour)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	refresh, err := s.Store.IssueRefreshToken(r.Context(), userID, storedClient, scope, 90*24*time.Hour)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeJSON(w, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(exp).Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

func (s *Server) tokenRefresh(w http.ResponseWriter, r *http.Request) {
	raw := r.FormValue("refresh_token")
	userID, clientID, scope, err := s.Store.ConsumeRefreshToken(r.Context(), raw)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	access, exp, err := s.Store.IssueAccessToken(r.Context(), userID, clientID, scope, 30*24*time.Hour)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeJSON(w, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(exp).Seconds()),
		"scope":        scope,
	})
}

func verifyPKCE(verifier, challenge, method string) bool {
	if method != "S256" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return computed == challenge
}

func randomCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
