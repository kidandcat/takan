package assistant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ResolveChat validates a delivery target.
//
// The assistant will send to any chat id it is given, and the callers that
// supply one — the loopback API, the MCP tool, the task and job APIs — are
// reachable by the CLI agent, which runs model-authored commands. A typo'd or
// model-invented id would deliver the operator's private conversation to a
// stranger's chat, so a target is only accepted once the assistant has actually
// served it: his own chat, or a group he has already used it in.
//
// Zero means the owner's own chat.
func (a *Assistant) ResolveChat(ctx context.Context, chatID int64) (int64, error) {
	if chatID == 0 || chatID == a.OwnerTelegram {
		return a.OwnerTelegram, nil
	}
	known, err := a.Store.ListAssistantChats(ctx, a.OwnerID)
	if err != nil {
		return 0, err
	}
	want := strconv.FormatInt(chatID, 10)
	for _, c := range known {
		if c.ChatID == want {
			return chatID, nil
		}
	}
	return 0, fmt.Errorf("unknown chat %d — the assistant has never served it; "+
		"message it there first, or omit chat_id to use your own chat", chatID)
}

// ResolveOutboundFile validates a local path offered for upload.
//
// atlas-send takes a path from the CLI agent, so without this the agent could
// exfiltrate any file the hub can read — /etc/takan/takan.env, the SQLite
// database — straight to Telegram. Uploads are confined to the assistant's own
// directories: the inbox it downloads into and the workspace it works in.
func (a *Assistant) ResolveOutboundFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", filepath.Base(path), err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", filepath.Base(path))
	}

	for _, root := range a.uploadRoots() {
		if root == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if within(real, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%s is outside the assistant's data directory; "+
		"copy it into the workspace first", filepath.Base(path))
}

// uploadRoots are the directories a file may be sent from.
func (a *Assistant) uploadRoots() []string {
	return []string{a.dataDir, a.workdir}
}

// within reports whether path is root or sits underneath it. It compares
// cleaned paths component-wise, so "/data-other" does not match root "/data".
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
