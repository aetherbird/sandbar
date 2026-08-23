package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateUserScope points HOME and XDG_CONFIG_HOME at fresh temp dirs so
// prompt-file discovery never sees the real machine's config.
func isolateUserScope(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func writePromptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBuildMessagesPromptFileLayering proves SYSTEM.md replaces the
// configured base persona while the assembled surfaces (environment block,
// skills) and APPEND_SYSTEM.md still land in the system prompt.
func TestBuildMessagesPromptFileLayering(t *testing.T) {
	isolateUserScope(t)
	a, store, done := setupTestAgent(t, false)
	defer done()

	ws := t.TempDir()
	writePromptFile(t, filepath.Join(ws, ".sandbar", "SYSTEM.md"), "CUSTOM SYSTEM in {{cwd}}")
	writePromptFile(t, filepath.Join(ws, ".sandbar", "APPEND_SYSTEM.md"), "APPENDED RULES")
	if err := os.MkdirAll(filepath.Join(ws, ".sandbar", "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".sandbar", "skills", "deploy", "SKILL.md"),
		[]byte("description: Deploy things\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := mustBuildMessages(t, a, thread.ID, ws, "")
	sys := msgs[0].Msg.Content

	if !strings.Contains(sys, "CUSTOM SYSTEM in "+ws) {
		t.Errorf("custom SYSTEM.md missing or unrendered: %.80q", sys)
	}
	if strings.Contains(sys, "You are a test assistant.") {
		t.Error("SYSTEM.md must replace the configured base persona")
	}
	if !strings.Contains(sys, "# Environment") {
		t.Error("environment block must survive SYSTEM.md replacement")
	}
	if !strings.Contains(sys, "# Skills") {
		t.Error("skills section must survive SYSTEM.md replacement")
	}
	if !strings.Contains(sys, "APPENDED RULES") {
		t.Error("APPEND_SYSTEM.md missing from prompt")
	}
	if !strings.HasSuffix(strings.TrimSpace(sys), "APPENDED RULES") {
		t.Error("APPEND_SYSTEM.md must be appended at the end of the assembled prompt")
	}
}

// TestMaybeGenerateTitleUsesTemplate proves TITLE_SYSTEM.md names the thread
// locally from the first user message — no LLM summarizer call.
func TestMaybeGenerateTitleUsesTemplate(t *testing.T) {
	isolateUserScope(t)
	a, store, done := setupTestAgent(t, false)
	defer done()

	ws := t.TempDir()
	writePromptFile(t, filepath.Join(ws, ".sandbar", "TITLE_SYSTEM.md"), "task: {{firstLine}}")
	a.cfg.Workspace = ws

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := "fix the login flow\nthen add tests"
	if _, err := store.AppendMessage(thread.ID, "user", &first, nil); err != nil {
		t.Fatal(err)
	}

	a.maybeGenerateTitle(thread.ID, "test-model")

	got, err := store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title == nil || *got.Title != "task: fix the login flow" {
		t.Errorf("title = %v, want template rendered over the first user message", got.Title)
	}
}
