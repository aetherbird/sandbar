package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/tools"
)

func setupTestAgent(t *testing.T, supportsTools bool) (*Agent, *memory.Store, func()) {
	dbPath := t.TempDir() + "/test.db"
	store, err := memory.OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	cfg := &config.Config{
		Persona: config.PersonaConfig{
			Name:         "Sandbar",
			SystemPrompt: "You are a test assistant.",
		},
		Providers: []config.ProviderConfig{
			{
				Name:    "test-provider",
				BaseURL: "", // set per-test
				APIKey:  "fake",
				Models: map[string]config.ModelConfig{
					"test-model": {SupportsTools: &supportsTools},
				},
			},
		},
	}

	registry := llm.NewRegistry(cfg)
	toolReg := tools.NewRegistry(t.TempDir(), "", "", nil)
	agent := New(cfg, store, registry, toolReg)
	return agent, store, func() { store.Close() }
}

// mustBuildMessages loads the indexed message view for a thread, failing the
// test when the history cannot be loaded or repaired.
func mustBuildMessages(t *testing.T, a *Agent, threadID, workspace, source string) []indexedMessage {
	t.Helper()
	msgs, err := a.buildMessages(threadID, workspace, source, false, nil)
	if err != nil {
		t.Fatalf("buildMessages: %v", err)
	}
	return msgs
}

func TestAgentChatCreatesThread(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi!\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, false)
	defer cleanup()

	// Patch the provider base URL to point at fake server.
	agent.cfg.Providers[0].BaseURL = ts.URL

	var events []llm.StreamEvent
	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if threadID == "" {
		t.Errorf("expected thread ID, got empty")
	}

	// Verify we got a token and done event.
	var hasToken, hasDone bool
	for _, ev := range events {
		if ev.Type == "token" && ev.Content == "Hi!" {
			hasToken = true
		}
		if ev.Type == "done" {
			hasDone = true
		}
	}
	if !hasToken {
		t.Errorf("missing expected token event: %v", events)
	}
	if !hasDone {
		t.Errorf("missing done event: %v", events)
	}

	// Verify thread and messages were persisted.
	threads, err := store.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}

	_, messages, err := store.GetThreadWithMessages(threads[0].ID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Role != "user" {
		t.Errorf("first message role: got %q, want user", messages[0].Role)
	}
	if messages[1].Role != "assistant" {
		t.Errorf("second message role: got %q, want assistant", messages[1].Role)
	}
}

func TestAgentChatResumesThread(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	content := "previous"
	_, err = store.AppendMessage(thread.ID, "user", &content, nil)
	if err != nil {
		t.Fatalf("append message: %v", err)
	}

	threadID, err := agent.Chat(context.Background(), Request{
		ThreadID:    thread.ID,
		ModelAlias:  "test-model",
		UserMessage: "next",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if threadID != thread.ID {
		t.Errorf("expected thread ID %q, got %q", thread.ID, threadID)
	}

	_, messages, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(messages))
	}
}

