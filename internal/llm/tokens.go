package llm

import (
	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"github.com/sashabaranov/go-openai"
)

// init installs the offline BPE loader so tiktoken never HTTP-fetches the
// encoding files — the CLI must work with no network access.
func init() {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
}

// TokenCounter estimates token counts for message arrays.
type TokenCounter struct {
	encoding *tiktoken.Tiktoken
}

// NewTokenCounter creates a counter using the cl100k_base encoding
// (OpenAI-compatible). Construction never fails: if the encoding cannot be
// loaded (e.g. an exotic runtime without the embedded asset), the returned
// counter carries a nil encoding and degrades to the chars÷4 heuristic, so
// compression paths can't hard-fail here.
func NewTokenCounter() *TokenCounter {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return &TokenCounter{}
	}
	return &TokenCounter{encoding: enc}
}

// CountMessages returns the estimated token count for a slice of messages.
// Uses tiktoken for OpenAI-compatible models; falls back to a chars÷4 heuristic
// (a rough chars-per-token estimate) if tiktoken fails.
func (tc *TokenCounter) CountMessages(msgs []openai.ChatCompletionMessage) int {
	if tc.encoding == nil {
		return countCharsFallback(msgs)
	}
	total := 0
	for i := range msgs {
		total += tc.CountMessage(&msgs[i])
	}
	total += 2 // reply primer — counted once per request, not per message
	return total
}

// CountMessage returns the estimated token count for a single message,
// including the per-message overhead but NOT the per-request reply primer.
// Use this when accumulating per-message estimates; summing CountMessages over
// individual messages would add the reply primer once per message and inflate
// the total (causing premature compression).
func (tc *TokenCounter) CountMessage(m *openai.ChatCompletionMessage) int {
	if tc.encoding == nil {
		return countCharsFallback([]openai.ChatCompletionMessage{*m})
	}
	total := 4 // per-message overhead
	total += len(tc.encoding.Encode(m.Content, nil, nil))
	total += len(tc.encoding.Encode(m.Role, nil, nil))
	total += len(tc.encoding.Encode(m.ToolCallID, nil, nil))
	for _, tcall := range m.ToolCalls {
		total += len(tc.encoding.Encode(tcall.ID, nil, nil))
		total += len(tc.encoding.Encode(string(tcall.Type), nil, nil))
		total += len(tc.encoding.Encode(tcall.Function.Name, nil, nil))
		total += len(tc.encoding.Encode(tcall.Function.Arguments, nil, nil))
	}
	return total
}

func countCharsFallback(msgs []openai.ChatCompletionMessage) int {
	// Roughly 4 characters per token for English text.
	const charsPerToken = 4
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / charsPerToken
		total += len(m.Role) / charsPerToken
		total += len(m.ToolCallID) / charsPerToken
		for _, tc := range m.ToolCalls {
			total += (len(tc.ID) + len(tc.Type) + len(tc.Function.Name) + len(tc.Function.Arguments)) / charsPerToken
		}
	}
	return total
}
