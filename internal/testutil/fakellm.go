// Package testutil provides reusable test helpers for the Sandbar project.
package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// NewFakeLLMServer creates an HTTP server that mocks a streaming LLM endpoint.
// Each response string is emitted as a single token, followed by [DONE].
func NewFakeLLMServer(t *testing.T, responses ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, text := range responses {
			chunk := map[string]interface{}{
				"id":      "chunk-1",
				"object":  "chat.completion.chunk",
				"created": 1,
				"model":   "test",
				"choices": []map[string]interface{}{
					{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": nil},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// NewFakeToolLLMServer creates an HTTP server that returns tool calls on the
// first Complete() call, no tool calls (stop) on the second, then streams on
// subsequent Chat() calls.
func NewFakeToolLLMServer(t *testing.T, toolCalls ...openai.ToolCall) *httptest.Server {
	t.Helper()
	callCount := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First Complete(): return tool calls.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			resp := map[string]interface{}{
				"id":      "comp-1",
				"object":  "chat.completion",
				"created": 1,
				"model":   "test",
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"message": map[string]interface{}{
							"role":       "assistant",
							"content":    "",
							"tool_calls": toolCalls,
						},
						"finish_reason": "tool_calls",
					},
				},
			}
			data, _ := json.Marshal(resp)
			w.Write(data)
			return
		}
		if callCount == 2 {
			// Second Complete(): return no tool calls (stop), agent falls through to streaming.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			resp := map[string]interface{}{
				"id":      "comp-2",
				"object":  "chat.completion",
				"created": 1,
				"model":   "test",
				"choices": []map[string]interface{}{
					{
						"index":         0,
						"message":       map[string]interface{}{"role": "assistant", "content": ""},
						"finish_reason": "stop",
					},
				},
			}
			data, _ := json.Marshal(resp)
			w.Write(data)
			return
		}
		// Subsequent calls: simple stream (Chat()).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		chunk := map[string]interface{}{
			"id":      "chunk-1",
			"object":  "chat.completion.chunk",
			"created": 1,
			"model":   "test",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": "Done"}, "finish_reason": nil},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}
