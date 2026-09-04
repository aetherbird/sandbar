package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/tools"
)

type scriptedCompressionSummarizer struct {
	contents []string
	calls    int
}

func (s *scriptedCompressionSummarizer) Summarize(_ context.Context, _ CompressionSummaryRequest) (*CompressionSummaryResult, error) {
	if s.calls >= len(s.contents) {
		return nil, fmt.Errorf("unexpected compression call %d", s.calls+1)
	}
	content := s.contents[s.calls]
	s.calls++
	return &CompressionSummaryResult{
		Content:          content,
		PromptTokens:     20,
		CompletionTokens: 5,
		TotalTokens:      25,
	}, nil
}

type scriptedCompressionSummarizerFactory struct {
	s *scriptedCompressionSummarizer
}

func (f *scriptedCompressionSummarizerFactory) NewSummarizer(_ llm.ResolvedModel) Summarizer {
	return f.s
}

type blockingSubagentRunner struct {
	started chan string
	release <-chan struct{}
}

func (r blockingSubagentRunner) SpawnSubagent(ctx context.Context, goal, _ string) (<-chan tools.SubagentEvent, error) {
	r.started <- goal
	ch := make(chan tools.SubagentEvent, 2)
	go func() {
		defer close(ch)
		select {
		case <-r.release:
			ch <- tools.SubagentEvent{Type: "token", Content: goal + " complete"}
			ch <- tools.SubagentEvent{Type: "done", Content: goal + " complete"}
		case <-ctx.Done():
			ch <- tools.SubagentEvent{Type: "error", Content: ctx.Err().Error()}
		}
	}()
	return ch, nil
}

func (r blockingSubagentRunner) ResumeSubagent(ctx context.Context, taskID string) (<-chan tools.SubagentEvent, error) {
	r.started <- taskID
	ch := make(chan tools.SubagentEvent, 2)
	go func() {
		defer close(ch)
		select {
		case <-r.release:
			ch <- tools.SubagentEvent{Type: "token", Content: taskID + " resumed"}
			ch <- tools.SubagentEvent{Type: "done", Content: taskID + " resumed"}
		case <-ctx.Done():
			ch <- tools.SubagentEvent{Type: "error", Content: ctx.Err().Error()}
		}
	}()
	return ch, nil
}

type countingSubagentRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *countingSubagentRunner) SpawnSubagent(context.Context, string, string) (<-chan tools.SubagentEvent, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return nil, errors.New("unexpected subagent spawn")
}

func (r *countingSubagentRunner) ResumeSubagent(context.Context, string) (<-chan tools.SubagentEvent, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return nil, errors.New("unexpected subagent resume")
}

func (r *countingSubagentRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// instantSubagentRunner completes every spawn immediately with canned output.
type instantSubagentRunner struct{}

func (instantSubagentRunner) SpawnSubagent(_ context.Context, goal, _ string) (<-chan tools.SubagentEvent, error) {
	ch := make(chan tools.SubagentEvent, 1)
	ch <- tools.SubagentEvent{Type: "done", Content: "result for " + goal}
	close(ch)
	return ch, nil
}

func (instantSubagentRunner) ResumeSubagent(_ context.Context, taskID string) (<-chan tools.SubagentEvent, error) {
	ch := make(chan tools.SubagentEvent, 1)
	ch <- tools.SubagentEvent{Type: "done", Content: "resumed " + taskID}
	close(ch)
	return ch, nil
}

func TestAgentWithoutSubagentsOmitsSchemasAndFailsClosed(t *testing.T) {
	tests := []struct {
		name          string
		firstResponse string
	}{
		{
			name:          "structured tool call",
			firstResponse: `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"delegate_native","type":"function","function":{"name":"delegate_task","arguments":"{\"goal\":\"must not run\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name:          "text embedded tool call",
			firstResponse: `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"<function=delegate_task>\n<parameter=goal>\nmust not run\n</parameter>\n</function>"},"finish_reason":"stop"}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			callCount := 0
			var firstPrompt string
			var firstToolNames []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				callCount++
				var body struct {
					Messages []openai.ChatCompletionMessage `json:"messages"`
					Tools    []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if callCount == 1 {
					if len(body.Messages) > 0 {
						firstPrompt = body.Messages[0].Content
					}
					for _, schema := range body.Tools {
						firstToolNames = append(firstToolNames, schema.Function.Name)
					}
				}
				if callCount == 1 {
					respondJSONStreamAware(w, body.Stream, test.firstResponse)
					return
				}
				respondJSONStreamAware(w, body.Stream, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			}))
			defer ts.Close()

			agent, _, cleanup := setupTestAgent(t, true)
			defer cleanup()
			agent.cfg.Providers[0].BaseURL = ts.URL
			agent.cfg.Persona.TitleModel = "missing-title-model"
			workspace := t.TempDir()
			restricted := tools.NewRegistry(workspace, "", "", nil, tools.WithoutSubagents())
			defer func() { _ = restricted.Close(context.Background()) }()
			runner := &countingSubagentRunner{}
			restricted.SetSubagentRunner(runner)
			agent.tools = restricted

			var events []llm.StreamEvent
			_, err := agent.Chat(context.Background(), Request{
				ModelAlias:  "test-model",
				UserMessage: "attempt delegation",
				Workspace:   workspace,
			}, func(event llm.StreamEvent) error {
				events = append(events, event)
				return nil
			})
			if err != nil {
				t.Fatalf("chat: %v", err)
			}
			if runner.count() != 0 {
				t.Fatalf("disabled subagent runner was invoked %d times", runner.count())
			}
			if !strings.Contains(firstPrompt, subagentsUnavailablePrompt) {
				t.Fatalf("system prompt lacks runtime capability notice: %q", firstPrompt)
			}
			for _, name := range firstToolNames {
				if name == "delegate_task" || name == "resume_task" {
					t.Fatalf("disabled schema %q was advertised: %v", name, firstToolNames)
				}
			}

			var sawCall, sawUnknownResult bool
			for _, event := range events {
				if event.Type == "tool_call" && event.ToolName == "delegate_task" {
					sawCall = true
				}
				if event.Type == "tool_result" && event.ToolName == "delegate_task" && strings.Contains(event.Content, "unknown tool") {
					sawUnknownResult = true
				}
				if strings.HasPrefix(event.Type, "subagent_") {
					t.Fatalf("disabled call emitted subagent lifecycle event: %+v", event)
				}
			}
			if !sawCall || !sawUnknownResult {
				t.Fatalf("disabled attempt was not observably rejected: %+v", events)
			}
		})
	}
}

