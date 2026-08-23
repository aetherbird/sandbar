package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/llm"
)

// llmRetryBackoff holds the waits before retry attempts 2 through 5, for five
// attempts total, for transient failures (5xx, EOF, network errors, a stalled
// stream). It is a package variable so tests can shrink it.
var llmRetryBackoff = []time.Duration{
	500 * time.Millisecond,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// rateLimitRetryBackoff holds the waits before retry attempts 2 through 6 for
// 429 rate-limit responses, for six attempts total (~3.8 minutes of budget).
// The later, longer waits give a quota window time to reset. It is a package
// variable so tests can shrink it.
var rateLimitRetryBackoff = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	120 * time.Second,
}

// isRateLimitError reports whether err is specifically an HTTP 429 rate-limit
// response (as opposed to any other retryable failure). The distinction drives
// the longer backoff schedule in runWithLLMRetry.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == 429
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return reqErr.HTTPStatusCode == 429
	}
	return false
}

// isRetryableLLMError reports whether err is a transient provider or transport
// failure worth retrying: HTTP 429 and 5xx responses, EOF/unexpected EOF, and
// network errors (including timeouts). User cancellation and other 4xx
// responses are never retryable. The llm client wraps provider errors with
// "complete: " / "start stream: " prefixes, so classification inspects the
// unwrapped cause via errors.As/Is.
func isRetryableLLMError(err error) bool {
	if err == nil {
		return false
	}
	// Caller-initiated cancellation (and our own deadline) must never be
	// retried: the caller is gone and the shared context would fail again.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A stalled provider stream is a transport failure on a connection the
	// caller still owns — a fresh attempt typically recovers it.
	if errors.Is(err, llm.ErrStreamIdle) {
		return true
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == 429 || apiErr.HTTPStatusCode >= 500
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		if reqErr.HTTPStatusCode == 429 || reqErr.HTTPStatusCode >= 500 {
			return true
		}
		if reqErr.HTTPStatusCode >= 400 && reqErr.HTTPStatusCode < 500 {
			return false
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// runWithLLMRetry invokes call, retrying transient provider errors with
// backoff. The schedule is chosen per-attempt from the error just observed:
// 429 rate limits use rateLimitRetryBackoff (six attempts, ~3.8 minutes of
// budget) while other transients use llmRetryBackoff (five attempts). Before
// each retry it emits an informational "intermediate" status event (rendered as
// ↻ in the CLI, so the long rate-limit waits never look frozen); like other
// informational events, emission errors are ignored. Cancellation during
// backoff aborts the wait.
func runWithLLMRetry(ctx context.Context, onEvent func(llm.StreamEvent) error, call func() error) error {
	for attempt := 1; ; attempt++ {
		err := call()
		if err == nil || !isRetryableLLMError(err) {
			return err
		}
		backoff := llmRetryBackoff
		reason := "transient provider error"
		if isRateLimitError(err) {
			backoff = rateLimitRetryBackoff
			reason = "rate limit (429)"
		}
		if attempt > len(backoff) {
			return err
		}
		_ = onEvent(llm.StreamEvent{
			Type:    "intermediate",
			Content: fmt.Sprintf("retrying after %s (attempt %d/%d): %v", reason, attempt+1, len(backoff)+1, err),
		})
		timer := time.NewTimer(backoff[attempt-1])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
