package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
)

// ErrStreamIdle reports a provider connection that stopped delivering data:
// a streaming response saw no chunk for streamIdleTimeout, or a
// non-streaming completion exceeded completionAttemptTimeout. Both usually
// mean a wedged gateway or silently-dropped connection. The error is
// retryable — a fresh connection typically recovers.
var ErrStreamIdle = errors.New("provider stream stalled: no data received within the idle timeout")

// ErrThinkStop reports a thinking model that emitted reasoning and stopped
// without an answer or tool call. The response arrived intact — this is model
// behavior, not a transport failure — so it must not trigger the streaming
// fallback (a second request would just re-roll the model for a deterministic
// report) and must surface verbatim to callers.
var ErrThinkStop = errors.New("model stopped after reasoning without producing an answer")

// errEmptyStreamResponse marks a stream that completed without any content,
// tool call, or reasoning. The buffered path treats that as an empty result;
// the live path unwraps it the same way instead of erroring the turn.
var errEmptyStreamResponse = errors.New("empty stream response")

// streamIdleTimeout bounds how long a streaming response may go between
// chunks. Providers emit deltas or keepalives continuously while generating,
// so silence for this long means the connection is dead. A var (not const)
// so tests can shorten it.
var streamIdleTimeout = 5 * time.Minute

// completionAttemptTimeout caps one non-streaming completion attempt. There
// is no progress signal before the response body arrives, so this bounds the
// whole attempt; hitting it with the caller's context still alive is a stall,
// not a caller cancellation. A var (not const) so tests can shorten it.
var completionAttemptTimeout = 10 * time.Minute

// Client wraps the OpenAI-compatible API for streaming chat.
type Client struct {
	client         *openai.Client
	model          string
	reasoningStyle string
	// compat carries optional models.json compat flags. Only
	// MaxTokensField and RequiresToolResultName are honored today; the
	// remaining flags (supportsDeveloperRole, supportsReasoningEffort,
	// thinkingFormat, requiresAssistantAfterToolResult, sendSessionId) are
	// deferred. anthropic-messages models use the dedicated wire client in
	// anthropic.go instead of this OpenAI-compatible one.
	compat *config.CompatFlags
}

// NewClient creates an LLM client for a resolved model.
func NewClient(baseURL, apiKey, model string) *Client {
	return NewClientWithReasoning(baseURL, apiKey, model, "")
}

// NewClientWithReasoning creates a client that translates the per-turn
// reasoning effort into the provider's dialect ("" / "openai" / "enable_thinking" / "none").
func NewClientWithReasoning(baseURL, apiKey, model, reasoningStyle string) *Client {
	return NewClientWithCompat(baseURL, apiKey, model, reasoningStyle, nil)
}

// NewClientWithCompat additionally carries the provider's models.json compat
// flags (nil = defaults). Callers with a ResolvedModel pass its Compat field.
func NewClientWithCompat(baseURL, apiKey, model, reasoningStyle string, compat *config.CompatFlags) *Client {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	// The library default is a bare &http.Client{} — zero timeout, no
	// deadlines. Bound every phase here: header-phase hangs via
	// ResponseHeaderTimeout, body-phase stalls via the stream watchdog and
	// the per-attempt cap.
	cfg.HTTPClient = boundedHTTPClient()
	return &Client{
		client:         openai.NewClientWithConfig(cfg),
		model:          model,
		reasoningStyle: reasoningStyle,
		compat:         compat,
	}
}

// applyMaxTokens routes the output budget through the field the provider
// expects: Compat.MaxTokensField "max_completion_tokens" (newer OpenAI
// endpoints, o-series) vs the max_tokens default.
func (c *Client) applyMaxTokens(req *openai.ChatCompletionRequest, n int) {
	if c.compat != nil && c.compat.MaxTokensField == "max_completion_tokens" {
		req.MaxCompletionTokens = n
		return
	}
	req.MaxTokens = n
}

// withToolResultNames fills Name on tool-result messages when
// Compat.RequiresToolResultName is explicitly true: some gateways reject a
// tool result whose name is missing. The name is recovered from the matching
// assistant tool_call id. A nil or false flag keeps this fork's behavior of
// omitting Name (legacy sent it by default). The slice is copied; the
// caller's messages are untouched.
func (c *Client) withToolResultNames(msgs []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if c.compat == nil || c.compat.RequiresToolResultName == nil || !*c.compat.RequiresToolResultName {
		return msgs
	}
	names := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				names[tc.ID] = tc.Function.Name
			}
		}
	}
	out := append([]openai.ChatCompletionMessage(nil), msgs...)
	for i, m := range out {
		if m.Role != openai.ChatMessageRoleTool || m.Name != "" {
			continue
		}
		if name, ok := names[m.ToolCallID]; ok {
			out[i].Name = name
		}
	}
	return out
}

