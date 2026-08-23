package main

import (
	"strings"

	"github.com/aetherbird/sandbar/internal/catalog"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
)

// Cost rollups: usage events are priced against the embedded models.dev
// catalog snapshot. A tracker resolves the active model once; when the model
// is unknown to the catalog or free, every segment is hidden (no "$0" noise).

// costTracker prices llm usage events for one active model.
type costTracker struct {
	model *catalog.Model
	total float64
}

// newCostTracker resolves alias against the catalog via the configured
// providers. A nil tracker (unknown model) hides every cost segment.
func newCostTracker(cfg *config.Config, alias string) *costTracker {
	model := lookupCatalogModel(cfg, alias)
	if model == nil || model.Free() {
		return nil
	}
	return &costTracker{model: model}
}

// add prices one usage event and returns its USD cost.
func (c *costTracker) add(ev llm.StreamEvent) float64 {
	if c == nil {
		return 0
	}
	cost := c.model.CostOf(llm.CompletionUsage{
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		CacheReadTokens:  ev.CacheReadTokens,
		CacheWriteTokens: ev.CacheWriteTokens,
	})
	c.total += cost
	return cost
}

// segment renders the cumulative-session cost for the status bar, or "" when
// pricing is not active.
func (c *costTracker) segment() string {
	if c == nil {
		return ""
	}
	return "⚑ " + catalog.FormatUSD(c.total)
}

// lookupCatalogModel maps a configured alias to a catalog entry. The wire
// model id is tried first as a provider-qualified name ("google/gemini-…",
// as OpenRouter and similar gateways spell it), then as a bare id under the
// configured provider (whose name the catalog normalizes, e.g.
// "anthropic-direct" → "anthropic").
func lookupCatalogModel(cfg *config.Config, alias string) *catalog.Model {
	if cfg == nil || strings.TrimSpace(alias) == "" {
		return nil
	}
	resolved, err := llm.NewRegistry(cfg).ResolveModel(alias)
	if err != nil {
		return nil
	}
	embedded := catalog.Embedded()
	wireID := resolved.ModelID
	if wireID == "" {
		wireID = alias
	}
	if _, rest, ok := strings.Cut(wireID, "/"); ok {
		if provider, _, _ := strings.Cut(wireID, "/"); provider != "" && rest != "" {
			if m := embedded.Lookup(provider, rest); m != nil {
				return m
			}
		}
	}
	return embedded.Lookup(resolved.ProviderName, wireID)
}
