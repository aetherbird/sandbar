// Package testutil provides reusable test helpers for the Sandbar project.
package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// NewFakeToolLLMServer creates an HTTP server that returns tool calls on the
// first Complete() call, no tool calls (stop) on the second, then streams on
// subsequent Chat() calls. Streaming requests ("stream":true — tool-capable
// turns via CompleteWithOptionsLive) get the same logical responses wrapped
// as SSE chunks, so one round-trip equals one logical response.
func NewFakeToolLLMServer(t *testing.T, toolCalls ...openai.ToolCall) *httptest.Server {
	t.Helper()
	callCount := 0
	respond := func(w http.ResponseWriter, r *http.Request, jsonResponse string) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(jsonResponse))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: %s\n\n", remarshalAsChunk(jsonResponse))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First Complete(): return tool calls.
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
			respond(w, r, string(data))
			return
		}
		if callCount == 2 {
			// Second Complete(): return no tool calls (stop), agent falls through to streaming.
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
			respond(w, r, string(data))
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

// remarshalAsChunk converts a non-streaming chat.completion JSON payload into
// the equivalent chat.completion.chunk payload (message → delta; streamed
// tool calls gain their index). Unparseable input passes through unchanged.
func remarshalAsChunk(jsonResponse string) string {
	var payload struct {
		Choices []struct {
			Message struct {
				Role      string          `json:"role"`
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(jsonResponse), &payload); err != nil || len(payload.Choices) == 0 {
		return jsonResponse
	}
	delta := map[string]interface{}{"role": payload.Choices[0].Message.Role, "content": payload.Choices[0].Message.Content}
	if len(payload.Choices[0].Message.ToolCalls) > 0 {
		var tcs []map[string]interface{}
		if json.Unmarshal(payload.Choices[0].Message.ToolCalls, &tcs) == nil {
			for i := range tcs {
				tcs[i]["index"] = i
			}
			delta["tool_calls"] = tcs
		}
	}
	chunk := map[string]interface{}{"id": "1", "object": "chat.completion.chunk",
		"choices": []interface{}{map[string]interface{}{"index": 0, "delta": delta, "finish_reason": payload.Choices[0].FinishReason}}}
	b, err := json.Marshal(chunk)
	if err != nil {
		return jsonResponse
	}
	return string(b)
}