// CompletionResult is the non-streaming response from the LLM.
type CompletionResult struct {
	Content   string
	ToolCalls []openai.ToolCall
	Usage     CompletionUsage // token usage from the provider
}

// CompletionUsage holds token counts from the provider.
type CompletionUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens/CacheWriteTokens are the prompt-caching columns
	// (Anthropic cache_read/cache_creation input tokens); 0 for providers
	// that do not report them.
	CacheReadTokens  int
	CacheWriteTokens int
}

// CompleteOptions bundles optional parameters for a completion call.
type CompleteOptions struct {
	Tools     []openai.Tool
	MaxTokens int    // 0 = no limit (provider default)
	Purpose   string // "main", "compression", "title", "subagent"; informational
	Effort    string // reasoning effort: "low"|"medium"|"high"; "" = provider default
}

// ChatOptions bundles optional parameters for a streaming chat call.
type ChatOptions struct {
	Effort string // reasoning effort: "low"|"medium"|"high"; "" = provider default
}

// applyEffort translates the per-turn reasoning effort into the provider's
// request dialect. Empty effort always means provider default and sends
// nothing. Styles: "openai" (default) sends reasoning_effort — providers that
// do not support it ignore the field; "enable_thinking" sends llama.cpp
// chat_template_kwargs (verified against the local llama-swap endpoint:
// Qwen3.x and Gemma4 both honor enable_thinking, with reasoning exposed as
// reasoning_content) — low disables thinking, medium/high enable it; "none"
// ignores effort entirely.
func (c *Client) applyEffort(req *openai.ChatCompletionRequest, effort string) {
	if effort == "" {
		return
	}
	switch c.reasoningStyle {
	case "enable_thinking":
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": effort != "low"}
	case "none":
	default:
		req.ReasoningEffort = effort
	}
}

// Complete performs a chat completion, preferring non-streaming for reliability.
// Falls back to streaming when the provider rejects non-streaming requests
// (e.g., Gemini 3.x with large tool schemas).
func (c *Client) Complete(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (*CompletionResult, error) {
	return c.CompleteWithOptions(ctx, messages, CompleteOptions{Tools: tools})
}

// CompleteWithOptions performs a chat completion with optional MaxTokens and
// other settings. MaxTokens is enforced in both the non-streaming and streaming
// fallback paths. The non-streaming attempt is capped at
// completionAttemptTimeout — a non-streaming response offers no progress
// signal, so a wedged connection would otherwise block the turn forever. On a
// fallback-worthy failure (transport error, EOF, 5xx, timeout) the call falls
// back to streaming, which gets a fresh connection and carries its own idle
// watchdog. A 429 or other 4xx is a definitive rejection: it is returned
// directly rather than doubling traffic against a rate-limited or rejecting
// endpoint.
func (c *Client) CompleteWithOptions(ctx context.Context, messages []openai.ChatCompletionMessage, opts CompleteOptions) (*CompletionResult, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, completionAttemptTimeout)
	defer cancel()
	result, err := c.completeNonStreaming(attemptCtx, messages, opts)
	if err == nil {
		return result, nil
	}
	if !IsFallbackWorthy(err) {
		return nil, err
	}
	return c.completeStreaming(ctx, messages, opts, nil)
}

// CompleteWithOptionsLive is CompleteWithOptions with a live event feed:
// reasoning and answer deltas are forwarded to send as they arrive on the
// streaming transport while the result is assembled with the same
// single-response semantics (native tool calls or terminal text). Streaming
// runs first — the only way to be live; a fallback-worthy failure retries
// non-streaming with no events. A send returning false stops forwarding, not
// the request.
func (c *Client) CompleteWithOptionsLive(ctx context.Context, messages []openai.ChatCompletionMessage, opts CompleteOptions, send func(StreamEvent) bool) (*CompletionResult, error) {
	result, err := c.completeStreaming(ctx, messages, opts, send)
	if err == nil {
		return result, nil
	}
	// An empty streamed terminal is a legitimate (if odd) provider response —
	// completeNonStreaming returns an empty result for one, so the live path
	// must too rather than failing the turn or burning a fallback request.
	if errors.Is(err, errEmptyStreamResponse) {
		return &CompletionResult{}, nil
	}
	if !IsFallbackWorthy(err) {
		return nil, err
	}
	return c.completeNonStreaming(ctx, messages, opts)
}

// IsFallbackWorthy reports whether a non-streaming completion error should be
// retried via the streaming fallback path. The fallback exists because some
// providers reject or cannot handle non-streaming requests (e.g. Gemini 3.x
// with large tool schemas). It recovers transport failures, EOF, stalled
// streams, 5xx server errors, and timeouts with a fresh streaming connection.
//
// It does NOT fall back on a 429 rate limit — a second request doubles load on
// the limiting endpoint and is itself doomed (the original bug this guards) —
// nor on other definitive 4xx rejections (401/403/404/422), nor on caller
// cancellation. The one 4xx that does fall back is the documented "streaming
// required" rejection, which is precisely the fallback's purpose.
//
// This is the fallback decision only, not the retry decision (that lives in the
// agent package's runWithLLMRetry); it lives here so the llm package stays
// dependency-free.
func IsFallbackWorthy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// A think-stop is a complete, faithful response — not a transport
	// failure. Retrying via the streaming fallback would re-roll the model
	// and mask the behavior being reported.
	if errors.Is(err, ErrThinkStop) {
		return false
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		if apiErr.HTTPStatusCode >= 500 {
			return true
		}
		if apiErr.HTTPStatusCode == 429 {
			return false
		}
		return isStreamingRequired(apiErr.Message)
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		if reqErr.HTTPStatusCode >= 500 {
			return true
		}
		if reqErr.HTTPStatusCode > 0 {
			return false
		}
	}
	// No HTTP status: transport failure, EOF, a stalled stream, a streamed
	// body that failed non-streaming JSON parsing, or a timeout. A fresh
	// streaming connection typically recovers.
	return true
}

// isStreamingRequired reports whether a 4xx error message is the provider's
// "non-streaming is unsupported" signal, which the fallback exists to handle.
func isStreamingRequired(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "stream")
}

func (c *Client) completeNonStreaming(ctx context.Context, messages []openai.ChatCompletionMessage, opts CompleteOptions) (*CompletionResult, error) {
	req := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: c.withToolResultNames(messages),
	}
	if len(opts.Tools) > 0 {
		req.Tools = opts.Tools
	}
	if opts.MaxTokens > 0 {
		c.applyMaxTokens(&req, opts.MaxTokens)
	}
	c.applyEffort(&req, opts.Effort)

	resp, err := c.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("empty response from model")
	}

	choice := resp.Choices[0]
	usage := CompletionUsage{}
	if resp.Usage.TotalTokens > 0 {
		usage = CompletionUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	if choice.Message.Content == "" && len(choice.Message.ToolCalls) == 0 {
		if choice.Message.ReasoningContent != "" {
			// Thinking model emitted reasoning and stopped without an
			// answer or tool call. Everything arrived intact — this is a
			// model behavior, not a transport/server failure, so it gets a
			// distinct, truthful error instead of "empty response".
			return nil, fmt.Errorf("%w (%d reasoning chars)", ErrThinkStop, len(choice.Message.ReasoningContent))
		}
		return nil, fmt.Errorf("empty response from model")
	}

	return &CompletionResult{
		Content:   choice.Message.Content,
		ToolCalls: choice.Message.ToolCalls,
		Usage:     usage,
	}, nil
}

func (c *Client) completeStreaming(ctx context.Context, messages []openai.ChatCompletionMessage, opts CompleteOptions, send func(StreamEvent) bool) (*CompletionResult, error) {
	req := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: c.withToolResultNames(messages),
		Stream:   true,
		StreamOptions: &openai.StreamOptions{
			IncludeUsage: true,
		},
	}
	if len(opts.Tools) > 0 {
		req.Tools = opts.Tools
	}
	if opts.MaxTokens > 0 {
		c.applyMaxTokens(&req, opts.MaxTokens)
	}
	c.applyEffort(&req, opts.Effort)

	stream, err := c.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}
	defer stream.Close()
	stalled := newStreamWatchdog(ctx, stream)
	defer stalled.stop()

	var content string
	var toolCalls []openai.ToolCall
	// reasoning accumulates reasoning_content deltas (thinking models) so a
	// terminal think-stop can be distinguished from a true empty response.
	var reasoning string
	toolCallParts := make(map[int]*openai.ToolCall)
	var usage CompletionUsage

	for {
		resp, err := stream.Recv()
		stalled.reset()
		if stalled.fired() {
			return nil, fmt.Errorf("complete: %w", ErrStreamIdle)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("complete: %w", err)
		}

		// Capture usage from the final chunk.
		if resp.Usage != nil {
			if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 || resp.Usage.TotalTokens > 0 {
				usage = CompletionUsage{
					PromptTokens:     resp.Usage.PromptTokens,
					CompletionTokens: resp.Usage.CompletionTokens,
					TotalTokens:      resp.Usage.TotalTokens,
				}
			}
		}

		if len(resp.Choices) == 0 {
			continue
		}

		delta := resp.Choices[0].Delta
		content += delta.Content
		// Collect reasoning deltas so a think-stop (reasoning with no
		// answer/tool call) can be reported truthfully at the end.
		reasoning += delta.ReasoningContent
		if send != nil {
			// Live consumers see each delta as it arrives: reasoning as
			// thinking (the TUI's reasoning indicator), answer text as
			// tokens. Usage stays with the assembled result — the agent
			// emits it once per completed call.
			if delta.ReasoningContent != "" {
				if !send(StreamEvent{Type: "thinking", Content: delta.ReasoningContent}) {
					send = nil // consumer gone; keep assembling
				}
			}
			if send != nil && delta.Content != "" {
				if !send(StreamEvent{Type: "token", Content: delta.Content}) {
					send = nil
				}
			}
		}

		for _, tc := range delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if _, exists := toolCallParts[idx]; !exists {
				toolCallParts[idx] = &openai.ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: openai.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			} else {
				existing := toolCallParts[idx]
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Type != "" {
					existing.Type = tc.Type
				}
				existing.Function.Arguments += tc.Function.Arguments
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
			}
		}
	}

	for i := 0; i < len(toolCallParts); i++ {
		if tc, ok := toolCallParts[i]; ok {
			toolCalls = append(toolCalls, *tc)
		}
	}

	if len(toolCalls) == 0 && content == "" {
		if reasoning != "" {
			// Same truthful think-stop report as the non-streaming path.
			return nil, fmt.Errorf("%w (%d reasoning chars)", ErrThinkStop, len(reasoning))
		}
		return nil, fmt.Errorf("%w: empty response from model", errEmptyStreamResponse)
	}

	return &CompletionResult{
		Content:   content,
		ToolCalls: toolCalls,
		Usage:     usage,
	}, nil
}

func (c *Client) Chat(ctx context.Context, messages []openai.ChatCompletionMessage) (<-chan StreamEvent, error) {
	return c.ChatWithOptions(ctx, messages, ChatOptions{})
}

func (c *Client) ChatWithOptions(ctx context.Context, messages []openai.ChatCompletionMessage, opts ChatOptions) (<-chan StreamEvent, error) {
	req := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: messages,
		Stream:   true,
		StreamOptions: &openai.StreamOptions{
			IncludeUsage: true,
		},
	}
	c.applyEffort(&req, opts.Effort)

	stream, err := c.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start stream: %w", err)
	}

	ch := make(chan StreamEvent)
	go func() {
		defer close(ch)
		defer stream.Close()
		stalled := newStreamWatchdog(ctx, stream)
		defer stalled.stop()

		parser := newThinkParser()
		for {
			resp, err := stream.Recv()
			stalled.reset()
			if stalled.fired() {
				select {
				case ch <- StreamEvent{Type: "error", Content: ErrStreamIdle.Error()}:
				case <-ctx.Done():
				}
				return
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				select {
				case ch <- StreamEvent{Type: "error", Content: err.Error()}:
				case <-ctx.Done():
				}
				return
			}

			// Usage data arrives in the final chunk when include_usage is true.
			if resp.Usage != nil && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 || resp.Usage.TotalTokens > 0) {
				select {
				case ch <- StreamEvent{
					Type:             "usage",
					PromptTokens:     resp.Usage.PromptTokens,
					CompletionTokens: resp.Usage.CompletionTokens,
					TotalTokens:      resp.Usage.TotalTokens,
				}:
				case <-ctx.Done():
					return
				}
			}

			if len(resp.Choices) == 0 {
				continue
			}

			delta := resp.Choices[0].Delta
			// Reasoning deltas (reasoning_content — the GLM/DeepSeek/Qwen
			// dialect) are a distinct phase from answer content: surface them
			// as thinking events so the UI can show its reasoning indicator,
			// exactly like inline <think> tags and Anthropic thinking deltas.
			if delta.ReasoningContent != "" {
				select {
				case ch <- StreamEvent{Type: "thinking", Content: delta.ReasoningContent}:
				case <-ctx.Done():
					return
				}
			}
			if delta.Content == "" {
				continue
			}

			events := parser.feed(delta.Content)
			for _, ev := range events {
				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// boundedHTTPClient clones the default transport with a response-header
// timeout of completionAttemptTimeout. For streaming requests the headers
// arrive before generation starts, so this only guards against a server that
// accepts the connection and never answers; body-phase liveness is the
// watchdog's job.
func boundedHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = completionAttemptTimeout
	return &http.Client{Transport: transport}
}
