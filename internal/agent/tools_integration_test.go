package agent

import (
	"context"
	"testing"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/testutil"
)

func TestAgentToolLoop(t *testing.T) {
	ts := testutil.NewFakeToolLLMServer(t, openai.ToolCall{
		ID:   "call_abc123",
		Type: "function",
		Function: openai.FunctionCall{
			Name:      "file_read",
			Arguments: `{"path":"hello.txt"}`,
		},
	})
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	var events []llm.StreamEvent
	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "read hello.txt",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if threadID == "" {
		t.Fatal("expected thread ID")
	}

	// Verify we got tool_call and tool_result events.
	var hasToolCall, hasToolResult bool
	for _, ev := range events {
		if ev.Type == "tool_call" && ev.ToolName == "file_read" {
			hasToolCall = true
		}
		if ev.Type == "tool_result" && ev.ToolName == "file_read" {
			hasToolResult = true
		}
	}
	if !hasToolCall {
		t.Errorf("missing tool_call event: %v", events)
	}
	if !hasToolResult {
		t.Errorf("missing tool_result event: %v", events)
	}

	// Verify messages were persisted.
	_, messages, err := store.GetThreadWithMessages(threadID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	// Expected: user, assistant (with tool call), tool result, assistant (follow-up)
	if len(messages) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(messages))
	}
	if messages[0].Role != "user" {
		t.Errorf("first message role: got %q, want user", messages[0].Role)
	}
	if messages[1].Role != "assistant" {
		t.Errorf("second message role: got %q, want assistant", messages[1].Role)
	}
	if messages[2].Role != "tool" {
		t.Errorf("third message role: got %q, want tool", messages[2].Role)
	}
}
