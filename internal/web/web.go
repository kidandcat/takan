package web

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kidandcat/takan/internal/agenthub"
	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules"
	emailmod "github.com/kidandcat/takan/modules/email"
	machinemod "github.com/kidandcat/takan/modules/machine"
)

//go:embed templates/*.html
var tmplFS embed.FS

// Server serves the HTMX panel.
type Server struct {
	Store     *store.Store
	Hub       *agenthub.Hub
	Box       *cryptox.Box
	PublicURL string
	// DataDir is the Colmena/data root (people photos live under people-photos/).
	DataDir string
	// AllowRegister enables public self-signup without invite (default false).
	AllowRegister bool
	// DefaultInviteQuota applied to newly registered users.
	DefaultInviteQuota int
	// AuthRateLimit optional IP throttle for login/register.
	AuthRateLimit func(key string) bool
	// OnMercadonaSave logs into Mercadona and stores session tokens (optional).
	OnMercadonaSave func(ctx context.Context, userID, email, password, postal string) error
	// OnMercadonaClear unlinks Mercadona session for the user.
	OnMercadonaClear func(ctx context.Context, userID string) error
	// OnToolsChanged notifies MCP clients (tools/list_changed) after module changes.
	OnToolsChanged func(userID string)
	tmpl           *template.Template
}

func New(st *store.Store, hub *agenthub.Hub, box *cryptox.Box, publicURL, dataDir string, allowRegister bool, defaultInviteQuota int) (*Server, error) {
	t, err := template.ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if defaultInviteQuota <= 0 {
		defaultInviteQuota = store.DefaultInviteQuota
	}
	return &Server{
		Store: st, Hub: hub, Box: box, PublicURL: publicURL, DataDir: dataDir,
		AllowRegister: allowRegister, DefaultInviteQuota: defaultInviteQuota, tmpl: t,
	}, nil
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
	mux.HandleFunc("GET /dashboard/email", s.dashEmail)
	mux.HandleFunc("GET /dashboard/memory", s.dashMemory)
	mux.HandleFunc("GET /dashboard/people", s.dashPeople)
	mux.HandleFunc("GET /dashboard/invites", s.dashInvites)
	mux.HandleFunc("GET /dashboard/admin", s.dashAdmin)
	// Old routes → overview / integrations
	mux.HandleFunc("GET /dashboard/connect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard/modules", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/integrations", http.StatusFound)
	})
	mux.HandleFunc("POST /dashboard/modules/{id}/toggle", s.toggleModule)
	mux.HandleFunc("POST /dashboard/invites", s.createInvite)
	mux.HandleFunc("POST /dashboard/invites/{id}/revoke", s.revokeInvite)
	mux.HandleFunc("POST /dashboard/admin/users", s.adminInvitePolicy)
	// Legacy admin POST path
	mux.HandleFunc("POST /dashboard/invites/admin", s.adminInvitePolicy)
	mux.HandleFunc("POST /dashboard/machines", s.createMachine)
	mux.HandleFunc("POST /dashboard/machines/{id}/delete", s.deleteMachine)
	mux.HandleFunc("POST /dashboard/machines/ai", s.saveMachineAI)
	mux.HandleFunc("POST /dashboard/machines/ai/delete-runner", s.deleteMachineAIRunner)
	mux.HandleFunc("POST /dashboard/mercadona", s.saveMercadona)
	mux.HandleFunc("POST /dashboard/mercadona/clear", s.clearMercadona)
	mux.HandleFunc("POST /dashboard/email", s.saveEmail)
	mux.HandleFunc("POST /dashboard/email/refresh", s.refreshEmail)
	mux.HandleFunc("POST /dashboard/email/clear", s.clearEmail)
	mux.HandleFunc("POST /dashboard/email/domains/toggle", s.toggleEmailDomain)
	mux.HandleFunc("POST /dashboard/memory", s.saveMemory)
	mux.HandleFunc("POST /dashboard/people", s.createPerson)
	mux.HandleFunc("POST /dashboard/people/{id}", s.updatePerson)
	mux.HandleFunc("POST /dashboard/people/{id}/delete", s.deletePerson)
	mux.HandleFunc("GET /dashboard/people/{id}/photo", s.personPhoto)
}

