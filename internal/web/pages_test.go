package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidandcat/takan/internal/store"
	"github.com/kidandcat/takan/modules"
)

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
	cookie := signedIn(t, s, st)

	pages := map[string]string{
		"/dashboard":              "Overview",
		"/dashboard/integrations": "integrations",
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

// TestRetiredPagesAreGone: the bot fleet, the Telegram channel layer, SIP and
// the Atlas assistant no longer exist. Takan is the MCP hub and its panel.
func TestRetiredPagesAreGone(t *testing.T) {
	s, st, h := testWeb(t)
	cookie := signedIn(t, s, st)

	for _, path := range []string{"/dashboard/bots", "/dashboard/sip", "/dashboard/telegram", "/dashboard/assistant"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		if rec := doReq(h, req); rec.Code != http.StatusNotFound {
			t.Fatalf("%s should be gone, got %d", path, rec.Code)
		}
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
	cookie := signedIn(t, s, st)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	body := doReq(h, req).Body.String()

	for _, mod := range modules.Catalog {
		if !strings.Contains(body, ">"+mod.Name+"\n") && !strings.Contains(body, mod.Name) {
			t.Errorf("module %q (%s) has no sidebar entry", mod.Name, mod.ID)
		}
	}
	for _, gone := range []string{"/dashboard/bots", "/dashboard/sip", "/dashboard/telegram", "/dashboard/assistant"} {
		if strings.Contains(body, `href="`+gone+`"`) {
			t.Errorf("the sidebar still links to %s", gone)
		}
	}
}
