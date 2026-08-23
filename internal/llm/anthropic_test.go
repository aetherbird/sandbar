package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// anSSE renders Messages SSE events as "data: ..." lines.
func anSSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

// anResolved builds the standard anthropic-messages test model.
func anResolved() ResolvedModel {
	return ResolvedModel{
		ProviderName: "anthropic",
		API:          "anthropic-messages",
		APIKey:       "sk-an",
		ModelID:      "claude-sonnet-4-5",
	}
}

// anCapture records what the test endpoint saw.
type anCapture struct {
	body    map[string]any
	path    string
	apiKey  string
	version string
	count   int
}

// anServe starts a Messages test endpoint that captures the request and
// replies with reply (SSE for streaming calls, JSON for completions).
func anServe(t *testing.T, reply string, status int) (*httptest.Server, *anCapture) {
	t.Helper()
	cap := &anCapture{body: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cap.body)
		cap.path = r.URL.Path
		cap.apiKey = r.Header.Get("x-api-key")
		cap.version = r.Header.Get("anthropic-version")
		cap.count++
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
		}
		if strings.Contains(reply, `"type":"message_start"`) || strings.HasPrefix(reply, "data:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// anAssertCommon checks the wire basics every Messages request must have.
func anAssertCommon(t *testing.T, cap *anCapture) {
	t.Helper()
	if cap.path != "/messages" {
		t.Fatalf("path = %q, want /messages", cap.path)
	}
	if cap.apiKey != "sk-an" {
		t.Errorf("x-api-key = %q", cap.apiKey)
	}
	if cap.version != AnthropicVersion {
		t.Errorf("anthropic-version = %q", cap.version)
	}
}

func anDrain(t *testing.T, ch <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var evs []StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func anTypes(evs []StreamEvent) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func anJoined(evs []StreamEvent, typ string) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Type == typ {
			b.WriteString(ev.Content)
		}
	}
	return b.String()
}

func anFind(evs []StreamEvent, typ string) *StreamEvent {
	for i := range evs {
		if evs[i].Type == typ {
			return &evs[i]
		}
	}
	return nil
}

// anStream runs one streaming call against a scripted SSE reply and returns
// the events plus the captured request body.
func anStream(t *testing.T, rm ResolvedModel, msgs []openai.ChatCompletionMessage, opts ChatOptions, reply string) ([]StreamEvent, map[string]any) {
	t.Helper()
	srv, cap := anServe(t, reply, 0)
	rm.BaseURL = srv.URL
	ch, err := NewWireClient(rm).ChatWithOptions(context.Background(), msgs, opts)
	if err != nil {
		t.Fatalf("ChatWithOptions: %v", err)
	}
	evs := anDrain(t, ch)
	anAssertCommon(t, cap)
	return evs, cap.body
}

// anComplete runs one non-streaming call against a scripted JSON reply.
func anComplete(t *testing.T, rm ResolvedModel, msgs []openai.ChatCompletionMessage, opts CompleteOptions, reply string) (*CompletionResult, map[string]any) {
	t.Helper()
	srv, cap := anServe(t, reply, 0)
	rm.BaseURL = srv.URL
	result, err := NewWireClient(rm).CompleteWithOptions(context.Background(), msgs, opts)
	if err != nil {
		t.Fatalf("CompleteWithOptions: %v", err)
	}
	anAssertCommon(t, cap)
	return result, cap.body
}

func anUser(text string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: text}
}