type pageData struct {
	Title               string
	User                *store.User
	Error               string
	Flash               string
	FlashIsError        bool
	MCPURL              string
	PublicURL           string
	OAuthClientID       string
	OAuthAuthorize      string
	OAuthToken          string
	OAuthMetadata       string
	Modules             []modView
	Machines            []machView
	InstallCmd          string
	// Machine AI tasks (panel)
	AITasksEnabled bool
	AIRunners      []machineAIRunnerView
	// AITabActive selects the AI tasks tab on narrow screens (after AI save/error).
	AITabActive bool
	MercadonaConfigured bool
	MercadonaEmail      string
	MercadonaPostal     string
	EmailConfigured     bool
	EmailDomainRows     []emailDomainView
	EmailKeySet         bool
	MemoryContent       string
	MemoryUpdated       string
	People              []personView
	PeopleCount         int
	// Dashboard stats (precomputed for templates)
	ModEnabledCount int
	ModTotalCount   int
	MachOnlineCount int
	MachTotalCount  int
	// ActiveNav highlights the sidebar item: overview|integrations|machine|mercadona|…
	ActiveNav string
	// AllowRegister controls public signup CTAs and /register form.
	AllowRegister bool
	// Invite registration
	InviteRequired bool
	InviteCode     string
	// Invites panel
	InviteQuota     *store.InviteQuotaInfo
	Invites         []inviteView
	NewInviteCode   string // flash: show once after create
	AdminUsers      []adminUserView
	IsAdmin         bool
}

type inviteView struct {
	ID, Note, Status, Created, Expires, UsedBy string
	Used                                       bool
}

type adminUserView struct {
	ID, Email       string
	Quota           int
	Unlimited, Admin bool
	Created         int
}

type emailDomainView struct {
	Name, Status, Sending, Receiving string
	Enabled                          bool
}

type personView struct {
	ID, Name, Relationship, Context, Notes, Email, Phone, Birthday string
	TagsLine, AliasesLine, Initial, ContactsJSON, PhotoURL         string
	Contacts                                                       []personContactView
	HasContacts, HasPhoto                                          bool
}

type personContactView struct {
	Key, Value string
}

type modView struct {
	ID, Name, Description string
	Enabled, Ready        bool
	// Path is the module settings page (e.g. /dashboard/machines).
	Path string
	// Summary is a one-line key status for overview cards.
	Summary string
	// DetailsLine is compact inline text (e.g. domains joined by commas).
	DetailsLine string
	// Facts are compact chips, optionally with online/offline status dots.
	Facts []modFact
}

// modFact is a compact status item on an overview module card.
type modFact struct {
	Label string
	// Kind: "" | "online" | "offline"
	Kind string
}

type machView struct {
	ID, Name string
	Online   bool
}

type machineAIRunnerView struct {
	ID, Name, Command string
	Enabled, Builtin  bool
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
	data.AllowRegister = s.AllowRegister
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
	s.page(w, "home.html", pageData{Title: "Home", AllowRegister: s.AllowRegister})
}

func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	s.page(w, "login.html", pageData{Title: "Log in"})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if s.AuthRateLimit != nil && !s.AuthRateLimit("login:"+clientIP(r)) {
		s.page(w, "login.html", pageData{Title: "Log in", Error: "Too many attempts — try again later"})
		return
	}
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
	n, _ := s.Store.UserCount(r.Context())
	inviteRequired := !s.AllowRegister && n > 0
	code := r.URL.Query().Get("invite")
	s.page(w, "register.html", pageData{
		Title: "Register", AllowRegister: true, // form always shown; invite may be required
		InviteRequired: inviteRequired, InviteCode: code,
	})
}

