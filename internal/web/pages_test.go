package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	asst "github.com/kidandcat/takan/internal/assistant"
	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules"
)

// stubAssistant stands in for the running assistant so the panel can be
// rendered without a Telegram connection.
type stubAssistant struct{}

func (stubAssistant) Status(context.Context) asst.Status {
	return asst.Status{
		Enabled: true, BotUsername: "casa_bot", OwnerTelegram: 282611642,
		KnownChats: 2, RunningTasks: 1, ScheduledJobs: 3, Messages: 120,
		PollHealthy: true, LastPollOK: time.Now(),
	}
}

func (stubAssistant) Jobs() []asst.Job {
	return []asst.Job{{
		ID: "a1b2c3d4", Name: "weekly review", Type: asst.JobMessage,
		Cron: "0 9 * * 1", NextRun: time.Now().Add(time.Hour), LastStatus: "ok", Runs: 12,
	}}
}

func (stubAssistant) Tasks() []asst.Task {
	return []asst.Task{{
		ID: "51be991d", Title: "build", State: asst.TaskRunning,
		StartedAt: time.Now().Add(-time.Minute), Promoted: true,
	}}
}

func (stubAssistant) KillTask(string) (bool, error)        { return true, nil }
func (stubAssistant) DeleteJob(string) (bool, error)       { return true, nil }
func (stubAssistant) RunJob(context.Context, string) error { return nil }
func (stubAssistant) Workdir() string                      { return "/opt/atlas/data/workspace" }

// signedIn returns a session cookie for a bootstrapped owner.
func signedIn(t *testing.T, s *Server, st *store.Store) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	owner, err := st.BootstrapOwner(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.CreateWebSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "takan_session", Value: token}
}

// TestEveryPanelPageRenders is a boot-level guard. html/template resolves names
// at execute time, so a typo in a template or a field removed from pageData
// only shows up when somebody opens that page — which, for a single-operator
// panel, can be weeks later.
func TestEveryPanelPageRenders(t *testing.T) {
	s, st, h := testWeb(t)
	s.Assistant = stubAssistant{}
	cookie := signedIn(t, s, st)

	pages := map[string]string{
		"/dashboard":              "Overview",
		"/dashboard/integrations": "integrations",
		"/dashboard/assistant":    "Assistant",
		"/dashboard/machines":     "Machines",
		"/dashboard/display":      "Display",
		"/dashboard/tv":           "TV",
		"/dashboard/mercadona":    "Mercadona",
		"/dashboard/email":        "Email",
		"/dashboard/people":       "People",
		"/dashboard/health":       "Health",
		"/dashboard/vault":        "Vault",
		"/dashboard/instance":     "Sign-in",
	}
	for path, marker := range pages {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := doReq(h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d\n%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// A failed template execution writes the error into the response.
		for _, bad := range []string{"executing", "can't evaluate field", "incomplete or empty template"} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s: template error in the rendered page:\n%s", path, body)
			}
		}
		if !strings.Contains(body, marker) {
			t.Fatalf("%s: rendered page does not look right, missing %q", path, marker)
		}
	}
}

// TestPublicPagesRender covers the signed-out half of the panel.
func TestPublicPagesRender(t *testing.T) {
	_, _, h := testWeb(t)
	for _, path := range []string{"/", "/login"} {
		rec := doReq(h, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "can't evaluate field") {
			t.Fatalf("%s: template error:\n%s", path, rec.Body.String())
		}
	}
}

// TestAssistantPageRendersWithoutARunningAssistant: the page is where a broken
// assistant gets diagnosed, so it must render when the assistant failed to start.
func TestAssistantPageRendersWithoutARunningAssistant(t *testing.T) {
	s, st, h := testWeb(t)
	s.Assistant = nil
	cookie := signedIn(t, s, st)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/assistant", nil)
	req.AddCookie(cookie)
	rec := doReq(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "did not start") {
		t.Fatalf("the page should explain what is wrong:\n%s", body)
	}
	if !strings.Contains(body, "OWNER_TELEGRAM_ID") {
		t.Fatalf("the page should name the missing settings:\n%s", body)
	}
}

// TestRetiredPagesAreGone: the bot fleet and the Telegram channel layer no
// longer exist, and SIP went with them.
func TestRetiredPagesAreGone(t *testing.T) {
	s, st, h := testWeb(t)
	cookie := signedIn(t, s, st)

	for _, path := range []string{"/dashboard/bots", "/dashboard/sip"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		if rec := doReq(h, req); rec.Code != http.StatusNotFound {
			t.Fatalf("%s should be gone, got %d", path, rec.Code)
		}
	}
	// The old Telegram page redirects, because it is where the operator would
	// look for the bot settings.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/telegram", nil)
	req.AddCookie(cookie)
	rec := doReq(h, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard/assistant" {
		t.Fatalf("expected a redirect to the assistant page, got %d %q",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestNoDomainLiteralsInTemplates: the panel host is configuration. A hardcoded
// domain would break any other deployment and quietly leak Jairo's.
func TestNoDomainLiteralsInTemplates(t *testing.T) {
	entries, err := tmplFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := tmplFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"takan.es", "atlas.jairo.cloud", "jairo.cloud"} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("templates/%s hardcodes %q; use .PublicURL", e.Name(), banned)
			}
		}
	}
}

// TestNavCoversEveryModule guards the grouped sidebar: a module in the catalog
// with no nav entry is unreachable.
func TestNavCoversEveryModule(t *testing.T) {
	s, st, h := testWeb(t)
	s.Assistant = stubAssistant{}
	cookie := signedIn(t, s, st)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	body := doReq(h, req).Body.String()

	for _, mod := range modules.Catalog {
		if !strings.Contains(body, ">"+mod.Name+"\n") && !strings.Contains(body, mod.Name) {
			t.Errorf("module %q (%s) has no sidebar entry", mod.Name, mod.ID)
		}
	}
	for _, gone := range []string{"/dashboard/bots", "/dashboard/sip"} {
		if strings.Contains(body, `href="`+gone+`"`) {
			t.Errorf("the sidebar still links to %s", gone)
		}
	}
}