func TestAnthropicTextTurnRequestAndStream(t *testing.T) {
	rm := anResolved()
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "be terse"},
		anUser("hello"),
	}
	reply := anSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":9,"cache_read_input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	)
	evs, body := anStream(t, rm, msgs, ChatOptions{}, reply)

	// Event stream: token deltas then one merged usage event (input from
	// message_start, output from message_delta), no done event.
	want := []string{"token", "token", "usage"}
	if got := anTypes(evs); !equalSlices(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	if got := anJoined(evs, "token"); got != "Hi there" {
		t.Fatalf("tokens = %q", got)
	}
	u := anFind(evs, "usage")
	if u == nil || u.PromptTokens != 9 || u.CompletionTokens != 3 || u.TotalTokens != 12 {
		t.Fatalf("usage = %+v, want prompt 9 completion 3 total 12", u)
	}
	if u == nil || u.CacheReadTokens != 5 || u.CacheWriteTokens != 0 {
		t.Fatalf("cache usage = %+v, want cacheRead 5 cacheWrite 0", u)
	}

	// Request shape.
	if body["model"] != "claude-sonnet-4-5" || body["stream"] != true {
		t.Fatalf("model/stream = %v/%v", body["model"], body["stream"])
	}
	if body["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v (default should apply)", body["max_tokens"])
	}
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system = %v (want one block)", body["system"])
	}
	sysBlock := sys[0].(map[string]any)
	if sysBlock["text"] != "be terse" || sysBlock["type"] != "text" {
		t.Fatalf("system block = %v", sysBlock)
	}
	if sysBlock["cache_control"] == nil {
		t.Fatal("system block should carry the cache_control breakpoint")
	}
	msgsOut := body["messages"].([]any)
	if len(msgsOut) != 1 {
		t.Fatalf("messages = %v", msgsOut)
	}
	first := msgsOut[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first role = %v", first["role"])
	}
	// Rolling breakpoint: the final user turn's last block.
	firstBlock := first["content"].([]any)[0].(map[string]any)
	if firstBlock["cache_control"] == nil {
		t.Fatalf("final user-turn block should carry cache_control: %v", firstBlock)
	}
	if _, ok := body["tools"]; ok {
		t.Fatal("tools should be omitted when empty")
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("thinking should be omitted when effort is empty")
	}
}

func TestAnthropicToolUseRoundTrip(t *testing.T) {
	rm := anResolved()
	rm.MaxTokens = 0 // exercise opts-wins max_tokens below
	msgs := []openai.ChatCompletionMessage{
		anUser("read main.go"),
		{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{
				ID:   "toolu_1",
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      "read",
					Arguments: `{"path":"main.go"}`,
				},
			}},
		},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "toolu_1", Content: "contents…"},
	}
	tools := []openai.Tool{{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "read",
			Description: "read a file",
			Parameters:  map[string]any{"type": "object"},
		},
	}}
	reply := `{"id":"msg_1","type":"message","role":"assistant","content":[` +
		`{"type":"text","text":"let me read"},` +
		`{"type":"tool_use","id":"toolu_9","name":"bash","input":{"cmd":"ls"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":40,"output_tokens":7,` +
		`"cache_read_input_tokens":6,"cache_creation_input_tokens":2}}`
	result, body := anComplete(t, rm, msgs, CompleteOptions{Tools: tools, MaxTokens: 1024}, reply)

	// Result mapping: text content, native tool call with marshaled input,
	// usage incl. cache columns.
	if result.Content != "let me read" {
		t.Fatalf("content = %q", result.Content)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %+v", result.ToolCalls)
	}
	tc := result.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Type != openai.ToolTypeFunction || tc.Function.Name != "bash" {
		t.Fatalf("toolCall = %+v", tc)
	}
	if tc.Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %q", tc.Function.Arguments)
	}
	u := result.Usage
	if u.PromptTokens != 40 || u.CompletionTokens != 7 || u.TotalTokens != 47 {
		t.Fatalf("usage = %+v, want 40/7/47", u)
	}
	if u.CacheReadTokens != 6 || u.CacheWriteTokens != 2 {
		t.Fatalf("cache usage = %+v, want read 6 write 2", u)
	}

	// Request shape: tool_use in an assistant turn, tool_result in a user
	// turn after it, tools advertised with input_schema, max_tokens from opts.
	if body["max_tokens"] != float64(1024) || body["stream"] != false {
		t.Fatalf("max_tokens/stream = %v/%v", body["max_tokens"], body["stream"])
	}
	msgsOut := body["messages"].([]any)
	if len(msgsOut) != 3 {
		t.Fatalf("messages = %v", msgsOut)
	}
	asst := msgsOut[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("second message role = %v", asst["role"])
	}
	ab := asst["content"].([]any)[0].(map[string]any)
	if ab["type"] != "tool_use" || ab["id"] != "toolu_1" || ab["name"] != "read" {
		t.Fatalf("tool_use block = %v", ab)
	}
	inp, _ := ab["input"].(map[string]any)
	if inp["path"] != "main.go" {
		t.Fatalf("input = %v", ab["input"])
	}
	res := msgsOut[2].(map[string]any)
	if res["role"] != "user" {
		t.Fatalf("tool_result turn role = %v", res["role"])
	}
	rb := res["content"].([]any)[0].(map[string]any)
	if rb["type"] != "tool_result" || rb["tool_use_id"] != "toolu_1" || rb["content"] != "contents…" {
		t.Fatalf("tool_result block = %v", rb)
	}
	// Rolling cache breakpoint on the LAST block of the LAST user turn (the
	// tool_result turn here).
	lastMsg := msgsOut[len(msgsOut)-1].(map[string]any)
	lastBlocks := lastMsg["content"].([]any)
	lastBlock := lastBlocks[len(lastBlocks)-1].(map[string]any)
	if lastBlock["cache_control"] == nil {
		t.Fatalf("last user-turn block should carry cache_control: %v", lastBlock)
	}
	toolsOut := body["tools"].([]any)
	t0 := toolsOut[0].(map[string]any)
	if t0["name"] != "read" || t0["description"] != "read a file" || t0["input_schema"] == nil {
		t.Fatalf("tool = %v", t0)
	}
}