func TestAgentDefaultRegistryDoesNotAddSubagentUnavailableNotice(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	thread, err := store.CreateThreadWithWorkspace(nil, nil, t.TempDir())
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	messages := mustBuildMessages(t, agent, thread.ID, thread.Workspace, "cli")
	if len(messages) == 0 {
		t.Fatal("buildMessages returned no system prompt")
	}
	if strings.Contains(messages[0].Msg.Content, subagentsUnavailablePrompt) {
		t.Fatalf("default runtime received disabled capability notice: %q", messages[0].Msg.Content)
	}
}

func TestAgentToolCapableTerminalResponseUsesSingleRequest(t *testing.T) {
	callCount := 0
	var requestHadTools bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body struct {
			Tools  []json.RawMessage `json:"tools"`
			Stream bool              `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requestHadTools = len(body.Tools) > 0
		respondJSONStreamAware(w, body.Stream, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"terminal answer"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	// Prevent background title generation from adding an unrelated request.
	agent.cfg.Persona.TitleModel = "missing-title-model"

	var events []llm.StreamEvent
	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "answer without a tool",
		Workspace:   t.TempDir(),
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("provider requests: got %d, want 1", callCount)
	}
	if !requestHadTools {
		t.Fatal("tool-capable request did not include tool schemas")
	}

	var tokenContent string
	var hasDone bool
	for _, ev := range events {
		switch ev.Type {
		case "token":
			tokenContent += ev.Content
		case "done":
			hasDone = true
		}
	}
	if tokenContent != "terminal answer" {
		t.Fatalf("terminal content: got %q, want %q", tokenContent, "terminal answer")
	}
	if !hasDone {
		t.Fatalf("missing done event: %v", events)
	}

	_, messages, err := store.GetThreadWithMessages(threadID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 2 || messages[1].Role != "assistant" || messages[1].Content == nil || *messages[1].Content != "terminal answer" {
		t.Fatalf("terminal response was not persisted: %+v", messages)
	}
}

func TestAgentToolCallAndResult(t *testing.T) {
	// Prepare a workspace with a file.
	workspace := t.TempDir()
	os.WriteFile(filepath.Join(workspace, "hello.txt"), []byte("world"), 0644)

	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// Non-streaming tool call response.
			respondJSON(w, r, `{
				"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test",
				"choices":[{
					"index":0,"message":{
						"role":"assistant","content":"",
						"tool_calls":[{
							"id":"call_abc123",
							"type":"function",
							"function":{"name":"file_read","arguments":"{\"path\":\"hello.txt\"}"}
						}]
					},
					"finish_reason":"tool_calls"
				}]
			}`)
			return
		}
		// Second Complete() call is the terminal response.
		respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	var events []llm.StreamEvent
	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "read hello.txt",
		Workspace:   workspace,
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

	// Verify tool_call event.
	var hasToolCall, hasToolResult, hasDone bool
	var toolCallID string
	var terminalContent string
	for _, ev := range events {
		if ev.Type == "tool_call" && ev.ToolName == "file_read" {
			hasToolCall = true
			toolCallID = ev.ToolCallID
		}
		if ev.Type == "tool_result" && ev.ToolName == "file_read" {
			hasToolResult = true
			if !strings.HasPrefix(ev.Content, "[sha256: ") ||
				!strings.HasSuffix(ev.Content, "\n"+lineHashOf("world")+" world\n") {
				t.Errorf("tool result content: got %q, want SHA header and hashline-stamped world", ev.Content)
			}
		}
		if ev.Type == "token" {
			terminalContent += ev.Content
		}
		if ev.Type == "done" {
			hasDone = true
		}
	}
	if !hasToolCall {
		t.Errorf("missing tool_call event: %v", events)
	}
	if !hasToolResult {
		t.Errorf("missing tool_result event: %v", events)
	}
	if terminalContent != "Done" || !hasDone {
		t.Errorf("terminal completion missing: content=%q done=%v events=%v", terminalContent, hasDone, events)
	}
	if callCount != 2 {
		t.Errorf("provider requests: got %d, want 2", callCount)
	}

	// Verify persistence: role=tool message should have matching tool_call_id.
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

	// Expected: user, assistant (with tool call), tool result, final assistant.
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages (user, assistant, tool, assistant), got %d", len(messages))
	}
	if messages[2].Role != "tool" {
		t.Errorf("third message role: got %q, want tool", messages[2].Role)
	}
	if messages[2].ToolCallID == nil || *messages[2].ToolCallID != toolCallID {
		t.Errorf("tool message tool_call_id mismatch: got %v, want %s", messages[2].ToolCallID, toolCallID)
	}
	if messages[3].Role != "assistant" || messages[3].Content == nil || *messages[3].Content != "Done" {
		t.Errorf("final assistant message mismatch: %+v", messages[3])
	}
}

func TestAgentToolCallResultsPreserveProviderOrder(t *testing.T) {
	workspace := t.TempDir()
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("A"), 0644)
	os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("B"), 0644)

	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			respondJSON(w, r, `{
				"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test",
				"choices":[{
					"index":0,"message":{
						"role":"assistant","content":"",
						"tool_calls":[
							{"id":"call_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"a.txt\"}"}},
							{"id":"call_2","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"b.txt\"}"}}
						]
					},
					"finish_reason":"tool_calls"
				}]
			}`)
			return
		}
		respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	// Wrap file_read to record execution order.
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	var toolCallOrder []string
	threadID, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "read both files",
		Workspace:   workspace,
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "tool_result" {
			toolCallOrder = append(toolCallOrder, ev.ToolCallID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if threadID == "" {
		t.Errorf("expected thread ID, got empty")
	}

	if len(toolCallOrder) != 2 {
		t.Fatalf("expected 2 tool results, got %d", len(toolCallOrder))
	}
	if toolCallOrder[0] != "call_1" || toolCallOrder[1] != "call_2" {
		t.Errorf("tool execution order: got %v, want [call_1 call_2]", toolCallOrder)
	}

	threads, err := store.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	_, messages, err := store.GetThreadWithMessages(threads[0].ID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	// user, assistant, tool(call_1), tool(call_2), final assistant
	if len(messages) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(messages))
	}
	if messages[2].ToolCallID == nil || *messages[2].ToolCallID != "call_1" {
		t.Errorf("first tool message id mismatch")
	}
	if messages[3].ToolCallID == nil || *messages[3].ToolCallID != "call_2" {
		t.Errorf("second tool message id mismatch")
	}
	if messages[4].Role != "assistant" || messages[4].Content == nil || *messages[4].Content != "Done" {
		t.Errorf("final assistant message mismatch: %+v", messages[4])
	}
}

func TestAgentIndependentToolCallsRunConcurrently(t *testing.T) {
	workspace := t.TempDir()
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			respondJSON(w, r, `{
				"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test",
				"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"a.txt\"}"}},
					{"id":"call_2","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"b.txt\"}"}}
				]},"finish_reason":"tool_calls"}]}`)
			return
		}
		respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"

	started := make(chan string, 2)
	release := make(chan struct{})
	toolReg := tools.NewRegistry(workspace, "", "", nil)
	toolReg.Clear()
	toolReg.Register(tools.Tool{
		Name:         "file_read",
		ParallelSafe: true,
		Schema:       map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}, "required": []string{"path"}},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			path, _ := args["path"].(string)
			started <- path
			select {
			case <-release:
				return "read " + path, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	})
	agent.tools = toolReg

	var mu sync.Mutex
	var resultOrder []string
	done := make(chan error, 1)
	go func() {
		_, err := agent.Chat(context.Background(), Request{
			ModelAlias:  "test-model",
			UserMessage: "read both",
			Workspace:   workspace,
		}, func(ev llm.StreamEvent) error {
			if ev.Type == "tool_result" {
				mu.Lock()
				resultOrder = append(resultOrder, ev.ToolCallID)
				mu.Unlock()
			}
			return nil
		})
		done <- err
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			<-done
			t.Fatal("both independent handlers did not start before either was released")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(resultOrder, ","); got != "call_1,call_2" {
		t.Fatalf("result order: got %q, want provider order", got)
	}
}

func TestAgentDelegateTasksRunConcurrentlyAndTagProgress(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"delegate_1","type":"function","function":{"name":"delegate_task","arguments":"{\"goal\":\"first\"}"}},{"id":"delegate_2","type":"function","function":{"name":"delegate_task","arguments":"{\"goal\":\"second\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	started := make(chan string, 2)
	release := make(chan struct{})
	agent.tools.SetSubagentRunner(blockingSubagentRunner{started: started, release: release})
	workspace := t.TempDir()

	var mu sync.Mutex
	var progressIDs []string
	done := make(chan error, 1)
	go func() {
		_, err := agent.Chat(context.Background(), Request{
			ModelAlias:  "test-model",
			UserMessage: "delegate both",
			Workspace:   workspace,
		}, func(ev llm.StreamEvent) error {
			if ev.Type == "subagent_token" {
				mu.Lock()
				progressIDs = append(progressIDs, ev.ToolCallID)
				mu.Unlock()
			}
			return nil
		})
		done <- err
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			<-done
			t.Fatal("both delegate_task handlers did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("chat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(progressIDs) != 2 {
		t.Fatalf("subagent progress ids: got %v", progressIDs)
	}
	seen := map[string]bool{}
	for _, id := range progressIDs {
		seen[id] = true
	}
	if !seen["delegate_1"] || !seen["delegate_2"] {
		t.Fatalf("subagent progress was not tagged by parent call: %v", progressIDs)
	}
}

// TestTropicalVerifyNudgeFiresOnce pins the Step 4 gate: a tropical turn
// whose last tool round did direct work (not a fresh delegation) gets exactly
// one verification nudge round before completing; a tropical turn ending in a
// delegation round completes clean; a non-tropical turn never nudges.
func TestTropicalVerifyNudgeFiresOnce(t *testing.T) {
	// Round 0 mixes a delegation with direct work, so the last tool round is
	// NOT a fresh delegation round — the gate must fire exactly once.
	// (shell_exec is batch-sequential, and the batch path executes rounds
	// in provider order, so the trace is deterministic.)
	delegatePlusExec := `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"r1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo hi\"}"}},{"id":"d1","type":"function","function":{"name":"delegate_task","arguments":"{\"goal\":\"do work\"}"}}]},"finish_reason":"tool_calls"}]}`
	finalText := `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}]}`

	tests := []struct {
		name       string
		tropical   bool
		script     []string // provider responses in order; finalText serves once exhausted
		wantNudges int      // whether the verify prompt is injected (0/1)
	}{
		{
			// Mixed round, then terminal text: the terminal branch fires
			// the gate (last round was not delegation-only), the nudge
			// goes out, and the repeated terminal text completes.
			name:       "tropical direct finish gets one nudge",
			tropical:   true,
			script:     []string{delegatePlusExec},
			wantNudges: 1,
		},
		{
			name:     "tropical delegation finish needs no nudge",
			tropical: true,
			script: []string{
				`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"d1","type":"function","function":{"name":"delegate_task","arguments":"{\"goal\":\"review everything\"}"}}]},"finish_reason":"tool_calls"}]}`,
				finalText,
			},
			wantNudges: 0,
		},
		{
			name:       "non-tropical never nudges",
			tropical:   false,
			script:     []string{delegatePlusExec, finalText},
			wantNudges: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			callCount := 0
			nudges := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				// Count the nudge once: it persists in history, so every
				// subsequent request contains it. Only the first sighting is
				// the injection.
				if nudges == 0 && strings.Contains(string(body), "fresh review pass") {
					nudges = 1
				}
				resp := finalText
				if callCount < len(tc.script) {
					resp = tc.script[callCount]
				}
				callCount++
				mu.Unlock()
				respondJSONBody(w, body, resp)
			}))
			defer ts.Close()

			agent, _, cleanup := setupTestAgent(t, true)
			defer cleanup()
			agent.cfg.Providers[0].BaseURL = ts.URL
			agent.cfg.Persona.TitleModel = "missing-title-model"
			agent.tools.SetSubagentRunner(instantSubagentRunner{})
			workspace := t.TempDir()

			req := Request{ModelAlias: "test-model", UserMessage: "go", Workspace: workspace, Tropical: tc.tropical}
			if _, err := agent.Chat(context.Background(), req, func(llm.StreamEvent) error { return nil }); err != nil {
				t.Fatalf("chat: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if nudges != tc.wantNudges {
				t.Fatalf("verify nudges = %d, want %d", nudges, tc.wantNudges)
			}
		})
	}
}

func TestAgentCancelledConcurrentBatchPersistsEveryToolResult(t *testing.T) {
	workspace := t.TempDir()
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"cancel_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"a\"}"}},{"id":"cancel_2","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"b\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	started := make(chan struct{}, 2)
	toolReg := tools.NewRegistry(workspace, "", "", nil)
	toolReg.Clear()
	toolReg.Register(tools.Tool{
		Name:         "file_read",
		ParallelSafe: true,
		Schema:       map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}, "required": []string{"path"}},
		Execute: func(ctx context.Context, _ map[string]interface{}) (string, error) {
			started <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	agent.tools = toolReg

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var threadID string
	go func() {
		var err error
		threadID, err = agent.Chat(ctx, Request{ModelAlias: "test-model", UserMessage: "cancel", Workspace: workspace}, func(llm.StreamEvent) error { return nil })
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
			t.Fatal("concurrent calls did not both start")
		}
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("chat error: got %v, want context canceled", err)
	}

	_, messages, err := store.GetThreadWithMessages(threadID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages: got %d, want user + assistant + 2 tool results", len(messages))
	}
	for i, want := range []string{"cancel_1", "cancel_2"} {
		msg := messages[i+2]
		if msg.Role != "tool" || msg.ToolCallID == nil || *msg.ToolCallID != want {
			t.Errorf("tool result %d: %+v, want id %q", i, msg, want)
		}
	}
}

func TestSpawnSubagentUsesActiveWorkspaceAndAdvertisesFilteredTools(t *testing.T) {
	configuredWorkspace := t.TempDir()
	activeWorkspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(activeWorkspace, "active.txt"), []byte("active workspace"), 0o644); err != nil {
		t.Fatalf("write active fixture: %v", err)
	}

	callCount := 0
	var systemPrompt string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body struct {
			Messages []openai.ChatCompletionMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if callCount == 1 && len(body.Messages) > 0 {
			systemPrompt = body.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"sub_read","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"active.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"workspace confirmed"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = configuredWorkspace
	agent.cfg.Subagent.Model = "test-model"
	agent.cfg.Subagent.Tools = []string{"file_read"}
	agent.cfg.Subagent.MaxTurns = 3

	ctx := tools.WithWorkspace(context.Background(), activeWorkspace)
	events, err := agent.SpawnSubagent(ctx, "inspect the active file", "")
	if err != nil {
		t.Fatalf("spawn subagent: %v", err)
	}
	var toolOutput string
	for ev := range events {
		if ev.Type == "tool_result" {
			toolOutput = ev.Content
		}
	}

	if !strings.Contains(toolOutput, "active workspace") {
		t.Fatalf("subagent read from wrong workspace: %q", toolOutput)
	}
	if !strings.Contains(systemPrompt, "Available tools: file_read.") {
		t.Fatalf("system prompt does not reflect filtered registry: %q", systemPrompt)
	}
	if strings.Contains(systemPrompt, "search_files") {
		t.Fatalf("system prompt advertises unavailable tool: %q", systemPrompt)
	}
}

func TestExecuteToolDiscardsModelWorkspaceAndUsesTrustedContext(t *testing.T) {
	trustedWorkspace := t.TempDir()
	modelWorkspace := t.TempDir()
	registry := tools.NewRegistry(trustedWorkspace, "", "", nil)
	defer func() { _ = registry.Close(context.Background()) }()

	var gotWorkspace string
	var modelArgumentPresent bool
	registry.Register(tools.Tool{
		Name: "capture_workspace",
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			gotWorkspace = tools.WorkspaceFromContext(ctx)
			_, modelArgumentPresent = args["workspace"]
			return gotWorkspace, nil
		},
	})
	agent := &Agent{tools: registry}
	out, err := agent.executeTool(context.Background(), "capture_workspace", fmt.Sprintf(`{"workspace":%q}`, modelWorkspace), trustedWorkspace)
	if err != nil {
		t.Fatalf("execute tool: %v", err)
	}
	if out != trustedWorkspace || gotWorkspace != trustedWorkspace {
		t.Fatalf("workspace = %q (output %q), want trusted %q", gotWorkspace, out, trustedWorkspace)
	}
	if modelArgumentPresent {
		t.Fatal("model-supplied workspace reached the tool arguments")
	}
}

func TestDeleteThreadCancelsOwnedBackgroundJobs(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	defer func() { _ = agent.Close(context.Background()) }()
	workspace := t.TempDir()
	thread, err := store.CreateThreadWithWorkspace(nil, nil, workspace)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := agent.tools.SetJobSupervisorConfig(tools.JobSupervisorConfig{TerminationGrace: 20 * time.Millisecond}); err != nil {
		t.Fatalf("configure background jobs: %v", err)
	}
	ctx := tools.WithWorkspace(tools.WithThreadID(context.Background(), thread.ID), workspace)
	startedJSON, err := agent.tools.Execute(ctx, "shell_exec", map[string]interface{}{
		"command": "sleep 30", "async": true,
	})
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	var started tools.JobSnapshot
	if err := json.Unmarshal([]byte(startedJSON), &started); err != nil {
		t.Fatalf("decode job: %v", err)
	}

	deleteCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := agent.DeleteThread(deleteCtx, thread.ID); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	if _, err := store.GetThread(thread.ID); err == nil {
		t.Fatal("deleted thread remains in the store")
	}
	statusJSON, err := agent.tools.Execute(ctx, "job", map[string]interface{}{
		"action": "status", "job_id": started.JobID,
	})
	if err != nil {
		t.Fatalf("read cancelled job status: %v", err)
	}
	var status tools.JobSnapshot
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatalf("decode cancelled job: %v", err)
	}
	if status.State != tools.JobCancelled {
		t.Fatalf("job state after thread deletion = %q, want cancelled", status.State)
	}
}

func TestDeleteThreadRespectsContextWhileActiveTurnOwnsLock(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	thread, err := store.CreateThreadWithWorkspace(nil, nil, t.TempDir())
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	unlock, err := agent.lockThread(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("lock thread: %v", err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = agent.DeleteThread(ctx, thread.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete locked thread error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("delete ignored context while waiting for turn lock: %v", elapsed)
	}
	if _, err := store.GetThread(thread.ID); err != nil {
		t.Fatalf("context-cancelled delete removed thread: %v", err)
	}
}

func TestAgentStreamingFallbackPreservesToolsAndExecutesNativeCall(t *testing.T) {
	workspace := t.TempDir()
	callCount := 0
	var requestTools []int
	var requestStreams []bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body struct {
			Tools  []json.RawMessage `json:"tools"`
			Stream bool              `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request %d: %v", callCount, err)
		}
		requestTools = append(requestTools, len(body.Tools))
		requestStreams = append(requestStreams, body.Stream)

		switch callCount {
		case 1:
			// Force CompleteWithOptionsLive to fall back: the live streaming
			// attempt is rejected with the documented "streaming required" 4xx,
			// so the buffered fallback must carry the tools.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"streaming required","type":"invalid_request_error"}}`)
		case 2:
			// Buffered fallback: native tool call as plain JSON.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_stream","type":"function","function":{"name":"file_write","arguments":"{\"path\":\"streamed.txt\",\"content\":\"hello\",\"expected_sha256\":\"absent\"}"}}]},"finish_reason":"tool_calls"}]}`)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	var events []llm.StreamEvent
	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "write streamed.txt",
		Workspace:   workspace,
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if callCount != 3 {
		t.Fatalf("provider requests: got %d, want 3", callCount)
	}
	if len(requestTools) != 3 || requestTools[0] == 0 || requestTools[1] == 0 || requestTools[2] == 0 {
		t.Fatalf("tool schemas were not preserved on every agent-loop request: %v", requestTools)
	}
	if len(requestStreams) != 3 || !requestStreams[0] || requestStreams[1] || !requestStreams[2] {
		t.Fatalf("unexpected request modes: %v", requestStreams)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "streamed.txt"))
	if err != nil {
		t.Fatalf("read tool output: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("tool output: got %q, want hello", got)
	}

	var hasCall, hasResult, hasDone bool
	var terminalContent string
	for _, ev := range events {
		switch ev.Type {
		case "tool_call":
			hasCall = hasCall || (ev.ToolCallID == "call_stream" && ev.ToolName == "file_write")
		case "tool_result":
			hasResult = hasResult || (ev.ToolCallID == "call_stream" && ev.ToolName == "file_write")
		case "token":
			terminalContent += ev.Content
		case "done":
			hasDone = true
		}
	}
	if !hasCall || !hasResult || !hasDone || terminalContent != "Done" {
		t.Fatalf("incomplete agent events: call=%v result=%v done=%v content=%q events=%v", hasCall, hasResult, hasDone, terminalContent, events)
	}
}

