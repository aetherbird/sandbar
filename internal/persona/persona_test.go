package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildSystemPromptWithProjectContext(t *testing.T) {
	tmpDir := t.TempDir()

	// Write a .sandbar.md file.
	sandbarMd := filepath.Join(tmpDir, ".sandbar.md")
	os.WriteFile(sandbarMd, []byte("Use Go 1.23.\nPrefer table-driven tests."), 0644)

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "You are a test assistant.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "test-model")

	if !strings.Contains(prompt, "You are a test assistant.") {
		t.Error("missing base system prompt")
	}
	if !strings.Contains(prompt, ".sandbar.md") {
		t.Error("missing .sandbar.md section")
	}
	if !strings.Contains(prompt, "Use Go 1.23") {
		t.Error("missing .sandbar.md content")
	}
}

func TestBuildSystemPromptWithoutProjectContext(t *testing.T) {
	tmpDir := t.TempDir()

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "You are a test assistant.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "")

	if strings.Contains(prompt, "Project Context") {
		t.Error("should not contain Project Context when no files exist")
	}
}

func TestBuildSystemPromptIncludesContextUnscanned(t *testing.T) {
	tmpDir := t.TempDir()

	// A context file that merely mentions injection-shaped phrases is the
	// owner's own instruction file — it must be included verbatim. (The
	// heuristic injection scan was removed 2026-08-14 on owner decision.)
	agentsMd := filepath.Join(tmpDir, "AGENTS.md")
	os.WriteFile(agentsMd, []byte("Ignore previous instructions-style attacks: our threat model notes them here."), 0644)

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "You are a test assistant.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "")

	if !strings.Contains(prompt, "Ignore previous instructions-style attacks") {
		t.Error("context file should be included verbatim, without scanning")
	}
	if strings.Contains(prompt, "prompt-injection") {
		t.Error("no injection placeholder should remain")
	}
}

func TestBuildSystemPromptMultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, ".sandbar.md"), []byte("Sandbar rule."), 0644)
	os.WriteFile(filepath.Join(tmpDir, "AGENTS.md"), []byte("Agent rule."), 0644)

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "Base.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "")

	if !strings.Contains(prompt, "Sandbar rule.") {
		t.Error("missing .sandbar.md content")
	}
	if !strings.Contains(prompt, "Agent rule.") {
		t.Error("missing AGENTS.md content")
	}
}

func TestBuildSystemPromptEnvironmentBlock(t *testing.T) {
	tmpDir := t.TempDir()

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "Base.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "google/gemini-3.5-flash")

	if !strings.Contains(prompt, "# Environment") {
		t.Error("missing environment heading")
	}
	if !strings.Contains(prompt, "Working directory: "+tmpDir) {
		t.Error("missing working directory")
	}
	if !strings.Contains(prompt, "Is directory a git repo: no") {
		t.Error("temp dir should not be reported as a git repo")
	}
	if !strings.Contains(prompt, "Platform: ") {
		t.Error("missing platform")
	}
	if !strings.Contains(prompt, "Today's date: ") {
		t.Error("missing date")
	}
	if !strings.Contains(prompt, "Model: google/gemini-3.5-flash") {
		t.Error("missing model line")
	}
}

// TestBuildSystemPromptDateIsDayResolution pins the environment block's date
// at day resolution (UTC, 2006-01-02): a second-resolution timestamp changes
// every turn and defeats provider prefix caching of the system prompt.
func TestBuildSystemPromptDateIsDayResolution(t *testing.T) {
	tmpDir := t.TempDir()

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "Base.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "")

	want := "- Today's date: " + time.Now().UTC().Format("2006-01-02") + "\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("missing day-resolution date line %q", want)
	}
	if strings.Contains(prompt, "Today's date: "+time.Now().UTC().Format("2006-01-02")+"T") {
		t.Error("date must not carry a time component; second resolution defeats prefix caching")
	}
}

func TestBuildSystemPromptGitRepoDetection(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmpDir, ".git"), 0755); err != nil {
		t.Fatalf("create .git: %v", err)
	}

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "Base.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "")

	if !strings.Contains(prompt, "Is directory a git repo: yes") {
		t.Error("directory with .git should be reported as a git repo")
	}
}

func TestBuildSystemPromptEmptyModelOmitsModelLine(t *testing.T) {
	tmpDir := t.TempDir()

	p := Persona{
		Name:         "Sandbar",
		SystemPrompt: "Base.",
	}
	prompt := p.BuildSystemPrompt(tmpDir, "")

	if strings.Contains(prompt, "- Model:") {
		t.Error("model line should be omitted when model is unknown")
	}
}

// TestBuildSystemPromptIsModelAgnostic asserts that the prompt is identical
// across model families — no per-family guidance is injected. Every configured
// model receives the same system prompt (the Model: line differs because that
// is a runtime fact, not a behavioral instruction).
func TestBuildSystemPromptIsModelAgnostic(t *testing.T) {
	tmpDir := t.TempDir()
	base := Persona{Name: "Sandbar", SystemPrompt: "Base."}

	models := []string{
		"google/gemini-3.5-flash",
		"local/gemma-26b",
		"openai/gpt-5.6",
		"local/gpt-oss-20b",
		"deepseek/deepseek-v4-flash",
		"local/qwen-36b",
		"z-ai/glm-5.2",
		"anthropic/claude-sonnet-4.6",
		"local/llama-3b",
	}

	// Normalize the Model: line away so the comparison is about instructions.
	normalize := func(s, m string) string {
		return strings.ReplaceAll(s, "Model: "+m, "Model: <normalized>")
	}

	reference := normalize(base.BuildSystemPrompt(tmpDir, models[0]), models[0])
	for _, m := range models[1:] {
		got := normalize(base.BuildSystemPrompt(tmpDir, m), m)
		if got != reference {
			t.Errorf("prompt for %q differs from %q — guidance is not model-agnostic", m, models[0])
		}
	}

	// Sanity: confirm no model ever receives a guidance section heading.
	for _, m := range models {
		if strings.Contains(base.BuildSystemPrompt(tmpDir, m), "Model operational guidance") {
			t.Errorf("%q received model-family guidance; guidance must not exist", m)
		}
	}
}