func TestAnthropicStreamToolCallAssembly(t *testing.T) {
	rm := anResolved()
	reply := anSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":40}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	)
	evs, _ := anStream(t, rm, []openai.ChatCompletionMessage{anUser("run ls")}, ChatOptions{}, reply)

	call := anFind(evs, "tool_call")
	if call == nil {
		t.Fatalf("no tool_call event: %v", anTypes(evs))
	}
	if call.ToolCallID != "toolu_9" || call.ToolName != "bash" {
		t.Fatalf("tool_call = %+v", call)
	}
	if string(call.Arguments) != `{"cmd":"ls"}` {
		t.Fatalf("assembled arguments = %q", call.Arguments)
	}
	// Exactly one tool_call event (assembled once at content_block_stop), and
	// one merged usage event.
	var calls, usages int
	for _, ev := range evs {
		switch ev.Type {
		case "tool_call":
			calls++
		case "usage":
			usages++
		}
	}
	if calls != 1 || usages != 1 {
		t.Fatalf("calls=%d usages=%d, want 1 and 1 (events: %v)", calls, usages, anTypes(evs))
	}
}

func TestAnthropicStreamEventSequence(t *testing.T) {
	rm := anResolved()
	reply := anSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"read"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	)
	evs, _ := anStream(t, rm, []openai.ChatCompletionMessage{anUser("q")}, ChatOptions{}, reply)
	want := []string{"thinking", "token", "tool_call", "usage"}
	if got := anTypes(evs); !equalSlices(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	if got := anJoined(evs, "thinking"); got != "pondering" {
		t.Fatalf("thinking = %q", got)
	}
	if got := anJoined(evs, "token"); got != "answer" {
		t.Fatalf("tokens = %q", got)
	}
}

func TestAnthropicMergesConsecutiveRoles(t *testing.T) {
	rm := anResolved()
	msgs := []openai.ChatCompletionMessage{
		anUser("one"),
		anUser("two"),
		{Role: openai.ChatMessageRoleAssistant, Content: "ok"},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "x", Content: "r1"},
	}
	reply := anSSE(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	)
	_, body := anStream(t, rm, msgs, ChatOptions{}, reply)
	msgsOut := body["messages"].([]any)
	if len(msgsOut) != 3 {
		t.Fatalf("messages = %v", msgsOut)
	}
	m0 := msgsOut[0].(map[string]any)
	if m0["role"] != "user" || len(m0["content"].([]any)) != 2 {
		t.Fatalf("consecutive user turns must merge: %v", m0)
	}
	m2 := msgsOut[2].(map[string]any)
	if m2["role"] != "user" {
		t.Fatalf("tool_result after assistant should map to a user turn: %v", m2)
	}
}

func TestAnthropicAssistantFirstGetsPreamble(t *testing.T) {
	rm := anResolved()
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleAssistant, Content: "hi"}}
	reply := anSSE(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
	_, body := anStream(t, rm, msgs, ChatOptions{}, reply)
	msgsOut := body["messages"].([]any)
	if len(msgsOut) < 2 || msgsOut[0].(map[string]any)["role"] != "user" {
		t.Fatalf("assistant-first conversation needs a user preamble: %v", msgsOut)
	}
}

