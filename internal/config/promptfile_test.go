package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalConfig is the smallest config that passes Load: one provider with a
// loopback-bound server and a valid model entry.
const minimalConfig = `
server:
  host: 127.0.0.1
  port: 8377
providers:
  - name: test-provider
    base_url: http://localhost:1/v1
    api_key: ""
    models:
      test-model:
        context_length: 8192
`

// writeConfig writes a config into dir and returns its path.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestSystemPromptFileOverridesInline proves system_prompt_file wins over an
// inline system_prompt and that relative paths resolve against the config
// file's directory (config and prompt travel together).
func TestSystemPromptFileOverridesInline(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "system-prompt.md")
	if err := os.WriteFile(promptPath, []byte("# File persona\n\nYou are from the file."), 0600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	path := writeConfig(t, dir, minimalConfig+`
persona:
  system_prompt: "inline persona"
  system_prompt_file: system-prompt.md
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(cfg.Persona.SystemPrompt, "You are from the file.") {
		t.Errorf("system_prompt_file did not override inline: %q", cfg.Persona.SystemPrompt)
	}
	if strings.Contains(cfg.Persona.SystemPrompt, "inline persona") {
		t.Errorf("inline persona should be replaced, not concatenated: %q", cfg.Persona.SystemPrompt)
	}
}

// TestSystemPromptFileMissingFailsLoudly proves a configured-but-missing prompt
// file is a hard error at load time, not a silent fallback to the default
// persona. A silently-swapped persona would change agent behavior invisibly.
func TestSystemPromptFileMissingFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, minimalConfig+`
persona:
  system_prompt_file: does-not-exist.md
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a missing system_prompt_file")
	}
	if !strings.Contains(err.Error(), "system_prompt_file") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestSystemPromptFileEmptyFailsLoudly proves a present-but-empty prompt file
// is rejected rather than silently falling back.
func TestSystemPromptFileEmptyFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty-prompt.md"), []byte("   \n\n"), 0600); err != nil {
		t.Fatalf("write empty prompt: %v", err)
	}
	path := writeConfig(t, dir, minimalConfig+`
persona:
  system_prompt_file: empty-prompt.md
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an empty system_prompt_file")
	}
}

// TestSystemPromptFileAbsolutePath proves an absolute system_prompt_file is
// used as-is (no config-dir joining).
func TestSystemPromptFileAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()
	promptPath := filepath.Join(otherDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("Absolute path persona."), 0600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	path := writeConfig(t, dir, minimalConfig+`
persona:
  system_prompt_file: `+promptPath+`
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(cfg.Persona.SystemPrompt, "Absolute path persona.") {
		t.Errorf("absolute prompt path not honored: %q", cfg.Persona.SystemPrompt)
	}
}

// TestSystemPromptInlineStillWorks proves the legacy inline form keeps working
// unchanged (backward compatibility for existing configs).
func TestSystemPromptInlineStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, minimalConfig+`
persona:
  system_prompt: "Legacy inline persona."
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(cfg.Persona.SystemPrompt, "Legacy inline persona.") {
		t.Errorf("inline persona not honored: %q", cfg.Persona.SystemPrompt)
	}
}

// TestSystemPromptDefaultWhenUnset proves the hardcoded fallback still applies
// when neither field is set.
func TestSystemPromptDefaultWhenUnset(t *testing.T) {
	path := writeConfig(t, t.TempDir(), minimalConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(cfg.Persona.SystemPrompt, "precise agentic assistant") {
		t.Errorf("default persona missing: %.60q", cfg.Persona.SystemPrompt)
	}
}

// TestSystemPromptRepoFileLoads proves the repository's own system-prompt.md
// parses and carries the expected sections — guards against an accidental
// truncation of the shipped prompt.
func TestSystemPromptRepoFileLoads(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "system-prompt.md"))
	if err != nil {
		t.Fatalf("read repo prompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "system-prompt.md"), src, 0600); err != nil {
		t.Fatalf("copy prompt: %v", err)
	}
	path := writeConfig(t, dir, minimalConfig+`
persona:
  system_prompt_file: system-prompt.md
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, want := range []string{"# Tool use", "# Working style", "# Scope discipline", "# Delivery", "Sandbar harness", "[INFERENCE]"} {
		if !strings.Contains(cfg.Persona.SystemPrompt, want) {
			t.Errorf("repo prompt missing %q", want)
		}
	}
}