func (s *Server) registerPost(w http.ResponseWriter, r *http.Request) {
	if s.AuthRateLimit != nil && !s.AuthRateLimit("register:"+clientIP(r)) {
		s.page(w, "register.html", pageData{
			Title: "Register", AllowRegister: true, Error: "Too many attempts — try again later",
			InviteRequired: !s.AllowRegister,
		})
		return
	}
	_ = r.ParseForm()
	n, _ := s.Store.UserCount(r.Context())
	inviteRequired := !s.AllowRegister && n > 0
	code := r.FormValue("invite")
	u, err := s.Store.CreateUserOpts(r.Context(), r.FormValue("email"), r.FormValue("password"), store.CreateUserOpts{
		InviteCode:    code,
		DefaultQuota:  s.DefaultInviteQuota,
		RequireInvite: inviteRequired,
		AllowOpen:     s.AllowRegister || n == 0,
	})
	if err != nil {
		s.page(w, "register.html", pageData{
			Title: "Register", AllowRegister: true, Error: err.Error(),
			InviteRequired: inviteRequired, InviteCode: code,
		})
		return
	}
	tok, err := s.Store.CreateWebSession(r.Context(), u.ID, 30*24*time.Hour)
	if err != nil {
		s.page(w, "register.html", pageData{Title: "Register", AllowRegister: true, Error: err.Error()})
		return
	}
	s.setSession(w, tok)
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func clientIP(r *http.Request) string {
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
		cfg, _ := machinemod.LoadConfig(r.Context(), s.Store, u.ID)
		data.AITasksEnabled = cfg.AITasksEnabled
		for _, rn := range cfg.Runners {
			data.AIRunners = append(data.AIRunners, machineAIRunnerView{
				ID: rn.ID, Name: rn.Name, Command: rn.Command,
				Enabled: rn.Enabled, Builtin: rn.Builtin,
			})
		}
		if tab := r.URL.Query().Get("tab"); tab == "tasks" || tab == "ai" {
			data.AITabActive = true
		}
	}
	if nav == "invites" {
		if c, err := r.Cookie("takan_invite_code"); err == nil && c.Value != "" {
			if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil {
				data.NewInviteCode = string(raw)
			}
			http.SetCookie(w, &http.Cookie{Name: "takan_invite_code", Value: "", Path: "/", MaxAge: -1})
		}
	}
	if f := r.URL.Query().Get("flash"); f != "" {
		data.Flash = f
		lf := strings.ToLower(f)
		data.FlashIsError = strings.Contains(lf, "error") ||
			strings.Contains(lf, "fail") ||
			strings.Contains(lf, "required") ||
			strings.Contains(lf, "re-enter")
		// After AI settings save/error, land on the AI tasks tab (mobile).
		if nav == "machine" && (strings.Contains(lf, "ai task") || strings.Contains(lf, "runner")) {
			data.AITabActive = true
		}
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
func (s *Server) dashEmail(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "email", "Email", "email.html")
}
func (s *Server) dashMemory(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "memory", "Memory", "memory.html")
}
func (s *Server) dashPeople(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "people", "People", "people.html")
}
func (s *Server) dashInvites(w http.ResponseWriter, r *http.Request) {
	s.dashPage(w, r, "invites", "Invites", "invites.html")
}
func (s *Server) dashAdmin(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !u.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.dashPage(w, r, "admin", "Admin", "admin.html")
}

