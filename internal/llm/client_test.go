package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
)

// TestChatStreamReasoningContent pins the GLM/DeepSeek/Qwen reasoning dialect:
// reasoning arrives in delta.reasoning_content, separate from answer content,
// and must surface as thinking events (the TUI's reasoning indicator depends
// on them) — previously these deltas were dropped silently, so a thinking
// model showed no activity at all until the answer began.
func TestChatStreamReasoningContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"deliberating \"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"hard\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Answer.\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	ch, err := client.Chat(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	var tokens, thinking []string
	for ev := range ch {
		switch ev.Type {
		case "token":
			tokens = append(tokens, ev.Content)
		case "thinking":
			thinking = append(thinking, ev.Content)
		}
	}
	if len(tokens) != 1 || tokens[0] != "Answer." {
		t.Errorf("tokens: %v", tokens)
	}
	if len(thinking) != 2 || thinking[0] != "deliberating " || thinking[1] != "hard" {
		t.Errorf("thinking events: %q", thinking)
	}
}

// TestCompleteWithOptionsLiveForwardsDeltasAndAssemblesTools pins the live
// tool-capable completion: reasoning and text deltas reach send as they
// arrive while streamed tool calls are assembled into the returned result —
// the contract the agent's tool loop and the TUI's thinking indicator rely on.
func TestCompleteWithOptionsLiveForwardsDeltasAndAssemblesTools(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		chunks := []string{
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"ponder "},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"ing"},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file_write","arguments":"{\"path\":\"a"}}]},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":".txt\"}"}}]},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	var got []string
	result, err := client.CompleteWithOptionsLive(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{}, func(ev StreamEvent) bool {
		got = append(got, ev.Type+":"+ev.Content)
		return true
	})
	if err != nil {
		t.Fatalf("live complete: %v", err)
	}
	if len(got) != 2 || got[0] != "thinking:ponder " || got[1] != "thinking:ing" {
		t.Fatalf("forwarded events = %q", got)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Name != "file_write" {
		t.Fatalf("assembled tool calls: %+v", result.ToolCalls)
	}
	if args := result.ToolCalls[0].Function.Arguments; args != `{"path":"a.txt"}` {
		t.Fatalf("split streamed arguments = %q", args)
	}
}

func TestChatStreamPlain(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	ch, err := client.Chat(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	var tokens []string
	var eventTypes []string
	for ev := range ch {
		eventTypes = append(eventTypes, ev.Type)
		if ev.Type == "token" {
			tokens = append(tokens, ev.Content)
		}
	}

	if len(tokens) != 1 || tokens[0] != "Hello" {
		t.Errorf("tokens: %v", tokens)
	}
	// Note: done event is now emitted by agent.streamAndPersist(), not the LLM client.
}

func TestChatStreamWithThink(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		chunks := []string{
			"<think>",
			"Thinking...",
			"</think>",
			"Answer.",
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	ch, err := client.Chat(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	var thinking, tokens []string
	for ev := range ch {
		switch ev.Type {
		case "thinking":
			thinking = append(thinking, ev.Content)
		case "token":
			tokens = append(tokens, ev.Content)
		}
	}

	if len(thinking) != 1 || thinking[0] != "Thinking..." {
		t.Errorf("thinking: %v", thinking)
	}
	if len(tokens) != 1 || tokens[0] != "Answer." {
		t.Errorf("tokens: %v", tokens)
	}
}

func TestChatStreamThinkAcrossChunks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Split <think> across two chunks.
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<thi\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"nk>reasoning</think>result\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	ch, err := client.Chat(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	var thinking, tokens []string
	for ev := range ch {
		switch ev.Type {
		case "thinking":
			thinking = append(thinking, ev.Content)
		case "token":
			tokens = append(tokens, ev.Content)
		}
	}

	if len(thinking) != 1 || thinking[0] != "reasoning" {
		t.Errorf("thinking: %v", thinking)
	}
	if len(tokens) != 1 || tokens[0] != "result" {
		t.Errorf("tokens: %v", tokens)
	}
}

func TestCompleteWithOptions_MaxTokensNonStreaming(t *testing.T) {
	var requestBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			requestBody = body
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	_, err := client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{MaxTokens: 500, Purpose: "compression"})
	if err != nil {
		t.Fatalf("CompleteWithOptions: %v", err)
	}

	if !strings.Contains(string(requestBody), `"max_tokens":500`) {
		t.Errorf("expected max_tokens:500 in request body, got: %s", string(requestBody))
	}
}

func TestCompleteWithOptions_ZeroMaxTokens_NotInRequest(t *testing.T) {
	var requestBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			requestBody = body
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	_, err := client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{MaxTokens: 0, Purpose: "main"})
	if err != nil {
		t.Fatalf("CompleteWithOptions: %v", err)
	}

	if strings.Contains(string(requestBody), `"max_tokens"`) {
		t.Errorf("expected no max_tokens when MaxTokens=0, got: %s", string(requestBody))
	}
}

