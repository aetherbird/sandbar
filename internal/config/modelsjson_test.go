package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAPIKey(t *testing.T) {
	t.Setenv("SB_JTEST_KEY", "sekret")
	cases := []struct {
		name, raw, want string
	}{
		{"literal", "ollama", "ollama"},
		{"dollar env", "$SB_JTEST_KEY", "sekret"},
		{"braced env", "${SB_JTEST_KEY}", "sekret"},
		{"unset env empties", "$SB_JTEST_MISSING_0192", ""},
		{"embedded", "prefix-${SB_JTEST_KEY}-suffix", "prefix-sekret-suffix"},
		{"dollar escape", "$$SB_JTEST_KEY", "$SB_JTEST_KEY"},
		{"bang escape", "$!", "$"},
		{"empty", "", ""},
		{"whitespace trimmed", "  padded  ", "padded"},
	}
	for _, tc := range cases {
		got, err := resolveAPIKey(tc.raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: resolveAPIKey(%q) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestResolveAPIKeyCommand(t *testing.T) {
	got, err := resolveAPIKey("!echo hello")
	if err != nil {
		t.Fatalf("command key: %v", err)
	}
	if got != "hello" {
		t.Errorf("command key = %q, want hello", got)
	}
	got, err = resolveAPIKey("! printf '  padded  '")
	if err != nil {
		t.Fatalf("command key trim: %v", err)
	}
	if got != "padded" {
		t.Errorf("command key trim = %q, want padded", got)
	}
	if _, err := resolveAPIKey("!exit 3"); err == nil {
		t.Error("failing command: expected error")
	}
}

const modelsJSONMergeBody = `{
  "providers": {
    "ollama": {
      "baseUrl": "http://json-host:11434/v1",
      "apiKey": "$SB_JTEST_MERGE_KEY",
      "compat": {
        "requiresToolResultName": true,
        "maxTokensField": "max_completion_tokens"
      },
      "models": [
        {"id": "qwen3", "name": "Qwen3 14B", "contextWindow": 40960, "maxTokens": 8192},
        {"id": "vendor/aliased", "modelId": "wire-id"}
      ]
    },
    "z-extra": {
      "baseUrl": "http://extra.example/v1",
      "api": "anthropic-messages",
      "apiKey": "lit",
      "models": [{"id": "e1", "contextWindow": 8000}]
    }
  }
}`

const modelsJSONMergeYAML = `providers:
  - name: yaml-only
    base_url: http://yaml.example/v1
    api_key: yaml-key
    models:
      m1:
        context_length: 100
        supports_tools: true
  - name: ollama
    base_url: http://yaml-ollama:11434/v1
    api_key: yaml-ollama-key
    models:
      m2:
        supports_tools: true
`

func writeConfigPair(t *testing.T, yamlBody, jsonBody string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if jsonBody != "" {
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(jsonBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg, dir
}

func TestModelsJSONMergeAndClash(t *testing.T) {
	t.Setenv("SB_JTEST_MERGE_KEY", "json-secret")
	cfg, _ := writeConfigPair(t, modelsJSONMergeYAML, modelsJSONMergeBody)

	// YAML providers first (clashing one dropped), then JSON providers
	// appended in sorted order.
	var names []string
	for _, p := range cfg.Providers {
		names = append(names, p.Name)
	}
	want := []string{"yaml-only", "ollama", "z-extra"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("providers = %v, want %v", names, want)
	}

	var yamlOnly, ollama, extra ProviderConfig
	for _, p := range cfg.Providers {
		switch p.Name {
		case "yaml-only":
			yamlOnly = p
		case "ollama":
			ollama = p
		case "z-extra":
			extra = p
		}
	}

	// Clash: models.json replaced the YAML provider wholesale.
	if ollama.BaseURL != "http://json-host:11434/v1" || ollama.APIKey != "json-secret" {
		t.Errorf("ollama replaced = %+v", ollama)
	}
	if _, ok := ollama.Models["m2"]; ok {
		t.Error("replaced provider kept a YAML-only model")
	}
	// Untouched YAML provider survives.
	if yamlOnly.APIKey != "yaml-key" || len(yamlOnly.Models) != 1 {
		t.Errorf("yaml-only provider altered: %+v", yamlOnly)
	}

	qwen := ollama.Models["qwen3"]
	if qwen.ContextLength == nil || *qwen.ContextLength != 40960 {
		t.Errorf("qwen3 context_length = %v, want 40960", qwen.ContextLength)
	}
	if qwen.MaxTokens == nil || *qwen.MaxTokens != 8192 {
		t.Errorf("qwen3 max_tokens = %v, want 8192", qwen.MaxTokens)
	}
	if qwen.Name != "Qwen3 14B" {
		t.Errorf("qwen3 name = %q, want display name", qwen.Name)
	}
	// Legacy schema has no supports_tools; the resolver defaults to tools on,
	// so imports must not write a redundant flag.
	if qwen.SupportsTools != nil {
		t.Errorf("qwen3 supports_tools = %v, want unset (resolver default)", qwen.SupportsTools)
	}
	if alias := ollama.Models["vendor/aliased"]; alias.ModelID == nil || *alias.ModelID != "wire-id" {
		t.Errorf("aliased model_id = %v, want wire-id", alias.ModelID)
	}

	if ollama.Compat == nil {
		t.Fatal("ollama compat = nil, want flags")
	}
	if ollama.Compat.RequiresToolResultName == nil || !*ollama.Compat.RequiresToolResultName {
		t.Errorf("compat.requiresToolResultName = %v, want true", ollama.Compat.RequiresToolResultName)
	}
	if ollama.Compat.MaxTokensField != "max_completion_tokens" {
		t.Errorf("compat.maxTokensField = %q", ollama.Compat.MaxTokensField)
	}
	if extra.API != "anthropic-messages" {
		t.Errorf("z-extra api = %q, want anthropic-messages", extra.API)
	}
	if extra.Compat != nil {
		t.Errorf("z-extra compat = %+v, want nil default", extra.Compat)
	}
}

func TestModelsJSONAbsentOrEmptyNoOp(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{{"absent", ""}, {"empty bytes", "   "}, {"no providers", `{"providers": {}}`}} {
		cfg, _ := writeConfigPair(t, modelsJSONMergeYAML, tc.body)
		if len(cfg.Providers) != 2 {
			t.Errorf("%s: providers = %d, want 2 (YAML untouched)", tc.name, len(cfg.Providers))
		}
		if cfg.Providers[1].BaseURL != "http://yaml-ollama:11434/v1" {
			t.Errorf("%s: YAML ollama provider not intact", tc.name)
		}
	}
}

func TestModelsJSONInvalidHardError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(modelsJSONMergeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "models.json")
	if err := os.WriteFile(bad, []byte(`{"providers":`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatal("expected hard error for invalid JSON")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q does not name the file %s", err, bad)
	}
}

func TestModelsJSONExplicitPath(t *testing.T) {
	cfgDir := t.TempDir()
	jsonDir := t.TempDir()
	alt := filepath.Join(jsonDir, "alt-registry.json")
	if err := os.WriteFile(alt, []byte(`{"providers":{"moved":{"baseUrl":"http://moved/v1","apiKey":"k","models":[{"id":"m"}]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Absolute explicit path wins over any models.json next to the config.
	if err := os.WriteFile(filepath.Join(cfgDir, "models.json"), []byte(`{"providers":{"ignored":{"baseUrl":"http://ignored/v1","apiKey":"k","models":[{"id":"m"}]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("models_json: "+alt+"\n"+modelsJSONMergeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(cfgDir, "config.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Providers) != 3 || cfg.Providers[2].Name != "moved" {
		t.Fatalf("absolute models_json: providers = %v, want moved appended", cfg.Providers)
	}

	// Relative explicit path resolves against the config directory.
	if err := os.WriteFile(filepath.Join(cfgDir, "rel-registry.json"), []byte(`{"providers":{"relative":{"baseUrl":"http://rel/v1","apiKey":"k","models":[{"id":"m"}]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("models_json: rel-registry.json\n"+modelsJSONMergeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(filepath.Join(cfgDir, "config.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Providers) != 3 || cfg.Providers[2].Name != "relative" {
		t.Fatalf("relative models_json: providers = %v, want relative appended", cfg.Providers)
	}

	// An explicitly configured path that is missing is a typo, not a no-op.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("models_json: nope-does-not-exist.json\n"+modelsJSONMergeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(cfgDir, "config.yaml")); err == nil {
		t.Fatal("expected error for missing explicit models_json")
	}
}

// The zero-config env boot is unaffected by any models.json: with no config
// file there is no config directory to consult, so the synthesized provider
// keeps working unchanged (a stray registry under $XDG_CONFIG_HOME must not
// leak into a boot that found no config).
func TestZeroConfigIgnoresModelsJSON(t *testing.T) {
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "sandbar"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "sandbar", "models.json"), []byte(`{"providers":{"stray":{"baseUrl":"http://stray/v1","apiKey":"k","models":[{"id":"m"}]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("OPENAI_API_KEY", "env-key")

	cfg, ok := DefaultFromEnv()
	if !ok {
		t.Fatal("env boot unavailable")
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "openai" || cfg.Providers[0].APIKey != "env-key" {
		t.Fatalf("env-boot providers = %+v, want only the synthesized openai provider", cfg.Providers)
	}
}
