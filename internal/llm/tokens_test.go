package llm

import (
	"path/filepath"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// TestCountMessages_PrimerCountedOnce verifies that the per-request reply primer
// is added exactly once by CountMessages, and that CountMessage (the per-message
// variant) does not include it. Regression: per-message estimates summed
// elsewhere inflated totals by 2 tokens per message, triggering premature
// compression.
func TestCountMessages_PrimerCountedOnce(t *testing.T) {
	tc := NewTokenCounter()

	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hello there friend"},
		{Role: openai.ChatMessageRoleAssistant, Content: "general kenobi"},
		{Role: openai.ChatMessageRoleUser, Content: "you are a bold one"},
	}

	var perMessageSum int
	for i := range msgs {
		perMessageSum += tc.CountMessage(&msgs[i])
	}

	full := tc.CountMessages(msgs)

	// CountMessages should equal the sum of per-message counts plus exactly one
	// reply primer (2 tokens) — not one primer per message.
	if full != perMessageSum+2 {
		t.Errorf("CountMessages = %d, want sum(CountMessage)+2 = %d (primer should be counted once)", full, perMessageSum+2)
	}
}

// TestCountCharsFallback_DividesByFour pins the no-tiktoken fallback estimate
// at ~4 characters per token (chars÷4). Regression: the fallback multiplied
// chars×4, inflating estimates ~16× and forcing constant compression.
func TestCountCharsFallback_DividesByFour(t *testing.T) {
	tc := &TokenCounter{} // nil encoding exercises the fallback path

	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "01234567890123456789"}, // 4 + 20 = 24 chars
	}

	want := (len(msgs[0].Content) + len(msgs[0].Role)) / 4
	if got := tc.CountMessages(msgs); got != want {
		t.Errorf("fallback CountMessages = %d, want %d (chars÷4, not chars×4)", got, want)
	}
}

// TestNewTokenCounter_ConstructsOffline asserts that construction succeeds
// without network access: the offline BPE loader (installed in init) must serve
// cl100k_base from embedded assets, and even a load failure must degrade to a
// usable chars÷4 counter instead of an error. A bogus cache dir keeps tiktoken
// from reusing any on-disk state a previous run may have downloaded.
func TestNewTokenCounter_ConstructsOffline(t *testing.T) {
	t.Setenv("TIKTOKEN_CACHE_DIR", filepath.Join(t.TempDir(), "bogus-cache"))

	tc := NewTokenCounter()
	if tc == nil {
		t.Fatal("NewTokenCounter returned nil")
	}

	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "The quick brown fox jumps over the lazy dog."},
	}
	if got := tc.CountMessages(msgs); got <= 0 {
		t.Errorf("CountMessages = %d, want a positive count for non-empty input", got)
	}
	if tc.encoding != nil {
		// The encoding loaded: verify it counts English text at a plausible
		// ratio (well under one token per character).
		chars := len(msgs[0].Content)
		if got := tc.CountMessages(msgs); got > chars {
			t.Errorf("CountMessages = %d exceeds %d input characters; encoding looks wrong", got, chars)
		}
	}
}

// TestNewTokenCounter_NilEncodingDegradesToCharsFallback pins the
// belt-and-braces path: a counter whose encoding failed to load still counts
// via chars÷4 rather than failing construction or counting nothing.
func TestNewTokenCounter_NilEncodingDegradesToCharsFallback(t *testing.T) {
	tc := NewTokenCounter()
	tc.encoding = nil

	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "0123456789012345678901234567890123456789"}, // 40 chars
	}
	want := (len(msgs[0].Content) + len(msgs[0].Role)) / 4
	if got := tc.CountMessages(msgs); got != want {
		t.Errorf("nil-encoding CountMessages = %d, want %d (chars÷4 fallback)", got, want)
	}
}
