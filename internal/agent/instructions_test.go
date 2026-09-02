package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendDirInstructions pins the per-directory instruction injection: a
// file tool touching a path under a directory with AGENTS.md gets those
// instructions appended to its result exactly once, tools without a workspace
// path are untouched, and the workspace root's own files (already in the
// system prompt) are never injected.
func TestAppendDirInstructions(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "a", "b")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("Sub rule."), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "CLAUDE.md"), []byte("Root rule."), 0644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{}
	args := fmt.Sprintf(`{"path": %q, "old_str": "x", "new_str": "y"}`, "a/b/code.go")

	first := a.appendDirInstructions("file_patch", args, ws, "ok")
	if !strings.Contains(first, "ok\n\n[sandbar: instructions from a/b/AGENTS.md — applies to files under a/b]\nSub rule.") {
		t.Fatalf("injection missing or malformed:\n%s", first)
	}

	// Second call in the same directory: already injected, output untouched.
	if second := a.appendDirInstructions("file_read", args, ws, "ok2"); second != "ok2" {
		t.Fatalf("duplicate injection:\n%s", second)
	}

	// Non-path tools and URL-like targets are untouched.
	if got := a.appendDirInstructions("web_search", `{"query": "x"}`, ws, "done"); got != "done" {
		t.Fatalf("non-path tool got injection:\n%s", got)
	}
	// A different directory under the same subdirectory's scope still dedups by
	// file, so a sibling file in a/b is not re-injected either — but a new
	// subdirectory with its own file is.
	if got := a.appendDirInstructions("file_read", `{"path": "a/b/other.go"}`, ws, "done"); got != "done" {
		t.Fatalf("sibling path re-injected:\n%s", got)
	}
	other := filepath.Join(ws, "c")
	os.MkdirAll(other, 0755)
	os.WriteFile(filepath.Join(other, "CLAUDE.md"), []byte("Other rule."), 0644)
	if got := a.appendDirInstructions("file_read", `{"path": "c/z.go"}`, ws, "done"); !strings.Contains(got, "Other rule.") {
		t.Fatalf("new directory not injected:\n%s", got)
	}

	// URL-like file_read targets and paths escaping the workspace: untouched.
	if got := a.appendDirInstructions("file_read", `{"path": "pr://42"}`, ws, "done"); got != "done" {
		t.Fatalf("URL-like target got injection:\n%s", got)
	}
	if got := a.appendDirInstructions("file_read", `{"path": "../outside.go"}`, ws, "done"); got != "done" {
		t.Fatalf("outside-workspace path got injection:\n%s", got)
	}

	// The workspace root's own files stop the search — never injected here.
	if got := a.appendDirInstructions("file_read", `{"path": "top.go"}`, ws, "done"); got != "done" {
		t.Fatalf("workspace-root file injected:\n%s", got)
	}
}
