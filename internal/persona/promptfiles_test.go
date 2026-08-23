package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePromptFile writes content at path, creating parent directories.
func writePromptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPromptFilesProjectWinsOverUser(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	user := filepath.Join(root, "config")

	// Project scope: .sandbar wins over .claude within the same scope.
	writePromptFile(t, filepath.Join(proj, ".sandbar", "SYSTEM.md"), "project sandbar system")
	writePromptFile(t, filepath.Join(proj, ".claude", "SYSTEM.md"), "project claude system (should lose)")
	writePromptFile(t, filepath.Join(proj, ".codex", "APPEND_SYSTEM.md"), "project codex append")
	// User scope: only a TITLE_SYSTEM.md lives here.
	writePromptFile(t, filepath.Join(user, "TITLE_SYSTEM.md"), "user title template")

	set := DiscoverPromptFiles(proj, user)
	if set.System != "project sandbar system" {
		t.Fatalf("System = %q, want project sandbar system (project+first base wins)", set.System)
	}
	if set.Append != "project codex append" {
		t.Fatalf("Append = %q, want project codex append", set.Append)
	}
	if set.Title != "user title template" {
		t.Fatalf("Title = %q, want user title template", set.Title)
	}
}

func TestDiscoverPromptFilesUserScopeBases(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	user := filepath.Join(root, "config")
	home := t.TempDir()
	// Redirect the home lookup by setting HOME for the duration.
	t.Setenv("HOME", home)

	// sandbar native config dir wins over ~/.claude within user scope.
	writePromptFile(t, filepath.Join(user, "SYSTEM.md"), "native system")
	writePromptFile(t, filepath.Join(home, ".claude", "SYSTEM.md"), "claude system (should lose)")
	// ~/.agents carries APPEND_SYSTEM.md with no native file.
	writePromptFile(t, filepath.Join(home, ".agents", "APPEND_SYSTEM.md"), "agents append")

	set := DiscoverPromptFiles(proj, user)
	if set.System != "native system" {
		t.Fatalf("System = %q, want native system (native dir first in user scope)", set.System)
	}
	if set.Append != "agents append" {
		t.Fatalf("Append = %q, want agents append", set.Append)
	}
}

func TestDiscoverPromptFilesNothingFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	set := DiscoverPromptFiles(filepath.Join(root, "proj"), filepath.Join(root, "config"))
	if set.System != "" || set.Append != "" || set.Title != "" {
		t.Fatalf("empty discovery should return empty set, got %+v", set)
	}
}

func TestRenderPromptVariables(t *testing.T) {
	out := RenderPrompt("cwd={{cwd}} date={{date}}", "/tmp/somewhere")
	if !strings.HasPrefix(out, "cwd=/tmp/somewhere date=") {
		t.Fatalf("render = %q, want cwd and date substituted", out)
	}
	if !strings.Contains(out, "date=") {
		t.Fatalf("render = %q, want date substituted", out)
	}
}

func TestRenderPromptBrokenTemplateFallsBack(t *testing.T) {
	// A syntax error or unknown variable must not break the prompt: the
	// raw content survives.
	for _, content := range []string{
		"{{broken",         // syntax error
		"{{cwd}} {{nope}}", // unknown variable
	} {
		out := RenderPrompt(content, "/tmp/x")
		if !strings.Contains(out, "{{") {
			t.Fatalf("RenderPrompt(%q) = %q, want raw fallback containing {{", content, out)
		}
	}
}

func TestRenderTitleTemplate(t *testing.T) {
	out := RenderTitle("{{firstLine}}", "fix the bug\nsecond line", "/ws")
	if out != "fix the bug" {
		t.Fatalf("title = %q, want first line", out)
	}
	out = RenderTitle("{{message}}", "  spaced message  ", "/ws")
	if out != "spaced message" {
		t.Fatalf("title = %q, want trimmed message", out)
	}
	out = RenderTitle("custom: {{firstLine}}", "hello", "/ws")
	if out != "custom: hello" {
		t.Fatalf("title = %q, want custom prefix", out)
	}
}

func TestRenderTitleTemplateFallback(t *testing.T) {
	// A broken title template falls back to the first line.
	out := RenderTitle("{{broken", "first line\nrest", "/ws")
	if out != "first line" {
		t.Fatalf("title = %q, want first line fallback", out)
	}
	// So does a template that renders to nothing.
	if out := RenderTitle("   ", "first line\nrest", "/ws"); out != "first line" {
		t.Fatalf("title = %q, want first line fallback for empty render", out)
	}
}
