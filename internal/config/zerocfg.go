package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultFromEnv synthesizes an in-memory zero-config configuration when the
// environment carries an OpenAI key: provider "openai" pointing at
// $OPENAI_BASE_URL (default https://api.openai.com/v1) with $OPENAI_API_KEY,
// and a single alias from $OPENAI_MODEL (default gpt-4o-mini). ok is false
// when no key is set — callers should surface their normal "no config" error
// in that case, not this default.
func DefaultFromEnv() (*Config, bool) {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return nil, false
	}
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = "gpt-4o-mini"
	}
	yaml := fmt.Sprintf(`providers:
  - name: openai
    base_url: %q
    api_key: %q
    models:
      %s:
        supports_tools: true
`, baseURL, apiKey, model)
	// The env values are already interpolated into the YAML; finalizeConfig
	// re-runs interpolation harmlessly and applies the same defaults and
	// validation a hand-written config file gets.
	cfg, err := finalizeConfig([]byte(yaml), "")
	if err != nil {
		// Only reachable if the OpenAI defaults stop validating; a broken
		// synthesized config must not silently boot.
		return nil, false
	}
	return cfg, true
}

// WriteDefaultConfigTemplate copies the commented example config to the
// user's config location on first zero-config boot. It never overwrites an
// existing file and fails silently — the env-derived config already works
// without it.
func WriteDefaultConfigTemplate() {
	dest := defaultConfigPath()
	if dest == "" {
		return
	}
	if _, err := os.Stat(dest); err == nil {
		return
	}
	example := findExampleConfig()
	if example == "" {
		return
	}
	data, err := os.ReadFile(example)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dest, data, 0o644)
}

// defaultConfigPath mirrors configSearchPaths' first fixed location.
func defaultConfigPath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "sandbar", "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "sandbar", "config.yaml")
	}
	return ""
}

// findExampleConfig locates config.yaml.example next to the running binary,
// then in the process working directory.
func findExampleConfig() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "config.yaml.example")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if _, err := os.Stat("config.yaml.example"); err == nil {
		return "config.yaml.example"
	}
	return ""
}
