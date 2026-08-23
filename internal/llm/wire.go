package llm

import (
	"context"

	"github.com/sashabaranov/go-openai"
)

// WireClient is the seam between agent code and a provider wire protocol: the
// methods agent code calls on a model client, so a native non-OpenAI wire
// implementation (the Anthropic Messages client in anthropic.go) can be
// substituted without touching call sites. The OpenAI-compatible *Client
// satisfies it; NewWireClient picks the implementation from a ResolvedModel's
// api field.
type WireClient interface {
	// Chat streams a chat completion and returns a channel of events.
	Chat(ctx context.Context, messages []openai.ChatCompletionMessage) (<-chan StreamEvent, error)
	// ChatWithOptions streams a chat completion with optional reasoning effort.
	ChatWithOptions(ctx context.Context, messages []openai.ChatCompletionMessage, opts ChatOptions) (<-chan StreamEvent, error)
	// Complete performs a chat completion, preferring non-streaming for reliability.
	Complete(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (*CompletionResult, error)
	// CompleteWithOptions performs a chat completion with optional MaxTokens
	// and other settings.
	CompleteWithOptions(ctx context.Context, messages []openai.ChatCompletionMessage, opts CompleteOptions) (*CompletionResult, error)
}

// Compile-time checks that both wire implementations satisfy the seam.
var (
	_ WireClient = (*Client)(nil)
	_ WireClient = (*anthropicClient)(nil)
)

// NewWireClient builds the wire client a resolved model's api field selects:
// "anthropic-messages" gets the native Anthropic Messages client; anything
// else ("" / "openai-completions") gets the OpenAI-compatible client.
func NewWireClient(resolved ResolvedModel) WireClient {
	if resolved.API == "anthropic-messages" {
		return newAnthropicClient(resolved)
	}
	return NewClientWithCompat(resolved.BaseURL, resolved.APIKey, resolved.ModelID, resolved.ReasoningStyle, resolved.Compat)
}
