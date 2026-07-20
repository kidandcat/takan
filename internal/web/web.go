package web

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules"
)

//go:embed templates/*.html
var tmplFS embed.FS

// Server serves the HTMX panel.
type Server struct {
	Store     *store.Store
	Hub       *agenthub.Hub
	Box       *cryptox.Box
	PublicURL string
	// OnMercadonaSave logs into Mercadona and stores session tokens (optional).
	OnMercadonaSave func(ctx context.Context, userID, email, password, postal string) error
	// OnMercadonaClear unlinks Mercadona session for the user.
	OnMercadonaClear func(ctx context.Context, userID string) error
	// OnToolsChanged notifies MCP clients (tools/list_changed) after module changes.
	OnToolsChanged func(userID string)
	tmpl           *template.Template
}

func New(st *store.Store, hub *agenthub.Hub, box *cryptox.Box, publicURL string) (*Server, error) {
	t, err := template.ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{Store: st, Hub: hub, Box: box, PublicURL: publicURL, tmpl: t}, nil
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /login", s.loginGet)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("GET /register", s.registerGet)
	mux.HandleFunc("POST /register", s.registerPost)
	mux.HandleFunc("GET /logout", s.logout)
	mux.HandleFunc("GET /dashboard", s.dashOverview)
	mux.HandleFunc("GET /dashboard/integrations", s.dashIntegrations)
	mux.HandleFunc("GET /dashboard/machines", s.dashMachines)
	mux.HandleFunc("GET /dashboard/mercadona", s.dashMercadona)
	// Old routes → overview / integrations
	mux.HandleFunc("GET /dashboard/connect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard/modules", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/integrations", http.StatusFound)
	})
	mux.HandleFunc("POST /dashboard/modules/{id}/toggle", s.toggleModule)
	mux.HandleFunc("POST /dashboard/machines", s.createMachine)
	mux.HandleFunc("POST /dashboard/machines/{id}/delete", s.deleteMachine)
	mux.HandleFunc("POST /dashboard/mercadona", s.saveMercadona)
	mux.HandleFunc("POST /dashboard/mercadona/clear", s.clearMercadona)
}

type pageData struct {
	Title               string
	User                *store.User
	Error               string
	Flash               string
	FlashIsError        bool
	MCPURL              string
	OAuthClientID       string
	OAuthAuthorize      string
	OAuthToken          string
	OAuthMetadata       string
	Modules             []modView
	Machines            []machView
	InstallCmd          string
	MercadonaConfigured bool
	MercadonaEmail      string
	MercadonaPostal     string
	// Dashboard stats (precomputed for templates)
	ModEnabledCount int
	ModTotalCount   int
	MachOnlineCount int
	MachTotalCount  int
	// ActiveNav highlights the sidebar item: overview|integrations|machine|mercadona|…
	ActiveNav string
}

type modView struct {
	ID, Name, Description string
	Enabled, Ready        bool
	// Path is the module settings page (e.g. /dashboard/machines).
	Path string
}

type machView struct {
	ID, Name string
	Online   bool
}

func (s *Server) render(w http.ResponseWriter, name string, data pageData) {
	if data.Title == "" {
		data.Title = "Takan"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// layout expects "content" defined by the page template — we execute named templates
	// by parsing each page as defining "content" and wrapping with layout.
	// Simpler: execute full set with define content in each file.
	err := s.tmpl.ExecuteTemplate(w, "layout", data)
	if err != nil {
		// try page-only if layout missing content
		http.Error(w, err.Error(), 500)
	}
}

// renderPage sets content from a page template name by re-parsing — actually
// all pages use {{define "content"}}. layout uses {{template "content" .}}.
// We need to clone and parse the right content. Easiest fix: one Execute of
// a synthetic approach — parse layout + specific content file per call.

func (s *Server) page(w http.ResponseWriter, contentFile string, data pageData) {
	if data.Title == "" {
		data.Title = "Takan"
	}
	t, err := template.ParseFS(tmplFS, "templates/layout.html", "templates/"+contentFile)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

const cookieName = "takan_session"

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(s.PublicURL, "https"),
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
}

func (s *Server) currentUser(r *http.Request) *store.User {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.Store.UserByWebSession(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return u
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if u := s.currentUser(r); u != nil {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
		return
	}
	s.page(w, "home.html", pageData{Title: "Home"})
}

func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	s.page(w, "login.html", pageData{Title: "Log in"})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	u, err := s.Store.Authenticate(r.Context(), r.FormValue("email"), r.FormValue("password"))
	if err != nil {
		s.page(w, "login.html", pageData{Title: "Log in", Error: "Invalid email or password"})
		return
	}
	tok, err := s.Store.CreateWebSession(r.Context(), u.ID, 30*24*time.Hour)
	if err != nil {
		s.page(w, "login.html", pageData{Title: "Log in", Error: err.Error()})
		return
	}
	s.setSession(w, tok)
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (s *Server) registerGet(w http.ResponseWriter, r *http.Request) {
	s.page(w, "register.html", pageData{Title: "Register"})
}

func (s *Server) registerPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	u, err := s.Store.CreateUser(r.Context(), r.FormValue("email"), r.FormValue("password"))
	if err != nil {
		s.page(w, "register.html", pageData{Title: "Register", Error: err.Error()})
		return
	}
	tok, err := s.Store.CreateWebSession(r.Context(), u.ID, 30*24*time.Hour)
	if err != nil {
		s.page(w, "register.html", pageData{Title: "Register", Error: err.Error()})
		return
	}
	s.setSession(w, tok)
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = s.Store.DeleteWebSession(r.Context(), c.Value)
	}
	s.clearSession(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return nil
	}
	return u
}

