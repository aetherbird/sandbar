package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/tools"
)

func sanitizeCall(id string) openai.ToolCall {
	return openai.ToolCall{
		ID:       id,
		Type:     openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "file_read", Arguments: "{}"},
	}
}

func sanitizeAssistant(content string, callIDs ...string) openai.ChatCompletionMessage {
	msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content}
	for _, id := range callIDs {
		msg.ToolCalls = append(msg.ToolCalls, sanitizeCall(id))
	}
	return msg
}

func sanitizeToolMsg(callID, content string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: callID, Content: content}
}

func sanitizeUser(content string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content}
}

func TestSanitizeProviderMessages(t *testing.T) {
	tests := []struct {
		name string
		in   []openai.ChatCompletionMessage
		want []openai.ChatCompletionMessage
	}{
		{
			name: "clean history passes through",
			in: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: "sys"},
				sanitizeUser("u1"),
				sanitizeAssistant("a1"),
				sanitizeAssistant("working", "c1", "c2"),
				sanitizeToolMsg("c1", "out1"),
				sanitizeToolMsg("c2", "out2"),
				sanitizeUser("u2"),
			},
			want: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: "sys"},
				sanitizeUser("u1"),
				sanitizeAssistant("a1"),
				sanitizeAssistant("working", "c1", "c2"),
				sanitizeToolMsg("c1", "out1"),
				sanitizeToolMsg("c2", "out2"),
				sanitizeUser("u2"),
			},
		},
		{
			name: "dangling group mid-history is sealed in place",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1"),
				sanitizeUser("u2"),
				sanitizeAssistant("done"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1"),
				sanitizeToolMsg("c1", interruptedToolResult),
				sanitizeUser("u2"),
				sanitizeAssistant("done"),
			},
		},
		{
			name: "dangling tail is sealed at the end",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1", "c2"),
				sanitizeToolMsg("c1", "out1"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1", "c2"),
				sanitizeToolMsg("c1", "out1"),
				sanitizeToolMsg("c2", interruptedToolResult),
			},
		},
		{
			name: "parallel group partially answered keeps the real result",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1", "c2", "c3"),
				sanitizeToolMsg("c2", "out2"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1", "c2", "c3"),
				sanitizeToolMsg("c2", "out2"),
				sanitizeToolMsg("c1", interruptedToolResult),
				sanitizeToolMsg("c3", interruptedToolResult),
			},
		},
		{
			name: "orphan tool results are dropped",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeToolMsg("stray", "orphan"),
				sanitizeAssistant("", "c1"),
				sanitizeToolMsg("c1", "out1"),
				sanitizeToolMsg("unknown", "orphan"),
				sanitizeUser("u2"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1"),
				sanitizeToolMsg("c1", "out1"),
				sanitizeUser("u2"),
			},
		},
		{
			name: "duplicate tool result keeps the first",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1"),
				sanitizeToolMsg("c1", "first"),
				sanitizeToolMsg("c1", "second"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "c1"),
				sanitizeToolMsg("c1", "first"),
			},
		},
		{
			name: "empty assistant messages are dropped",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant(""),
				sanitizeAssistant("kept"),
				sanitizeUser("u2"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("kept"),
				sanitizeUser("u2"),
			},
		},
		{
			name: "interleaved groups repair independently",
			in: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "g1c1"),
				sanitizeToolMsg("g1c1", "out1"),
				sanitizeUser("u2"),
				sanitizeAssistant("", "g2c1", "g2c2"),
				sanitizeToolMsg("g2c1", "out1"),
				sanitizeUser("u3"),
			},
			want: []openai.ChatCompletionMessage{
				sanitizeUser("u1"),
				sanitizeAssistant("", "g1c1"),
				sanitizeToolMsg("g1c1", "out1"),
				sanitizeUser("u2"),
				sanitizeAssistant("", "g2c1", "g2c2"),
				sanitizeToolMsg("g2c1", "out1"),
				sanitizeToolMsg("g2c2", interruptedToolResult),
				sanitizeUser("u3"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The input must not be modified.
			frozen := append([]openai.ChatCompletionMessage(nil), tt.in...)
			got := sanitizeProviderMessages(tt.in)
			if !reflect.DeepEqual(tt.in, frozen) {
				t.Fatalf("input was mutated: got %+v, want %+v", tt.in, frozen)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("sanitizeProviderMessages:\n got %+v\nwant %+v", got, tt.want)
			}
			if err := validateProviderPayload(got); err != nil {
				t.Fatalf("sanitized output failed validation: %v", err)
			}
		})
	}
}

