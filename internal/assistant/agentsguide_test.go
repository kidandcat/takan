package assistant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgentsGuideOnlyWritesOnChange: the file lives in a directory the operator
// works in, and rewriting it every boot churns its mtime, which hides a real
// change in an `ls -lt` and makes every backup see a modified file.
func TestAgentsGuideOnlyWritesOnChange(t *testing.T) {
	workdir := t.TempDir()
	inbox := filepath.Join(workdir, "inbox")
	path := filepath.Join(workdir, "AGENTS.md")

	if err := WriteAgentsGuide(workdir, inbox); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Backdate it so an unnecessary rewrite is visible.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentsGuide(workdir, inbox); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(old) {
		t.Fatal("identical content must not be rewritten")
	}

	// A different inbox path is a real change and must land.
	if err := WriteAgentsGuide(workdir, "/somewhere/else"); err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changed), "/somewhere/else") {
		t.Fatal("a changed guide must be written")
	}
	_ = first
}

func TestWriteAgentsGuideRejectsAnEmptyWorkdir(t *testing.T) {
	if err := WriteAgentsGuide("", "/inbox"); err == nil {
		t.Fatal("an empty workdir must be an error, not a file at the process cwd")
	}
}
