package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultFromEnvSynthesizesOpenAIProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")

	cfg, ok := DefaultFromEnv()
	if !ok {
		t.Fatal("DefaultFromEnv ok = false with OPENAI_API_KEY set")
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.Name != "openai" || p.BaseURL != "https://api.openai.com/v1" || p.APIKey != "sk-test-key" {
		t.Fatalf("provider = %+v", p)
	}
	if _, defined := p.Models["gpt-4o-mini"]; !defined {
		t.Fatalf("default model alias missing: %+v", p.Models)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("synthesized config invalid: %v", err)
	}
}

func TestDefaultFromEnvHonorsOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", " sk-test ")
	t.Setenv("OPENAI_BASE_URL", "https://proxy.example/v1")
	t.Setenv("OPENAI_MODEL", "gpt-4.1")

	cfg, ok := DefaultFromEnv()
	if !ok {
		t.Fatal("DefaultFromEnv ok = false")
	}
	p := cfg.Providers[0]
	if p.BaseURL != "https://proxy.example/v1" {
		t.Fatalf("base_url = %q", p.BaseURL)
	}
	if p.APIKey != "sk-test" {
		t.Fatalf("api_key = %q, want trimmed", p.APIKey)
	}
	if _, defined := p.Models["gpt-4.1"]; !defined {
		t.Fatalf("model override missing: %+v", p.Models)
	}
}

func TestDefaultFromEnvUnsetReturnsFalse(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if cfg, ok := DefaultFromEnv(); ok || cfg != nil {
		t.Fatalf("DefaultFromEnv = (%v, %v), want (nil, false)", cfg, ok)
	}
	t.Setenv("OPENAI_API_KEY", "   ")
	if cfg, ok := DefaultFromEnv(); ok || cfg != nil {
		t.Fatalf("whitespace-only key: DefaultFromEnv = (%v, %v), want (nil, false)", cfg, ok)
	}
}

func TestWriteDefaultConfigTemplateCreatesAndNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	exampleDir := t.TempDir()
	example := filepath.Join(exampleDir, "config.yaml.example")
	if err := os.WriteFile(example, []byte("# template\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, ".config", "sandbar", "config.yaml")

	// The example is found relative to the working directory.
	t.Chdir(exampleDir)

	WriteDefaultConfigTemplate()
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("template not written: %v", err)
	}
	if string(data) != "# template\n" {
		t.Fatalf("template content = %q", data)
	}

	// Existing file is never overwritten.
	if err := os.WriteFile(dest, []byte("# custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	WriteDefaultConfigTemplate()
	data, _ = os.ReadFile(dest)
	if string(data) != "# custom\n" {
		t.Fatalf("existing config was overwritten: %q", data)
	}
}
