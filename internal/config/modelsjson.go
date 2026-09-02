package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CompatFlags tames half-compatible OpenAI servers, ported from the legacy
// sandbar models.json registry. Nil pointer fields mean "use the default" so
// the zero value is always valid; a provider needing no tweaks omits compat
// entirely. JSON tags match the legacy file format; YAML tags follow this
// fork's snake_case convention so config.yaml providers can set them too.
type CompatFlags struct {
	SupportsDeveloperRole            *bool  `json:"supportsDeveloperRole,omitempty" yaml:"supports_developer_role,omitempty"`
	SupportsReasoningEffort          *bool  `json:"supportsReasoningEffort,omitempty" yaml:"supports_reasoning_effort,omitempty"`
	MaxTokensField                   string `json:"maxTokensField,omitempty" yaml:"max_tokens_field,omitempty"` // max_tokens (default) | max_completion_tokens
	RequiresToolResultName           *bool  `json:"requiresToolResultName,omitempty" yaml:"requires_tool_result_name,omitempty"`
	RequiresAssistantAfterToolResult *bool  `json:"requiresAssistantAfterToolResult,omitempty" yaml:"requires_assistant_after_tool_result,omitempty"`
	ThinkingFormat                   string `json:"thinkingFormat,omitempty" yaml:"thinking_format,omitempty"` // "" | reasoning_effort | deepseek | openrouter
	// SendSessionID sends the conversation's SessionID as an X-Session-Id
	// header for providers with sticky session routing (KV-cache reuse).
	SendSessionID bool `json:"sendSessionId,omitempty" yaml:"send_session_id,omitempty"`
}

// modelsJSONFile is the root of models.json.
type modelsJSONFile struct {
	Providers map[string]modelsJSONProvider `json:"providers"`
}

// modelsJSONProvider is one provider stanza in the legacy models.json
// schema. Unknown fields are ignored so files stay forward-compatible.
type modelsJSONProvider struct {
	BaseURL string            `json:"baseUrl"`
	API     string            `json:"api,omitempty"` // "" = openai-compatible; anthropic-messages recognized
	APIKey  string            `json:"apiKey,omitempty"`
	Compat  *CompatFlags      `json:"compat,omitempty"`
	Models  []modelsJSONModel `json:"models"`
}

// modelsJSONModel is one model entry under a provider.
type modelsJSONModel struct {
	ID            string `json:"id"` // user-facing id (may be an alias)
	Name          string `json:"name,omitempty"`
	WireID        string `json:"modelId,omitempty"`       // id sent on the wire when the alias differs
	ContextWindow int    `json:"contextWindow,omitempty"` // tokens; 0 = unknown
	MaxTokens     int    `json:"maxTokens,omitempty"`
}

// resolveAPIKey expands an apiKey value, ported from legacy: "$ENV"/"${ENV}"
// (with $$ and $! escapes) -> env value; "!command" -> command stdout
// (trimmed); anything else is a literal (local servers use dummies like
// "ollama"). Unset env vars expand to empty — providers with missing keys
// fail at request time with a clear error, not at load.
func resolveAPIKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return "", nil
	case strings.HasPrefix(raw, "!"):
		cmd := strings.TrimSpace(strings.TrimPrefix(raw, "!"))
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			return "", fmt.Errorf("apiKey command %q: %w", cmd, err)
		}
		return strings.TrimSpace(string(out)), nil
	default:
		return expandEnv(raw), nil
	}
}

// expandEnv resolves $VAR/${VAR} with $$ and $! escapes; unresolved vars
// expand to empty.
func expandEnv(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		next := s[i+1]
		switch {
		case next == '$' || next == '!':
			b.WriteByte('$')
			i++
		case next == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteByte(c)
				continue
			}
			b.WriteString(os.Getenv(s[i+2 : i+end]))
			i += end
		default:
			j := i + 1
			for j < len(s) && isEnvByte(s[j]) {
				j++
			}
			b.WriteString(os.Getenv(s[i+1 : j]))
			i = j - 1
		}
	}
	return b.String()
}

func isEnvByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// modelsJSONPath picks the models.json to consult: the config's explicit
// models_json key (relative paths resolve against the config file's
// directory, like system_prompt_file), otherwise models.json sitting next to
// the loaded config file. The zero-config env boot passes configDir "" and
// never consults a registry — its provider keeps working unchanged, and no
// stray models.json from a fixed system location can leak into a boot that
// found no config at all. "" means no candidate.
func modelsJSONPath(cfg *Config, configDir string) string {
	if cfg.ModelsJSON != "" {
		p := cfg.ModelsJSON
		if !filepath.IsAbs(p) && configDir != "" {
			p = filepath.Join(configDir, p)
		}
		return p
	}
	if configDir != "" {
		return filepath.Join(configDir, "models.json")
	}
	return ""
}