func TestAgentExecutesObservedHaikuFunctionCallsInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	callCount := 0
	toolMarkup := `<function_calls>
<invoke name="bash">
<parameter name="tool">bash</parameter>
<parameter name="arguments">
<parameter name="command">printf &quot;%s&quot; &apos;hello &amp; goodbye&apos; &gt; observed.txt</parameter>
</invoke>
</function_calls>`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			respondJSON(w, r, fmt.Sprintf(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, toolMarkup))
			return
		}
		respondJSON(w, r, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	agent, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Persona.TitleModel = "missing-title-model"
	agent.tools = tools.NewRegistry(workspace, "", "", nil)

	var events []llm.StreamEvent
	_, err := agent.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "create observed.txt",
		Workspace:   workspace,
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("provider requests: got %d, want 2", callCount)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "observed.txt"))
	if err != nil {
		t.Fatalf("read observed tool output: %v", err)
	}
	if string(got) != "hello & goodbye" {
		t.Fatalf("observed tool output: got %q", got)
	}

	var hasShellCall, hasShellResult bool
	for _, ev := range events {
		if ev.Type == "tool_call" && ev.ToolName == "shell_exec" {
			hasShellCall = true
			var args map[string]string
			if err := json.Unmarshal(ev.Arguments, &args); err != nil {
				t.Fatalf("tool event arguments are not JSON: %v", err)
			}
			if len(args) != 1 || !strings.Contains(args["command"], "observed.txt") {
				t.Fatalf("unsafe or incomplete tool arguments: %#v", args)
			}
		}
		if ev.Type == "tool_result" && ev.ToolName == "shell_exec" {
			hasShellResult = true
		}
	}
	if !hasShellCall || !hasShellResult {
		t.Fatalf("observed wrapper did not execute through normal tool events: %v", events)
	}
}

