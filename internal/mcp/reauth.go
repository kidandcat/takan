package mcp

import "sync"

// forceReauth tracks users that must complete a new OAuth token grant
// (authorize or refresh) before MCP accepts their bearer again.
// Used when the tool set changes: most clients ignore list_changed.
type forceReauth struct {
	mu    sync.Mutex
	users map[string]struct{}
}

func (f *forceReauth) Mark(userID string) {
	if userID == "" {
		return
	}
	f.mu.Lock()
	if f.users == nil {
		f.users = make(map[string]struct{})
	}
	f.users[userID] = struct{}{}
	f.mu.Unlock()
}

func (f *forceReauth) Needs(userID string) bool {
	if userID == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.users[userID]
	return ok
}

func (f *forceReauth) Clear(userID string) {
	if userID == "" {
		return
	}
	f.mu.Lock()
	delete(f.users, userID)
	f.mu.Unlock()
}