func TestAgentChatUnknownModel(t *testing.T) {
	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "nonexistent",
		UserMessage: "hello",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestAgentChatMaxTurns(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.MaxTurns = 1

	var events []llm.StreamEvent
	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger tool loop",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasMaxTurns bool
	for _, ev := range events {
		if ev.Type == "error" && ev.Content == "max turn limit reached" {
			hasMaxTurns = true
		}
	}
	if !hasMaxTurns {
		t.Errorf("expected max turn error event, got events: %v", events)
	}
	if callCount != 1 {
		t.Fatalf("finite cap made %d provider calls, want 1", callCount)
	}
}

func TestAgentChatZeroMaxTurnsIsUnlimited(t *testing.T) {
	const toolTurns = 55 // exceed both the former implicit 10 and shipped 50 caps
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount <= toolTurns {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"missing-%d.txt\"}"}}]},"finish_reason":"tool_calls"}]}`, callCount, callCount)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"completed after the long tool loop"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	agent.cfg.MaxTurns = 0

	var events []llm.StreamEvent
	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "complete a long tool loop",
		Workspace:   t.TempDir(),
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("unlimited chat: %v", err)
	}
	if callCount != toolTurns+1 {
		t.Fatalf("provider calls: got %d, want %d", callCount, toolTurns+1)
	}
	var terminalContent string
	var hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case "error":
			if ev.Content == "max turn limit reached" {
				t.Fatalf("zero max_turns unexpectedly capped the loop: %v", events)
			}
		case "token":
			terminalContent += ev.Content
		case "done":
			hasDone = true
		}
	}
	if terminalContent != "completed after the long tool loop" || !hasDone {
		t.Fatalf("terminal completion missing: content=%q done=%v", terminalContent, hasDone)
	}
	_, persisted, err := store.GetThreadWithMessages(threadID)
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	if want := 1 + 2*toolTurns + 1; len(persisted) != want {
		t.Fatalf("persisted message count: got %d, want %d", len(persisted), want)
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, threadID, "", "web"))); err != nil {
		t.Fatalf("long unlimited-turn history is not safely resumable: %v", err)
	}
}

func TestSpawnSubagentZeroMaxTurnsIsUnlimited(t *testing.T) {
	const toolTurns = 16 // exceed the former implicit sub-agent cap of 14
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount <= toolTurns {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"sub_call_%d","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"missing-%d.txt\"}"}}]},"finish_reason":"tool_calls"}]}`, callCount, callCount)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"sub-agent completed"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"
	agent.cfg.Subagent.MaxTurns = 0

	events, err := agent.SpawnSubagent(context.Background(), "inspect", "test context")
	if err != nil {
		t.Fatalf("spawn sub-agent: %v", err)
	}
	var terminalContent string
	var hasDone bool
	for ev := range events {
		if ev.Type == "error" && ev.Content == "max subagent turns reached" {
			t.Fatalf("zero subagent.max_turns unexpectedly capped the loop after %d calls", callCount)
		}
		if ev.Type == "token" {
			terminalContent += ev.Content
		}
		if ev.Type == "done" {
			hasDone = true
		}
	}
	if callCount != toolTurns+1 {
		t.Fatalf("sub-agent provider calls: got %d, want %d", callCount, toolTurns+1)
	}
	if terminalContent != "sub-agent completed" || !hasDone {
		t.Fatalf("sub-agent terminal completion missing: content=%q done=%v", terminalContent, hasDone)
	}
}

