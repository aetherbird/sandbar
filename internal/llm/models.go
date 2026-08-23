package llm

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aetherbird/sandbar/internal/config"
)

// ResolvedModel is a fully resolved model configuration.
type ResolvedModel struct {
	ProviderName   string
	BaseURL        string
	APIKey         string
	API            string // wire protocol: ""/openai-completions = OpenAI-compatible; anthropic-messages = native Messages client (see NewWireClient)
	ContextLength  int
	SupportsTools  bool
	ModelID        string              // if empty, the alias itself is used for API calls
	MaxTokens      int                 // per-model output-token cap; 0 = provider default
	ReasoningStyle string              // provider dialect for --effort; see config.ProviderConfig
	Compat         *config.CompatFlags // nil = defaults; see config.CompatFlags
}

// Registry resolves model aliases into concrete configurations.
type Registry struct {
	providers []config.ProviderConfig
}

// NewRegistry creates a model registry from the loaded config.
func NewRegistry(cfg *config.Config) *Registry {
	return &Registry{providers: cfg.Providers}
}

// ResolveModel finds a model alias across all providers and merges defaults.
//
// Two resolution modes:
//   - Provider-qualified (e.g. "z-ai/glm-5.1",
//     "local-models/my-org/model-x"): if the segment before
//     the first "/" matches a provider name, the remainder is resolved within
//     that provider only. This is how ListModels and the CLI picker produce
//     aliases — every model is explicitly tied to its provider.
//   - Bare alias (e.g. "deepseek/deepseek-v4-flash"): if the first segment
//     doesn't match a provider name, all providers are searched in config
//     order and the first match wins. This path exists for the compression
//     model config field and existing thread records in the database that
//     store bare aliases.
func (r *Registry) ResolveModel(alias string) (ResolvedModel, error) {
	// Check for provider-qualified lookup: "provider-name/rest/of/alias".
	// The first "/"-delimited segment must exactly match a provider name.
	if idx := strings.IndexByte(alias, '/'); idx > 0 {
		prefix := alias[:idx]
		rest := alias[idx+1:]
		for _, p := range r.providers {
			if p.Name == prefix {
				m, ok := p.Models[rest]
				if !ok {
					break // provider found, but model not in it
				}
				return r.resolveWithin(p, rest, m), nil
			}
		}
		// Fall through to bare-alias search if the prefix didn't match
		// a provider name (e.g. "my-org/..." where my-org isn't a provider).
	}

	// Bare alias: first provider to define it wins.
	for _, p := range r.providers {
		m, ok := p.Models[alias]
		if !ok {
			continue
		}
		return r.resolveWithin(p, alias, m), nil
	}

	return ResolvedModel{}, fmt.Errorf("unknown model alias: %s", alias)
}

// resolveWithin builds a ResolvedModel from a specific provider + alias,
// applying model_defaults inheritance.
func (r *Registry) resolveWithin(p config.ProviderConfig, alias string, m config.ModelConfig) ResolvedModel {
	resolved := ResolvedModel{
		ProviderName:   p.Name,
		BaseURL:        p.BaseURL,
		APIKey:         p.APIKey,
		API:            p.API,
		Compat:         p.Compat,
		ReasoningStyle: p.ReasoningStyle,
	}

	// ContextLength inheritance: model > model_defaults > 0.
	if m.ContextLength != nil {
		resolved.ContextLength = *m.ContextLength
	} else if p.ModelDefaults.ContextLength != nil {
		resolved.ContextLength = *p.ModelDefaults.ContextLength
	}

	// MaxTokens inheritance: model > model_defaults > 0 (provider default).
	if m.MaxTokens != nil {
		resolved.MaxTokens = *m.MaxTokens
	} else if p.ModelDefaults.MaxTokens != nil {
		resolved.MaxTokens = *p.ModelDefaults.MaxTokens
	}

	// SupportsTools inheritance: model > model_defaults > false.
	if m.SupportsTools != nil {
		resolved.SupportsTools = *m.SupportsTools
	} else if p.ModelDefaults.SupportsTools != nil {
		resolved.SupportsTools = *p.ModelDefaults.SupportsTools
	} else {
		resolved.SupportsTools = false
	}

	// ModelID: if set in config, use it for API calls; otherwise the alias is used.
	if m.ModelID != nil {
		resolved.ModelID = *m.ModelID
	} else {
		resolved.ModelID = alias
	}

	return resolved
}

// ListModels returns all configured model aliases, each prefixed with its
// provider name (e.g. "z-ai/glm-5.1", "local-models/my-org/model-x").
// Every entry is provider-qualified so there is never ambiguity about which
// host serves a model. Sorted alphabetically by display name.
func (r *Registry) ListModels() []string {
	var displays []string
	for _, p := range r.providers {
		for alias := range p.Models {
			displays = append(displays, p.Name+"/"+alias)
		}
	}
	sort.Strings(displays)
	return displays
}
