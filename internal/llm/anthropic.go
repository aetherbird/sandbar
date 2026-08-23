// Package llm: native Anthropic Messages API wire client, ported from legacy
// sandbar's provider layer. Speaks the Messages streaming protocol — SSE events
// (message_start/content_block_start/content_block_delta/content_block_stop/
// message_delta/message_stop) rather than OpenAI chunks — so Anthropic models
// get correct tool-use blocks, native thinking deltas, split usage merge, and
// prompt caching that the OpenAI-compat bridge degrades.
//
// The fork's lingua franca is openai.ChatCompletionMessage, so translation
// happens at this boundary: system messages become the top-level system block;
// assistant ToolCalls become tool_use blocks; role "tool" messages become
// user-turn tool_result blocks (the Messages API's shape — results ride in user
// messages, not a "tool" role). Consecutive same-role messages merge (the API
// requires alternating roles), and an assistant-first conversation gets a
// "(continue)" user preamble. Prompt-caching breakpoints (cache_control:
// ephemeral) go on the system block and the last content block of the final
// user turn — the rolling breakpoint, so turn N+1's request extends turn N's
// cached prefix instead of invalidating it.

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// AnthropicVersion is the pinned Messages API version header.
const AnthropicVersion = "2023-06-01"

// anthropicDefaultBaseURL serves models with no BaseURL configured
// (Anthropic-compatible relays override it via models.json).
const anthropicDefaultBaseURL = "https://api.anthropic.com/v1"

// anthropicDefaultMaxTokens is the budget when neither options nor the
// resolved model set one — the Messages API requires max_tokens on every
// request.
const anthropicDefaultMaxTokens = 4096

// anthropicMinAnswerTokens is the answer headroom reserved above a thinking
// budget; a budget that would leave less is dropped entirely.
const anthropicMinAnswerTokens = 1024

// anthropicScanMax bounds one SSE data line (large tool inputs stream as long
// partial_json lines).
const anthropicScanMax = 4 << 20

// anthropicClient is the "anthropic-messages" wire client.
type anthropicClient struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int          // resolved per-model cap; 0 = provider default
	http      *http.Client // nil shares the bounded default
}

// newAnthropicClient builds the Messages wire client for a resolved model.
func newAnthropicClient(resolved ResolvedModel) *anthropicClient {
	baseURL := resolved.BaseURL
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	return &anthropicClient{
		baseURL:   baseURL,
		apiKey:    resolved.APIKey,
		model:     resolved.ModelID,
		maxTokens: resolved.MaxTokens,
	}
}

// compile-time seam check lives in wire.go.

// Chat streams a Messages call.
func (c *anthropicClient) Chat(ctx context.Context, messages []openai.ChatCompletionMessage) (<-chan StreamEvent, error) {
	return c.ChatWithOptions(ctx, messages, ChatOptions{})
}

