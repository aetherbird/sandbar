package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"

	"sandbar/internal/llm"
)

func TestIsRateLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429 api error", &openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}, true},
		{"wrapped 429", fmt.Errorf("complete: %w", &openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}), true},
		{"429 request error", &openai.RequestError{HTTPStatusCode: 429, Err: errors.New("boom")}, true},
		{"500 api error", &openai.APIError{HTTPStatusCode: 500, Message: "server error"}, false},
		{"500 request error", &openai.RequestError{HTTPStatusCode: 500, Err: errors.New("boom")}, false},
		{"401 api error", &openai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}, false},
		{"EOF", errors.New("unexpected EOF"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRateLimitError(tc.err); got != tc.want {
				t.Errorf("isRateLimitError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// collectEvents returns an onEvent that records every event for inspection.
func collectEvents(events *[]llm.StreamEvent) func(llm.StreamEvent) error {
	return func(ev llm.StreamEvent) error {
		*events = append(*events, ev)
		return nil
	}
}

func TestRunWithLLMRetryRateLimitUsesLongSchedule(t *testing.T) {
	// Distinct shrink lengths: rate-limit (5 waits => 6 attempts) vs transient
	// (2 waits => 3 attempts). A 429 must exhaust the longer schedule.
	shrinkRateLimitRetryBackoff(t)
	old := llmRetryBackoff
	llmRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { llmRetryBackoff = old })

	calls := 0
	var events []llm.StreamEvent
	err := runWithLLMRetry(context.Background(), collectEvents(&events), func() error {
		calls++
		return &openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 6 {
		t.Errorf("calls = %d, want 6 (rate-limit schedule)", calls)
	}
	// Every retry event must name the rate-limit reason and count to 6.
	if len(events) != 5 {
		t.Fatalf("retry events = %d, want 5", len(events))
	}
	for i, ev := range events {
		if ev.Type != "intermediate" {
			t.Errorf("event %d type = %q, want intermediate", i, ev.Type)
		}
		if !strings.Contains(ev.Content, "rate limit (429)") {
			t.Errorf("event %d content missing rate-limit reason: %q", i, ev.Content)
		}
	}
}

func TestRunWithLLMRetryTransientUsesShortSchedule(t *testing.T) {
	// 5xx stays on the short schedule: three attempts total.
	shrinkLLMRetryBackoff(t)
	old := rateLimitRetryBackoff
	rateLimitRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { rateLimitRetryBackoff = old })

	calls := 0
	var events []llm.StreamEvent
	err := runWithLLMRetry(context.Background(), collectEvents(&events), func() error {
		calls++
		return &openai.APIError{HTTPStatusCode: 500, Message: "server error"}
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (transient schedule)", calls)
	}
	for _, ev := range events {
		if !strings.Contains(ev.Content, "transient provider error") {
			t.Errorf("transient event should say 'transient provider error', got %q", ev.Content)
		}
	}
}

func TestRunWithLLMRetryCancelDuringRateLimitBackoff(t *testing.T) {
	// A long rate-limit wait must abort promptly on cancellation.
	old := rateLimitRetryBackoff
	rateLimitRetryBackoff = []time.Duration{time.Hour}
	t.Cleanup(func() { rateLimitRetryBackoff = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	err := runWithLLMRetry(ctx, collectEvents(&[]llm.StreamEvent{}), func() error {
		calls++
		cancel() // cancel after the first 429 so the backoff wait aborts
		return &openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestRunWithLLMRetryDoesNotRetryNonRetryable(t *testing.T) {
	calls := 0
	err := runWithLLMRetry(context.Background(), collectEvents(&[]llm.StreamEvent{}), func() error {
		calls++
		return &openai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (401 is not retryable)", calls)
	}
}
