package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

// withShortIdle shrinks the idle watchdog for the duration of a test and
// restores it after.
func withShortIdle(t *testing.T, d time.Duration) {
	t.Helper()
	old := streamIdleTimeout
	streamIdleTimeout = d
	t.Cleanup(func() { streamIdleTimeout = old })
}

// withShortAttemptCap shrinks the per-attempt (and header) timeout. Must run
// before the client is constructed — the header timeout is captured into the
// transport at NewClient time.
func withShortAttemptCap(t *testing.T, d time.Duration) {
	t.Helper()
	old := completionAttemptTimeout
	completionAttemptTimeout = d
	t.Cleanup(func() { completionAttemptTimeout = old })
}

// stalledServer serves a streaming response that emits one chunk, then
// nothing — the connection stays open and silent, exactly the wedged-gateway
// failure that used to hang turns indefinitely.
func stalledServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		// Flush the chunk, then go silent forever.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestChatStreamIdleWatchdogUnblocksStalledStream proves a silent connection
// produces a retryable ErrStreamIdle error event within the idle window
// instead of blocking forever.
func TestChatStreamIdleWatchdogUnblocksStalledStream(t *testing.T) {
	withShortIdle(t, 250*time.Millisecond)
	ts := stalledServer(t)

	client := NewClient(ts.URL, "fake-key", "test-model")
	ch, err := client.Chat(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for ev := range ch {
		if ev.Type == "error" {
			if ev.Content != ErrStreamIdle.Error() {
				t.Fatalf("error event: got %q want the idle-stall message", ev.Content)
			}
			return // success: stalled stream unblocked with the sentinel
		}
	}
	select {
	case <-deadline:
		t.Fatal("channel closed without an error event")
	default:
	}
	t.Fatal("stream ended without reporting the stall")
}

// TestCompleteStreamingIdleWatchdog proves the non-streaming fallback's
// streaming path (completeStreaming) also unblocks on a stalled connection.
func TestCompleteStreamingIdleWatchdog(t *testing.T) {
	withShortIdle(t, 250*time.Millisecond)
	withShortAttemptCap(t, 250*time.Millisecond) // cap the non-streaming read of the stalled body
	ts := stalledServer(t)

	client := NewClient(ts.URL, "fake-key", "test-model")
	done := make(chan error, 1)
	go func() {
		// The non-streaming attempt fails immediately (the handler never
		// returns a JSON completion), so this exercises completeStreaming.
		_, err := client.Complete(context.Background(), []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "hi"},
		}, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ErrStreamIdle) {
			t.Fatalf("complete error: got %v, want ErrStreamIdle", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled complete did not unblock within the idle window")
	}
}

// TestCompleteAttemptTimeoutCapsNonStreamingHang proves the per-attempt cap on
// the non-streaming path: a server that accepts the request and never responds
// fails within the cap instead of blocking forever.
func TestCompleteAttemptTimeoutCapsNonStreamingHang(t *testing.T) {
	withShortAttemptCap(t, 250*time.Millisecond)

	// Release handlers via a test-owned channel: r.Context().Done() alone can
	// stay undelivered when the client aborts pre-headers, which would hang
	// httptest.Server.Close in t.Cleanup forever.
	handlers := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-handlers // accept, then hang until the test ends
	}))
	t.Cleanup(func() {
		close(handlers)
		ts.Close()
	})

	client := NewClient(ts.URL, "fake-key", "test-model")
	done := make(chan error, 1)
	go func() {
		_, err := client.Complete(context.Background(), []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "hi"},
		}, nil)
		done <- err
	}()

	select {
	case err := <-done:
		// The capped non-streaming attempt falls back to streaming, whose
		// header timeout also fires against this never-answering server;
		// either way the call must return instead of hanging.
		if err == nil {
			t.Fatal("expected an error from a hanging provider")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hanging non-streaming completion was not capped")
	}
}