// Complete performs a non-streaming Messages call.
func (c *anthropicClient) Complete(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (*CompletionResult, error) {
	return c.CompleteWithOptions(ctx, messages, CompleteOptions{Tools: tools})
}

// ChatWithOptions streams one Messages call, emitting fork StreamEvents:
// token deltas, thinking deltas, an assembled tool_call event per tool_use
// block, and one usage event (the message_start/message_delta halves merged).
// Like the OpenAI client's ChatWithOptions it emits no done event — the agent
// layer owns that. The POST happens synchronously so pre-stream failures
// (transport, HTTP status) return as errors; failures mid-body surface as an
// error event on the channel.
func (c *anthropicClient) ChatWithOptions(ctx context.Context, messages []openai.ChatCompletionMessage, opts ChatOptions) (<-chan StreamEvent, error) {
	payload, err := c.buildPayload(messages, nil, opts.Effort, c.effectiveMaxTokens(0), true)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("start stream: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("start stream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, anthropicHTTPError("start stream", resp, body)
	}
	ch := make(chan StreamEvent)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		c.parseStream(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// CompleteWithOptions performs one non-streaming Messages POST. The fork's
// client falls back to streaming on transport/5xx failures; the Messages
// endpoint has no such split, so a single POST it is. No internal retry — the
// agent's runWithLLMRetry owns retries (HTTP failures classify via
// openai.RequestError, so 429/5xx stay retryable and other 4xx stay final).
func (c *anthropicClient) CompleteWithOptions(ctx context.Context, messages []openai.ChatCompletionMessage, opts CompleteOptions) (*CompletionResult, error) {
	payload, err := c.buildPayload(messages, opts.Tools, opts.Effort, c.effectiveMaxTokens(opts.MaxTokens), false)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, anthropicHTTPError("complete", resp, body)
	}

	var msg anResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, anthropicScanMax)).Decode(&msg); err != nil {
		return nil, fmt.Errorf("complete: decode response: %w", err)
	}

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []openai.ToolCall
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			content.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			toolCalls = append(toolCalls, openai.ToolCall{
				ID:   b.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      b.Name,
					Arguments: anthropicArgsString(b.Input),
				},
			})
		}
	}
	if content.Len() == 0 && len(toolCalls) == 0 {
		if reasoning.Len() > 0 {
			// Thinking model stopped after reasoning — same truthful report
			// as the OpenAI client's paths.
			return nil, fmt.Errorf("%w (%d reasoning chars)", ErrThinkStop, reasoning.Len())
		}
		return nil, fmt.Errorf("empty response from model")
	}

	usage := CompletionUsage{}
	if u := msg.Usage; u != nil {
		usage = u.completionUsage()
	}
	return &CompletionResult{
		Content:   content.String(),
		ToolCalls: toolCalls,
		Usage:     usage,
	}, nil
}

// effectiveMaxTokens resolves the output budget: options win, then the
// resolved per-model cap, then the legacy default.
func (c *anthropicClient) effectiveMaxTokens(opt int) int {
	if opt > 0 {
		return opt
	}
	if c.maxTokens > 0 {
		return c.maxTokens
	}
	return anthropicDefaultMaxTokens
}

// endpoint is the Messages URL; the fork's BaseURL convention includes the
// /v1 suffix (as go-openai BaseURLs do).
func (c *anthropicClient) endpoint() string {
	return strings.TrimRight(c.baseURL, "/") + "/messages"
}

func (c *anthropicClient) httpClient() *http.Client {
	if c.http != nil {
		return c.http
	}
	return boundedHTTPClient()
}

func (c *anthropicClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", AnthropicVersion)
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
}

// anthropicHTTPError wraps a non-2xx response in openai.RequestError so the
// agent's retry classification (429/5xx retryable, other 4xx final) sees the
// same shape the OpenAI client produces.
func anthropicHTTPError(what string, resp *http.Response, body []byte) error {
	return fmt.Errorf("%s: %w", what, &openai.RequestError{
		HTTPStatusCode: resp.StatusCode,
		HTTPStatus:     resp.Status,
		Err:            errors.New(strings.TrimSpace(string(body))),
		Body:           body,
	})
}

// --- wire request types -----------------------------------------------------

// anRequest is the Messages API request body.
type anRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	Stream    bool         `json:"stream"`
	System    []anSysBlock `json:"system,omitempty"`
	Messages  []anMessage  `json:"messages"`
	Tools     []anTool     `json:"tools,omitempty"`
	Thinking  *anThinking  `json:"thinking,omitempty"`
}

// anSysBlock is one system-prompt block; the block carries the
// prompt-caching breakpoint (cache_control) so the system prompt + tool
// definitions hit the cache across turns.
type anSysBlock struct {
	Type         string      `json:"type"`
	Text         string      `json:"text"`
	CacheControl *anCacheCtl `json:"cache_control,omitempty"`
}

// anCacheCtl is the Messages API cache_control directive.
type anCacheCtl struct {
	Type string `json:"type"` // "ephemeral"
}

// anThinking is the Messages API extended-thinking block; budget tokens scale
// with the requested effort.
type anThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// anMessage is one user/assistant turn.
type anMessage struct {
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

// anBlock is one content block; fields are a union discriminated by Type —
// for tool_use the response side marshals Input as any, for tool_result the
// request side carries Content as a plain string.
type anBlock struct {
	Type string `json:"type"`

	// text / tool_use / thinking (thinking: response side only)
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"` // parsed arguments object

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"` // string or block array
	IsError   bool   `json:"is_error,omitempty"`

	// cache_control rides on the block it prefixes.
	CacheControl *anCacheCtl `json:"cache_control,omitempty"`
}

// anTool advertises one tool.
type anTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

// anthropicThinkingBudget maps an effort to a Messages thinking budget; 0
// means "no thinking block" (provider default).
func anthropicThinkingBudget(effort string) int {
	switch effort {
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 32768
	}
	return 0
}

// buildPayload encodes a Messages request. Prompt-caching breakpoints go on
// the system block and the last content block of the final user turn (the
// rolling conversation breakpoint), matching legacy sandbar.
func (c *anthropicClient) buildPayload(messages []openai.ChatCompletionMessage, tools []openai.Tool, effort string, maxTokens int, stream bool) ([]byte, error) {
	system, msgs := anthropicSplitMessages(messages)
	if msgs == nil {
		msgs = []anMessage{}
	}
	body := anRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		Stream:    stream,
		Messages:  msgs,
	}
	caching := len(msgs) > 0
	if system != "" {
		sys := anSysBlock{Type: "text", Text: system}
		if caching {
			sys.CacheControl = &anCacheCtl{Type: "ephemeral"}
		}
		body.System = []anSysBlock{sys}
	}
	for _, t := range tools {
		if t.Function == nil {
			continue
		}
		body.Tools = append(body.Tools, anTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: orEmptyObject(t.Function.Parameters),
		})
	}
	if b := anthropicThinkingBudget(effort); b > 0 {
		// The API requires headroom for the answer above the thinking budget,
		// so a budget that would not leave enough is dropped.
		if b < maxTokens-anthropicMinAnswerTokens {
			body.Thinking = &anThinking{Type: "enabled", BudgetTokens: b}
		}
	}
	if caching {
		// The rolling breakpoint: the last content block of the last USER
		// turn. Placing it on the final turn means turn N+1's request shares
		// turn N's cached prefix (everything before the breakpoint).
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != "user" || len(msgs[i].Content) == 0 {
				continue
			}
			last := len(msgs[i].Content) - 1
			msgs[i].Content[last].CacheControl = &anCacheCtl{Type: "ephemeral"}
			break
		}
	}
	return json.Marshal(body)
}

// anthropicSplitMessages maps the OpenAI-shaped conversation onto a system
// string plus Messages turns. Tool messages become user-turn tool_result
// blocks; consecutive messages of the same mapped role merge since the API
// requires strictly alternating roles; a conversation that starts with an
// assistant turn gets a user preamble.
func anthropicSplitMessages(messages []openai.ChatCompletionMessage) (system string, out []anMessage) {
	var sysParts []string
	push := func(role string, blocks ...anBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anMessage{Role: role, Content: blocks})
	}
	for _, m := range messages {
		switch m.Role {
		case openai.ChatMessageRoleSystem:
			if m.Content != "" {
				sysParts = append(sysParts, m.Content)
			}
		case openai.ChatMessageRoleUser:
			if t := anthropicMessageText(m); t != "" {
				push("user", anBlock{Type: "text", Text: t})
			}
		case openai.ChatMessageRoleAssistant:
			if m.Content != "" {
				push("assistant", anBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				if tc.Type != openai.ToolTypeFunction && tc.Type != "" {
					continue
				}
				push("assistant", anBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: parseJSONArgs(tc.Function.Arguments),
				})
			}
		case openai.ChatMessageRoleTool:
			if m.ToolCallID == "" {
				continue
			}
			push("user", anBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
		}
	}
	// The API requires the first turn to be user; a conversation that starts
	// with an assistant turn gets a user preamble.
	if len(out) > 0 && out[0].Role != "user" {
		out = append([]anMessage{{Role: "user", Content: []anBlock{{Type: "text", Text: "(continue)"}}}}, out...)
	}
	return strings.Join(sysParts, "\n\n"), out
}

