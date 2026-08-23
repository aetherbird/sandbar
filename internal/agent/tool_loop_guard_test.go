package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/tools"
)

func testToolCall(id, name, arguments string) openai.ToolCall {
	return openai.ToolCall{
		ID:   id,
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      name,
			Arguments: arguments,
		},
	}
}

func TestToolLoopGuardIgnoresCallIDsAndJSONKeyOrder(t *testing.T) {
	var guard toolLoopGuard
	first := guard.Observe([]openai.ToolCall{testToolCall("call-1", "example", `{"a":1,"b":2}`)})
	second := guard.Observe([]openai.ToolCall{testToolCall("call-2", "example", `{ "b": 2, "a": 1 }`)})
	if first.Consecutive != 1 || second.Consecutive != 2 {
		t.Fatalf("equivalent calls were not counted together: first=%+v second=%+v", first, second)
	}
	if second.Skip || second.Abort {
		t.Fatalf("guard intervened before warning threshold: %+v", second)
	}

	changed := guard.Observe([]openai.ToolCall{testToolCall("call-3", "example", `{"a":2,"b":2}`)})
	if changed.Consecutive != 1 || changed.Skip || changed.Abort {
		t.Fatalf("changed arguments did not reset guard: %+v", changed)
	}
}

func TestAgentRepeatedIdenticalToolCallsWarnThenStop(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "same.txt"), []byte("same result"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"repeat-%d","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"same.txt\"}"}}]},"finish_reason":"tool_calls"}]}`, callCount)
	}))
	defer server.Close()

	a, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = server.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	a.cfg.MaxTurns = 0
	a.tools = tools.NewRegistry(workspace, "", "", nil)

	var events []llm.StreamEvent
	threadID, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "repeat forever", Workspace: workspace,
	}, func(event llm.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, ErrRepeatedToolCallLoop) {
		t.Fatalf("agent error: got %v, want repeated-call loop", err)
	}
	if callCount != repeatedToolCallAbortAt {
		t.Fatalf("provider calls: got %d, want %d", callCount, repeatedToolCallAbortAt)
	}

	var normalResults, warningResults, loopErrors int
	for _, event := range events {
		switch event.Type {
		case "tool_result":
			if strings.HasSuffix(event.Content, "same result\n") {
				normalResults++
			}
			if strings.Contains(event.Content, "repeated identical tool call") {
				warningResults++
			}
		case "error":
			if strings.Contains(event.Content, ErrRepeatedToolCallLoop.Error()) {
				loopErrors++
			}
		}
	}
	if normalResults != repeatedToolCallWarningAt-1 || warningResults != repeatedToolCallAbortAt-repeatedToolCallWarningAt+1 || loopErrors != 1 {
		t.Fatalf("unexpected loop events: normal=%d warning=%d errors=%d events=%v", normalResults, warningResults, loopErrors, events)
	}

	_, persisted, readErr := store.GetThreadWithMessages(threadID)
	if readErr != nil {
		t.Fatalf("read persisted history: %v", readErr)
	}
	if want := 1 + 2*repeatedToolCallAbortAt; len(persisted) != want {
		t.Fatalf("persisted messages: got %d, want %d", len(persisted), want)
	}
	if payloadErr := validateProviderPayload(toRawMessages(mustBuildMessages(t, a, threadID, workspace, "web"))); payloadErr != nil {
		t.Fatalf("loop termination left invalid provider history: %v", payloadErr)
	}
}

func TestAgentRepeatedToolCallWarningAllowsRecovery(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		switch callCount {
		case 1, 2, 3:
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"same-%d","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}`, callCount)
		case 4:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"changed","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"b.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`)
		}
	}))
	defer server.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = server.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	a.cfg.MaxTurns = 0
	a.tools = tools.NewRegistry(workspace, "", "", nil)

	var results []string
	var terminal string
	_, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "recover", Workspace: workspace,
	}, func(event llm.StreamEvent) error {
		if event.Type == "tool_result" {
			results = append(results, event.Content)
		}
		if event.Type == "token" {
			terminal += event.Content
		}
		return nil
	})
	if err != nil {
		t.Fatalf("agent did not recover: %v", err)
	}
	if callCount != 5 || terminal != "recovered" {
		t.Fatalf("completion: calls=%d terminal=%q", callCount, terminal)
	}
	if len(results) != 4 || !strings.HasSuffix(results[0], lineHashOf("A")+" A\n") || !strings.HasSuffix(results[1], lineHashOf("A")+" A\n") ||
		results[2] != repeatedToolCallResult(toolLoopDecision{Consecutive: 3, Skip: true}) || !strings.HasSuffix(results[3], lineHashOf("B")+" B\n") {
		t.Fatalf("tool results: got %q", results)
	}
}

// lineHashOf mirrors the tools package's 8-hex line hash for asserting
// hashline-stamped file_read output in agent tests.
func lineHashOf(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])[:8]
}

func TestSpawnSubagentRepeatedIdenticalToolCallsStops(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"sub-repeat-%d","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"same.txt\"}"}}]},"finish_reason":"tool_calls"}]}`, callCount)
	}))
	defer server.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = server.URL
	a.cfg.Workspace = t.TempDir()
	a.cfg.Subagent.Model = "test-model"
	a.cfg.Subagent.MaxTurns = 0

	events, err := a.SpawnSubagent(context.Background(), "loop", "repeat")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	var loopError string
	for event := range events {
		if event.Type == "error" && strings.Contains(event.Content, ErrRepeatedToolCallLoop.Error()) {
			loopError = event.Content
		}
	}
	if loopError == "" {
		t.Fatal("subagent did not report repeated-call loop")
	}
	if callCount != repeatedToolCallAbortAt {
		t.Fatalf("subagent provider calls: got %d, want %d", callCount, repeatedToolCallAbortAt)
	}
}