func TestAnthropicErrorEvent(t *testing.T) {
	rm := anResolved()
	reply := anSSE(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	evs, _ := anStream(t, rm, []openai.ChatCompletionMessage{anUser("q")}, ChatOptions{}, reply)
	errEv := anFind(evs, "error")
	if errEv == nil {
		t.Fatalf("no error event: %v", anTypes(evs))
	}
	if want := "overloaded_error: Overloaded"; errEv.Content != want {
		t.Fatalf("error = %q, want %q", errEv.Content, want)
	}
}

func TestAnthropicHTTP400NotRetried(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer srv.Close()
	rm := anResolved()
	rm.BaseURL = srv.URL
	_, err := NewWireClient(rm).ChatWithOptions(context.Background(), []openai.ChatCompletionMessage{anUser("q")}, ChatOptions{})
	if err == nil {
		t.Fatal("400 must surface as an error")
	}
	if n != 1 {
		t.Fatalf("400 must not retry: %d attempts", n)
	}
	// Same policy on the non-streaming path.
	n = 0
	if _, err := NewWireClient(rm).CompleteWithOptions(context.Background(), []openai.ChatCompletionMessage{anUser("q")}, CompleteOptions{}); err == nil {
		t.Fatal("400 must surface as an error (complete)")
	}
	if n != 1 {
		t.Fatalf("400 must not retry (complete): %d attempts", n)
	}
}

func TestAnthropicEffortThinkingBudget(t *testing.T) {
	cases := []struct {
		name       string
		effort     string
		maxTokens  int
		wantBudget int // 0 = no thinking block
	}{
		{"low fits default headroom", "low", 4096, 2048},
		{"medium dropped at default max", "medium", 0, 0}, // 8192 >= 4096-1024
		{"high fits large budget", "high", 40000, 32768},
		{"empty effort sends none", "", 40000, 0},
	}
	for _, tc := range cases {
		rm := anResolved()
		rm.MaxTokens = tc.maxTokens
		reply := `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		_, body := anComplete(t, rm, []openai.ChatCompletionMessage{anUser("q")}, CompleteOptions{Effort: tc.effort}, reply)
		th, ok := body["thinking"].(map[string]any)
		if tc.wantBudget == 0 {
			if ok {
				t.Errorf("%s: thinking = %v, want omitted", tc.name, body["thinking"])
			}
			continue
		}
		if !ok {
			t.Errorf("%s: no thinking block: %v", tc.name, body)
			continue
		}
		if th["type"] != "enabled" || th["budget_tokens"] != float64(tc.wantBudget) {
			t.Errorf("%s: thinking = %v, want enabled/%d", tc.name, th, tc.wantBudget)
		}
	}
}

func TestAnthropicCompleteNonStreaming(t *testing.T) {
	rm := anResolved()
	rm.MaxTokens = 2048 // resolved cap applies when options set none
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		anUser("hi"),
	}
	reply := `{"content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":4}}`
	result, body := anComplete(t, rm, msgs, CompleteOptions{}, reply)
	if result.Content != "Hello" {
		t.Fatalf("content = %q", result.Content)
	}
	if result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 4 || result.Usage.TotalTokens != 14 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if body["max_tokens"] != float64(2048) {
		t.Fatalf("max_tokens = %v, want resolved 2048", body["max_tokens"])
	}
	if _, ok := body["system"]; !ok {
		t.Fatal("system message should map to the system block")
	}
}

func TestNewWireClientRouting(t *testing.T) {
	if _, ok := NewWireClient(ResolvedModel{ModelID: "m"}).(*Client); !ok {
		t.Error("empty api must route to the OpenAI-compatible client")
	}
	if _, ok := NewWireClient(ResolvedModel{API: "openai-completions", ModelID: "m"}).(*Client); !ok {
		t.Error("openai-completions api must route to the OpenAI-compatible client")
	}
	if _, ok := NewWireClient(ResolvedModel{API: "anthropic-messages", ModelID: "m"}).(*anthropicClient); !ok {
		t.Error("anthropic-messages api must route to the Anthropic wire client")
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