func TestSpawnSubagentPositiveMaxTurnsStillCaps(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"sub_capped_%d","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"missing.txt\"}"}}]},"finish_reason":"tool_calls"}]}`, callCount)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"
	agent.cfg.Subagent.MaxTurns = 2

	events, err := agent.SpawnSubagent(context.Background(), "inspect", "test context")
	if err != nil {
		t.Fatalf("spawn sub-agent: %v", err)
	}
	var hasLimitError, hasDone bool
	for ev := range events {
		if ev.Type == "error" && ev.Content == "max subagent turns reached" {
			hasLimitError = true
		}
		if ev.Type == "done" {
			hasDone = true
		}
	}
	if callCount != 2 {
		t.Fatalf("finite sub-agent cap made %d provider calls, want 2", callCount)
	}
	if !hasLimitError || hasDone {
		t.Fatalf("finite sub-agent cap events: limit_error=%v done=%v", hasLimitError, hasDone)
	}
}

func TestTruncateMessages(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "user1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "assistant1"},
		{Role: openai.ChatMessageRoleUser, Content: "user2"},
	}
	// Large context length should not truncate.
	result := truncateMessages(msgs, 100000)
	if len(result) != len(msgs) {
		t.Errorf("expected no truncation, got %d messages", len(result))
	}

	// Very small context length should truncate down to system + last user.
	result = truncateMessages(msgs, 10)
	if len(result) < 2 {
		t.Errorf("expected at least 2 messages, got %d", len(result))
	}
	if result[0].Role != openai.ChatMessageRoleSystem {
		t.Errorf("first message should be system, got %s", result[0].Role)
	}
}

func TestAgentChatCallbackError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi!\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return fmt.Errorf("callback error")
	})
	if err == nil {
		t.Fatal("expected error from callback")
	}
}

func TestAgentExecuteToolUnknown(t *testing.T) {
	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()

	_, err := agent.executeTool(context.Background(), "nonexistent_tool", "{}", "/tmp")
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestAgentChatContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf first\"}"}},{"id":"call_2","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf second\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.MaxTurns = 5
	workspace := t.TempDir()
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	threadID, err := agent.Chat(ctx, Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger tools",
		Workspace:   workspace,
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "tool_result" && ev.ToolCallID == "call_1" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("chat error: got %v, want context canceled", err)
	}

	_, persisted, err := store.GetThreadWithMessages(threadID)
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	if len(persisted) != 4 {
		t.Fatalf("persisted history length: got %d, want user+assistant+2 tools: %+v", len(persisted), persisted)
	}
	if persisted[2].ToolCallID == nil || *persisted[2].ToolCallID != "call_1" ||
		persisted[3].ToolCallID == nil || *persisted[3].ToolCallID != "call_2" {
		t.Fatalf("pending tool calls were not closed in order: %+v", persisted)
	}
	if persisted[3].Content == nil || !strings.Contains(*persisted[3].Content, "interrupted") {
		t.Fatalf("unexecuted call did not get an explicit closing result: %+v", persisted[3])
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, threadID, workspace, "web"))); err != nil {
		t.Fatalf("cancelled thread cannot be safely resumed: %v", err)
	}
}

func TestAgentMaybeGenerateTitle(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Test Title"}}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	// Wait for background title generation.
	for i := 0; i < 50; i++ {
		thread, _ := store.GetThread(threadID)
		if thread != nil && thread.Title != nil && *thread.Title != "" {
			break
		}
	}
}

func TestAgentResolveModelError(t *testing.T) {
	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "nonexistent-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestAgentMaxTurns(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.MaxTurns = 1

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger tool",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
}

func TestAgentNonFunctionToolCall(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"other","function":{"name":"shell_exec","arguments":"{\"command\":\"echo hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// Non-function tool gets skipped; agent retries Complete().
		// Return no tool_calls to fall through to streaming.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.MaxTurns = 5

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger tool",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
}

func TestAgentRejectsEmptyToolCallIDBeforePersistence(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"

	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger malformed tool",
		Workspace:   t.TempDir(),
	}, func(llm.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "invalid tool-call group") {
		t.Fatalf("expected malformed tool-call error, got %v", err)
	}
	_, persisted, readErr := store.GetThreadWithMessages(threadID)
	if readErr != nil {
		t.Fatalf("read history: %v", readErr)
	}
	if len(persisted) != 1 || persisted[0].Role != openai.ChatMessageRoleUser {
		t.Fatalf("malformed assistant call was persisted: %+v", persisted)
	}
}

func TestAgentRejectsReusedToolCallIDBeforeSecondAssistantPersistence(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_reused","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	workspace := t.TempDir()
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger reused tool id",
		Workspace:   workspace,
	}, func(llm.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "invalid tool-call group") {
		t.Fatalf("expected reused tool-call error, got %v", err)
	}
	if callCount != 2 {
		t.Fatalf("provider calls: got %d, want 2", callCount)
	}
	_, persisted, readErr := store.GetThreadWithMessages(threadID)
	if readErr != nil {
		t.Fatalf("read history: %v", readErr)
	}
	if len(persisted) != 3 {
		t.Fatalf("reused ID poisoned persistence: %+v", persisted)
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, threadID, workspace, "web"))); err != nil {
		t.Fatalf("history before rejected reused ID is invalid: %v", err)
	}
}

func TestPersistAssistantTurnAtomicRejectsCompressedOutToolCallID(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	oldUser := "old request"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleUser, &oldUser, nil); err != nil {
		t.Fatalf("append old user: %v", err)
	}
	const hiddenCallID = "call_hidden_by_compression"
	if _, err := agent.persistAssistantTurn(thread.ID, "", []openai.ToolCall{{
		ID: hiddenCallID, Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
	}}); err != nil {
		t.Fatalf("persist old assistant: %v", err)
	}
	oldResult := "old result"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleTool, &oldResult, strPtr(hiddenCallID)); err != nil {
		t.Fatalf("append old tool result: %v", err)
	}
	currentUser := "current request"
	current, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleUser, &currentUser, nil)
	if err != nil {
		t.Fatalf("append current user: %v", err)
	}
	if err := store.SaveCompression(memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "old request and tool work",
		FirstKeptSeq: current.Seq,
	}); err != nil {
		t.Fatalf("save compression: %v", err)
	}

	history := mustBuildMessages(t, agent, thread.ID, "", "web")
	duplicate := []openai.ToolCall{{
		ID: hiddenCallID, Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
	}}
	if err := validateNewToolCallGroup(history, "", duplicate); err != nil {
		t.Fatalf("fixture ID was not hidden from provider history: %v", err)
	}
	if _, err := agent.persistAssistantTurn(thread.ID, "", duplicate); err == nil {
		t.Fatal("expected compressed-out duplicate ID to fail persistence")
	}

	_, persisted, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(persisted) != 4 {
		t.Fatalf("failed atomic insert left an assistant row: %+v", persisted)
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, thread.ID, "", "web"))); err != nil {
		t.Fatalf("thread is not safely resumable after compressed-out duplicate: %v", err)
	}
}

func TestPersistAssistantTurnAtomicRejectsToolCallIDFromAnotherThread(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()

	source, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create source thread: %v", err)
	}
	const sharedCallID = "call_from_another_thread"
	if _, err := agent.persistAssistantTurn(source.ID, "", []openai.ToolCall{{
		ID: sharedCallID, Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
	}}); err != nil {
		t.Fatalf("persist source assistant: %v", err)
	}
	sourceResult := "source result"
	if _, err := store.AppendMessage(source.ID, openai.ChatMessageRoleTool, &sourceResult, strPtr(sharedCallID)); err != nil {
		t.Fatalf("append source tool result: %v", err)
	}

	target, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	targetUser := "target request"
	if _, err := store.AppendMessage(target.ID, openai.ChatMessageRoleUser, &targetUser, nil); err != nil {
		t.Fatalf("append target user: %v", err)
	}
	duplicate := []openai.ToolCall{{
		ID: sharedCallID, Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
	}}
	if err := validateNewToolCallGroup(mustBuildMessages(t, agent, target.ID, "", "web"), "", duplicate); err != nil {
		t.Fatalf("cross-thread ID unexpectedly appeared in provider history: %v", err)
	}
	if _, err := agent.persistAssistantTurn(target.ID, "", duplicate); err == nil {
		t.Fatal("expected cross-thread duplicate ID to fail persistence")
	}

	_, persisted, err := store.GetThreadWithMessages(target.ID)
	if err != nil {
		t.Fatalf("read target history: %v", err)
	}
	if len(persisted) != 1 || persisted[0].Role != openai.ChatMessageRoleUser {
		t.Fatalf("cross-thread duplicate left a target assistant row: %+v", persisted)
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, target.ID, "", "web"))); err != nil {
		t.Fatalf("target thread is not safely resumable: %v", err)
	}
}

func TestPersistAssistantTurnAtomicRollsBackPartialMultiCallInsert(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()

	source, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create source thread: %v", err)
	}
	const duplicateCallID = "call_duplicate_second"
	if _, err := agent.persistAssistantTurn(source.ID, "", []openai.ToolCall{{
		ID: duplicateCallID, Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
	}}); err != nil {
		t.Fatalf("seed duplicate call ID: %v", err)
	}
	sourceResult := "source result"
	if _, err := store.AppendMessage(source.ID, openai.ChatMessageRoleTool, &sourceResult, strPtr(duplicateCallID)); err != nil {
		t.Fatalf("append source tool result: %v", err)
	}

	target, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	targetUser := "target request"
	if _, err := store.AppendMessage(target.ID, openai.ChatMessageRoleUser, &targetUser, nil); err != nil {
		t.Fatalf("append target user: %v", err)
	}
	const firstCallID = "call_inserted_before_failure"
	_, err = agent.persistAssistantTurn(target.ID, "", []openai.ToolCall{
		{
			ID: firstCallID, Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
		},
		{
			ID: duplicateCallID, Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`},
		},
	})
	if err == nil {
		t.Fatal("expected second tool-call insert to fail")
	}
	if !strings.Contains(err.Error(), `tool call 1 ("`+duplicateCallID+`")`) {
		t.Fatalf("failure did not occur on the second tool call: %v", err)
	}

	var firstCallCount int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE id = ?`, firstCallID).Scan(&firstCallCount); err != nil {
		t.Fatalf("count rolled-back first call: %v", err)
	}
	if firstCallCount != 0 {
		t.Fatalf("first tool call survived rollback: count=%d", firstCallCount)
	}
	_, persisted, err := store.GetThreadWithMessages(target.ID)
	if err != nil {
		t.Fatalf("read target history: %v", err)
	}
	if len(persisted) != 1 || persisted[0].Role != openai.ChatMessageRoleUser {
		t.Fatalf("partial multi-call insert left an assistant row: %+v", persisted)
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, target.ID, "", "web"))); err != nil {
		t.Fatalf("target thread is not safely resumable after rollback: %v", err)
	}
}

func TestAgentStreamAndPersistError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"server error"}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error from failed stream")
	}
}

func TestAgentOnEventErrorDuringToolCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.MaxTurns = 5

	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger tool",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "tool_call" {
			return fmt.Errorf("tool call rejected")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected error from onEvent")
	}
	_, persisted, readErr := store.GetThreadWithMessages(threadID)
	if readErr != nil {
		t.Fatalf("read persisted history: %v", readErr)
	}
	if len(persisted) != 3 || persisted[2].Role != openai.ChatMessageRoleTool || persisted[2].ToolCallID == nil || *persisted[2].ToolCallID != "call_1" {
		t.Fatalf("callback failure left an incomplete tool group: %+v", persisted)
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, threadID, "./workspace", "web"))); err != nil {
		t.Fatalf("callback-failed thread cannot be safely resumed: %v", err)
	}
}

func TestAgentOnEventErrorDuringProcessing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.MaxTurns = 5

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger tool",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "intermediate" {
			return fmt.Errorf("processing rejected")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected error from onEvent")
	}
}

func TestAgentStreamErrorEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"3\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	// Force streaming path by making the first complete call not return tool calls.
	// Since supportsTools=false, it goes straight to streaming.
	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
}

func TestTruncateMessagesDrops(t *testing.T) {
	// Create many messages with a very small context budget to force dropping.
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("a", 1000)},
		{Role: openai.ChatMessageRoleAssistant, Content: strings.Repeat("b", 1000)},
		{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("c", 1000)},
		{Role: openai.ChatMessageRoleAssistant, Content: strings.Repeat("d", 1000)},
		{Role: openai.ChatMessageRoleUser, Content: "final"},
	}
	result := truncateMessages(msgs, 50)
	if len(result) >= len(msgs) {
		t.Fatalf("expected messages to be dropped, got %d", len(result))
	}
	// System message should be preserved.
	if result[0].Role != openai.ChatMessageRoleSystem {
		t.Fatal("system message should be preserved")
	}
	// Last message should be preserved.
	if result[len(result)-1].Content != "final" {
		t.Fatal("last message should be preserved")
	}
}

func TestIsRetryableLLMError(t *testing.T) {
	timeoutErr := &net.DNSError{IsTimeout: true, Name: "provider.example"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", &openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}, true},
		{"500", &openai.APIError{HTTPStatusCode: 500, Message: "server error"}, true},
		{"wrapped 503", fmt.Errorf("complete: %w", &openai.APIError{HTTPStatusCode: 503, Message: "unavailable"}), true},
		{"401", &openai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}, false},
		{"400 request error", &openai.RequestError{HTTPStatusCode: 400, Err: errors.New("bad request")}, false},
		{"500 request error", &openai.RequestError{HTTPStatusCode: 500, Err: errors.New("boom")}, true},
		{"context canceled", context.Canceled, false},
		{"wrapped context canceled", fmt.Errorf("complete: %w", context.Canceled), false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"net timeout", timeoutErr, true},
		{"plain error", errors.New("empty response from model"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableLLMError(tc.err); got != tc.want {
				t.Errorf("isRetryableLLMError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// shrinkLLMRetryBackoff keeps retry tests fast by replacing the production
// backoff schedule for the duration of the test.
func shrinkLLMRetryBackoff(t *testing.T) {
	t.Helper()
	old := llmRetryBackoff
	llmRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { llmRetryBackoff = old })
}

// shrinkRateLimitRetryBackoff keeps the rate-limit schedule (five waits, six
// attempts) but shortens each wait so 429 tests run quickly.
func shrinkRateLimitRetryBackoff(t *testing.T) {
	t.Helper()
	old := rateLimitRetryBackoff
	rateLimitRetryBackoff = []time.Duration{
		time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond,
	}
	t.Cleanup(func() { rateLimitRetryBackoff = old })
}

func TestAgentRetriesTransientStreamError(t *testing.T) {
	shrinkLLMRetryBackoff(t)
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"provider overloaded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"recovered\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	var events []llm.StreamEvent
	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if callCount != 3 {
		t.Fatalf("provider calls: got %d, want 3", callCount)
	}
	var retries int
	var recovered bool
	for _, ev := range events {
		if ev.Type == "intermediate" && strings.Contains(ev.Content, "retrying after transient provider error") {
			retries++
		}
		if ev.Type == "token" && ev.Content == "recovered" {
			recovered = true
		}
	}
	if retries != 2 {
		t.Errorf("retry status events: got %d, want 2 (%v)", retries, events)
	}
	if !recovered {
		t.Errorf("missing recovered token after retries: %v", events)
	}
}

func TestAgentRetryCapsRateLimitAttempts(t *testing.T) {
	shrinkRateLimitRetryBackoff(t)
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// Six attempts total (rateLimitRetryBackoff has five waits), one request
	// each — a 429 must NOT trigger the streaming fallback.
	if callCount != 6 {
		t.Fatalf("provider calls: got %d, want 6", callCount)
	}
}

func TestAgentDoesNotRetryContextCanceled(t *testing.T) {
	shrinkLLMRetryBackoff(t)
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"unreachable"}}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := agent.Chat(ctx, Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   "./workspace",
	}, func(ev llm.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if callCount != 0 {
		t.Fatalf("provider was called %d times for a canceled context, want 0", callCount)
	}
}

func TestAgentRetriesTransientCompleteError(t *testing.T) {
	shrinkLLMRetryBackoff(t)
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// client.Complete tries non-streaming first and falls back to
		// streaming; both must fail before the agent-level retry kicks in.
		if callCount <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"provider overloaded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"recovered answer"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"

	var events []llm.StreamEvent
	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
		Workspace:   t.TempDir(),
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if callCount != 3 {
		t.Fatalf("provider calls: got %d, want 3 (failed non-streaming + failed streaming fallback + recovered)", callCount)
	}
	var hasRetryStatus bool
	for _, ev := range events {
		if ev.Type == "intermediate" && strings.Contains(ev.Content, "retrying after transient provider error") {
			hasRetryStatus = true
		}
	}
	if !hasRetryStatus {
		t.Errorf("missing retry status event: %v", events)
	}
}

func TestSpawnSubagentErrorEventCarriesPartialOutput(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial work\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"mid-stream boom\",\"type\":\"server_error\"}}\n\n")
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"

	events, err := agent.SpawnSubagent(context.Background(), "inspect", "test context")
	if err != nil {
		t.Fatalf("spawn sub-agent: %v", err)
	}
	var errEvent *tools.SubagentEvent
	for ev := range events {
		if ev.Type == "done" {
			t.Fatal("failing sub-agent unexpectedly emitted done")
		}
		if ev.Type == "error" {
			e := ev
			errEvent = &e
		}
	}
	if errEvent == nil {
		t.Fatal("missing error event")
	}
	if !strings.Contains(errEvent.Content, "mid-stream boom") {
		t.Errorf("error event content: got %q", errEvent.Content)
	}
	if errEvent.Partial != "partial work" {
		t.Errorf("error event partial: got %q, want %q", errEvent.Partial, "partial work")
	}
}

func TestSpawnSubagentCancelWithoutReaderClosesChannel(t *testing.T) {
	// Enough chunks to overflow the buffered event channel while nobody reads.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 128; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"%d\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"

	ctx, cancel := context.WithCancel(context.Background())
	events, err := agent.SpawnSubagent(ctx, "inspect", "test context")
	if err != nil {
		t.Fatalf("spawn sub-agent: %v", err)
	}

	// Read nothing: once the 64-event buffer fills, the sub-agent goroutine
	// blocks on send and only the ctx.Done guard can release it.
	time.Sleep(100 * time.Millisecond)
	cancel()

	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // channel closed: the goroutine exited instead of leaking
			}
		case <-timeout:
			t.Fatal("subagent events channel never closed after cancel; goroutine leaked")
		}
	}
}

func TestSubagentResumePreservesContext(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if callCount == 1 {
			// First call: return a tool call for file_read.
			fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"sub_call_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"hello.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			// Subsequent calls: return terminal text.
			fmt.Fprint(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"initial run completed"},"finish_reason":"stop"}]}`)
		}
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"
	agent.cfg.Subagent.MaxTurns = 10

	if err := os.WriteFile(agent.cfg.Workspace+"/hello.txt", []byte("world"), 0644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	// Phase 1: initial spawn with task_id.
	taskID := "test-subagent-resume-1"
	ctx := tools.WithSubagentTaskID(context.Background(), taskID)

	events, err := agent.SpawnSubagent(ctx, "inspect", "context data")
	if err != nil {
		t.Fatalf("spawn subagent: %v", err)
	}

	var sawToolResult, sawDone bool
	var firstRunResult string
	for ev := range events {
		if ev.Type == "tool_result" && ev.Tool == "file_read" {
			sawToolResult = true
		}
		if ev.Type == "done" {
			sawDone = true
			firstRunResult = ev.Content
		}
		if ev.Type == "error" {
			t.Fatalf("subagent error: %s (partial: %s)", ev.Content, ev.Partial)
		}
	}

	if !sawToolResult {
		t.Fatal("subagent did not execute file_read tool")
	}
	if !sawDone {
		t.Fatal("subagent did not emit done event")
	}
	if !strings.Contains(firstRunResult, "initial run completed") {
		t.Logf("first run result: %q", firstRunResult)
	}
	_ = firstRunResult

	// Verify DB has persisted messages and turn=1.
	var dbMessagesJSON, dbStatus string
	var dbTurn int
	err = store.DB().QueryRow(
		`SELECT messages_json, status, turn FROM subagent_tasks WHERE id = ?`, taskID,
	).Scan(&dbMessagesJSON, &dbStatus, &dbTurn)
	if err != nil {
		t.Fatalf("read subagent task from db: %v", err)
	}
	if dbStatus != "completed" {
		t.Fatalf("status: got %q, want completed", dbStatus)
	}
	if dbTurn != 2 {
		t.Fatalf("turn: got %d, want 2 (tool turn + terminal turn)", dbTurn)
	}

	var dbMsgs []openai.ChatCompletionMessage
	if err := json.Unmarshal([]byte(dbMessagesJSON), &dbMsgs); err != nil {
		t.Fatalf("deserialize db messages: %v", err)
	}
	// system + user + assistant(tool_calls) + tool(result) + final assistant text.
	if len(dbMsgs) != 5 {
		t.Fatalf("persisted messages: got %d, want 5 (system+user+assistant+tool+final assistant)", len(dbMsgs))
	}
	if dbMsgs[2].Role != "assistant" {
		t.Errorf("db msg 2 role: got %q, want assistant (with tool_calls)", dbMsgs[2].Role)
	}
	if dbMsgs[3].Role != "tool" {
		t.Errorf("db msg 3 role: got %q, want tool (result)", dbMsgs[3].Role)
	}
	if dbMsgs[4].Role != "assistant" || dbMsgs[4].Content == "" {
		t.Errorf("db msg 4: got role=%q content=%q, want final assistant text", dbMsgs[4].Role, dbMsgs[4].Content)
	}

	// Phase 2: simulate an interruption, then resume.
	updateTaskStatus(t, store, taskID, "interrupted")

	events, err = agent.ResumeSubagent(context.Background(), taskID)
	if err != nil {
		t.Fatalf("resume subagent: %v", err)
	}

	var finalContent string
	for ev := range events {
		if ev.Type == "token" {
			finalContent += ev.Content
		}
		if ev.Type == "done" {
			finalContent = ev.Content
		}
		if ev.Type == "error" {
			t.Fatalf("resume error: %s", ev.Content)
		}
	}

	if !strings.Contains(finalContent, "completed") && !strings.Contains(finalContent, "resume") {
		t.Fatalf("resume output: got %q, want completion text containing 'completed'", finalContent)
	}

	// Verify final DB state: completed, turn=2, messages grown.
	err = store.DB().QueryRow(
		`SELECT status, turn FROM subagent_tasks WHERE id = ?`, taskID,
	).Scan(&dbStatus, &dbTurn)
	if err != nil {
		t.Fatalf("read final subagent task: %v", err)
	}
	if dbStatus != "completed" {
		t.Fatalf("final status: got %q, want completed", dbStatus)
	}
	if dbTurn != 3 {
		t.Fatalf("final turn: got %d, want 3 (2 initial + 1 resume)", dbTurn)
	}

	err = store.DB().QueryRow(
		`SELECT messages_json FROM subagent_tasks WHERE id = ?`, taskID,
	).Scan(&dbMessagesJSON)
	if err != nil {
		t.Fatalf("read final messages_json: %v", err)
	}
	if err := json.Unmarshal([]byte(dbMessagesJSON), &dbMsgs); err != nil {
		t.Fatalf("deserialize final messages: %v", err)
	}
	if len(dbMsgs) < 4 {
		t.Fatalf("final messages: got %d, want >= 4 (prior messages preserved)", len(dbMsgs))
	}
	// The terminal response from the resume is not appended to messages
	// (it lives in the partial builder). At minimum the prior context is intact.
	if dbMsgs[0].Role != "system" {
		t.Errorf("final msg 0 role: got %q, want system", dbMsgs[0].Role)
	}
	if dbMsgs[2].Role != "assistant" {
		t.Errorf("final msg 2 role: got %q, want assistant (tool calls)", dbMsgs[2].Role)
	}
	if dbMsgs[3].Role != "tool" {
		t.Errorf("final msg 3 role: got %q, want tool (result)", dbMsgs[3].Role)
	}
}

