package llm

import (
	"strings"
	"testing"

	"sandbar/internal/config"
)

func ptr[T any](v T) *T { return &v }

func TestResolveModelExplicitOverride(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:    "p1",
				BaseURL: "http://example.com/v1",
				APIKey:  "key",
				Models: map[string]config.ModelConfig{
					"m1": {ContextLength: ptr(100), SupportsTools: ptr(false)},
				},
				ModelDefaults: config.ModelConfig{
					ContextLength: ptr(200),
					SupportsTools: ptr(true),
				},
			},
		},
	}

	reg := NewRegistry(cfg)
	m, err := reg.ResolveModel("m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.ContextLength != 100 {
		t.Errorf("context_length: got %d, want 100", m.ContextLength)
	}
	if m.SupportsTools != false {
		t.Errorf("supports_tools: got %v, want false", m.SupportsTools)
	}
}

func TestResolveModelDefaultInheritance(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:    "p1",
				BaseURL: "http://example.com/v1",
				APIKey:  "key",
				Models: map[string]config.ModelConfig{
					"m1": {},
				},
				ModelDefaults: config.ModelConfig{
					ContextLength: ptr(200),
					SupportsTools: ptr(true),
				},
			},
		},
	}

	reg := NewRegistry(cfg)
	m, err := reg.ResolveModel("m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.ContextLength != 200 {
		t.Errorf("context_length: got %d, want 200", m.ContextLength)
	}
	if m.SupportsTools != true {
		t.Errorf("supports_tools: got %v, want true", m.SupportsTools)
	}
}

func TestResolveModelGlobalDefault(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:    "p1",
				BaseURL: "http://example.com/v1",
				APIKey:  "key",
				Models: map[string]config.ModelConfig{
					"m1": {},
				},
			},
		},
	}

	reg := NewRegistry(cfg)
	m, err := reg.ResolveModel("m1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.ContextLength != 0 {
		t.Errorf("context_length: got %d, want 0", m.ContextLength)
	}
	if m.SupportsTools != false {
		t.Errorf("supports_tools: got %v, want false", m.SupportsTools)
	}
}

func TestResolveModelUnknown(t *testing.T) {
	cfg := &config.Config{Providers: []config.ProviderConfig{}}
	reg := NewRegistry(cfg)
	_, err := reg.ResolveModel("unknown")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestResolveProviderQualifiedTargetsSpecificHost(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:    "host-a",
				BaseURL: "http://a:8080/v1",
				APIKey:  "key-a",
				Models: map[string]config.ModelConfig{
					"local/shared-model": {ContextLength: ptr(100)},
				},
			},
			{
				Name:    "host-b",
				BaseURL: "http://b:8080/v1",
				APIKey:  "key-b",
				Models: map[string]config.ModelConfig{
					"local/shared-model": {ContextLength: ptr(200)},
				},
			},
		},
	}
	reg := NewRegistry(cfg)

	// Provider-qualified alias targets host-b specifically.
	m, err := reg.ResolveModel("host-b/local/shared-model")
	if err != nil {
		t.Fatalf("resolve provider-qualified: %v", err)
	}
	if m.ProviderName != "host-b" {
		t.Errorf("expected host-b, got %s", m.ProviderName)
	}
	if m.BaseURL != "http://b:8080/v1" {
		t.Errorf("expected host-b base URL, got %s", m.BaseURL)
	}
	if m.ContextLength != 200 {
		t.Errorf("expected ctx 200 from host-b, got %d", m.ContextLength)
	}

	// Provider-qualified alias targeting host-a.
	m, err = reg.ResolveModel("host-a/local/shared-model")
	if err != nil {
		t.Fatalf("resolve provider-qualified: %v", err)
	}
	if m.ProviderName != "host-a" {
		t.Errorf("expected host-a, got %s", m.ProviderName)
	}
}