// TestAgentChatSerializesConcurrentTurnsOnSameThread runs two Chat calls on one
// thread concurrently and requires them to serialize: the persisted history
// must show two complete, non-interleaved turns and stay a valid payload.
func TestAgentChatSerializesConcurrentTurnsOnSameThread(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		// Widen the window in which unsynchronized turns would interleave.
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if n%2 == 1 {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"printf out%d\"}"}}]},"finish_reason":"tool_calls"}]}`, n, n)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"turn %d done"},"finish_reason":"stop"}]}`, n)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	workspace := t.TempDir()
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, msg := range []string{"first", "second"} {
		wg.Add(1)
		go func(i int, msg string) {
			defer wg.Done()
			_, errs[i] = agent.Chat(context.Background(), Request{
				ThreadID:    thread.ID,
				ModelAlias:  "test-model",
				UserMessage: msg,
				Workspace:   workspace,
			}, func(llm.StreamEvent) error { return nil })
		}(i, msg)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}

	_, persisted, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	if len(persisted) != 8 {
		t.Fatalf("persisted message count: got %d, want 8 (two serialized turns): %+v", len(persisted), persisted)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant", "user", "assistant", "tool", "assistant"}
	for i, want := range wantRoles {
		if persisted[i].Role != want {
			t.Fatalf("turn interleaving at message %d: got role %q, want %q (history: %+v)", i, persisted[i].Role, want, persisted)
		}
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, thread.ID, workspace, "web"))); err != nil {
		t.Fatalf("concurrent turns left an invalid provider payload: %v", err)
	}
}

// TestBuildMessagesHealsTrailingToolCallGroup poisons a thread with crash
// residue (a trailing assistant tool-call group missing one result) and
// requires buildMessages to persist the interrupted result once.
func TestBuildMessagesHealsTrailingToolCallGroup(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	userMsg := "do work"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleUser, &userMsg, nil); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if _, err := agent.persistAssistantTurn(thread.ID, "", []openai.ToolCall{
		sanitizeCall("heal_call_1"),
		sanitizeCall("heal_call_2"),
	}); err != nil {
		t.Fatalf("persist assistant turn: %v", err)
	}
	// Crash residue: only the first call ever received a result.
	partial := "real output"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleTool, &partial, strPtr("heal_call_1")); err != nil {
		t.Fatalf("append partial result: %v", err)
	}

	msgs := mustBuildMessages(t, agent, thread.ID, "", "web")
	if err := validateProviderPayload(toRawMessages(msgs)); err != nil {
		t.Fatalf("healed view is not a valid provider payload: %v", err)
	}

	// The missing result is persisted exactly at the end of the thread.
	_, persisted, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	if len(persisted) != 4 {
		t.Fatalf("persisted history length: got %d, want user+assistant+2 tool results: %+v", len(persisted), persisted)
	}
	last := persisted[3]
	if last.Role != openai.ChatMessageRoleTool || last.ToolCallID == nil || *last.ToolCallID != "heal_call_2" {
		t.Fatalf("trailing seal missing: %+v", last)
	}
	if last.Content == nil || !strings.Contains(*last.Content, "interrupted") {
		t.Fatalf("trailing seal content: %v", last.Content)
	}

	// Healing is idempotent: a second load persists nothing new.
	if msgs2 := mustBuildMessages(t, agent, thread.ID, "", "web"); len(msgs2) != len(msgs) {
		t.Fatalf("second heal changed the view: %d vs %d messages", len(msgs2), len(msgs))
	}
	_, persisted2, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("re-read persisted history: %v", err)
	}
	if len(persisted2) != 4 {
		t.Fatalf("healing was not idempotent: %d persisted messages, want 4", len(persisted2))
	}
}

