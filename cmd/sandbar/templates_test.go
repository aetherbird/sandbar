package main

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateUserScope points HOME and XDG_CONFIG_HOME at fresh temp dirs so
// template discovery never sees the real machine's config.
func isolateUserScope(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func writeTemplateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSlashTemplateCommand proves an unknown "/name args" line expands a
// matching prompt template and submits it as the user message.
func TestSlashTemplateCommand(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeTemplateFile(t, filepath.Join(ws, ".sandbar", "prompts", "review.md"),
		"---\ndescription: Review a file\n---\n\nReview $1 carefully with $@.\n")

	m := appModel{sess: &session{workspace: ws, modelAlias: "test-model"}, width: 100}
	_ = m.slashCmd("/review auth.go fresh eyes")

	if !m.streaming {
		t.Fatal("template command must launch a stream with the expanded body")
	}
	if len(m.history) != 1 || m.history[0] != "/review auth.go fresh eyes" {
		t.Errorf("history = %v, want the typed template line kept for recall", m.history)
	}
}

// TestSlashTemplateCommandQueuedWhileStreaming proves a template expansion
// mid-turn is stashed like any steering message instead of dropped.
func TestSlashTemplateCommandQueuedWhileStreaming(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeTemplateFile(t, filepath.Join(ws, ".sandbar", "prompts", "review.md"),
		"Review $1 carefully.\n")

	m := appModel{sess: &session{workspace: ws}, width: 100, streaming: true}
	_ = m.slashCmd("/review auth.go")

	if len(m.pendingSends) != 1 || m.pendingSends[0] != "Review auth.go carefully.\n" {
		t.Errorf("pendingSends = %q, want the expanded body queued", m.pendingSends)
	}
}

// TestSlashCommandBeatsTemplate proves a registered command keeps precedence
// over a prompt template of the same name.
func TestSlashCommandBeatsTemplate(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeTemplateFile(t, filepath.Join(ws, ".sandbar", "prompts", "help.md"), "You are helpless.")

	m := appModel{sess: &session{workspace: ws}, width: 100}
	_ = m.slashCmd("/help")

	if m.streaming {
		t.Error("/help must dispatch the registered command, not a same-named template")
	}
}

// TestSlashUnknownWithoutTemplate proves the unknown-command notice still
// fires when nothing matches.
func TestSlashUnknownWithoutTemplate(t *testing.T) {
	isolateUserScope(t)
	m := appModel{sess: &session{workspace: t.TempDir()}, width: 100}
	_ = m.slashCmd("/nope")

	if m.streaming {
		t.Error("unknown command must not start a stream")
	}
	if len(m.history) != 0 {
		t.Errorf("history = %v, want nothing recorded", m.history)
	}
}