func updateTaskStatus(t *testing.T, store *memory.Store, taskID, status string) {
	now := time.Now().Unix()
	_, err := store.DB().Exec(
		`UPDATE subagent_tasks SET status = ?, updated_at = ? WHERE id = ?`,
		status, now, taskID,
	)
	if err != nil {
		t.Fatalf("update task status: %v", err)
	}
}

func TestValidateEffort(t *testing.T) {
	for _, ok := range []string{"", "low", "medium", "high"} {
		if err := ValidateEffort(ok); err != nil {
			t.Errorf("effort %q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"ultra", "LOW", "max", "1"} {
		if err := ValidateEffort(bad); err == nil {
			t.Errorf("effort %q should be rejected", bad)
		}
	}
}

func TestBuildMessagesPlanModeDirective(t *testing.T) {
	a := newTestAgentWithFakeSummarizer(t, &fakeSummarizer{})
	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := a.store.AppendMessage(thread.ID, "user", strPtr("plan the thing"), nil); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	msgs, err := a.buildMessages(thread.ID, a.cfg.Workspace, "cli", true, nil)
	if err != nil {
		t.Fatalf("buildMessages: %v", err)
	}
	if msgs[0].Msg.Role != openai.ChatMessageRoleSystem || !strings.Contains(msgs[0].Msg.Content, "PLAN MODE") {
		t.Fatalf("plan-mode directive missing from system prompt: %q", msgs[0].Msg.Content)
	}
	plain, err := a.buildMessages(thread.ID, a.cfg.Workspace, "cli", false, nil)
	if err != nil {
		t.Fatalf("buildMessages plain: %v", err)
	}
	if strings.Contains(plain[0].Msg.Content, "PLAN MODE") {
		t.Fatal("plan-mode directive leaked into a normal turn")
	}
}
