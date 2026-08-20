package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisplaySetHTMLAndContent(t *testing.T) {
	dir := t.TempDir()
	d := newDisplayServer("127.0.0.1:0", dir)
	if !strings.Contains(string(d.html), "waiting") {
		t.Fatalf("idle: %q", d.html)
	}
	if err := d.setHTML("<html><body>hello oficina</body></html>"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/content", nil)
	rec := httptest.NewRecorder()
	d.content(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "hello oficina") {
		t.Fatalf("body: %s", body)
	}
	persisted, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "hello oficina") {
		t.Fatalf("persist: %s", persisted)
	}
}

func TestDisplayEmptyClearsToIdle(t *testing.T) {
	d := newDisplayServer("", t.TempDir())
	_ = d.setHTML("<html>x</html>")
	if err := d.setHTML(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.html), "waiting") {
		t.Fatalf("want idle, got %s", d.html)
	}
}

func TestDisplayRejectsHugeHTML(t *testing.T) {
	d := newDisplayServer("", t.TempDir())
	big := strings.Repeat("a", maxDisplayHTML+1)
	if err := d.setHTML(big); err == nil {
		t.Fatal("expected size error")
	}
}

func TestDisplayReloadsPersistedHTML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>saved</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDisplayServer("", dir)
	if !strings.Contains(string(d.html), "saved") {
		t.Fatalf("got %s", d.html)
	}
}
