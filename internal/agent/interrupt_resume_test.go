package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/tools"
)

// TestAgentChatInterruptedTurnAnnouncesThreadID reproduces the session-killing
// bug behind the "loses ALL context" transcript: the only stream event that
// carried a ThreadID was the terminal "done", which an interrupted turn never
// emits. The CLI therefore kept "" as the active thread and the next message
// silently created a brand-new thread — the model was sent only the system
// prompt plus the new message (~2.9K tokens) and replied as if the
// conversation had just started.
//
// Contract under test: ANY turn — including one cancelled mid-tool-batch —
// must announce the thread ID it created or resumed, before any provider call.
func TestAgentChatInterruptedTurnAnnouncesThreadID(t *testing.T) {
	workspace := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, r, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf first\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer server.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = server.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	a.cfg.MaxTurns = 0
	a.tools = tools.NewRegistry(workspace, "", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var announcedID, returnedID string
	var chatErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		returnedID, chatErr = a.Chat(ctx, Request{
			ModelAlias: "test-model", UserMessage: "first user message", Workspace: workspace,
		}, func(ev llm.StreamEvent) error {
			if ev.ThreadID != "" {
				announcedID = ev.ThreadID
			}
			// Interrupt once the tool batch has started executing.
			if ev.Type == "tool_result" {
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted chat did not return")
	}
	if !errors.Is(chatErr, context.Canceled) {
		t.Fatalf("chat error: got %v, want context canceled", chatErr)
	}
	if announcedID == "" {
		t.Fatal("interrupted turn announced no thread ID: the session would orphan its thread and the next message would start a brand-new conversation")
	}
	if returnedID != announcedID {
		t.Fatalf("announced thread ID %q does not match returned %q", announcedID, returnedID)
	}
}

// TestAgentChatInterruptedFirstTurnResumesWithHistory guards the invariant the
// thread-ID announcement exists to protect: when the follow-up message resumes
// the SAME thread (which the CLI can only do if the ID reached it), the
// provider payload still contains the pre-interrupt user message and the sealed
// tool results.
func TestAgentChatInterruptedFirstTurnResumesWithHistory(t *testing.T) {
	workspace := t.TempDir()
	callCount := 0
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if callCount == 1 {
			respondJSONBody(w, raw, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf first\"}"}},{"id":"t2","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf second\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		respondJSONBody(w, raw, `{"choices":[{"message":{"role":"assistant","content":"resumed with history"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = server.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	a.cfg.MaxTurns = 0
	a.tools = tools.NewRegistry(workspace, "", "", nil)

	// Turn 1: interrupt mid-tool-batch — t1 completes, t2 is still pending and
	// must be sealed with the interrupted marker.
	ctx1, cancel1 := context.WithCancel(context.Background())
	threadID, err := a.Chat(ctx1, Request{
		ModelAlias: "test-model", UserMessage: "first user message", Workspace: workspace,
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "tool_result" && ev.ToolCallID == "t1" {
			cancel1()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn 1 error: got %v, want context canceled", err)
	}
	if threadID == "" {
		t.Fatal("turn 1 returned no thread ID")
	}

	// Turn 2: resume the same thread, exactly as the CLI would once it knows
	// the ID.
	var terminal string
	_, err = a.Chat(context.Background(), Request{
		ThreadID: threadID, ModelAlias: "test-model", UserMessage: "how's it going?", Workspace: workspace,
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "token" {
			terminal += ev.Content
		}
		return nil
	})
	if err != nil {
		t.Fatalf("turn 2 error: %v", err)
	}
	if terminal != "resumed with history" {
		t.Fatalf("terminal content: got %q", terminal)
	}
	if len(bodies) < 2 {
		t.Fatalf("expected 2 provider requests, got %d", len(bodies))
	}
	payload := bodies[1]
	if !strings.Contains(payload, "first user message") {
		t.Fatalf("resumed provider payload lost the pre-interrupt user message:\n%s", payload)
	}
	if !strings.Contains(payload, interruptedToolResult) {
		t.Fatalf("resumed provider payload missing the sealed interrupted tool result:\n%s", payload)
	}
}