// ModelsJSONCandidate resolves the models.json overlay path a Load of
// configPath would consult, without loading anything. "" means no candidate
// (zero-config boot). Doctor uses this to report presence and parse status.
func ModelsJSONCandidate(cfg *Config, configPath string) string {
	if cfg == nil {
		return ""
	}
	configDir := ""
	if configPath != "" {
		configDir = filepath.Dir(configPath)
	}
	return modelsJSONPath(cfg, configDir)
}

// ParseModelsJSON validates that data is a parseable models.json document.
// It does not resolve API keys or merge anything — callers that only need a
// health signal (doctor) can check a file without a full config load.
func ParseModelsJSON(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var f modelsJSONFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	return nil
}

// mergeModelsJSON overlays providers from a legacy-style models.json onto
// the YAML config: JSON providers are appended after the YAML ones and
// replace same-name YAML providers outright (a clash is not an error). A
// missing auto-discovered file, an empty file, or one with no providers is a
// no-op; malformed JSON is a hard error naming the file. API keys are
// resolved here rather than at request time (unlike legacy) because the
// fork's ResolvedModel carries an already-resolved key.
func mergeModelsJSON(cfg *Config, configDir string) error {
	path := modelsJSONPath(cfg, configDir)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if cfg.ModelsJSON != "" {
				// An explicitly configured path that is missing is a typo,
				// not an optional overlay — fail loudly like a missing
				// system_prompt_file.
				return fmt.Errorf("models.json %s: file not found", path)
			}
			return nil
		}
		return fmt.Errorf("models.json %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var f modelsJSONFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("models.json %s: %w", path, err)
	}

	names := make([]string, 0, len(f.Providers))
	replaced := make(map[string]bool, len(f.Providers))
	for name := range f.Providers {
		names = append(names, name)
		replaced[name] = true
	}
	// Sorted so bare-alias resolution (first provider wins) is stable
	// despite map iteration order.
	sort.Strings(names)

	var merged []ProviderConfig
	for _, p := range cfg.Providers {
		if !replaced[p.Name] {
			merged = append(merged, p)
		}
	}
	for _, name := range names {
		p, err := modelsJSONProviderToConfig(name, f.Providers[name], path)
		if err != nil {
			return err
		}
		merged = append(merged, p)
	}
	cfg.Providers = merged
	return nil
}

// modelsJSONProviderToConfig converts one legacy stanza into a
// ProviderConfig, resolving the API key.
func modelsJSONProviderToConfig(name string, p modelsJSONProvider, path string) (ProviderConfig, error) {
	key, err := resolveAPIKey(p.APIKey)
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("models.json %s: provider %q: %w", path, name, err)
	}
	models := make(map[string]ModelConfig, len(p.Models))
	for _, m := range p.Models {
		mc := ModelConfig{
			Name: m.Name,
			// Legacy always advertised tools; the resolver now defaults to
			// tool support, so imports write no flag at all.
		}
		if m.ContextWindow > 0 {
			mc.ContextLength = intPtr(m.ContextWindow)
		}
		if m.WireID != "" {
			mc.ModelID = strPtr(m.WireID)
		}
		if m.MaxTokens > 0 {
			mc.MaxTokens = intPtr(m.MaxTokens)
		}
		models[m.ID] = mc
	}
	return ProviderConfig{
		Name:    name,
		BaseURL: p.BaseURL,
		API:     p.API,
		APIKey:  key,
		Compat:  p.Compat,
		// Map the legacy thinkingFormat quirk onto the fork's
		// reasoning_style so imported providers keep reasoning-effort
		// translation. reasoning_effort/openrouter speak reasoning_effort
		// ("openai" dialect); deepseek is approximated with the same dialect
		// (the fork has no separate reasoning_content parser); none disables.
		ReasoningStyle: thinkingFormatToReasoningStyle(p.Compat),
		Models:         models,
	}, nil
}

func thinkingFormatToReasoningStyle(c *CompatFlags) string {
	if c == nil || c.ThinkingFormat == "" {
		return ""
	}
	switch c.ThinkingFormat {
	case "none":
		return "none"
	default: // reasoning_effort, openrouter, deepseek
		return "openai"
	}
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }
