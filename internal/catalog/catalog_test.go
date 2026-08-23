package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aetherbird/sandbar/internal/llm"
)

func TestEmbeddedSnapshotParsesAndLooksUp(t *testing.T) {
	c := Embedded()
	if len(c.Providers) == 0 {
		t.Fatal("embedded snapshot is empty")
	}
	if c.Size() < 100 {
		t.Fatalf("embedded snapshot suspiciously small: %d models", c.Size())
	}
	// A known Anthropic model with real pricing.
	m := c.Lookup("anthropic", "claude-opus-4-5")
	if m == nil {
		t.Fatalf("claude-opus-4-5 missing from snapshot (providers: %d)", len(c.Providers))
	}
	if m.Cost.Input == 0 || m.Cost.Output == 0 {
		t.Fatalf("opus pricing missing: %+v", m.Cost)
	}
	if m.Limit.Context == 0 {
		t.Fatalf("opus context limit missing: %+v", m.Limit)
	}
	if m.Free() {
		t.Fatalf("opus must not be reported free: %+v", m.Cost)
	}
}

func TestLookupNormalizesProviderAliases(t *testing.T) {
	c := Embedded()
	direct := c.Lookup("anthropic", "claude-opus-4-5")
	if direct == nil {
		t.Skip("snapshot lacks anthropic entry")
	}
	for _, alias := range []string{"anthropic-direct", "my-anthropic"} {
		if got := c.Lookup(alias, "claude-opus-4-5"); got == nil {
			t.Errorf("Lookup(%q, …) = nil, want alias resolution", alias)
		}
	}
}

func TestLookupUnknownReturnsNil(t *testing.T) {
	c := Embedded()
	if m := c.Lookup("nope", "whatever"); m != nil {
		t.Errorf("unknown provider returned %+v", m)
	}
	if m := c.Lookup("anthropic", "nonexistent-model"); m != nil {
		t.Errorf("unknown model returned %+v", m)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Error("Parse must reject non-JSON")
	}
}

func TestCostOfMath(t *testing.T) {
	// $5/M input, $25/M output, $0.5/M cache read, $6.25/M cache write.
	m := &Model{Cost: Cost{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}}
	u := llm.CompletionUsage{PromptTokens: 1_000_000, CompletionTokens: 100_000, CacheReadTokens: 200_000, CacheWriteTokens: 100_000}
	// billed input = 1M - 200k - 100k = 700k → $3.50
	// output 100k → $2.50; cache read 200k → $0.10; write 100k → $0.625
	want := 3.50 + 2.50 + 0.10 + 0.625
	if got := m.CostOf(u); diff(got, want) > 1e-9 {
		t.Fatalf("CostOf = %v, want %v", got, want)
	}
	// Cache tokens must not be double-billed as input.
	u2 := llm.CompletionUsage{PromptTokens: 300_000, CacheReadTokens: 300_000, CompletionTokens: 0}
	want2 := 0.15
	if got := m.CostOf(u2); diff(got, want2) > 1e-9 {
		t.Fatalf("CostOf all-cached = %v, want %v (no double billing)", got, want2)
	}
}

func TestCostOfNilModelIsZero(t *testing.T) {
	var m *Model
	if got := m.CostOf(llm.CompletionUsage{PromptTokens: 100}); got != 0 {
		t.Fatalf("nil model CostOf = %v, want 0", got)
	}
	if !m.Free() {
		t.Fatal("nil model must report free (segment hidden)")
	}
}

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "$0"},
		{0.004, "$0.004"},
		{1.5, "$1.5000"},
		{12.3456, "$12.35"},
	}
	for _, c := range cases {
		if got := FormatUSD(c.in); got != c.want {
			t.Errorf("FormatUSD(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRefreshParsesAndErrors(t *testing.T) {
	good := `{"acme":{"id":"acme","name":"Acme","models":{"big":{"id":"big","name":"Big","limit":{"context":8192,"output":1024},"cost":{"input":1,"output":2}}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(good))
	}))
	defer srv.Close()
	c, err := Refresh(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if m := c.Lookup("acme", "big"); m == nil || m.Limit.Context != 8192 || m.Cost.Input != 1 {
		t.Fatalf("refreshed catalog lookup = %+v", m)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer bad.Close()
	if _, err := Refresh(context.Background(), bad.URL); err == nil {
		t.Error("non-200 must error")
	}
}

func diff(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}
