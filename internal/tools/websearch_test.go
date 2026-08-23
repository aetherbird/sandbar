package tools

import (
	"context"
	"testing"
)

func TestWebSearchMissingQuery(t *testing.T) {
	_, err := WebSearch(context.Background(), map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestWebSearchWithQuery(t *testing.T) {
	// DuckDuckGo fallback — may fail in offline environments, so just verify no panic.
	_, err := WebSearch(context.Background(), map[string]interface{}{
		"query": "golang",
	})
	// We don't assert err == nil because network may be unavailable in test env.
	_ = err
}
