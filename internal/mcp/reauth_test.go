package mcp

import "testing"

func TestForceReauthMarkClear(t *testing.T) {
	var f forceReauth
	if f.Needs("u1") {
		t.Fatal("empty should not need reauth")
	}
	f.Mark("u1")
	if !f.Needs("u1") {
		t.Fatal("expected need reauth")
	}
	if f.Needs("u2") {
		t.Fatal("other user should be clean")
	}
	f.Clear("u1")
	if f.Needs("u1") {
		t.Fatal("cleared")
	}
}

func TestDropUserSessions(t *testing.T) {
	h := NewSessionHub()
	a := h.Create("user-a")
	b := h.Create("user-b")
	_ = h.Create("user-a")
	if n := h.DropUserSessions("user-a"); n != 2 {
		t.Fatalf("dropped %d want 2", n)
	}
	if h.Get(a.ID) != nil {
		t.Fatal("session a should be gone")
	}
	if h.Get(b.ID) == nil {
		t.Fatal("session b should remain")
	}
}