func TestResolveBareAliasFallback(t *testing.T) {
	// Bare aliases (no provider prefix matching a provider name) still
	// resolve via first-provider-wins. This is for the compression model
	// config field and old thread records in the DB.
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:    "openrouter-direct",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "key",
				Models: map[string]config.ModelConfig{
					"deepseek/deepseek-v4-flash": {ContextLength: ptr(131072)},
				},
			},
		},
	}
	reg := NewRegistry(cfg)
	m, err := reg.ResolveModel("deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("resolve bare alias: %v", err)
	}
	if m.ProviderName != "openrouter-direct" {
		t.Errorf("expected openrouter-direct, got %s", m.ProviderName)
	}
	if m.ContextLength != 131072 {
		t.Errorf("expected ctx 131072, got %d", m.ContextLength)
	}
}

func TestResolveModelModelsJSONFields(t *testing.T) {
	compat := &config.CompatFlags{MaxTokensField: "max_completion_tokens"}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:           "json-p",
				BaseURL:        "http://json.example/v1",
				APIKey:         "k",
				API:            "anthropic-messages",
				Compat:         compat,
				ReasoningStyle: "none",
				Models: map[string]config.ModelConfig{
					"capped": {MaxTokens: ptr(512)},
					"plain":  {},
				},
				ModelDefaults: config.ModelConfig{MaxTokens: ptr(64)},
			},
		},
	}
	reg := NewRegistry(cfg)

	m, err := reg.ResolveModel("json-p/capped")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.API != "anthropic-messages" {
		t.Errorf("api: got %q, want anthropic-messages", m.API)
	}
	if m.Compat != compat {
		t.Errorf("compat pointer not propagated: %v", m.Compat)
	}
	if m.MaxTokens != 512 {
		t.Errorf("max_tokens: got %d, want 512 (model override)", m.MaxTokens)
	}
	if m.ReasoningStyle != "none" {
		t.Errorf("reasoning_style: got %q", m.ReasoningStyle)
	}

	// MaxTokens inherits from model_defaults when the model sets none.
	m, err = reg.ResolveModel("json-p/plain")
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	if m.MaxTokens != 64 {
		t.Errorf("max_tokens: got %d, want 64 (model_defaults)", m.MaxTokens)
	}
}

func TestListModels(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name: "p1",
				Models: map[string]config.ModelConfig{
					"m1": {},
					"m2": {},
				},
			},
		},
	}
	reg := NewRegistry(cfg)
	list := reg.ListModels()
	if len(list) != 2 {
		t.Fatalf("list length: got %d, want 2", len(list))
	}
	// Every entry should be provider-qualified.
	for _, m := range list {
		if !strings.HasPrefix(m, "p1/") {
			t.Errorf("expected provider-qualified, got %q", m)
		}
	}
}

func TestListModelsWithDuplicates(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name: "host-a",
				Models: map[string]config.ModelConfig{
					"local/dup":    {},
					"local/uniq-a": {},
				},
			},
			{
				Name: "host-b",
				Models: map[string]config.ModelConfig{
					"local/dup":    {},
					"local/uniq-b": {},
				},
			},
		},
	}
	reg := NewRegistry(cfg)
	list := reg.ListModels()

	// All 4 entries should be provider-qualified — no bare aliases.
	if len(list) != 4 {
		t.Fatalf("list length: got %d, want 4", len(list))
	}
	hasUniqA := false
	hasUniqB := false
	hasDupA := false
	hasDupB := false
	for _, m := range list {
		switch m {
		case "host-a/local/uniq-a":
			hasUniqA = true
		case "host-b/local/uniq-b":
			hasUniqB = true
		case "host-a/local/dup":
			hasDupA = true
		case "host-b/local/dup":
			hasDupB = true
		}
	}
	if !hasUniqA {
		t.Error("missing host-a/local/uniq-a")
	}
	if !hasUniqB {
		t.Error("missing host-b/local/uniq-b")
	}
	if !hasDupA {
		t.Error("missing host-a/local/dup")
	}
	if !hasDupB {
		t.Error("missing host-b/local/dup")
	}
}