func TestAgentRepeatedMidLoopCompressionPreservesIndexedStateAndRawHistory(t *testing.T) {
	workspace := t.TempDir()
	fileA := strings.Repeat("alpha-0001 beta-0002 gamma-0003 delta-0004\n", 500)
	fileB := strings.Repeat("epsilon-0005 zeta-0006 eta-0007 theta-0008\n", 500)
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(fileA), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte(fileB), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	var requests [][]openai.ChatCompletionMessage
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []openai.ChatCompletionMessage `json:"messages"`
			Stream   bool                           `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		requests = append(requests, body.Messages)

		switch len(requests) {
		case 1:
			respondJSONStreamAware(w, body.Stream, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			respondJSONStreamAware(w, body.Stream, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_b","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"b.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		case 3:
			respondJSONStreamAware(w, body.Stream, `{"id":"chatcmpl-3","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
		default:
			http.Error(w, "unexpected provider request", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	a, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	// The runtime environment block in the system prompt (~40 tokens) adds to
	// the fixed prefix; the context must leave room for the checkpoint summary
	// on top of it, otherwise the mid-loop checkpoint falls back to truncation.
	ctxLen := 384
	modelCfg := a.cfg.Providers[0].Models["test-model"]
	modelCfg.ContextLength = &ctxLen
	a.cfg.Providers[0].Models["test-model"] = modelCfg
	a.cfg.Compression = config.CompressionConfig{
		Enabled:          true,
		Threshold:        0.80,
		TargetRatio:      0.20,
		MinSummaryTokens: 8,
		MaxSummaryTokens: 64,
		Model:            "test-model",
		TimeoutSeconds:   5,
	}
	a.registry = llm.NewRegistry(a.cfg)
	a.tools = tools.NewRegistry(workspace, "", "", nil)
	summarizer := &scriptedCompressionSummarizer{contents: []string{
		"checkpoint one " + strings.Repeat("preserved detail ", 50),
		"checkpoint two " + strings.Repeat("updated detail ", 50),
	}}
	a.summarizers = &scriptedCompressionSummarizerFactory{s: summarizer}

	var events []llm.StreamEvent
	threadID, err := a.Chat(context.Background(), Request{
		ModelAlias:  "test-model",
		UserMessage: "Read both files and report when finished.",
		Workspace:   workspace,
	}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("provider request count: got %d, want 3", len(requests))
	}
	if summarizer.calls != 2 {
		t.Fatalf("compression calls: got %d, want 2", summarizer.calls)
	}
	for i, request := range requests {
		if err := validateProviderPayload(request); err != nil {
			t.Fatalf("provider request %d is invalid: %v", i+1, err)
		}
	}
	requestText := func(messages []openai.ChatCompletionMessage) string {
		var b strings.Builder
		for _, message := range messages {
			b.WriteString(message.Content)
			b.WriteByte('\n')
		}
		return b.String()
	}
	second := requestText(requests[1])
	if strings.Count(second, activeTurnSummaryWrapper) != 1 || !strings.Contains(second, "checkpoint one") || strings.Contains(second, "checkpoint two") {
		t.Fatalf("second request did not contain exactly checkpoint one: %q", second)
	}
	third := requestText(requests[2])
	if strings.Count(third, activeTurnSummaryWrapper) != 1 || strings.Contains(third, "checkpoint one") || !strings.Contains(third, "checkpoint two") {
		t.Fatalf("third request did not replace checkpoint one with checkpoint two: %q", third)
	}

	var relevant []string
	var pendingStart *llm.CompressionEvent
	for _, event := range events {
		switch event.Type {
		case "tool_result", "compression_start", "compression_end", "compression_error", "auxiliary_usage":
			relevant = append(relevant, event.Type)
		}
		if event.Type == "compression_start" {
			if event.Compression == nil || event.Compression.BeforeTokens <= event.Compression.BudgetTokens {
				t.Fatalf("compression start did not describe the triggering pre-compression context: %+v", event.Compression)
			}
			if pendingStart != nil {
				t.Fatalf("compression start was not paired before the next start: %+v", event.Compression)
			}
			pendingStart = event.Compression
		}
		if event.Type == "compression_end" && (event.Compression == nil || event.Compression.SummaryCallCount != 1 || event.Compression.SummaryUsageCallCount != 1) {
			t.Fatalf("compression terminal call coverage: %+v", event.Compression)
		}
		if event.Type == "compression_end" {
			if pendingStart == nil || event.Compression.BeforeTokens != pendingStart.BeforeTokens || event.Compression.BudgetTokens != pendingStart.BudgetTokens {
				t.Fatalf("compression start/end measurement mismatch: start=%+v end=%+v", pendingStart, event.Compression)
			}
			pendingStart = nil
		}
		if event.Type == "auxiliary_usage" && event.UsagePurpose == "compression" && event.UsageCallCount != 1 {
			t.Fatalf("auxiliary usage call coverage: %+v", event)
		}
	}
	if pendingStart != nil {
		t.Fatalf("compression start had no terminal event: %+v", pendingStart)
	}
	wantEvents := []string{
		"tool_result", "compression_start", "compression_end", "auxiliary_usage",
		"tool_result", "compression_start", "compression_end", "auxiliary_usage",
	}
	if fmt.Sprint(relevant) != fmt.Sprint(wantEvents) {
		t.Fatalf("compression event order: got %v, want %v", relevant, wantEvents)
	}

	_, persisted, err := store.GetThreadWithMessages(threadID)
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant", "tool", "assistant"}
	if len(persisted) != len(wantRoles) {
		t.Fatalf("persisted message count: got %d, want %d", len(persisted), len(wantRoles))
	}
	for i, wantRole := range wantRoles {
		if persisted[i].Role != wantRole {
			t.Fatalf("persisted role %d: got %q, want %q", i, persisted[i].Role, wantRole)
		}
	}
	if persisted[2].Content == nil || !strings.Contains(*persisted[2].Content, "alpha-0001") ||
		persisted[4].Content == nil || !strings.Contains(*persisted[4].Content, "epsilon-0005") {
		t.Fatalf("raw tool output was not retained in persistence: %+v", persisted)
	}
	compression, err := store.GetLatestCompression(threadID)
	if err != nil {
		t.Fatalf("read compression record: %v", err)
	}
	if compression != nil {
		t.Fatalf("active-turn checkpoint was persisted as durable compression: %+v", compression)
	}
}

func TestAgentCompressionErrorEventIncludesDiagnosticAndAttemptMetadata(t *testing.T) {
	workspace := t.TempDir()
	large := strings.Repeat("alpha beta gamma delta\n", 1000)
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(large), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		callCount++
		// Tool-capable turns stream (CompleteWithOptionsLive): honor the
		// stream flag with SSE so each logical response is one round-trip.
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			delta := `{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Done"},"finish_reason":null}]}`
			if callCount == 1 {
				delta = `{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_large","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"large.txt\"}"}}]},"finish_reason":null}]}`
			}
			fmt.Fprintf(w, "data: %s\n\n", delta)
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_large","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"large.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	ctxLen := 256
	modelCfg := a.cfg.Providers[0].Models["test-model"]
	modelCfg.ContextLength = &ctxLen
	a.cfg.Providers[0].Models["test-model"] = modelCfg
	a.cfg.Compression = config.CompressionConfig{
		Enabled: true, Threshold: 0.80, TargetRatio: 0.20,
		MinSummaryTokens: 8, MaxSummaryTokens: 64, Model: "test-model", TimeoutSeconds: 5,
	}
	a.registry = llm.NewRegistry(a.cfg)
	a.tools = tools.NewRegistry(workspace, "", "", nil)
	a.summarizers = &fakeSummarizerFactory{s: &fakeSummarizer{returnErr: fmt.Errorf("summary unavailable")}}

	var events []llm.StreamEvent
	if _, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "read the large file", Workspace: workspace,
	}, func(event llm.StreamEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("chat: %v", err)
	}

	var starts, errorsSeen, auxiliary int
	for _, event := range events {
		switch event.Type {
		case "compression_start":
			starts++
		case "compression_error":
			errorsSeen++
			if event.Compression == nil || !strings.Contains(event.Compression.Error, "summary unavailable") {
				t.Fatalf("compression_error omitted diagnostic: %+v", event.Compression)
			}
			if !event.Compression.SummaryAttempted || event.Compression.Outcome != string(CompressionOutcomeError) {
				t.Fatalf("compression_error omitted attempt/outcome metadata: %+v", event.Compression)
			}
			if event.Compression.SummaryCallCount != 1 || event.Compression.SummaryUsageCallCount != 0 {
				t.Fatalf("compression_error call coverage: %+v", event.Compression)
			}
		case "auxiliary_usage":
			auxiliary++
		}
	}
	if starts != 1 || errorsSeen != 1 {
		t.Fatalf("compression start/error pairing: starts=%d errors=%d events=%v", starts, errorsSeen, events)
	}
	if auxiliary != 0 {
		t.Fatalf("unmeasured summary emitted measured auxiliary usage: %d", auxiliary)
	}
}

// orderingSummarizer fails its call unless the compression_start event already
// reached the event sink — pinning the event-order fix (start used to be
// emitted after the work completed, back-to-back with end).
type orderingSummarizer struct {
	startSeen *bool
	inner     Summarizer
}

func (o *orderingSummarizer) Summarize(ctx context.Context, req CompressionSummaryRequest) (*CompressionSummaryResult, error) {
	if !*o.startSeen {
		return nil, errors.New("compression_start was not emitted before the summarizer ran")
	}
	return o.inner.Summarize(ctx, req)
}

type orderingSummarizerFactory struct{ inner *orderingSummarizer }

func (f *orderingSummarizerFactory) NewSummarizer(_ llm.ResolvedModel) Summarizer { return f.inner }

// TestCompressionStartEmittedBeforeSummarizerRuns verifies the turn-start
// compression_start event reaches the sink before the summarizer is invoked,
// and that the terminal event carries the operation's elapsed time.
func TestCompressionStartEmittedBeforeSummarizerRuns(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	ctxLen := 256
	modelCfg := a.cfg.Providers[0].Models["test-model"]
	modelCfg.ContextLength = &ctxLen
	a.cfg.Providers[0].Models["test-model"] = modelCfg
	a.cfg.Compression = config.CompressionConfig{
		Enabled: true, Threshold: 0.80, TargetRatio: 0.20,
		MinSummaryTokens: 8, MaxSummaryTokens: 64, Model: "test-model", TimeoutSeconds: 5,
	}
	a.registry = llm.NewRegistry(a.cfg)
	workspace := t.TempDir()
	a.tools = tools.NewRegistry(workspace, "", "", nil)

	// Seed a thread whose history alone exceeds the budget, so turn-start
	// compression fires before any provider call.
	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	big := strings.Repeat("alpha beta gamma delta\n", 500)
	if _, err := a.store.AppendMessage(thread.ID, "user", &big, nil); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	if _, err := a.store.AppendMessage(thread.ID, "assistant", &big, nil); err != nil {
		t.Fatalf("seed assistant message: %v", err)
	}

	startSeen := false
	a.summarizers = &orderingSummarizerFactory{inner: &orderingSummarizer{
		startSeen: &startSeen,
		inner:     &fakeSummarizer{returnContent: "Ordering summary."},
	}}

	var events []llm.StreamEvent
	if _, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", ThreadID: thread.ID, UserMessage: "continue", Workspace: workspace,
	}, func(ev llm.StreamEvent) error {
		if ev.Type == "compression_start" {
			startSeen = true
		}
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("chat: %v", err)
	}

	// The summarizer itself would have failed the turn-start compression with
	// an ordering error (turning the outcome into a fallback), so a compressed
	// outcome here proves the start event preceded the summarizer call.
	var startIdx, endIdx = -1, -1
	for i, ev := range events {
		switch ev.Type {
		case "compression_start":
			startIdx = i
			if ev.Compression == nil || ev.Compression.BeforeTokens <= ev.Compression.BudgetTokens {
				t.Fatalf("start event did not carry the pre-work snapshot: %+v", ev.Compression)
			}
		case "compression_end":
			endIdx = i
			if ev.Compression == nil || ev.Compression.ElapsedMS < 0 {
				t.Fatalf("end event missing elapsed time: %+v", ev.Compression)
			}
		}
	}
	if startIdx < 0 || endIdx < 0 || startIdx > endIdx {
		t.Fatalf("compression event order: start=%d end=%d events=%v", startIdx, endIdx, events)
	}
}