func TestComplete_BackwardCompat(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	result, err := client.Complete(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Content != "Hello" {
		t.Errorf("expected 'Hello', got %q", result.Content)
	}
}

func TestCompleteWithOptions_EffortInRequest(t *testing.T) {
	var requestBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody = body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	_, err := client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{Effort: "high"})
	if err != nil {
		t.Fatalf("CompleteWithOptions: %v", err)
	}
	if !strings.Contains(string(requestBody), `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort:high in request body, got: %s", string(requestBody))
	}

	// Empty effort must not send the field — providers that reject unknown
	// values would otherwise fail every ordinary turn.
	requestBody = nil
	_, err = client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{})
	if err != nil {
		t.Fatalf("CompleteWithOptions: %v", err)
	}
	if strings.Contains(string(requestBody), "reasoning_effort") {
		t.Errorf("reasoning_effort must be omitted when unset, got: %s", string(requestBody))
	}
}

func TestChatWithOptions_EffortInStreamRequest(t *testing.T) {
	var requestBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	ch, err := client.ChatWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, ChatOptions{Effort: "low"})
	if err != nil {
		t.Fatalf("ChatWithOptions: %v", err)
	}
	for range ch {
	}
	if !strings.Contains(string(requestBody), `"reasoning_effort":"low"`) {
		t.Errorf("expected reasoning_effort:low in stream request body, got: %s", string(requestBody))
	}
}

func TestCompleteWithOptions_NoStreamingFallbackOn429(t *testing.T) {
	var requestCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	_, err := client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{})
	if err == nil {
		t.Fatal("expected error from 429")
	}
	if requestCount != 1 {
		t.Fatalf("requests = %d, want 1 (429 must not trigger streaming fallback)", requestCount)
	}
}

func TestCompleteWithOptions_FallsBackOn500(t *testing.T) {
	var requestCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"overloaded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "fake-key", "test-model")
	result, err := client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}, CompleteOptions{})
	if err != nil {
		t.Fatalf("CompleteWithOptions: %v", err)
	}
	if result.Content != "ok" {
		t.Errorf("content = %q, want ok", result.Content)
	}
	if requestCount != 2 {
		t.Fatalf("requests = %d, want 2 (500 falls back to streaming)", requestCount)
	}
}

func TestIsFallbackWorthy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429 api error", &openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}, false},
		{"401 api error", &openai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}, false},
		{"400 request error", &openai.RequestError{HTTPStatusCode: 400, Err: errors.New("bad request")}, false},
		{"400 streaming required", &openai.APIError{HTTPStatusCode: 400, Message: "streaming required"}, true},
		{"500 api error", &openai.APIError{HTTPStatusCode: 500, Message: "boom"}, true},
		{"503 request error", &openai.RequestError{HTTPStatusCode: 503, Err: errors.New("unavailable")}, true},
		{"EOF", io.EOF, true},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"plain error", errors.New("empty response from model"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFallbackWorthy(tc.err); got != tc.want {
				t.Errorf("IsFallbackWorthy(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestEffortDialectsPerProvider(t *testing.T) {
	makeServer := func(capture *[]byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			*capture = body
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`)
		}))
	}
	cases := []struct {
		style    string
		effort   string
		contains string
		absent   string
	}{
		{style: "", effort: "high", contains: `"reasoning_effort":"high"`, absent: "chat_template_kwargs"},
		{style: "openai", effort: "medium", contains: `"reasoning_effort":"medium"`, absent: "chat_template_kwargs"},
		{style: "enable_thinking", effort: "high", contains: `"chat_template_kwargs":{"enable_thinking":true}`, absent: "reasoning_effort"},
		{style: "enable_thinking", effort: "low", contains: `"chat_template_kwargs":{"enable_thinking":false}`, absent: "reasoning_effort"},
		{style: "none", effort: "high", contains: `"model"`, absent: "reasoning_effort"},
	}
	for _, tc := range cases {
		var body []byte
		ts := makeServer(&body)
		client := NewClientWithReasoning(ts.URL, "k", "m", tc.style)
		if _, err := client.CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "hi"},
		}, CompleteOptions{Effort: tc.effort}); err != nil {
			t.Fatalf("style=%q effort=%q: %v", tc.style, tc.effort, err)
		}
		if !strings.Contains(string(body), tc.contains) {
			t.Errorf("style=%q effort=%q: body missing %s: %s", tc.style, tc.effort, tc.contains, string(body))
		}
		if tc.absent != "" && strings.Contains(string(body), tc.absent) {
			t.Errorf("style=%q effort=%q: body must not contain %s", tc.style, tc.effort, tc.absent)
		}
		ts.Close()
	}
}

// TestThinkStopVsEmptyResponse proves the three-case acceptance: a normal
// turn passes, a reasoning-only turn (thinking model stopped after thinking)
// gets the truthful think-stop error, and a truly empty turn keeps the
// generic empty-response error. Regression guard for the ling-3.0-flash
// failure mode where think-stops were mislabeled as empty responses.
func TestThinkStopVsEmptyResponse(t *testing.T) {
	serve := func(body string) *Client {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}))
		t.Cleanup(ts.Close)
		return NewClient(ts.URL+"/v1", "test", "test-model")
	}

	// 1) Normal answer: no error.
	normal := serve(`{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello"}}],"usage":{"total_tokens":5}}`)
	if _, err := normal.Complete(context.Background(), nil, nil); err != nil {
		t.Fatalf("normal turn errored: %v", err)
	}

	// 2) Reasoning-only (think-stop): truthful error naming the shape.
	thinkStop := serve(`{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"","reasoning_content":"thought hard"}}],"usage":{"total_tokens":9}}`)
	_, err := thinkStop.Complete(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped after reasoning") {
		t.Fatalf("think-stop error = %v, want 'stopped after reasoning'", err)
	}
	// The think-stop error must not be a fallback-worthy phantom: nothing
	// is wrong with the transport, so the streaming fallback adds nothing.
	if IsFallbackWorthy(err) {
		t.Fatalf("think-stop should not trigger streaming fallback: %v", err)
	}

	// 3) Truly empty: legacy error unchanged.
	trueEmpty := serve(`{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":""}}],"usage":{"total_tokens":5}}`)
	_, err = trueEmpty.Complete(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "empty response from model") {
		t.Fatalf("true-empty error = %v, want 'empty response from model'", err)
	}
}

func TestCompatMaxTokensField(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":3}}`)
	}))
	defer ts.Close()

	compat := &config.CompatFlags{MaxTokensField: "max_completion_tokens"}
	client := NewClientWithCompat(ts.URL, "k", "m", "", compat)
	if _, err := client.CompleteWithOptions(context.Background(), nil, CompleteOptions{MaxTokens: 77}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Errorf("max_completion_tokens missing from request: %v", body)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Errorf("max_tokens unexpectedly set alongside max_completion_tokens: %v", body)
	}
	if body["max_completion_tokens"].(float64) != 77 {
		t.Errorf("max_completion_tokens = %v, want 77", body["max_completion_tokens"])
	}

	// Default compat keeps the classic field.
	body = nil
	plain := NewClient(ts.URL, "k", "m")
	if _, err := plain.CompleteWithOptions(context.Background(), nil, CompleteOptions{MaxTokens: 88}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, ok := body["max_tokens"]; !ok {
		t.Errorf("max_tokens missing from request: %v", body)
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Errorf("max_completion_tokens unexpectedly set: %v", body)
	}
}

func TestCompatRequiresToolResultName(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":3}}`)
	}))
	defer ts.Close()

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
			ID:       "call_1",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "file_read", Arguments: "{}"},
		}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call_1", Content: "data"},
	}

	// Flag off (fork default): Name stays empty, caller's slice untouched.
	plain := NewClient(ts.URL, "k", "m")
	if _, err := plain.Complete(context.Background(), messages, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	toolMsg := lastToolMessage(t, body)
	if toolMsg["name"] != nil && toolMsg["name"] != "" {
		t.Errorf("name set without flag: %v", toolMsg)
	}
	if messages[1].Name != "" {
		t.Error("caller's message slice was mutated")
	}

	// Flag on: Name recovered from the assistant tool call id.
	body = nil
	flagged := NewClientWithCompat(ts.URL, "k", "m", "", &config.CompatFlags{RequiresToolResultName: boolPtr(true)})
	if _, err := flagged.Complete(context.Background(), messages, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	toolMsg = lastToolMessage(t, body)
	if toolMsg["name"] != "file_read" {
		t.Errorf("tool result name = %v, want file_read", toolMsg["name"])
	}
}

func lastToolMessage(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("no messages in request body: %v", body)
	}
	for i := len(raw) - 1; i >= 0; i-- {
		if m, ok := raw[i].(map[string]any); ok && m["role"] == "tool" {
			return m
		}
	}
	t.Fatalf("no tool message in request: %v", body)
	return nil
}

func boolPtr(v bool) *bool { return &v }