func (s *Server) dashPage(w http.ResponseWriter, r *http.Request, nav, title, tmpl string) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	data := s.buildDashboard(r.Context(), u)
	data.ActiveNav = nav
	data.Title = title
	if nav == "machine" {
		if c, err := r.Cookie("takan_install"); err == nil && c.Value != "" {
			if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil {
				data.InstallCmd = string(raw)
			}
			http.SetCookie(w, &http.Cookie{Name: "takan_install", Value: "", Path: "/", MaxAge: -1})
		}
	}
	if f := r.URL.Query().Get("flash"); f != "" {
		data.Flash = f
		lf := strings.ToLower(f)
		data.FlashIsError = strings.Contains(lf, "error") ||
			strings.Contains(lf, "fail") ||
			strings.Contains(lf, "required") ||
			strings.Contains(lf, "re-enter")
	}
	s.page(w, tmpl, data)
}

func (s *Server) dashOverview(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "overview", "Overview", "dashboard.html")
}
func (s *Server) dashIntegrations(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "integrations", "Integrations", "integrations.html")
}
func (s *Server) dashMachines(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "machine", "Machines", "machines.html")
}
func (s *Server) dashMercadona(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "mercadona", "Mercadona", "mercadona.html")
}

func (s *Server) buildDashboard(ctx context.Context, u *store.User) pageData {
	data := pageData{
		Title:          "Dashboard",
		User:           u,
		MCPURL:         s.PublicURL + "/mcp",
		OAuthClientID:  "takan",
		OAuthAuthorize: s.PublicURL + "/oauth/authorize",
		OAuthToken:     s.PublicURL + "/oauth/token",
		OAuthMetadata:  s.PublicURL + "/.well-known/oauth-authorization-server",
	}
	mods, _ := s.Store.ListModules(ctx, u.ID)
	cat := map[string]modules.Info{}
	for _, c := range modules.Catalog {
		cat[c.ID] = c
	}
	for _, m := range mods {
		info := cat[m.ModuleID]
		mv := modView{
			ID: m.ModuleID, Name: info.Name, Description: info.Description, Enabled: m.Enabled,
		}
		if mv.Name == "" {
			mv.Name = m.ModuleID
		}
		switch m.ModuleID {
		case "machine":
			mv.Path = "/dashboard/machines"
			ms, _ := s.Store.ListMachines(ctx, u.ID)
			online := false
			for _, mac := range ms {
				if s.Hub.Online(mac.ID) {
					online = true
					break
				}
			}
			mv.Ready = m.Enabled && online
		case "mercadona":
			mv.Path = "/dashboard/mercadona"
			_, _, _, ok, _ := s.Store.GetMercadonaCreds(ctx, u.ID)
			// Ready when creds exist; session link is verified when tools run.
			mv.Ready = m.Enabled && ok
		default:
			mv.Path = "/dashboard/" + m.ModuleID
		}
		data.Modules = append(data.Modules, mv)
		if m.Enabled {
			data.ModEnabledCount++
		}
	}
	data.ModTotalCount = len(data.Modules)
	ms, _ := s.Store.ListMachines(ctx, u.ID)
	for _, m := range ms {
		online := s.Hub.Online(m.ID)
		data.Machines = append(data.Machines, machView{ID: m.ID, Name: m.Name, Online: online})
		if online {
			data.MachOnlineCount++
		}
	}
	data.MachTotalCount = len(data.Machines)
	email, _, postal, ok, _ := s.Store.GetMercadonaCreds(ctx, u.ID)
	data.MercadonaConfigured = ok
	data.MercadonaEmail = email
	data.MercadonaPostal = postal
	return data
}