// anthropicMessageText flattens a user message: Content when set, else the
// text parts of MultiContent (image parts have no Messages text form here).
func anthropicMessageText(m openai.ChatCompletionMessage) string {
	if m.Content != "" {
		return m.Content
	}
	var parts []string
	for _, p := range m.MultiContent {
		if p.Type == openai.ChatMessagePartTypeText {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseJSONArgs decodes a tool-call arguments string into an object;
// unparseable arguments become an empty object rather than a failed call.
func parseJSONArgs(args string) any {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}
	}
	var v any
	if json.Unmarshal([]byte(args), &v) != nil {
		return map[string]any{}
	}
	if v == nil {
		return map[string]any{}
	}
	return v
}

// orEmptyObject substitutes a bare object schema when v is nil (the Messages
// API requires input_schema).
func orEmptyObject(v any) any {
	if v == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return v
}

// anthropicArgsString marshals a parsed tool_use input object back to compact
// JSON for openai.FunctionCall.Arguments; empty input becomes "{}".
func anthropicArgsString(input any) string {
	if input == nil {
		return "{}"
	}
	b, err := json.Marshal(input)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

// --- SSE event types --------------------------------------------------------

// anEvent is one Messages SSE event payload; fields union across event types
// (only the ones each type carries are meaningful).
type anEvent struct {
	Type string `json:"type"`

	// message_start
	Message *anMessageInfo `json:"message,omitempty"`

	// content_block_start
	Index        int      `json:"index"`
	ContentBlock *anBlock `json:"content_block"`

	// content_block_delta
	Delta *anDelta `json:"delta"`

	// message_delta
	Usage *anUsage `json:"usage"` // output-side only in message_delta

	// error
	Error *anError `json:"error"`
}

// anMessageInfo carries the message-level usage from message_start. Field
// names are the Messages API's (input_tokens et al.), which is why this is
// not CompletionUsage.
type anMessageInfo struct {
	Usage *anUsage `json:"usage"`
}

// anUsage is the Messages API usage shape (input/output token names, cache
// columns).
type anUsage struct {
	Input              int `json:"input_tokens"`
	Output             int `json:"output_tokens"`
	CacheReadInput     int `json:"cache_read_input_tokens"`
	CacheCreationInput int `json:"cache_creation_input_tokens"`
}

// merge folds one wire usage into acc (partial: message_start carries
// input-side counts, message_delta carries output-side).
func (u *anUsage) merge(acc *CompletionUsage) {
	if u == nil {
		return
	}
	if u.Input > 0 {
		acc.PromptTokens = u.Input
	}
	if u.Output > 0 {
		acc.CompletionTokens = u.Output
	}
	if u.CacheReadInput > 0 {
		acc.CacheReadTokens = u.CacheReadInput
	}
	if u.CacheCreationInput > 0 {
		acc.CacheWriteTokens = u.CacheCreationInput
	}
}

// completionUsage folds the non-streaming response usage into CompletionUsage.
func (u *anUsage) completionUsage() CompletionUsage {
	var acc CompletionUsage
	u.merge(&acc)
	if acc.TotalTokens == 0 {
		acc.TotalTokens = acc.PromptTokens + acc.CompletionTokens
	}
	return acc
}

// anDelta is one content_block_delta payload (delta for message_delta events
// shares the shape: stop_reason rides alongside usage).
type anDelta struct {
	Type        string `json:"type"` // text_delta | input_json_delta | thinking_delta
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	StopReason  string `json:"stop_reason"`
}

// anError is the error event payload.
type anError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// anResponse is the non-streaming Messages response.
type anResponse struct {
	Content    []anBlock `json:"content"`
	StopReason string    `json:"stop_reason"`
	Usage      *anUsage  `json:"usage"`
}

// parseStream consumes the Messages SSE body, emitting fork events: token and
// thinking deltas as they arrive, one assembled tool_call event per tool_use
// block, then one usage event with the message_start/message_delta halves
// merged (including cache columns). No done event — the agent owns that.
func (c *anthropicClient) parseStream(ctx context.Context, body io.ReadCloser, ch chan<- StreamEvent) {
	send := func(ev StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	watch := newBodyWatchdog(body)
	defer watch.stop()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), anthropicScanMax)

	// Per-index block state.
	type blockState struct {
		kind    string // "text" | "tool_use" | "thinking"
		id      string // tool_use id
		name    string
		args    strings.Builder
		flushed bool
	}
	blocks := map[int]*blockState{}
	maxIdx := -1
	flushBlock := func(st *blockState) bool {
		st.flushed = true
		if st.kind != "tool_use" {
			return true // text/thinking streamed as deltas already
		}
		args := st.args.String()
		if args == "" {
			args = "{}"
		}
		return send(StreamEvent{
			Type:       "tool_call",
			ToolCallID: st.id,
			ToolName:   st.name,
			Arguments:  json.RawMessage(args),
		})
	}

	var usage CompletionUsage
	var usageSeen bool
	for sc.Scan() {
		watch.reset()
		line := strings.TrimSuffix(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev anEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				ev.Message.Usage.merge(&usage)
				usageSeen = true
			}
		case "content_block_start":
			st := &blockState{}
			if cb := ev.ContentBlock; cb != nil {
				switch cb.Type {
				case "text":
					st.kind = "text"
				case "thinking":
					st.kind = "thinking"
				case "tool_use":
					st.kind = "tool_use"
					st.id = cb.ID
					st.name = cb.Name
				}
			}
			blocks[ev.Index] = st
			if ev.Index > maxIdx {
				maxIdx = ev.Index
			}
		case "content_block_delta":
			st := blocks[ev.Index]
			if st == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					if !send(StreamEvent{Type: "token", Content: ev.Delta.Text}) {
						return
					}
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					if !send(StreamEvent{Type: "thinking", Content: ev.Delta.Thinking}) {
						return
					}
				}
			case "input_json_delta":
				st.args.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			if st := blocks[ev.Index]; st != nil && !st.flushed {
				if !flushBlock(st) {
					return
				}
			}
		case "message_delta":
			if ev.Usage != nil {
				ev.Usage.merge(&usage)
				usageSeen = true
			}
		case "message_stop":
			// handled after the loop
		case "error":
			msg := "unknown error"
			if ev.Error != nil {
				msg = ev.Error.Type + ": " + ev.Error.Message
			}
			send(StreamEvent{Type: "error", Content: msg})
			return
		}
	}
	if err := sc.Err(); err != nil {
		if watch.fired() {
			send(StreamEvent{Type: "error", Content: ErrStreamIdle.Error()})
			return
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			send(StreamEvent{Type: "error", Content: fmt.Sprintf("read stream: %v", err)})
		}
		return
	}
	// Safety net: flush any block that never saw content_block_stop, in index
	// order so multiple stragglers emit deterministically.
	for i := 0; i <= maxIdx; i++ {
		if st := blocks[i]; st != nil && !st.flushed {
			if !flushBlock(st) {
				return
			}
		}
	}
	if usageSeen {
		u := usage
		if u.TotalTokens == 0 {
			u.TotalTokens = u.PromptTokens + u.CompletionTokens
		}
		if !send(StreamEvent{
			Type:             "usage",
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.TotalTokens,
			CacheReadTokens:  u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
		}) {
			return
		}
	}
}

// newBodyWatchdog closes body when no bytes arrive for streamIdleTimeout, the
// SSE analogue of the OpenAI stream watchdog — a silent connection must not
// hang the turn forever. Closing the body unblocks the scanner with an error.
type bodyWatchdog struct {
	idle   *time.Timer
	firedC chan struct{}
	done   chan struct{}
}

func newBodyWatchdog(body io.Closer) *bodyWatchdog {
	w := &bodyWatchdog{
		idle:   time.NewTimer(streamIdleTimeout),
		firedC: make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		select {
		case <-w.idle.C:
			_ = body.Close()
			close(w.firedC)
		case <-w.done:
		}
	}()
	return w
}

func (w *bodyWatchdog) reset() { w.idle.Reset(streamIdleTimeout) }

func (w *bodyWatchdog) fired() bool {
	select {
	case <-w.firedC:
		return true
	default:
		return false
	}
}

func (w *bodyWatchdog) stop() {
	w.idle.Stop()
	close(w.done)
}
