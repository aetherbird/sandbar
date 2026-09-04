package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aetherbird/sandbar/internal/llm"
)

// TestTropicalModeInjectsDirectiveAndMapsEffortToHigh pins the Tropical-mode
// contract: the effort string "tropical" never reaches the provider — the
// wire sees reasoning_effort "high" — and the system prompt carries the
// heavy-subagent directive.
func TestTropicalModeInjectsDirectiveAndMapsEffortToHigh(t *testing.T) {
	var mu sync.Mutex
	var sawEffort string
	var sawSystem string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		s := string(body)
		if i := strings.Index(s, `"reasoning_effort"`); i >= 0 {
			rest := s[i+len(`"reasoning_effort":`):]
			end := strings.IndexAny(rest, ",}")
			sawEffort = strings.Trim(rest[:end], `" `)
		}
		if strings.Contains(s, "Tropical Mode") {
			sawSystem = "yes"
		}
		respondJSONBody(w, body, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	if _, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "go", Workspace: t.TempDir(), Effort: "tropical",
	}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawEffort != "high" {
		t.Fatalf("wire reasoning_effort = %q, want high (tropical must never reach the provider)", sawEffort)
	}
	if sawSystem == "" {
		t.Fatal("system prompt missing the Tropical Mode directive")
	}
}

// TestTropicalFieldEquivalence pins the first-class Request.Tropical field:
// it injects the directive and maps wire effort to high exactly like the
// legacy "tropical" effort string.
func TestTropicalFieldEquivalence(t *testing.T) {
	var mu sync.Mutex
	var sawEffort string
	var sawSystem string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		s := string(body)
		if i := strings.Index(s, `"reasoning_effort"`); i >= 0 {
			rest := s[i+len(`"reasoning_effort":`):]
			end := strings.IndexAny(rest, ",}")
			sawEffort = strings.Trim(rest[:end], `" `)
		}
		if strings.Contains(s, "Tropical Mode") {
			sawSystem = "yes"
		}
		respondJSONBody(w, body, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	if _, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "go", Workspace: t.TempDir(), Tropical: true,
	}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawEffort != "high" {
		t.Fatalf("wire reasoning_effort = %q, want high", sawEffort)
	}
	if sawSystem == "" {
		t.Fatal("system prompt missing the Tropical Mode directive")
	}
}

// TestTropicalContextFlagEquivalence pins the WithTropicalRequest transport:
// a context-annotated turn behaves as Tropical even with a plain request.
func TestTropicalContextFlagEquivalence(t *testing.T) {
	var mu sync.Mutex
	sawSystem := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sawSystem = sawSystem || strings.Contains(string(body), "Tropical Mode")
		mu.Unlock()
		respondJSONBody(w, body, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	ctx := WithTropicalRequest(context.Background())
	if _, err := a.Chat(ctx, Request{
		ModelAlias: "test-model", UserMessage: "go", Workspace: t.TempDir(),
	}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawSystem {
		t.Fatal("system prompt missing the Tropical Mode directive for context-flagged turn")
	}
}

// TestNonTropicalEffortOmitsDirective pins the negative: a plain effort (or
// none) never injects the directive.
func TestNonTropicalEffortOmitsDirective(t *testing.T) {
	var mu sync.Mutex
	sawDirective := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sawDirective = sawDirective || strings.Contains(string(body), "Tropical Mode")
		mu.Unlock()
		respondJSONBody(w, body, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	if _, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "go", Workspace: t.TempDir(), Effort: "high",
	}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("chat: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawDirective {
		t.Fatal("Tropical directive injected for a non-tropical effort")
	}
}