func (s *Server) toggleModule(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	_ = r.ParseForm()
	en := r.FormValue("enabled") == "1"
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, id, en)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.redirectBack(w, r, "/dashboard")
}

// redirectBack sends the browser to Referer when it is on this host, else fallback.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	ref := r.Header.Get("Referer")
	if ref != "" {
		if u, err := url.Parse(ref); err == nil {
			sameHost := u.Host == "" || u.Host == r.Host
			if !sameHost && s.PublicURL != "" {
				if pub, err := url.Parse(s.PublicURL); err == nil {
					sameHost = u.Host == pub.Host
				}
			}
			if sameHost {
				path := u.RequestURI()
				if path == "" {
					path = "/"
				}
				// Only bounce back into the panel.
				if strings.HasPrefix(path, "/dashboard") {
					http.Redirect(w, r, path, http.StatusFound)
					return
				}
			}
		}
	}
	http.Redirect(w, r, fallback, http.StatusFound)
}

func (s *Server) createMachine(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	_, raw, err := s.Store.CreateMachine(r.Context(), u.ID, r.FormValue("name"))
	if err != nil {
		http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "machine", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	// Name is already registered on the server with this token; only token is needed on the machine.
	cmd := fmt.Sprintf("curl -fsSL %s/install.sh | bash -s -- %s", s.PublicURL, raw)
	http.SetCookie(w, &http.Cookie{
		Name: "takan_install", Value: base64.RawURLEncoding.EncodeToString([]byte(cmd)),
		Path: "/", MaxAge: 300, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.PublicURL, "https"),
	})
	http.Redirect(w, r, "/dashboard/machines", http.StatusFound)
}

func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = s.Store.DeleteMachine(r.Context(), u.ID, r.PathValue("id"))
	http.Redirect(w, r, "/dashboard/machines", http.StatusFound)
}

func (s *Server) saveMercadona(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	email := r.FormValue("email")
	pass := r.FormValue("password")
	postal := r.FormValue("postal_code")
	if pass == "" {
		_, oldEnc, _, ok, _ := s.Store.GetMercadonaCreds(r.Context(), u.ID)
		if !ok {
			http.Redirect(w, r, "/dashboard/mercadona?flash="+urlQuery("password required"), http.StatusFound)
			return
		}
		plain, err := s.Box.Open(oldEnc)
		if err != nil {
			http.Redirect(w, r, "/dashboard/mercadona?flash="+urlQuery("re-enter password"), http.StatusFound)
			return
		}
		pass = plain
	}
	enc, err := s.Box.Seal(pass)
	if err != nil {
		http.Redirect(w, r, "/dashboard/mercadona?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if err := s.Store.SaveMercadonaCreds(r.Context(), u.ID, email, enc, postal); err != nil {
		http.Redirect(w, r, "/dashboard/mercadona?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if s.OnMercadonaSave != nil {
		if err := s.OnMercadonaSave(r.Context(), u.ID, email, pass, postal); err != nil {
			http.Redirect(w, r, "/dashboard/mercadona?flash="+urlQuery("Mercadona login failed: "+err.Error()), http.StatusFound)
			return
		}
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "mercadona", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/mercadona?flash=Mercadona+linked", http.StatusFound)
}

func (s *Server) clearMercadona(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = s.Store.DeleteMercadonaCreds(r.Context(), u.ID)
	if s.OnMercadonaClear != nil {
		_ = s.OnMercadonaClear(r.Context(), u.ID)
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/mercadona", http.StatusFound)
}

func urlQuery(s string) string {
	return url.QueryEscape(s)
}

// CurrentUser is used by OAuth authorize to reuse the panel session.
func (s *Server) CurrentUser(r *http.Request) *store.User {
	return s.currentUser(r)
}

// CreateWebSession exposes session creation for OAuth login.
func (s *Server) CreateWebSession(ctx context.Context, userID string) (string, error) {
	return s.Store.CreateWebSession(ctx, userID, 30*24*time.Hour)
}

// SetSessionCookie is used after OAuth login.
func (s *Server) SetSessionCookie(w http.ResponseWriter, token string) {
	s.setSession(w, token)
}