func (s *Server) buildDashboard(ctx context.Context, u *store.User) pageData {
	data := pageData{
		Title:          "Dashboard",
		User:           u,
		MCPURL:         s.PublicURL + "/mcp",
		PublicURL:      s.PublicURL,
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
			onlineN := 0
			for _, mac := range ms {
				on := s.Hub.Online(mac.ID)
				kind := "offline"
				if on {
					onlineN++
					kind = "online"
				}
				mv.Facts = append(mv.Facts, modFact{Label: mac.Name, Kind: kind})
			}
			if len(ms) == 0 {
				mv.Summary = "No machines registered"
			} else {
				mv.Summary = fmt.Sprintf("%d online · %d total", onlineN, len(ms))
			}
			mv.Ready = m.Enabled && onlineN > 0
		case "mercadona":
			mv.Path = "/dashboard/mercadona"
			em, _, postal, ok, _ := s.Store.GetMercadonaCreds(ctx, u.ID)
			if ok {
				mv.Summary = "Linked"
				mv.DetailsLine = em + " · CP " + postal
			} else {
				mv.Summary = "Not linked"
			}
			mv.Ready = m.Enabled && ok
		case "email":
			mv.Path = "/dashboard/email"
			_, domains, ok, _ := s.Store.GetEmailSettings(ctx, u.ID)
			en := store.EnabledEmailDomains(domains)
			if !ok {
				mv.Summary = "No API key"
			} else if len(en) == 0 {
				mv.Summary = fmt.Sprintf("0 enabled · %d discovered", len(domains))
			} else {
				mv.Summary = fmt.Sprintf("%d enabled · %d total", len(en), len(domains))
				mv.DetailsLine = strings.Join(en, ", ")
			}
			mv.Ready = m.Enabled && ok && len(en) > 0
		case "memory":
			mv.Path = "/dashboard/memory"
			content, updated, mok, _ := s.Store.GetMemory(ctx, u.ID)
			if !mok || strings.TrimSpace(content) == "" {
				mv.Summary = "Empty"
			} else {
				lines := strings.Count(content, "\n") + 1
				mv.Summary = fmt.Sprintf("%d lines · %d chars", lines, len(content))
				var bits []string
				if !updated.IsZero() {
					bits = append(bits, "updated "+updated.UTC().Format("2006-01-02 15:04"))
				}
				for _, line := range strings.Split(content, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					if len(line) > 64 {
						line = line[:61] + "…"
					}
					bits = append(bits, line)
					break
				}
				mv.DetailsLine = strings.Join(bits, " · ")
			}
			mv.Ready = m.Enabled
		case "people":
			mv.Path = "/dashboard/people"
			n, _ := s.Store.CountPeople(ctx, u.ID)
			if n == 0 {
				mv.Summary = "No people yet"
			} else {
				mv.Summary = fmt.Sprintf("%d people", n)
				list, _ := s.Store.ListPeople(ctx, u.ID, "", 8)
				var names []string
				for _, p := range list {
					if p.Relationship != "" {
						names = append(names, p.Name+" ("+p.Relationship+")")
					} else {
						names = append(names, p.Name)
					}
				}
				mv.DetailsLine = strings.Join(names, ", ")
				if n > len(list) {
					mv.DetailsLine += fmt.Sprintf(" · +%d more", n-len(list))
				}
			}
			mv.Ready = m.Enabled
		default:
			mv.Path = "/dashboard/" + m.ModuleID
		}
		// Cap machine facts on overview cards.
		if len(mv.Facts) > 6 {
			extra := len(mv.Facts) - 5
			mv.Facts = append(mv.Facts[:5], modFact{Label: fmt.Sprintf("+%d more", extra)})
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
	if _, domains, eok, _ := s.Store.GetEmailSettings(ctx, u.ID); eok {
		data.EmailConfigured = true
		data.EmailKeySet = true
		for _, d := range domains {
			data.EmailDomainRows = append(data.EmailDomainRows, emailDomainView{
				Name: d.Name, Status: d.Status, Sending: d.Sending, Receiving: d.Receiving, Enabled: d.Enabled,
			})
		}
	}
	if content, updated, mok, _ := s.Store.GetMemory(ctx, u.ID); mok {
		data.MemoryContent = content
		if !updated.IsZero() {
			data.MemoryUpdated = updated.UTC().Format(time.RFC3339)
		}
	}
	if plist, err := s.Store.ListPeople(ctx, u.ID, "", 100); err == nil {
		data.PeopleCount = len(plist)
		for _, p := range plist {
			pv := personView{
				ID: p.ID, Name: p.Name, Relationship: p.Relationship,
				Context: p.Context, Notes: p.Notes, Email: p.Email, Phone: p.Phone,
				Birthday: p.Birthday, Initial: personInitial(p.Name),
			}
			if len(p.Tags) > 0 {
				pv.TagsLine = strings.Join(p.Tags, ", ")
			}
			if len(p.Aliases) > 0 {
				pv.AliasesLine = strings.Join(p.Aliases, ", ")
			}
			for _, pair := range store.ContactPairs(p.Contacts) {
				pv.Contacts = append(pv.Contacts, personContactView{Key: pair[0], Value: pair[1]})
			}
			pv.HasContacts = len(pv.Contacts) > 0
			if b, err := json.Marshal(p.Contacts); err == nil && len(p.Contacts) > 0 {
				pv.ContactsJSON = string(b)
			} else {
				pv.ContactsJSON = "{}"
			}
			if p.Photo != "" {
				pv.HasPhoto = true
				// Cache-bust when the person row is updated (photo change updates updated_at).
				pv.PhotoURL = fmt.Sprintf("/dashboard/people/%s/photo?v=%d", url.PathEscape(p.ID), p.UpdatedAt.Unix())
			}
			data.People = append(data.People, pv)
		}
	}
	// Invites panel data
	data.IsAdmin = u.IsAdmin
	if q, err := s.Store.InviteQuota(ctx, u.ID); err == nil {
		data.InviteQuota = q
	}
	if list, err := s.Store.ListInvites(ctx, u.ID); err == nil {
		for _, inv := range list {
			iv := inviteView{
				ID: inv.ID, Note: inv.Note,
				Created: inv.CreatedAt.UTC().Format("2006-01-02 15:04"),
			}
			if inv.ExpiresAt != nil {
				iv.Expires = inv.ExpiresAt.UTC().Format("2006-01-02")
			}
			if inv.UsedAt != nil {
				iv.Used = true
				iv.Status = "used"
				iv.UsedBy = inv.UsedBy
			} else if inv.ExpiresAt != nil && time.Now().UTC().After(*inv.ExpiresAt) {
				iv.Status = "expired"
			} else {
				iv.Status = "open"
			}
			data.Invites = append(data.Invites, iv)
		}
	}
	if u.IsAdmin {
		if users, err := s.Store.ListUsersForAdmin(ctx); err == nil {
			for _, au := range users {
				av := adminUserView{
					ID: au.ID, Email: au.Email, Quota: au.InviteQuota,
					Unlimited: au.InviteUnlimited, Admin: au.IsAdmin,
				}
				if info, err := s.Store.InviteQuota(ctx, au.ID); err == nil {
					av.Created = info.Created
				}
				data.AdminUsers = append(data.AdminUsers, av)
			}
		}
	}
	return data
}

func (s *Server) toggleModule(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
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
		http.Redirect(w, r, "/dashboard/integrations?flash="+urlQuery("unknown module"), http.StatusFound)
		return
	}
	_ = r.ParseForm()
	en := r.FormValue("enabled") == "1"
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, id, en)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.redirectBack(w, r, "/dashboard")
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	note := r.FormValue("note")
	ttl := 30 * 24 * time.Hour
	if d := r.FormValue("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			if n > 365 {
				n = 365
			}
			ttl = time.Duration(n) * 24 * time.Hour
		}
	}
	inv, err := s.Store.CreateInvite(r.Context(), u.ID, note, ttl)
	if err != nil {
		http.Redirect(w, r, "/dashboard/invites?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	// One-time flash of the raw code via cookie (not logged).
	http.SetCookie(w, &http.Cookie{
		Name: "takan_invite_code", Value: base64.RawURLEncoding.EncodeToString([]byte(inv.RawCode)),
		Path: "/", MaxAge: 300, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.PublicURL, "https"),
	})
	http.Redirect(w, r, "/dashboard/invites?flash=Invite+created+—+copy+the+code+now", http.StatusFound)
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.RevokeUnusedInvite(r.Context(), u.ID, r.PathValue("id")); err != nil {
		http.Redirect(w, r, "/dashboard/invites?flash="+urlQuery("could not revoke"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/invites?flash=Invite+revoked", http.StatusFound)
}

func (s *Server) adminInvitePolicy(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !u.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	target := r.FormValue("user_id")
	quota, _ := strconv.Atoi(r.FormValue("quota"))
	unlimited := r.FormValue("unlimited") == "1"
	isAdmin := r.FormValue("is_admin") == "1"
	if err := s.Store.SetUserInvitePolicy(r.Context(), target, quota, unlimited, isAdmin); err != nil {
		http.Redirect(w, r, "/dashboard/admin?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/admin?flash=User+updated", http.StatusFound)
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

func (s *Server) saveMachineAI(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	cfg := machinemod.Config{
		AITasksEnabled: r.FormValue("ai_tasks_enabled") == "1",
	}
	ids := r.Form["runner_id"]
	names := r.Form["runner_name"]
	cmds := r.Form["runner_command"]
	builtins := r.Form["runner_builtin"]
	enabledSet := map[string]bool{}
	for _, id := range r.Form["runner_enabled"] {
		enabledSet[id] = true
	}
	n := len(ids)
	for i := 0; i < n; i++ {
		id := strings.TrimSpace(ids[i])
		name, cmd, builtin := "", "", false
		if i < len(names) {
			name = names[i]
		}
		if i < len(cmds) {
			cmd = cmds[i]
		}
		if i < len(builtins) {
			builtin = builtins[i] == "1"
		}
		if id == "" && name == "" && cmd == "" {
			continue
		}
		cfg.Runners = append(cfg.Runners, machinemod.Runner{
			ID: id, Name: name, Command: cmd,
			Enabled: enabledSet[id], Builtin: builtin,
		})
	}
	// Optional free command added in the same form.
	newName := strings.TrimSpace(r.FormValue("new_name"))
	newCmd := strings.TrimSpace(r.FormValue("new_command"))
	if newCmd != "" {
		id := strings.TrimSpace(r.FormValue("new_id"))
		if id == "" {
			id = newName
		}
		cfg.Runners = append(cfg.Runners, machinemod.Runner{
			ID: id, Name: newName, Command: newCmd, Enabled: true, Builtin: false,
		})
	}
	if err := machinemod.SaveConfig(r.Context(), s.Store, u.ID, cfg); err != nil {
		http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("AI tasks settings saved"), http.StatusFound)
}

func (s *Server) deleteMachineAIRunner(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	delID := strings.TrimSpace(r.FormValue("id"))
	if delID == "" {
		http.Redirect(w, r, "/dashboard/machines", http.StatusFound)
		return
	}
	cfg, err := machinemod.LoadConfig(r.Context(), s.Store, u.ID)
	if err != nil {
		http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	var next []machinemod.Runner
	for _, rn := range cfg.Runners {
		if rn.ID == delID {
			if rn.Builtin {
				http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("error: cannot delete builtin runner — disable it instead"), http.StatusFound)
				return
			}
			continue
		}
		next = append(next, rn)
	}
	cfg.Runners = next
	if err := machinemod.SaveConfig(r.Context(), s.Store, u.ID, cfg); err != nil {
		http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/machines?flash="+urlQuery("Runner removed"), http.StatusFound)
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

func (s *Server) saveEmail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	key := strings.TrimSpace(r.FormValue("api_key"))
	var plainKey string
	if key == "" {
		oldEnc, _, ok, _ := s.Store.GetEmailSettings(r.Context(), u.ID)
		if !ok {
			http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("api key required"), http.StatusFound)
			return
		}
		var err error
		plainKey, err = s.Box.Open(oldEnc)
		if err != nil {
			http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("re-enter api key"), http.StatusFound)
			return
		}
	} else {
		plainKey = key
		enc, err := s.Box.Seal(key)
		if err != nil {
			http.Redirect(w, r, "/dashboard/email?flash="+urlQuery(err.Error()), http.StatusFound)
			return
		}
		if err := s.Store.SaveEmailAPIKey(r.Context(), u.ID, enc); err != nil {
			http.Redirect(w, r, "/dashboard/email?flash="+urlQuery(err.Error()), http.StatusFound)
			return
		}
	}
	if err := s.syncEmailDomains(r.Context(), u.ID, plainKey); err != nil {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("key saved but domain sync failed: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "email", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/email?flash=Email+saved", http.StatusFound)
}

func (s *Server) refreshEmail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	enc, _, ok, _ := s.Store.GetEmailSettings(r.Context(), u.ID)
	if !ok {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("save api key first"), http.StatusFound)
		return
	}
	plain, err := s.Box.Open(enc)
	if err != nil {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("decrypt key failed"), http.StatusFound)
		return
	}
	if err := s.syncEmailDomains(r.Context(), u.ID, plain); err != nil {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/email?flash=Domains+refreshed", http.StatusFound)
}

func (s *Server) syncEmailDomains(ctx context.Context, userID, apiKey string) error {
	fromAPI, err := emailmod.FetchDomains(ctx, apiKey)
	if err != nil {
		return err
	}
	_, prev, _, _ := s.Store.GetEmailSettings(ctx, userID)
	merged := store.MergeEmailDomains(prev, fromAPI)
	return s.Store.SaveEmailDomains(ctx, userID, merged)
}

func (s *Server) toggleEmailDomain(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("domain"))
	en := r.FormValue("enabled") == "1"
	_, domains, ok, err := s.Store.GetEmailSettings(r.Context(), u.ID)
	if err != nil || !ok {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("email not configured"), http.StatusFound)
		return
	}
	found := false
	for i := range domains {
		if strings.EqualFold(domains[i].Name, name) {
			domains[i].Enabled = en
			found = true
			break
		}
	}
	if !found {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery("domain not found"), http.StatusFound)
		return
	}
	if err := s.Store.SaveEmailDomains(r.Context(), u.ID, domains); err != nil {
		http.Redirect(w, r, "/dashboard/email?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	// HTMX: swap only the domain list so scroll position is preserved.
	if r.Header.Get("HX-Request") == "true" {
		data := pageData{User: u, EmailConfigured: true}
		for _, d := range domains {
			data.EmailDomainRows = append(data.EmailDomainRows, emailDomainView{
				Name: d.Name, Status: d.Status, Sending: d.Sending, Receiving: d.Receiving, Enabled: d.Enabled,
			})
		}
		s.renderEmailDomainList(w, data)
		return
	}
	http.Redirect(w, r, "/dashboard/email#email-domains", http.StatusFound)
}

func (s *Server) renderEmailDomainList(w http.ResponseWriter, data pageData) {
	t, err := template.ParseFS(tmplFS, "templates/email.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "email_domain_list", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) clearEmail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = s.Store.DeleteEmailSettings(r.Context(), u.ID)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/email", http.StatusFound)
}

func (s *Server) saveMemory(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	content := r.FormValue("content")
	if err := s.Store.SetMemory(r.Context(), u.ID, content); err != nil {
		http.Redirect(w, r, "/dashboard/memory?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "memory", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/memory?flash=Memory+saved", http.StatusFound)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func personInitial(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "?"
	}
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}

const maxPersonPhotoBytes = 3 << 20 // 3 MiB

func (s *Server) createPerson(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := r.ParseMultipartForm(maxPersonPhotoBytes + (1 << 20)); err != nil {
		_ = r.ParseForm()
	}
	p := store.Person{
		UserID:       u.ID,
		Name:         r.FormValue("name"),
		Relationship: r.FormValue("relationship"),
		Context:      r.FormValue("context"),
		Notes:        r.FormValue("notes"),
		Email:        r.FormValue("email"),
		Phone:        r.FormValue("phone"),
		Contacts:     parseFormContacts(r),
		Birthday:     r.FormValue("birthday"),
		Aliases:      splitCSV(r.FormValue("aliases")),
		Tags:         splitCSV(r.FormValue("tags")),
	}
	out, err := s.Store.CreatePerson(r.Context(), p)
	if err != nil {
		http.Redirect(w, r, "/dashboard/people?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if _, err := s.savePersonPhotoFromForm(r, u.ID, out.ID); err != nil {
		http.Redirect(w, r, "/dashboard/people?flash="+urlQuery("person saved but photo: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "people", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/people?flash=Person+added", http.StatusFound)
}

func (s *Server) updatePerson(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := r.ParseMultipartForm(maxPersonPhotoBytes + (1 << 20)); err != nil {
		_ = r.ParseForm()
	}
	id := r.PathValue("id")
	fields := map[string]string{
		"name":         r.FormValue("name"),
		"relationship": r.FormValue("relationship"),
		"context":      r.FormValue("context"),
		"notes":        r.FormValue("notes"),
		"email":        r.FormValue("email"),
		"phone":        r.FormValue("phone"),
		"birthday":     r.FormValue("birthday"),
	}
	if fields["name"] == "" {
		http.Redirect(w, r, "/dashboard/people?flash="+urlQuery("name required"), http.StatusFound)
		return
	}
	if _, err := s.Store.UpdatePersonFields(r.Context(), u.ID, id, fields,
		splitCSV(r.FormValue("aliases")), splitCSV(r.FormValue("tags")), true, true,
		parseFormContacts(r), true); err != nil {
		http.Redirect(w, r, "/dashboard/people?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if r.FormValue("remove_photo") == "1" {
		if err := s.clearPersonPhoto(r.Context(), u.ID, id); err != nil {
			http.Redirect(w, r, "/dashboard/people?flash="+urlQuery(err.Error()), http.StatusFound)
			return
		}
	} else if _, err := s.savePersonPhotoFromForm(r, u.ID, id); err != nil {
		http.Redirect(w, r, "/dashboard/people?flash="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/people?flash=Person+updated", http.StatusFound)
}

// parseFormContacts builds a map from parallel contact_key[] / contact_value[] form fields.
func parseFormContacts(r *http.Request) map[string]string {
	keys := r.Form["contact_key"]
	vals := r.Form["contact_value"]
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]string)
	for i, k := range keys {
		k = strings.TrimSpace(k)
		v := ""
		if i < len(vals) {
			v = strings.TrimSpace(vals[i])
		}
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) deletePerson(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	if p, err := s.Store.GetPerson(r.Context(), u.ID, id); err == nil && p != nil {
		s.removePersonPhotoFiles(u.ID, p.ID, p.Photo)
	}
	_ = s.Store.DeletePerson(r.Context(), u.ID, id)
	http.Redirect(w, r, "/dashboard/people", http.StatusFound)
}

func (s *Server) personPhoto(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	p, err := s.Store.GetPerson(r.Context(), u.ID, id)
	if err != nil || p == nil || p.Photo == "" {
		http.NotFound(w, r)
		return
	}
	path := s.personPhotoPath(u.ID, p.ID, p.Photo)
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", photoContentType(p.Photo))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "photo."+p.Photo, st.ModTime(), f)
}

func (s *Server) personPhotoDir(userID string) string {
	return filepath.Join(s.DataDir, "people-photos", userID)
}

func (s *Server) personPhotoPath(userID, personID, ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
	return filepath.Join(s.personPhotoDir(userID), personID+"."+ext)
}

// savePersonPhotoFromForm stores an uploaded photo if present. Returns (saved, err).
func (s *Server) savePersonPhotoFromForm(r *http.Request, userID, personID string) (bool, error) {
	file, hdr, err := r.FormFile("photo")
	if err == http.ErrMissingFile || err == http.ErrNotMultipart {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("photo upload: %w", err)
	}
	defer file.Close()
	if hdr.Size > maxPersonPhotoBytes {
		return false, fmt.Errorf("photo too large (max 3 MB)")
	}
	// Read limited bytes + sniff type.
	limited := io.LimitReader(file, maxPersonPhotoBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return false, fmt.Errorf("read photo: %w", err)
	}
	if len(data) > maxPersonPhotoBytes {
		return false, fmt.Errorf("photo too large (max 3 MB)")
	}
	if len(data) == 0 {
		return false, nil
	}
	ct := http.DetectContentType(data)
	ext, ok := photoExtForContentType(ct)
	if !ok {
		return false, fmt.Errorf("unsupported image type (use JPEG, PNG, WebP or GIF)")
	}
	dir := s.personPhotoDir(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	// Drop previous extension if different.
	if cur, err := s.Store.GetPerson(r.Context(), userID, personID); err == nil && cur != nil && cur.Photo != "" && cur.Photo != ext {
		_ = os.Remove(s.personPhotoPath(userID, personID, cur.Photo))
	}
	path := s.personPhotoPath(userID, personID, ext)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if _, err := s.Store.UpdatePersonFields(r.Context(), userID, personID, map[string]string{"photo": ext},
		nil, nil, false, false, nil, false); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}

func (s *Server) clearPersonPhoto(ctx context.Context, userID, personID string) error {
	p, err := s.Store.GetPerson(ctx, userID, personID)
	if err != nil {
		return err
	}
	s.removePersonPhotoFiles(userID, personID, p.Photo)
	_, err = s.Store.UpdatePersonFields(ctx, userID, personID, map[string]string{"photo": ""},
		nil, nil, false, false, nil, false)
	return err
}

func (s *Server) removePersonPhotoFiles(userID, personID, knownExt string) {
	if knownExt != "" {
		_ = os.Remove(s.personPhotoPath(userID, personID, knownExt))
	}
	for _, ext := range []string{"jpg", "jpeg", "png", "webp", "gif"} {
		_ = os.Remove(s.personPhotoPath(userID, personID, ext))
	}
}

func photoExtForContentType(ct string) (string, bool) {
	switch {
	case strings.HasPrefix(ct, "image/jpeg"):
		return "jpg", true
	case strings.HasPrefix(ct, "image/png"):
		return "png", true
	case strings.HasPrefix(ct, "image/webp"):
		return "webp", true
	case strings.HasPrefix(ct, "image/gif"):
		return "gif", true
	default:
		return "", false
	}
}

func photoContentType(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
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
