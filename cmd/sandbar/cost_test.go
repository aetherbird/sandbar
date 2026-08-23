package main

import (
	"strings"
	"testing"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
)

func costTestConfig(provider string) *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{{
			Name:   provider,
			BaseURL: "https://example.test/v1",
			APIKey: "k",
			Models: map[string]config.ModelConfig{
				"claude-opus-4-5": {SupportsTools: boolPtrTrue()},
				"totally-unknown": {SupportsTools: boolPtrTrue()},
				"free-local":      {SupportsTools: boolPtrTrue()},
			},
		}},
	}
}

func boolPtrTrue() *bool {
	v := true
	return &v
}

// TestCostTrackerPricesUsageEvents: known catalog model accumulates cost
// across events (cache split included) and renders a non-empty segment.
func TestCostTrackerPricesUsageEvents(t *testing.T) {
	cfg := costTestConfig("anthropic")
	costs := newCostTracker(cfg, "anthropic/claude-opus-4-5")
	if costs == nil {
		t.Fatal("known priced model produced a nil tracker")
	}
	if seg := costs.segment(); seg == "" || !strings.HasPrefix(seg, "⚑ $") {
		t.Fatalf("initial segment = %q", seg)
	}
	costs.add(llm.StreamEvent{Type: "usage", PromptTokens: 1_000_000, CompletionTokens: 100_000, CacheReadTokens: 200_000, CacheWriteTokens: 100_000})
	costs.add(llm.StreamEvent{Type: "usage", PromptTokens: 500_000, CompletionTokens: 10_000})
	// opus 4.5: $5/M in, $25/M out, $0.5/M cache read, $6.25/M cache write
	// event 1: (1M-300k)*5 + 100k*25 + 200k*0.5 + 100k*6.25 = 3.5+2.5+0.1+0.625 = 6.725
	// event 2: 500k*5 + 10k*25 = 2.5+0.25 = 2.75 → total 9.475
	if seg := costs.segment(); seg != "⚑ $9.4750" {
		t.Fatalf("segment after two events = %q, want ⚑ $9.4750", seg)
	}
}

// TestCostTrackerUnknownModelIsNoOp: a model missing from the catalog yields
// a nil tracker whose segment and add are no-ops.
func TestCostTrackerUnknownModelIsNoOp(t *testing.T) {
	cfg := costTestConfig("anthropic")
	if costs := newCostTracker(cfg, "anthropic/totally-unknown"); costs != nil {
		t.Fatalf("unknown catalog model produced a tracker: %+v", costs)
	}
	if costs := newCostTracker(nil, "anthropic/claude-opus-4-5"); costs != nil {
		t.Fatalf("nil config produced a tracker: %+v", costs)
	}
	var costs *costTracker
	if seg := costs.segment(); seg != "" {
		t.Fatalf("nil tracker segment = %q, want empty", seg)
	}
	if c := costs.add(llm.StreamEvent{Type: "usage", PromptTokens: 1000}); c != 0 {
		t.Fatalf("nil tracker add = %v, want 0", c)
	}
}

// TestCostTrackerFreeModelHidden: catalog entries with all-zero pricing (or
// local models not in the catalog at all) keep the segment hidden.
func TestCostTrackerFreeModelHidden(t *testing.T) {
	cfg := costTestConfig("my-ollama")
	if costs := newCostTracker(cfg, "my-ollama/free-local"); costs != nil {
		t.Fatalf("free model produced a tracker: %+v", costs)
	}
}

// TestCostTrackerAliasResolution: a provider name that only resembles the
// catalog id (the normalizeProvider aliases) still resolves.
func TestCostTrackerAliasResolution(t *testing.T) {
	cfg := costTestConfig("anthropic-direct")
	if costs := newCostTracker(cfg, "anthropic-direct/claude-opus-4-5"); costs == nil {
		t.Fatal("aliased provider name should resolve to the catalog")
	}
}

// TestOneShotCostFooter: one-shot mode prints the total cost to stderr at
// exit for a priced model, and stays silent for an unknown one.
func TestOneShotCostFooter(t *testing.T) {
	cfg := costTestConfig("anthropic")
	events := []llm.StreamEvent{
		{Type: "token", Content: "hi"},
		{Type: "usage", PromptTokens: 1_000_000, CompletionTokens: 100_000},
		{Type: "done"},
	}

	var stderr strings.Builder
	be := &fakeCLIBackend{events: events}
	if err := runOneShot(be, cfg, "anthropic/claude-opus-4-5", "", "q", "", false, false, strings.NewReader(""), false, &strings.Builder{}, &stderr); err != nil {
		t.Fatalf("one-shot: %v", err)
	}
	// 1M in * $5/M + 100k out * $25/M = $7.5
	if !strings.Contains(stderr.String(), "cost $7.5000") {
		t.Fatalf("stderr footer = %q, want cost $7.5000", stderr.String())
	}

	unknown := &fakeCLIBackend{events: events}
	stderr.Reset()
	if err := runOneShot(unknown, cfg, "anthropic/totally-unknown", "", "q", "", false, false, strings.NewReader(""), false, &strings.Builder{}, &stderr); err != nil {
		t.Fatalf("one-shot unknown: %v", err)
	}
	if strings.Contains(stderr.String(), "cost") {
		t.Fatalf("unknown model must not print a cost footer: %q", stderr.String())
	}
}