// TestBuildMessagesRepairsMidHistoryDanglingGroupInMemoryOnly poisons a thread
// with a dangling tool-call group in the middle of history and requires the
// repair to stay in-memory: the persisted sequence is never rewritten.
func TestBuildMessagesRepairsMidHistoryDanglingGroupInMemoryOnly(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	u1 := "first request"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleUser, &u1, nil); err != nil {
		t.Fatalf("append u1: %v", err)
	}
	if _, err := agent.persistAssistantTurn(thread.ID, "", []openai.ToolCall{sanitizeCall("mid_call_1")}); err != nil {
		t.Fatalf("persist dangling assistant turn: %v", err)
	}
	u2 := "second request"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleUser, &u2, nil); err != nil {
		t.Fatalf("append u2: %v", err)
	}
	answer := "final answer"
	if _, err := store.AppendMessage(thread.ID, openai.ChatMessageRoleAssistant, &answer, nil); err != nil {
		t.Fatalf("append final assistant: %v", err)
	}

	msgs := mustBuildMessages(t, agent, thread.ID, "", "web")
	if err := validateProviderPayload(toRawMessages(msgs)); err != nil {
		t.Fatalf("repaired view is not a valid provider payload: %v", err)
	}
	// system, u1, assistant(calls), synthetic seal, u2, final assistant.
	if len(msgs) != 6 {
		t.Fatalf("repaired view length: got %d, want 6: %+v", len(msgs), msgs)
	}
	seal := msgs[3]
	if seal.Msg.Role != openai.ChatMessageRoleTool || seal.Msg.ToolCallID != "mid_call_1" || !seal.Synthetic || seal.Seq != 0 {
		t.Fatalf("mid-history repair should be a synthetic in-memory seal: %+v", seal)
	}
	if seal.Msg.Content != interruptedToolResult {
		t.Fatalf("seal content: got %q, want %q", seal.Msg.Content, interruptedToolResult)
	}

	// The store is untouched: still user, assistant, user, assistant.
	_, persisted, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	if len(persisted) != 4 {
		t.Fatalf("mid-history repair rewrote persisted messages: %+v", persisted)
	}
}

// TestBuildMessagesFailsOnStoreError requires a DB load failure to surface as
// an error instead of silently degrading to a system-prompt-only view.
func TestBuildMessagesFailsOnStoreError(t *testing.T) {
	agent, store, _ := setupTestAgent(t, false)

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	store.Close() // force subsequent reads to fail

	if _, err := agent.buildMessages(thread.ID, "", "web", false, nil); err == nil {
		t.Fatal("expected an error when the store is unavailable")
	}
}

// TestStreamAndPersistCancelSealsPendingCalls cancels the streaming path after
// the first of two text-parsed tool calls completes; the second must still
// receive a durable interrupted result.
func TestStreamAndPersistCancelSealsPendingCalls(t *testing.T) {
	content := `<function_call>{"name":"shell_exec","arguments":{"command":"printf first"}}</function_call>` +
		`<function_call>{"name":"shell_exec","arguments":{"command":"printf second"}}</function_call>`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n", strconv.Quote(content))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, false)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	workspace := t.TempDir()
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	threadID, err := agent.Chat(ctx, Request{
		ModelAlias:  "test-model",
		UserMessage: "trigger streamed tools",
		Workspace:   workspace,
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "tool_result" {
			cancel() // interrupt before the second call starts
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
		t.Fatalf("persisted history length: got %d, want user+assistant+2 tool results: %+v", len(persisted), persisted)
	}
	if persisted[2].ToolCallID == nil || persisted[3].ToolCallID == nil || *persisted[2].ToolCallID == *persisted[3].ToolCallID {
		t.Fatalf("tool results do not close distinct calls: %+v", persisted)
	}
	if persisted[3].Content == nil || !strings.Contains(*persisted[3].Content, "interrupted") {
		t.Fatalf("unexecuted streamed call did not get a closing result: %+v", persisted[3])
	}
	if err := validateProviderPayload(toRawMessages(mustBuildMessages(t, agent, threadID, workspace, "web"))); err != nil {
		t.Fatalf("cancelled streamed thread cannot be safely resumed: %v", err)
	}
}
