package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/tools"
)

// ----------------------------------------------------------------------------
// Fake summarizer infrastructure
// ----------------------------------------------------------------------------

// fakeSummarizerRequest records an invocation of the fake summarizer.
type fakeSummarizerRequest struct {
	ModelAlias          string
	MessageCount        int
	MaxOutputTokens     int
	MinimumUsefulTokens int
	Retry               bool
	Messages            []openai.ChatCompletionMessage
}

// fakeSummarizer is a test double that returns canned summaries.
type fakeSummarizer struct {
	returnContent    string
	returnContents   []string
	returnErr        error
	returnErrs       []error
	missingUsageCall map[int]bool
	recorded         []fakeSummarizerRequest
	blockUntil       <-chan struct{} // if set, blocks until closed or context cancelled
}

func (f *fakeSummarizer) Summarize(ctx context.Context, req CompressionSummaryRequest) (*CompressionSummaryResult, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
	default:
	}
	if f.blockUntil != nil {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while blocked: %w", ctx.Err())
		case <-f.blockUntil:
		}
	}
	f.recorded = append(f.recorded, fakeSummarizerRequest{
		ModelAlias:          req.ModelAlias,
		MessageCount:        len(req.Messages),
		MaxOutputTokens:     req.MaxOutputTokens,
		MinimumUsefulTokens: req.MinimumUsefulTokens,
		Retry:               req.Retry,
		Messages:            req.Messages,
	})
	callIndex := len(f.recorded) - 1
	if callIndex < len(f.returnErrs) && f.returnErrs[callIndex] != nil {
		return nil, f.returnErrs[callIndex]
	}
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	content := f.returnContent
	if len(f.returnContents) > 0 {
		index := callIndex
		if index >= len(f.returnContents) {
			index = len(f.returnContents) - 1
		}
		content = f.returnContents[index]
	}
	if content == "" {
		content = "Fake summary of conversation."
	}
	result := &CompressionSummaryResult{
		Content:          content,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	if f.missingUsageCall[callIndex] {
		result.PromptTokens = 0
		result.CompletionTokens = 0
		result.TotalTokens = 0
	}
	return result, nil
}

// fakeSummarizerFactory always returns the same fake summarizer.
type fakeSummarizerFactory struct {
	s *fakeSummarizer
}

func (f *fakeSummarizerFactory) NewSummarizer(_ llm.ResolvedModel) Summarizer {
	return f.s
}

// ----------------------------------------------------------------------------
// Test agent factory
// ----------------------------------------------------------------------------

func newTestAgentWithFakeSummarizer(t *testing.T, fake *fakeSummarizer) *Agent {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	store, err := memory.OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctxLen := 4096
	supportsTools := true
	cfg := &config.Config{
		Persona: config.PersonaConfig{
			Name:         "TestAgent",
			SystemPrompt: "You are a test assistant.",
		},
		Providers: []config.ProviderConfig{
			{
				Name:    "test-provider",
				BaseURL: "http://localhost:9999", // unreachable; tests never call real LLM
				APIKey:  "fake",
				Models: map[string]config.ModelConfig{
					"test-model": {
						SupportsTools: &supportsTools,
						ContextLength: &ctxLen,
					},
				},
			},
		},
		Compression: config.CompressionConfig{
			Enabled:          true,
			Threshold:        0.80,
			TargetRatio:      0.20,
			MinSummaryTokens: 1000,
			MaxSummaryTokens: 12000,
			Model:            "test-model",
		},
	}

	registry := llm.NewRegistry(cfg)
	toolReg := tools.NewRegistry(t.TempDir(), "", "", nil)
	a := New(cfg, store, registry, toolReg)
	a.summarizers = &fakeSummarizerFactory{s: fake}
	return a
}

// ----------------------------------------------------------------------------
// validateProviderPayload tests
// ----------------------------------------------------------------------------

func TestValidateProviderPayload_Clean(t *testing.T) {
	id1 := "call_1"
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{
				{ID: id1, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read", Arguments: "{}"}},
			},
		},
		{Role: "tool", Content: "file contents", ToolCallID: id1},
		{Role: openai.ChatMessageRoleAssistant, Content: "done"},
	}
	if err := validateProviderPayload(msgs); err != nil {
		t.Errorf("expected no error for valid payload, got: %v", err)
	}
}

func TestValidateProviderPayload_OrphanToolMessage(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		// tool message with no preceding assistant tool_calls
		{Role: "tool", Content: "orphan", ToolCallID: "nonexistent"},
	}
	err := validateProviderPayload(msgs)
	if err == nil {
		t.Error("expected error for orphan tool message, got nil")
	}
}

func TestValidateProviderPayload_MissingToolResult(t *testing.T) {
	id1 := "call_1"
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{
				{ID: id1, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read", Arguments: "{}"}},
			},
		},
		// Missing tool result for id1.
		{Role: openai.ChatMessageRoleAssistant, Content: "done"},
	}
	err := validateProviderPayload(msgs)
	if err == nil {
		t.Error("expected error for missing tool result, got nil")
	}
}

func TestValidateProviderPayload_EmptyToolCallID(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		// tool message with empty ID
		{Role: "tool", Content: "result", ToolCallID: ""},
	}
	err := validateProviderPayload(msgs)
	if err == nil {
		t.Error("expected error for empty ToolCallID, got nil")
	}
}

func TestValidateProviderPayload_RequiresPrecedingAssistantGroup(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: "tool", Content: "too early", ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
			ID: "call_1", Type: openai.ToolTypeFunction,
		}}},
	}
	if err := validateProviderPayload(msgs); err == nil {
		t.Fatal("expected tool result before assistant call to be rejected")
	}
}

func TestValidateProviderPayload_RejectsDuplicateToolCallIDs(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}},
		{Role: "tool", Content: "one", ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}},
		{Role: "tool", Content: "two", ToolCallID: "call_1"},
	}
	if err := validateProviderPayload(msgs); err == nil {
		t.Fatal("expected duplicate tool call IDs to be rejected")
	}
}

func TestValidateProviderPayload_RejectsDuplicateToolResults(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}},
		{Role: openai.ChatMessageRoleTool, Content: "one", ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleTool, Content: "duplicate", ToolCallID: "call_1"},
	}
	if err := validateProviderPayload(msgs); err == nil {
		t.Fatal("expected duplicate tool results to be rejected")
	}
}

func TestValidateProviderPayload_RejectsPartiallyResolvedToolGroup(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: openai.ToolTypeFunction},
			{ID: "call_2", Type: openai.ToolTypeFunction},
		}},
		{Role: openai.ChatMessageRoleTool, Content: "one", ToolCallID: "call_1"},
	}
	if err := validateProviderPayload(msgs); err == nil {
		t.Fatal("expected a tool group with one missing result to be rejected")
	}
}

// ----------------------------------------------------------------------------
// groupMessagesForCompression tests
// ----------------------------------------------------------------------------

func TestGroupMessages_UserAssistantTool(t *testing.T) {
	id1 := "call_1"
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "q1"},
		{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{
				{ID: id1, Type: openai.ToolTypeFunction},
			},
		},
		{Role: "tool", Content: "result", ToolCallID: id1},
		{Role: openai.ChatMessageRoleUser, Content: "q2"},
		{Role: openai.ChatMessageRoleAssistant, Content: "a2"},
	}
	groups := groupMessagesForCompression(msgs)
	if len(groups) != 4 {
		t.Fatalf("expected 4 groups, got %d: %+v", len(groups), groups)
	}
	// Group 1: user q1
	if groups[0].Kind != "user" || groups[0].Start != 0 || groups[0].End != 1 {
		t.Errorf("group[0] wrong: %+v", groups[0])
	}
	// Group 2: assistant+tool = indices 1,2
	if groups[1].Kind != "assistant_tool" || groups[1].Start != 1 || groups[1].End != 3 {
		t.Errorf("group[1] wrong: %+v", groups[1])
	}
	// Group 3: user q2
	if groups[2].Kind != "user" || groups[2].Start != 3 || groups[2].End != 4 {
		t.Errorf("group[2] wrong: %+v", groups[2])
	}
	// Group 4: assistant a2
	if groups[3].Kind != "assistant" || groups[3].Start != 4 || groups[3].End != 5 {
		t.Errorf("group[3] wrong: %+v", groups[3])
	}
}

func TestTruncateIndexedToBudget_DropsWholeMultiCallGroup(t *testing.T) {
	counter := llm.NewTokenCounter()
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "system"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current request"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read"}},
			{ID: "call_2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "read"}},
		}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_1", Content: strings.Repeat("one ", 100)}},
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_2", Content: strings.Repeat("two ", 100)}},
	}
	budget := counter.CountMessages(toRawMessages(msgs[:2]))
	got := truncateIndexedToBudget(msgs, budget, counter)
	if len(got) != 2 || got[0].Msg.Role != openai.ChatMessageRoleSystem || got[1].Msg.Role != openai.ChatMessageRoleUser {
		t.Fatalf("fallback split a tool group instead of dropping it: %+v", got)
	}
	if tokens := counter.CountMessages(toRawMessages(got)); tokens > budget {
		t.Fatalf("truncated payload exceeds budget: got %d, budget %d", tokens, budget)
	}
	if err := validateProviderPayload(toRawMessages(got)); err != nil {
		t.Fatalf("truncated payload is invalid: %v", err)
	}
}

func TestTruncateIndexedToBudget_DropsOlderGroupsBeforeCurrentWork(t *testing.T) {
	counter := llm.NewTokenCounter()
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "system"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("old request ", 100)}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "old_call", Type: openai.ToolTypeFunction}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "old_call", Content: strings.Repeat("old output ", 100)}},
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current request"}},
		{Seq: 5, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "current_call", Type: openai.ToolTypeFunction}}}},
		{Seq: 6, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "current_call", Content: "current output"}},
	}
	want := []indexedMessage{msgs[0], msgs[4], msgs[5], msgs[6]}
	budget := counter.CountMessages(toRawMessages(want))
	got := truncateIndexedToBudget(msgs, budget, counter)
	if len(got) != len(want) {
		t.Fatalf("truncated message count: got %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Seq != want[i].Seq || got[i].Msg.Role != want[i].Msg.Role {
			t.Fatalf("message %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := validateProviderPayload(toRawMessages(got)); err != nil {
		t.Fatalf("truncated payload is invalid: %v", err)
	}
	if tokens := counter.CountMessages(toRawMessages(got)); tokens > budget {
		t.Fatalf("truncated payload exceeds budget: got %d, budget %d", tokens, budget)
	}
}

// ----------------------------------------------------------------------------
// compressIfNeeded with fake summarizer
// ----------------------------------------------------------------------------

func TestCompressIfNeeded_NoCompression_BelowThreshold(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "hello"}},
		{Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "world"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 128000, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeNone {
		t.Errorf("expected none, got %s", result.Outcome)
	}
	if len(fake.recorded) != 0 {
		t.Errorf("expected 0 summarizer calls, got %d", len(fake.recorded))
	}
}

func TestCompressIfNeeded_TurnStart_CallsSummarizer(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Summary of old stuff."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	// Build a message list large enough to exceed contextLength*threshold.
	// contextLength=100, threshold=0.80 -> budget=80. We need >80 tokens.
	// Each message ~4 tokens overhead + content, so make content long.
	bigContent := strings.Repeat("word ", 30) // ~30 tokens
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "System prompt."}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current question?"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Errorf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}
	if len(fake.recorded) != 1 {
		t.Errorf("expected 1 summarizer call, got %d", len(fake.recorded))
	}
	// Verify target_ratio enforcement: max_output_tokens should be clamped to MinSummaryTokens.
	if fake.recorded[0].MaxOutputTokens != 1000 {
		t.Errorf("expected MaxOutputTokens=1000 (clamped to min_summary_tokens), got %d", fake.recorded[0].MaxOutputTokens)
	}
	// Result should contain compression summary message.
	foundSummary := false
	for _, im := range result.Messages {
		if im.Kind == "compression_summary" {
			foundSummary = true
			if !strings.Contains(im.Msg.Content, "Summary of old stuff.") {
				t.Errorf("summary message missing expected content: %q", im.Msg.Content)
			}
		}
	}
	if !foundSummary {
		t.Error("no compression_summary message in result")
	}
	// System prompt must still be first.
	if result.Messages[0].Kind != "system" {
		t.Errorf("first message is not system prompt: %+v", result.Messages[0])
	}
	// Latest user turn must be preserved.
	lastMsg := result.Messages[len(result.Messages)-1]
	if lastMsg.Msg.Content != "Current question?" {
		t.Errorf("expected latest user turn preserved, got: %q", lastMsg.Msg.Content)
	}
	// Payload must be valid.
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Errorf("compressed payload fails validation: %v", err)
	}
}

func TestCompressIfNeeded_MidLoop_CreatesActiveTurnCheckpoint(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Read the large report and found the failing row."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigContent := strings.Repeat("word ", 120)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Find the failing row."}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{
				ID: "call_1", Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: "file_read", Arguments: `{"path":"report.txt"}`},
			}},
		}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", ToolCallID: "call_1", Content: bigContent}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected semantic mid-loop compression, got %s (%s)", result.Outcome, result.FallbackReason)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("expected one summarizer call, got %d", len(fake.recorded))
	}
	if len(result.Messages) != 3 || result.Messages[1].Msg.Content != "Find the failing row." {
		t.Fatalf("current user was not retained verbatim: %+v", result.Messages)
	}
	if result.Messages[2].Kind != activeTurnSummaryKind {
		t.Fatalf("expected active checkpoint, got %+v", result.Messages[2])
	}
	provider := toRawMessages(result.Messages)
	if len(provider) != 2 || !strings.Contains(provider[1].Content, activeTurnSummaryWrapper) {
		t.Fatalf("checkpoint was not merged into provider user message: %+v", provider)
	}
	if err := validateProviderPayload(provider); err != nil {
		t.Fatalf("checkpoint payload invalid: %v", err)
	}
	if result.FirstKeptSeq != 0 {
		t.Fatalf("transient checkpoint must not claim a durable boundary: %d", result.FirstKeptSeq)
	}
}

func TestCompressIfNeeded_MidLoop_ReplacesAndChainsCheckpoint(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "checkpoint one"}
	a := newTestAgentWithFakeSummarizer(t, fake)
	large := strings.Repeat("work ", 120)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Complete the task."}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", ToolCallID: "call_1", Content: large}},
	}

	first := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeMidLoop)
	if first.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("first checkpoint: %s (%s)", first.Outcome, first.FallbackReason)
	}
	fake.returnContent = "checkpoint two"
	secondInput := append([]indexedMessage(nil), first.Messages...)
	secondInput = append(secondInput,
		indexedMessage{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_2", Type: openai.ToolTypeFunction}}}},
		indexedMessage{Seq: 5, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", ToolCallID: "call_2", Content: large}},
	)
	second := a.compressIfNeeded(context.Background(), "", secondInput, "test-model", 100, CompressionModeMidLoop)
	if second.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("second checkpoint: %s (%s)", second.Outcome, second.FallbackReason)
	}
	if len(fake.recorded) != 2 {
		t.Fatalf("summarizer calls: got %d, want 2", len(fake.recorded))
	}
	var secondBatch strings.Builder
	for _, msg := range fake.recorded[1].Messages {
		secondBatch.WriteString(msg.Content)
	}
	if !strings.Contains(secondBatch.String(), "checkpoint one") {
		t.Fatalf("second checkpoint did not chain the first: %q", secondBatch.String())
	}
	checkpointCount := 0
	for _, msg := range second.Messages {
		if msg.Kind == activeTurnSummaryKind {
			checkpointCount++
			if msg.Msg.Content != "checkpoint two" {
				t.Fatalf("stale checkpoint content: %q", msg.Msg.Content)
			}
		}
	}
	if checkpointCount != 1 {
		t.Fatalf("checkpoint count: got %d, want 1", checkpointCount)
	}
	provider := toRawMessages(second.Messages)
	if strings.Count(provider[len(provider)-1].Content, activeTurnSummaryWrapper) != 1 {
		t.Fatalf("provider prompt accumulated checkpoint wrappers: %q", provider[len(provider)-1].Content)
	}
}

func largeActiveTurnFixture(groupCount int) []indexedMessage {
	sizes := make([]int, groupCount)
	for i := range sizes {
		sizes[i] = 5000
	}
	return activeTurnFixtureWithGroupSizes(sizes)
}

func activeTurnFixtureWithGroupSizes(sizes []int) []indexedMessage {
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "System prompt."}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Repair the compiler and preserve all evidence."}},
	}
	for i, size := range sizes {
		marker := fmt.Sprintf("GROUP-%02d-VERBATIM ", i)
		msgs = append(msgs, indexedMessage{
			Seq:  2 + i,
			Kind: "thread_message",
			Msg: openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: marker + strings.Repeat("analysis ", size),
			},
		})
	}
	return msgs
}

func TestCompressIfNeeded_MidLoop_UnevenGroupsCannotCrossRecentTailFloor(t *testing.T) {
	fake := &fakeSummarizer{returnContent: strings.Repeat("checkpoint ", 1200)}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	a.cfg.Compression.PostCompressionRatio = 0.45
	// The three newest groups total only ~3K. The immediately preceding 15K
	// group must remain raw as an indivisible boundary group; the old 30K group
	// is the only prefix eligible for summarization.
	msgs := activeTurnFixtureWithGroupSizes([]int{30000, 15000, 1000, 1000, 1000})

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected uneven-prefix compression, got %+v", result)
	}
	floor := activeTurnRecentTailTarget(65536, 0)
	if result.RecentTailTokens < floor || result.AfterTokens < floor {
		t.Fatalf("compression crossed raw tail floor: after=%d raw_tail=%d floor=%d", result.AfterTokens, result.RecentTailTokens, floor)
	}
	if result.TargetTokens != activeTurnTargetBudget(65536, (65536*70)/100, 0.45) {
		t.Fatalf("target provenance mismatch: %+v", result)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("summarizer calls: got %d, want 1", len(fake.recorded))
	}
	batch := formatMessagesForCompression(fake.recorded[0].Messages)
	if !strings.Contains(batch, "GROUP-00-VERBATIM") || strings.Contains(batch, "GROUP-01-VERBATIM") {
		t.Fatalf("uneven boundary group was summarized instead of retained")
	}
	for i := 1; i < 5; i++ {
		want := msgs[2+i].Msg.Content
		got := result.Messages[len(result.Messages)-(5-i)].Msg.Content
		if got != want {
			t.Fatalf("protected uneven group %d was not byte-for-byte retained", i)
		}
	}
}

func TestCompressIfNeeded_MidLoop_NoPrefixFailsSafeInsteadOfCollapsingTail(t *testing.T) {
	fake := &fakeSummarizer{returnContent: strings.Repeat("checkpoint ", 1200)}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	// Meeting the ~10K floor requires protecting the 45K boundary group plus
	// three tiny newest groups, leaving no older prefix to summarize.
	msgs := activeTurnFixtureWithGroupSizes([]int{45000, 1000, 1000, 1000})

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeError || !errors.Is(result.Err, ErrUnsafeProviderPayload) {
		t.Fatalf("unachievable tail floor must stop before provider call: %+v", result)
	}
	if !strings.Contains(result.FallbackReason, "recent_tail_floor_violation") {
		t.Fatalf("missing tail-floor diagnostic: %q", result.FallbackReason)
	}
	if result.AfterTokens != result.BeforeTokens || result.AfterTokens < activeTurnRecentTailTarget(65536, 0) {
		t.Fatalf("unsafe fallback collapsed context: before=%d after=%d", result.BeforeTokens, result.AfterTokens)
	}
	if len(fake.recorded) != 0 {
		t.Fatalf("no compressible prefix should not call summarizer: %d", len(fake.recorded))
	}
}

func TestCompressIfNeeded_MidLoop_PreservesRecentRawTailAndTargetsHalfContext(t *testing.T) {
	fake := &fakeSummarizer{returnContent: strings.Repeat("checkpoint ", 1200)}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	msgs := largeActiveTurnFixture(12)

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed result, got %s (%s)", result.Outcome, result.FallbackReason)
	}
	if got := activeTurnRecentTailTarget(65536, 0); got < 8192 || got > 12288 {
		t.Fatalf("65K automatic recent tail outside 8-12K range: %d", got)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("summarizer calls: got %d, want 1", len(fake.recorded))
	}
	if fake.recorded[0].MinimumUsefulTokens < 768 {
		t.Fatalf("large prefix did not request a substantive checkpoint: %+v", fake.recorded[0])
	}

	// The oldest work was included in the summary batch and removed from the
	// provider payload, while a multi-group recent suffix survives byte-for-byte.
	batchText := formatMessagesForCompression(fake.recorded[0].Messages)
	if !strings.Contains(batchText, "GROUP-00-VERBATIM") || strings.Contains(batchText, "GROUP-11-VERBATIM") {
		t.Fatalf("summarizer batch did not contain only the old prefix")
	}
	if result.CompressedCount <= 0 || result.CompressedCount >= len(msgs)-2 {
		t.Fatalf("compression did not split old prefix from recent tail: compressed=%d", result.CompressedCount)
	}
	for i := 9; i < 12; i++ {
		want := msgs[2+i].Msg.Content
		got := result.Messages[len(result.Messages)-(12-i)].Msg.Content
		if got != want {
			t.Fatalf("recent group %d changed during compression", i)
		}
	}
	hardBudget := (65536 * 70) / 100
	if result.AfterTokens > activeTurnTargetBudget(65536, hardBudget, 0.50) {
		t.Fatalf("post-compression target missed: after=%d target=%d", result.AfterTokens, activeTurnTargetBudget(65536, hardBudget, 0.50))
	}
	if result.TargetTokens != activeTurnTargetBudget(65536, hardBudget, 0.50) {
		t.Fatalf("configured post-compression target missing from result: %+v", result)
	}
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Fatalf("split payload invalid: %v", err)
	}
}

func TestCompressIfNeeded_MidLoop_ShortSummaryRetriesThenFallsBack(t *testing.T) {
	fake := &fakeSummarizer{returnContents: []string{"tiny", "still tiny"}}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	msgs := largeActiveTurnFixture(12)

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeFallback || !result.FallbackUsed {
		t.Fatalf("short checkpoint should use deterministic fallback: %+v", result)
	}
	if !strings.Contains(result.FallbackReason, "summary_too_short_after_retry") {
		t.Fatalf("missing short-summary diagnostic: %q", result.FallbackReason)
	}
	if len(fake.recorded) != 2 || fake.recorded[0].Retry || !fake.recorded[1].Retry {
		t.Fatalf("expected one explicit retry: %+v", fake.recorded)
	}
	if result.SummaryPromptTokens != 200 || result.SummaryCompletionTokens != 100 || result.SummaryTotalTokens != 300 {
		t.Fatalf("retry usage was not aggregated: %+v", result)
	}
	if result.SummaryCallCount != 2 || result.SummaryUsageCallCount != 2 {
		t.Fatalf("retry call coverage was not recorded: %+v", result)
	}
	if result.AfterTokens > result.BudgetTokens {
		t.Fatalf("fallback exceeds hard budget: after=%d budget=%d", result.AfterTokens, result.BudgetTokens)
	}
	if result.Messages[len(result.Messages)-1].Msg.Content != msgs[len(msgs)-1].Msg.Content {
		t.Fatal("deterministic fallback did not preserve newest raw group")
	}
	for _, msg := range result.Messages {
		if msg.Kind == activeTurnSummaryKind {
			t.Fatal("rejected short checkpoint leaked into provider payload")
		}
	}
}

func TestCompressIfNeeded_MidLoop_ShortSummaryRetryCanRecover(t *testing.T) {
	fake := &fakeSummarizer{returnContents: []string{"tiny", strings.Repeat("expanded checkpoint ", 1200)}}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	msgs := largeActiveTurnFixture(12)

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expanded retry should recover: %+v", result)
	}
	if len(fake.recorded) != 2 || !fake.recorded[1].Retry {
		t.Fatalf("retry metadata missing: %+v", fake.recorded)
	}
	if result.SummaryTotalTokens != 300 {
		t.Fatalf("successful retry usage was not aggregated: %+v", result)
	}
	if result.SummaryCallCount != 2 || result.SummaryUsageCallCount != 2 {
		t.Fatalf("successful retry call coverage = %d/%d, want 2/2", result.SummaryUsageCallCount, result.SummaryCallCount)
	}
	terminal := buildCompressionEndEvent(result)
	usage := buildCompressionAuxiliaryUsageEvent(result)
	if terminal.SummaryCallCount != 2 || terminal.SummaryUsageCallCount != 2 || usage == nil || usage.UsageCallCount != 2 || usage.TotalTokens != 300 {
		t.Fatalf("successful retry events lost call coverage: terminal=%+v usage=%+v", terminal, usage)
	}
}

func TestCompressIfNeeded_MidLoop_ShortSummaryRetryErrorKeepsPartialUsageCoverage(t *testing.T) {
	fake := &fakeSummarizer{
		returnContents: []string{"tiny"},
		returnErrs:     []error{nil, errors.New("retry unavailable")},
	}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	msgs := largeActiveTurnFixture(12)

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeError || !strings.Contains(result.FallbackReason, "retry_failed") {
		t.Fatalf("retry error did not reach compression result: %+v", result)
	}
	if result.SummaryCallCount != 2 || result.SummaryUsageCallCount != 1 {
		t.Fatalf("retry error coverage = %d/%d, want 1/2", result.SummaryUsageCallCount, result.SummaryCallCount)
	}
	if result.SummaryPromptTokens != 100 || result.SummaryCompletionTokens != 50 || result.SummaryTotalTokens != 150 {
		t.Fatalf("covered first-call usage was not retained: %+v", result)
	}
	ev := buildCompressionErrorEvent(result)
	if ev.SummaryCallCount != 2 || ev.SummaryUsageCallCount != 1 {
		t.Fatalf("retry error event lost coverage: %+v", ev)
	}
	usage := buildCompressionAuxiliaryUsageEvent(result)
	if usage == nil || usage.UsageCallCount != 1 || usage.TotalTokens != 150 {
		t.Fatalf("retry error auxiliary aggregate lost partial coverage: %+v", usage)
	}
}

func TestCompressIfNeeded_MidLoop_SuccessfulRetryMarksMissingUsageIncomplete(t *testing.T) {
	fake := &fakeSummarizer{
		returnContents:   []string{"tiny", strings.Repeat("expanded checkpoint ", 1200)},
		missingUsageCall: map[int]bool{1: true},
	}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	result := a.compressIfNeeded(context.Background(), "", largeActiveTurnFixture(12), "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("retry should succeed independently of usage telemetry: %+v", result)
	}
	if result.SummaryCallCount != 2 || result.SummaryUsageCallCount != 1 || result.SummaryTotalTokens != 150 {
		t.Fatalf("missing retry usage was presented as complete: %+v", result)
	}
}

func TestCompressIfNeeded_MidLoop_QualityFloorUsesTrimmedToolPayload(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "small but sufficient after trimming"}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Investigate the output."}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_huge", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "shell_exec"}}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_huge", Content: strings.Repeat("oversized evidence ", 20000)}},
	}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, indexedMessage{Seq: 4 + i, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("RECENT-%d ", i) + strings.Repeat("analysis ", 5000),
		}})
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("trimmed summarizer payload should not force a bogus retry/fallback: %+v", result)
	}
	if len(fake.recorded) != 1 || fake.recorded[0].Retry {
		t.Fatalf("raw oversized output inflated quality floor and caused retry: %+v", fake.recorded)
	}
	if fake.recorded[0].MinimumUsefulTokens != 0 {
		t.Fatalf("quality floor used untrimmed tool output: %+v", fake.recorded[0])
	}
	if result.PrunedToolOutputs == 0 {
		t.Fatal("oversized summarizer tool payload was not recorded as trimmed")
	}
}

func TestCompressIfNeeded_MidLoop_SummarizerErrorUsesGroupSafeFallback(t *testing.T) {
	fake := &fakeSummarizer{returnErr: errors.New("summary unavailable")}
	a := newTestAgentWithFakeSummarizer(t, fake)
	large := strings.Repeat("output ", 150)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current task"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", ToolCallID: "call_1", Content: large}},
	}
	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeError || result.Err == nil || !result.FallbackUsed {
		t.Fatalf("unexpected error fallback: %+v", result)
	}
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Fatalf("summarizer error produced malformed fallback: %v", err)
	}
	for _, msg := range result.Messages {
		if msg.Msg.Role == "tool" {
			t.Fatal("fallback retained an orphan tool result")
		}
	}
}

func TestCompressIfNeeded_MidLoop_PostCallFallbackPreservesSummaryTelemetry(t *testing.T) {
	fake := &fakeSummarizer{returnContent: strings.Repeat("ineffective summary ", 500)}
	a := newTestAgentWithFakeSummarizer(t, fake)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current task"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_1", Content: strings.Repeat("output ", 150)}},
	}
	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeFallback || !result.FallbackUsed {
		t.Fatalf("expected post-call fallback, got %+v", result)
	}
	if !result.SummaryAttempted {
		t.Fatal("post-call fallback did not report summary_attempted")
	}
	if result.SummaryPromptTokens != 100 || result.SummaryCompletionTokens != 50 || result.SummaryTotalTokens != 150 {
		t.Fatalf("post-call fallback lost summary usage: %+v", result)
	}
	if result.AfterTokens > result.BudgetTokens {
		t.Fatalf("post-call fallback exceeds budget: after=%d budget=%d", result.AfterTokens, result.BudgetTokens)
	}
}

func TestCompressIfNeeded_MidLoop_ContextCancellation(t *testing.T) {
	block := make(chan struct{})
	fake := &fakeSummarizer{blockUntil: block}
	a := newTestAgentWithFakeSummarizer(t, fake)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current task"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", ToolCallID: "call_1", Content: strings.Repeat("output ", 150)}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan CompressionResult, 1)
	go func() {
		done <- a.compressIfNeeded(ctx, "", msgs, "test-model", 100, CompressionModeMidLoop)
	}()
	cancel()
	result := <-done
	close(block)
	if result.Outcome != CompressionOutcomeError || result.Err == nil {
		t.Fatalf("expected cancelled mid-loop compression error, got %+v", result)
	}
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Fatalf("cancel fallback malformed: %v", err)
	}
}

func TestShouldAttemptCompression_65536ContextBoundary(t *testing.T) {
	cfg := config.CompressionConfig{Enabled: true, Threshold: 0.80}
	const budget = 52428
	counter := llm.NewTokenCounter()
	makeMessages := func(repeats int) []indexedMessage {
		return []indexedMessage{
			{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
			{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Analyze output"}},
			{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction}}}},
			{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_1", Content: strings.Repeat("word ", repeats)}},
		}
	}
	count := func(repeats int) int {
		return counter.CountMessages(toRawMessages(makeMessages(repeats)))
	}
	low, high := 0, 1
	for count(high) <= budget {
		low, high = high, high*2
	}
	for low+1 < high {
		mid := low + (high-low)/2
		if count(mid) <= budget {
			low = mid
		} else {
			high = mid
		}
	}
	below, above := makeMessages(high-1), makeMessages(high)
	if got := counter.CountMessages(toRawMessages(below)); got > budget {
		t.Fatalf("n-1 payload crossed exact budget: got %d, budget %d", got, budget)
	}
	if got := counter.CountMessages(toRawMessages(above)); got <= budget {
		t.Fatalf("n payload did not cross exact budget: got %d, budget %d", got, budget)
	}
	if shouldAttemptCompression(below, 65536, cfg) {
		t.Fatal("n-1 payload should not trigger compression")
	}
	if !shouldAttemptCompression(above, 65536, cfg) {
		t.Fatal("n payload should trigger compression")
	}

	fake := &fakeSummarizer{returnContent: "Boundary checkpoint."}
	a := newTestAgentWithFakeSummarizer(t, fake)
	result := a.compressIfNeeded(context.Background(), "", above, "test-model", 65536, CompressionModeMidLoop)
	if result.BudgetTokens != budget {
		t.Fatalf("reported budget: got %d, want %d", result.BudgetTokens, budget)
	}
	// This exact-boundary fixture is one indivisible assistant group. There is
	// no old prefix that can be summarized while retaining the 65K raw-tail
	// floor, so Sandbar must stop rather than collapse it to a tiny checkpoint.
	if result.Outcome != CompressionOutcomeError || !errors.Is(result.Err, ErrUnsafeProviderPayload) {
		t.Fatalf("single-group boundary should fail safe: %+v", result)
	}
	if result.AfterTokens != result.BeforeTokens {
		t.Fatalf("failed-safe payload changed: before=%d after=%d", result.BeforeTokens, result.AfterTokens)
	}
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Fatalf("compressed boundary payload is invalid: %v", err)
	}
}

func TestCompressIfNeeded_SummarizerError_FallsBack(t *testing.T) {
	fake := &fakeSummarizer{returnErr: errors.New("network error")}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeError {
		t.Errorf("expected error outcome, got %s", result.Outcome)
	}
	if result.Err == nil {
		t.Error("expected non-nil Err on summarizer failure")
	}
	if !result.FallbackUsed {
		t.Error("expected FallbackUsed on summarizer failure")
	}
	// Even on error, messages should be non-nil.
	if result.Messages == nil {
		t.Error("Messages should not be nil even on error")
	}
}

func TestCompressIfNeeded_CompressionDisabled_Truncates(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Enabled = false

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeFallback {
		t.Errorf("expected fallback when compression disabled, got %s", result.Outcome)
	}
	if len(fake.recorded) != 0 {
		t.Error("summarizer should not be called when compression is disabled")
	}
}

func TestCompressIfNeeded_UncompressibleCoreReturnsUnsafePayloadError(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: strings.Repeat("system ", 100)}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("request ", 100)}},
	}
	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeError || !errors.Is(result.Err, ErrUnsafeProviderPayload) {
		t.Fatalf("expected explicit unsafe-payload error, got %+v", result)
	}
	if result.AfterTokens <= result.BudgetTokens {
		t.Fatalf("fixture unexpectedly fit budget: after=%d budget=%d", result.AfterTokens, result.BudgetTokens)
	}
	if len(fake.recorded) != 0 {
		t.Fatalf("uncompressible core should not invoke summarizer: %d calls", len(fake.recorded))
	}
}

func TestCompressIfNeeded_UnknownContextLength_SkipsCompression(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "hello"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 0, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeNone {
		t.Errorf("expected none when contextLength=0, got %s", result.Outcome)
	}
}

func TestCompressIfNeeded_ContextCancellation_PreCancelled(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := a.compressIfNeeded(ctx, "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeError {
		t.Errorf("expected error outcome for pre-cancelled context, got %s", result.Outcome)
	}
	if !result.FallbackUsed {
		t.Error("expected FallbackUsed on pre-cancelled context")
	}
	if result.Err == nil {
		t.Error("expected non-nil Err on pre-cancelled context")
	}
	if len(fake.recorded) != 0 {
		t.Errorf("summarizer should not record a call when context is pre-cancelled, got %d calls", len(fake.recorded))
	}
}

func TestCompressIfNeeded_ContextCancellation_MidCall(t *testing.T) {
	blockCh := make(chan struct{})
	fake := &fakeSummarizer{blockUntil: blockCh}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan CompressionResult, 1)
	go func() {
		resultCh <- a.compressIfNeeded(ctx, "", msgs, "test-model", 100, CompressionModeTurnStart)
	}()

	// Cancel the request while the summarizer is blocked inside its call.
	cancel()
	close(blockCh)

	result := <-resultCh

	if result.Outcome != CompressionOutcomeError {
		t.Errorf("expected error outcome for mid-call cancelled context, got %s", result.Outcome)
	}
	if !result.FallbackUsed {
		t.Error("expected FallbackUsed on mid-call cancelled context")
	}
	if result.Err == nil {
		t.Error("expected non-nil Err on mid-call cancelled context")
	}
	// The fake may or may not have recorded the call depending on race timing,
	// but the outcome must always be an error with fallback.
}

// ----------------------------------------------------------------------------
// indexedMessage type tests
// ----------------------------------------------------------------------------

func TestIndexedMessage_Fields(t *testing.T) {
	im := indexedMessage{
		Seq:       42,
		Synthetic: false,
		Kind:      "thread_message",
		Msg: openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: "hello",
		},
	}
	if im.Seq != 42 {
		t.Errorf("expected Seq=42, got %d", im.Seq)
	}
	if im.Synthetic {
		t.Error("expected Synthetic=false")
	}
	if im.Kind != "thread_message" {
		t.Errorf("expected Kind=thread_message, got %q", im.Kind)
	}
}

func TestWrapMessages_SystemIsSynthetic(t *testing.T) {
	raw := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}
	wrapped := wrapMessages(raw)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 wrapped messages, got %d", len(wrapped))
	}
	if !wrapped[0].Synthetic {
		t.Error("system prompt should be Synthetic=true")
	}
	if wrapped[0].Kind != "system" {
		t.Errorf("system prompt kind wrong: %q", wrapped[0].Kind)
	}
	if wrapped[1].Synthetic {
		t.Error("user message should not be Synthetic")
	}
	if wrapped[1].Kind != "thread_message" {
		t.Errorf("user message kind wrong: %q", wrapped[1].Kind)
	}
}

// ----------------------------------------------------------------------------
// alignCompressionBoundary tests
// ----------------------------------------------------------------------------

func TestAlignBoundary_PreservesLatestUserTurn(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "q1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "a1"},
		{Role: openai.ChatMessageRoleUser, Content: "current question"}, // must never be compressed
	}
	groups := groupMessagesForCompression(msgs)
	// targetIndex points past all messages
	boundary := alignCompressionBoundary(groups, len(msgs), msgs)
	// Boundary must not include the last user message (index 2)
	for i := boundary; i < len(msgs); i++ {
		if msgs[i].Content == "current question" {
			// Good: latest user turn is in the kept region.
			return
		}
	}
	// Also fine if boundary is 0 (nothing to compress).
	if boundary == 0 {
		return
	}
	t.Errorf("latest user turn not protected: boundary=%d, msgs=%d", boundary, len(msgs))
}

// ----------------------------------------------------------------------------
// buildMessages tests
// ----------------------------------------------------------------------------

// TestBuildMessages_ReturnsIndexedMetadata verifies that buildMessages returns
// []indexedMessage with correct Seq values from the DB and no truncation.
func TestBuildMessages_ReturnsIndexedMetadata(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Append three messages to get known seq values.
	content1 := "hello"
	content2 := "world"
	content3 := "third"
	m1, err := a.store.AppendMessage(thread.ID, "user", &content1, nil)
	if err != nil {
		t.Fatalf("append m1: %v", err)
	}
	m2, err := a.store.AppendMessage(thread.ID, "assistant", &content2, nil)
	if err != nil {
		t.Fatalf("append m2: %v", err)
	}
	m3, err := a.store.AppendMessage(thread.ID, "user", &content3, nil)
	if err != nil {
		t.Fatalf("append m3: %v", err)
	}

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")

	// Must have system prompt + 3 persisted messages = 4.
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	// First message is the synthetic system prompt.
	if !msgs[0].Synthetic || msgs[0].Kind != "system" || msgs[0].Seq != 0 {
		t.Errorf("msgs[0] is not synthetic system prompt: %+v", msgs[0])
	}
	if msgs[0].Msg.Role != "system" {
		t.Errorf("msgs[0] role wrong: %q", msgs[0].Msg.Role)
	}

	// Persisted messages carry real DB seq values.
	if msgs[1].Seq != m1.Seq || msgs[1].Synthetic || msgs[1].Kind != "thread_message" {
		t.Errorf("msgs[1] metadata wrong: %+v (want Seq=%d)", msgs[1], m1.Seq)
	}
	if msgs[2].Seq != m2.Seq || msgs[2].Synthetic || msgs[2].Kind != "thread_message" {
		t.Errorf("msgs[2] metadata wrong: %+v (want Seq=%d)", msgs[2], m2.Seq)
	}
	if msgs[3].Seq != m3.Seq || msgs[3].Synthetic || msgs[3].Kind != "thread_message" {
		t.Errorf("msgs[3] metadata wrong: %+v (want Seq=%d)", msgs[3], m3.Seq)
	}

	// Seq values must be monotonically increasing.
	if msgs[1].Seq >= msgs[2].Seq || msgs[2].Seq >= msgs[3].Seq {
		t.Errorf("expected increasing seq: %d %d %d", msgs[1].Seq, msgs[2].Seq, msgs[3].Seq)
	}
}

// TestBuildMessages_NoTruncation verifies that buildMessages does NOT truncate
// messages even when there are many of them (truncation now belongs to
// compressIfNeeded / truncateIndexedMessages).
func TestBuildMessages_NoTruncation(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Add enough messages that old truncation would have kicked in.
	// The test agent has contextLength=4096 and threshold=0.80 -> budget 3277 tokens.
	// We add 20 short messages (well under that budget) and verify all 20 are present.
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		c := "message content"
		if _, err := a.store.AppendMessage(thread.ID, role, &c, nil); err != nil {
			t.Fatalf("append message %d: %v", i, err)
		}
	}

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")

	// system + 20 = 21
	if len(msgs) != 21 {
		t.Errorf("expected 21 messages (no truncation), got %d", len(msgs))
	}
}

// TestCompressIfNeeded_TargetRatioClamping verifies that the MaxOutputTokens
// passed to the summarizer is computed from target_ratio and clamped to
// min_summary_tokens / max_summary_tokens.
func TestCompressIfNeeded_TargetRatioClamping(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Clamped summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	// Set explicit min/max to verify clamping behavior.
	// Default target_ratio=0.20, contextLength=100 => total ~120 tokens,
	// target_ratio*total = 24, which is below min=500, so maxOutput should be 500.
	a.cfg.Compression.MinSummaryTokens = 500
	a.cfg.Compression.MaxSummaryTokens = 800

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Errorf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("expected 1 summarizer call, got %d", len(fake.recorded))
	}
	if fake.recorded[0].MaxOutputTokens != 500 {
		t.Errorf("expected MaxOutputTokens=500 (clamped to min), got %d", fake.recorded[0].MaxOutputTokens)
	}
}

// TestCompressIfNeeded_TargetRatioMaxClamped verifies that high target_ratio
// is clamped to max_summary_tokens.
func TestCompressIfNeeded_TargetRatioMaxClamped(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Max-clamped summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	// Set a high target_ratio so the computed value exceeds max.
	a.cfg.Compression.TargetRatio = 0.90
	a.cfg.Compression.MinSummaryTokens = 100
	a.cfg.Compression.MaxSummaryTokens = 60

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Errorf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("expected 1 summarizer call, got %d", len(fake.recorded))
	}
	if fake.recorded[0].MaxOutputTokens != 60 {
		t.Errorf("expected MaxOutputTokens=60 (clamped to max), got %d", fake.recorded[0].MaxOutputTokens)
	}
}

// ----------------------------------------------------------------------------
// Secret redaction tests
// ----------------------------------------------------------------------------

func TestRedactSecrets_EnvAPIKey(t *testing.T) {
	input := "OPENAI_API_KEY=sk-abc123def456\nSOME_OTHER=value"
	got := redactSecrets(input)
	if strings.Contains(got, "sk-abc123def456") {
		t.Errorf("expected API key redacted, got: %q", got)
	}
	if !strings.Contains(got, "OPENAI_API_KEY=[REDACTED]") {
		t.Errorf("expected key name preserved with [REDACTED], got: %q", got)
	}
	if !strings.Contains(got, "SOME_OTHER=value") {
		t.Errorf("expected unrelated line untouched, got: %q", got)
	}
}

func TestRedactSecrets_BearerToken(t *testing.T) {
	input := "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	got := redactSecrets(input)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
		t.Errorf("expected bearer token redacted, got: %q", got)
	}
	if !strings.Contains(got, "Authorization: Bearer [REDACTED]") {
		t.Errorf("expected 'Authorization: Bearer [REDACTED]', got: %q", got)
	}
}

func TestRedactSecrets_PrivateKey(t *testing.T) {
	input := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGy0AHB7MhgwKVPSmwaFkYLv\n-----END RSA PRIVATE KEY-----"
	got := redactSecrets(input)
	if strings.Contains(got, "MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGy0AHB7MhgwKVPSmwaFkYLv") {
		t.Errorf("expected private key redacted, got: %q", got)
	}
	if !strings.Contains(got, "[REDACTED PRIVATE KEY]") {
		t.Errorf("expected '[REDACTED PRIVATE KEY]', got: %q", got)
	}
}

func TestRedactSecrets_AWSKey(t *testing.T) {
	input := "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"
	got := redactSecrets(input)
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("expected AWS key redacted, got: %q", got)
	}
}

func TestRedactSecrets_URLCreds(t *testing.T) {
	input := "connect to https://admin:secret123@db.example.com:5432"
	got := redactSecrets(input)
	if strings.Contains(got, "secret123") {
		t.Errorf("expected URL password redacted, got: %q", got)
	}
	if !strings.Contains(got, "https://[REDACTED]:[REDACTED]@db.example.com:5432") {
		t.Errorf("expected URL credentials replaced, got: %q", got)
	}
}

func TestRedactSecrets_JSONKey(t *testing.T) {
	input := `{"api_key": "sk-live-123456", "name": "test"}`
	got := redactSecrets(input)
	if strings.Contains(got, "sk-live-123456") {
		t.Errorf("expected JSON api_key redacted, got: %q", got)
	}
	if !strings.Contains(got, `"api_key": "[REDACTED]"`) {
		t.Errorf("expected JSON key preserved with [REDACTED], got: %q", got)
	}
	if !strings.Contains(got, `"name": "test"`) {
		t.Errorf("expected unrelated JSON field untouched, got: %q", got)
	}
}

func TestRedactSecrets_GitHubToken(t *testing.T) {
	input := "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	got := redactSecrets(input)
	if strings.Contains(got, "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") {
		t.Errorf("expected GitHub token redacted, got: %q", got)
	}
}

func TestCompressIfNeeded_RedactsSummarizerInput(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigContent := strings.Repeat("word ", 50)
	secretContent := "My secret is OPENAI_API_KEY=sk-live-abc123 and Bearer tokensecret456"
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent + " " + secretContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current question?"}},
	}

	// Use a smaller context length so both bigContent messages enter the compression batch.
	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 50, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("expected 1 summarizer call, got %d", len(fake.recorded))
	}

	// Verify the batch sent to the summarizer has redacted secrets.
	var batchText strings.Builder
	for _, m := range fake.recorded[0].Messages {
		batchText.WriteString(m.Content)
	}
	if strings.Contains(batchText.String(), "sk-live-abc123") {
		t.Errorf("summarizer input should not contain raw API key")
	}
	if strings.Contains(batchText.String(), "tokensecret456") {
		t.Errorf("summarizer input should not contain raw bearer token")
	}
	if !strings.Contains(batchText.String(), "OPENAI_API_KEY=[REDACTED]") {
		t.Errorf("summarizer input should preserve key name with [REDACTED]")
	}

	// Verify the synthetic compression summary message in the result also has no leaked secrets.
	for _, im := range result.Messages {
		if im.Kind == "compression_summary" {
			if strings.Contains(im.Msg.Content, "sk-live-abc123") {
				t.Errorf("compression summary message should not contain raw API key")
			}
		}
	}
}

func TestCompressIfNeeded_RedactsSummaryOutput(t *testing.T) {
	// Fake summarizer returns a summary that itself contains a secret.
	fake := &fakeSummarizer{returnContent: "The user has a GitHub token ghp_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Q?"}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s", result.Outcome)
	}
	if strings.Contains(result.Summary, "ghp_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") {
		t.Errorf("result.Summary should not contain raw GitHub token, got: %q", result.Summary)
	}
	// Find the synthetic compression summary message.
	for _, im := range result.Messages {
		if im.Kind == "compression_summary" {
			if strings.Contains(im.Msg.Content, "ghp_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") {
				t.Errorf("compression summary message should not contain raw GitHub token, got: %q", im.Msg.Content)
			}
		}
	}
}

// TestBuildMessages_InjectCompressionSummary verifies that buildMessages injects
// a synthetic compression_summary message when a valid compression record exists.
func TestBuildMessages_InjectCompressionSummary(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Append three messages.
	c1 := "hello"
	c2 := "world"
	c3 := "third"
	_, _ = a.store.AppendMessage(thread.ID, "user", &c1, nil)
	m2, _ := a.store.AppendMessage(thread.ID, "assistant", &c2, nil)
	m3, _ := a.store.AppendMessage(thread.ID, "user", &c3, nil)

	// Manually save a compression record so buildMessages will inject it.
	rec := memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "Earlier conversation about greetings.",
		FirstKeptSeq: m2.Seq,
	}
	if err := a.store.SaveCompression(rec); err != nil {
		t.Fatalf("save compression: %v", err)
	}

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")

	// Expected: system + compression_summary + messages from m2 onward (m2, m3)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Kind != "system" {
		t.Errorf("msgs[0] should be system, got %q", msgs[0].Kind)
	}
	if msgs[1].Kind != "compression_summary" {
		t.Errorf("msgs[1] should be compression_summary, got %q", msgs[1].Kind)
	}
	if !strings.Contains(msgs[1].Msg.Content, "Earlier conversation about greetings.") {
		t.Errorf("summary content missing expected text: %q", msgs[1].Msg.Content)
	}
	if msgs[1].Msg.Role != openai.ChatMessageRoleUser {
		t.Errorf("summary message role should be user, got %q", msgs[1].Msg.Role)
	}
	if msgs[2].Seq != m2.Seq {
		t.Errorf("msgs[2] should have Seq=%d, got %d", m2.Seq, msgs[2].Seq)
	}
	if msgs[3].Seq != m3.Seq {
		t.Errorf("msgs[3] should have Seq=%d, got %d", m3.Seq, msgs[3].Seq)
	}
}

// TestBuildMessages_LoadsFromSeq verifies that buildMessages loads only messages
// from first_kept_seq onward when a compression record exists.
func TestBuildMessages_LoadsFromSeq(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	c1 := "first"
	c2 := "second"
	c3 := "third"
	c4 := "fourth"
	m1, _ := a.store.AppendMessage(thread.ID, "user", &c1, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &c2, nil)
	m3, _ := a.store.AppendMessage(thread.ID, "user", &c3, nil)
	m4, _ := a.store.AppendMessage(thread.ID, "assistant", &c4, nil)

	// Save compression record that keeps messages from m3 onward.
	rec := memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "Summary of first two turns.",
		FirstKeptSeq: m3.Seq,
	}
	if err := a.store.SaveCompression(rec); err != nil {
		t.Fatalf("save compression: %v", err)
	}

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")

	// system + summary + m3 + m4 = 4
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	// m1 should NOT be present.
	for _, im := range msgs {
		if im.Seq == m1.Seq {
			t.Errorf("message with Seq=%d (m1) should not be present", m1.Seq)
		}
	}
	// m3 and m4 should be present.
	foundM3, foundM4 := false, false
	for _, im := range msgs {
		if im.Seq == m3.Seq {
			foundM3 = true
		}
		if im.Seq == m4.Seq {
			foundM4 = true
		}
	}
	if !foundM3 {
		t.Error("m3 should be present")
	}
	if !foundM4 {
		t.Error("m4 should be present")
	}
}

// TestBuildMessages_InvalidCompressionRecord_FallsBack verifies that buildMessages
// falls back to loading the full thread when the compression record points to a
// deleted or nonexistent first_kept_seq.
func TestBuildMessages_InvalidCompressionRecord_FallsBack(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	c1 := "first"
	c2 := "second"
	_, _ = a.store.AppendMessage(thread.ID, "user", &c1, nil)
	m2, _ := a.store.AppendMessage(thread.ID, "assistant", &c2, nil)

	// Save a compression record with first_kept_seq beyond any existing message.
	rec := memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "Summary.",
		FirstKeptSeq: m2.Seq + 100,
	}
	if err := a.store.SaveCompression(rec); err != nil {
		t.Fatalf("save compression: %v", err)
	}

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")

	// Should fall back to full thread: system + m1 + m2 = 3
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (full thread fallback), got %d", len(msgs))
	}
	// No compression_summary should be injected because the record was invalid.
	for _, im := range msgs {
		if im.Kind == "compression_summary" {
			t.Error("invalid compression record should not inject summary")
		}
	}
}

// TestCompressIfNeeded_PersistsRecord verifies that a successful compression
// saves a CompressionRecord to the store.
func TestCompressIfNeeded_PersistsRecord(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Persisted summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: bigContent}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: bigContent}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current question?"}},
	}

	result := a.compressIfNeeded(context.Background(), thread.ID, msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}

	// Verify the record was persisted.
	rec, err := a.store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest compression: %v", err)
	}
	if rec == nil {
		t.Fatal("expected compression record to be persisted, got nil")
	}
	if rec.Summary != "Persisted summary." {
		t.Errorf("expected summary %q, got %q", "Persisted summary.", rec.Summary)
	}
	// Turn-start compression targets post_compression_ratio (0.50 of the 100
	// context here), so the batch consumes both prefix messages — not just
	// enough to dip under the 80-token budget — and only the current user
	// turn survives verbatim.
	if rec.FirstKeptSeq != 3 {
		t.Errorf("expected FirstKeptSeq=3, got %d", rec.FirstKeptSeq)
	}
	if rec.CompressedMessageCount == 0 {
		t.Error("expected CompressedMessageCount > 0")
	}
	if rec.ThreadID != thread.ID {
		t.Errorf("expected ThreadID %q, got %q", thread.ID, rec.ThreadID)
	}
	if rec.FallbackUsed {
		t.Error("expected FallbackUsed=false for successful compression")
	}
}

// ----------------------------------------------------------------------------
// Event helper tests
// ----------------------------------------------------------------------------

func TestShouldAttemptCompression_BelowThreshold(t *testing.T) {
	msgs := []indexedMessage{
		{Seq: 0, Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: "system", Content: "system"}},
		{Seq: 1, Synthetic: false, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "user", Content: "hi"}},
		{Seq: 2, Synthetic: false, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "assistant", Content: "hello"}},
	}
	cfg := config.CompressionConfig{Enabled: true, Threshold: 0.80}
	// 8K context, small messages — well below threshold.
	if shouldAttemptCompression(msgs, 8192, cfg) {
		t.Error("expected shouldAttemptCompression=false for small messages")
	}
}

func TestShouldAttemptCompression_AboveThreshold(t *testing.T) {
	// Build a large message list that exceeds threshold.
	var msgs []indexedMessage
	msgs = append(msgs, indexedMessage{Seq: 0, Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: "system", Content: "system"}})
	for i := 0; i < 50; i++ {
		content := strings.Repeat("This is a test message with some content. ", 100)
		msgs = append(msgs, indexedMessage{Seq: i + 1, Synthetic: false, Kind: "thread_message",
			Msg: openai.ChatCompletionMessage{Role: "user", Content: content}})
		msgs = append(msgs, indexedMessage{Seq: i + 51, Synthetic: false, Kind: "thread_message",
			Msg: openai.ChatCompletionMessage{Role: "assistant", Content: content}})
	}
	cfg := config.CompressionConfig{Enabled: true, Threshold: 0.80}
	if !shouldAttemptCompression(msgs, 4096, cfg) {
		t.Error("expected shouldAttemptCompression=true for large messages")
	}
}

func TestShouldAttemptCompression_Disabled(t *testing.T) {
	msgs := []indexedMessage{
		{Seq: 0, Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: "system", Content: "system"}},
		{Seq: 1, Synthetic: false, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "user", Content: "hi"}},
	}
	cfg := config.CompressionConfig{Enabled: false, Threshold: 0.80}
	if shouldAttemptCompression(msgs, 4096, cfg) {
		t.Error("expected false when compression disabled")
	}
}

func TestBuildCompressionEndEvent(t *testing.T) {
	result := CompressionResult{
		Outcome:                 CompressionOutcomeCompressed,
		SummaryAttempted:        true,
		SummaryModelAlias:       "test-model",
		SummaryModelID:          "provider/test-model",
		BeforeTokens:            5000,
		AfterTokens:             2000,
		BudgetTokens:            3200,
		TargetTokens:            2048,
		RecentTailTargetTokens:  900,
		RecentTailTokens:        1024,
		CompressedCount:         10,
		SummaryPromptTokens:     100,
		SummaryCompletionTokens: 50,
		SummaryTotalTokens:      150,
		SummaryCallCount:        2,
		SummaryUsageCallCount:   2,
	}
	ev := buildCompressionEndEvent(result)
	if ev.ModelAlias != "test-model" {
		t.Errorf("expected model_alias=test-model, got %s", ev.ModelAlias)
	}
	if ev.BeforeTokens != 5000 {
		t.Errorf("expected before_tokens=5000, got %d", ev.BeforeTokens)
	}
	if ev.AfterTokens != 2000 {
		t.Errorf("expected after_tokens=2000, got %d", ev.AfterTokens)
	}
	if ev.TargetTokens != 2048 || ev.RecentTailTargetTokens != 900 || ev.RecentTailTokens != 1024 {
		t.Errorf("missing target/tail provenance: %+v", ev)
	}
	if ev.CompressedMessageCount != 10 {
		t.Errorf("expected compressed_message_count=10, got %d", ev.CompressedMessageCount)
	}
	if ev.SummaryTotalTokens != 150 {
		t.Errorf("expected summary_total_tokens=150, got %d", ev.SummaryTotalTokens)
	}
	if ev.SummaryCallCount != 2 || ev.SummaryUsageCallCount != 2 {
		t.Errorf("expected summary call coverage 2/2, got %d/%d", ev.SummaryUsageCallCount, ev.SummaryCallCount)
	}
	if ev.Outcome != string(CompressionOutcomeCompressed) || !ev.SummaryAttempted {
		t.Fatalf("missing semantic compression metadata: %+v", ev)
	}
}

func TestBuildCompressionErrorEvent(t *testing.T) {
	result := CompressionResult{
		Outcome:        CompressionOutcomeError,
		BeforeTokens:   5000,
		FallbackUsed:   true,
		FallbackReason: "summarizer_error: timeout",
		Err:            errors.New("timeout"),
	}
	ev := buildCompressionErrorEvent(result)
	if ev.Error != "timeout" {
		t.Errorf("expected error='timeout', got %q", ev.Error)
	}
	if !ev.FallbackUsed {
		t.Error("expected fallback_used=true")
	}
	if ev.FallbackReason != "summarizer_error: timeout" {
		t.Errorf("expected fallback_reason='summarizer_error: timeout', got %q", ev.FallbackReason)
	}
}

func TestBuildCompressionStartEvent(t *testing.T) {
	result := CompressionResult{
		BeforeTokens:           8000,
		BudgetTokens:           3200,
		TargetTokens:           2400,
		RecentTailTargetTokens: 900,
		RecentTailTokens:       1000,
	}
	ev := buildCompressionStartEvent(result, 3200)
	if ev.ThresholdTokens != 3200 {
		t.Errorf("expected threshold_tokens=3200, got %d", ev.ThresholdTokens)
	}
	if ev.BeforeTokens != 8000 {
		t.Errorf("expected before_tokens=8000, got %d", ev.BeforeTokens)
	}
	if ev.TargetTokens != 2400 || ev.RecentTailTargetTokens != 900 || ev.RecentTailTokens != 1000 {
		t.Errorf("missing start target/tail provenance: %+v", ev)
	}
}

// ----------------------------------------------------------------------------
// pruneOldToolOutputs tests
// ----------------------------------------------------------------------------

func TestPruneOldToolOutputs_BasicPruning(t *testing.T) {
	// Messages: sys, user1, tool1 (big), tool2 (small), user2, assistant, tool3 (big, recent).
	bigContent := strings.Repeat("x", 3000)
	smallContent := "small output"
	recentBigContent := strings.Repeat("y", 3000)

	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "old question"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "shell_exec"}}},
		}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: bigContent, ToolCallID: "call_1"}},
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "call_2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "file_read"}}},
		}},
		{Seq: 5, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: smallContent, ToolCallID: "call_2"}},
		{Seq: 6, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current question"}},
		{Seq: 7, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "call_3", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "web_search"}}},
		}},
		{Seq: 8, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: recentBigContent, ToolCallID: "call_3"}},
	}

	pruned, count := pruneOldToolOutputs(msgs)
	if count != 1 {
		t.Fatalf("expected 1 pruned tool output, got %d", count)
	}

	// Old big tool output (index 3) should be pruned.
	if len(pruned[3].Msg.Content) >= len(bigContent) {
		t.Error("old big tool output was not pruned")
	}
	if !strings.Contains(pruned[3].Msg.Content, "shell_exec output pruned") {
		t.Errorf("expected tool name in metadata, got: %s...", pruned[3].Msg.Content[len(pruned[3].Msg.Content)-100:])
	}

	// Small old tool output (index 5) should be untouched.
	if pruned[5].Msg.Content != smallContent {
		t.Error("small old tool output was modified")
	}

	// Recent big tool output (index 8) should NOT be pruned.
	if len(pruned[8].Msg.Content) < len(recentBigContent) {
		t.Error("recent tool output was incorrectly pruned")
	}

	// System prompt must not be touched.
	if pruned[0].Kind != "system" {
		t.Error("system prompt was modified")
	}
}

func TestPruneOldToolOutputs_NoPruningNeeded(t *testing.T) {
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "hello"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: "short output", ToolCallID: "call_1"}},
	}

	pruned, count := pruneOldToolOutputs(msgs)
	if count != 0 {
		t.Errorf("expected 0 pruned, got %d", count)
	}
	if len(pruned) != 3 {
		t.Errorf("expected same message count, got %d", len(pruned))
	}
}

func TestPruneOldToolOutputs_AllOldPruned(t *testing.T) {
	bigContent := strings.Repeat("x", 3000)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: bigContent, ToolCallID: "call_1"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: bigContent, ToolCallID: "call_2"}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "latest question"}},
	}

	pruned, count := pruneOldToolOutputs(msgs)
	if count != 2 {
		t.Fatalf("expected 2 pruned, got %d", count)
	}
	// Both should be truncated to ~2000 chars + metadata.
	if len(pruned[1].Msg.Content) > 2200 {
		t.Errorf("pruned[1] too long: %d", len(pruned[1].Msg.Content))
	}
	if len(pruned[2].Msg.Content) > 2200 {
		t.Errorf("pruned[2] too long: %d", len(pruned[2].Msg.Content))
	}
}

func TestPruneOldToolOutputs_NoUserTurn(t *testing.T) {
	// Edge case: no user message yet. Nothing should be pruned.
	bigContent := strings.Repeat("x", 3000)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: bigContent, ToolCallID: "call_1"}},
	}

	pruned, count := pruneOldToolOutputs(msgs)
	if count != 0 {
		t.Errorf("expected 0 pruned when no user turn, got %d", count)
	}
	if len(pruned[1].Msg.Content) != len(bigContent) {
		t.Error("tool output was pruned despite no user turn")
	}
}

func TestCompressIfNeeded_MidLoop_PrunesInsteadOfTruncating(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	// Build messages where an OLD tool output is huge but pruning brings us under budget.
	bigToolOutput := strings.Repeat("x", 4000)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "System prompt."}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "call_old", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "shell_exec"}}},
		}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: bigToolOutput, ToolCallID: "call_old"}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current question"}},
	}

	// Use a small context length so the total exceeds budget.
	// contextLength=200, threshold=0.80 -> budget=160
	// bigToolOutput alone is 4000 chars which with token overhead > 160 tokens.
	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 200, CompressionModeMidLoop)
	// Mid-loop should have pruned, and if pruning wasn't enough, fallen back.
	// Either Outcome should be Pruned or Fallback.
	if result.Outcome != CompressionOutcomePruned && result.Outcome != CompressionOutcomeFallback {
		t.Errorf("expected pruned or fallback outcome in mid-loop mode, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}
	if result.PrunedToolOutputs == 0 && result.Outcome == CompressionOutcomePruned {
		t.Error("expected PrunedToolOutputs > 0 for pruned outcome")
	}
	// Verify no summarizer was called.
	if len(fake.recorded) != 0 {
		t.Errorf("summarizer should not be called in mid-loop mode, got %d calls", len(fake.recorded))
	}
	// Verify payload validity.
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Errorf("mid-loop result fails payload validation: %v", err)
	}
}

func TestCompressIfNeeded_MidLoop_PruningPreservesRecentTurn(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)

	bigOld := strings.Repeat("old_", 5000)
	// Keep recent tool output small so pruning alone is sufficient.
	recentSmall := "recent tool output"

	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "System prompt."}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "call_old", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "old_tool"}}},
		}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: bigOld, ToolCallID: "call_old"}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "recent question"}},
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "call_recent", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "recent_tool"}}},
		}},
		{Seq: 5, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: "tool", Content: recentSmall, ToolCallID: "call_recent"}},
	}

	// Use a high context length so after pruning we're safely under budget.
	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 32000, CompressionModeMidLoop)

	// The recent tool output must NOT be pruned.
	for _, im := range result.Messages {
		if im.Seq == 5 && im.Kind == "thread_message" {
			if !strings.Contains(im.Msg.Content, "recent tool output") {
				t.Error("recent tool output was pruned — should be intact")
			}
		}
	}

	// Maintains payload validity.
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Errorf("payload invalid after mid-loop pruning: %v", err)
	}
}

func TestCompressIfNeeded_MidLoop_PruningOnlyReportsTargetAndRawTail(t *testing.T) {
	fake := &fakeSummarizer{}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70
	a.cfg.Compression.PostCompressionRatio = 0.50
	a.cfg.Compression.RecentTailTokens = 100
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "old", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "shell_exec"}}}}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "old", Content: strings.Repeat("old evidence ", 10000)}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Continue from the recent work."}},
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: strings.Repeat("recent analysis ", 200)}},
	}

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 4096, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomePruned {
		t.Fatalf("expected pruning-only compression, got %+v", result)
	}
	if result.TargetTokens != 2048 || result.RecentTailTargetTokens != 100 || result.RecentTailTokens < result.RecentTailTargetTokens {
		t.Fatalf("pruning-only provenance is incoherent: %+v", result)
	}
	terminal := buildCompressionEndEvent(result)
	if terminal.TargetTokens != result.TargetTokens || terminal.RecentTailTargetTokens != result.RecentTailTargetTokens || terminal.RecentTailTokens != result.RecentTailTokens {
		t.Fatalf("pruning-only event lost target/tail provenance: %+v", terminal)
	}
	if result.SummaryCallCount != 0 || len(fake.recorded) != 0 {
		t.Fatalf("pruning-only result fabricated a summarizer call: result=%+v calls=%d", result, len(fake.recorded))
	}
}

// ----------------------------------------------------------------------------
// Repeated compression chaining tests
// ----------------------------------------------------------------------------

// TestRepeatedCompression_FirstKeptSeqAdvances verifies that when a thread
// already has a compression record, a second compression produces a new record
// with a higher first_kept_seq, proving the boundary advances.
func TestRepeatedCompression_FirstKeptSeqAdvances(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Second summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	bigContent := strings.Repeat("word ", 100) // ~100 tokens each

	// Append 5 messages: seq 1..5
	c1 := bigContent
	c2 := bigContent
	c3 := bigContent
	c4 := bigContent
	c5 := bigContent
	_, _ = a.store.AppendMessage(thread.ID, "user", &c1, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &c2, nil)
	m3, _ := a.store.AppendMessage(thread.ID, "user", &c3, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &c4, nil)
	_, _ = a.store.AppendMessage(thread.ID, "user", &c5, nil)

	// Save first compression record: keep from seq 3 onward.
	rec1 := memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "First summary.",
		FirstKeptSeq: m3.Seq,
	}
	if err := a.store.SaveCompression(rec1); err != nil {
		t.Fatalf("save first compression: %v", err)
	}

	// Append two more messages after the first compression.
	c6 := bigContent
	c7 := bigContent
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &c6, nil)
	_, _ = a.store.AppendMessage(thread.ID, "user", &c7, nil)

	// Build messages: should inject the old summary and load from seq 3.
	msgs := mustBuildMessages(t, a, thread.ID, "", "web")

	// Count how many messages we have: system + summary + 5 loaded messages = 7
	if len(msgs) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(msgs))
	}
	if msgs[1].Kind != "compression_summary" {
		t.Fatalf("expected compression_summary at index 1, got %q", msgs[1].Kind)
	}

	// Force compression with a small context length.
	// contextLength=256, threshold=0.80 => budget=204 tokens. The raw
	// history exceeds that while the short fake summary plus tail fits.
	result := a.compressIfNeeded(context.Background(), thread.ID, msgs, "test-model", 256, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}

	// Verify the new record was persisted with a higher first_kept_seq.
	rec2, err := a.store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest compression: %v", err)
	}
	if rec2 == nil {
		t.Fatal("expected second compression record, got nil")
	}
	if rec2.FirstKeptSeq <= rec1.FirstKeptSeq {
		t.Errorf("expected second FirstKeptSeq (%d) > first FirstKeptSeq (%d)", rec2.FirstKeptSeq, rec1.FirstKeptSeq)
	}
	if rec2.Summary != "Second summary." {
		t.Errorf("expected new summary %q, got %q", "Second summary.", rec2.Summary)
	}
}

// TestRepeatedCompression_ChainsSummary verifies that the second compression
// includes the prior compression summary in the summarizer input, so the new
// summary is built iteratively from the old one.
func TestRepeatedCompression_ChainsSummary(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Chained summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	bigContent := strings.Repeat("word ", 100)

	_, _ = a.store.AppendMessage(thread.ID, "user", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &bigContent, nil)
	m3, _ := a.store.AppendMessage(thread.ID, "user", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "user", &bigContent, nil)

	rec1 := memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "Prior summary about greetings.",
		FirstKeptSeq: m3.Seq,
	}
	if err := a.store.SaveCompression(rec1); err != nil {
		t.Fatalf("save first compression: %v", err)
	}

	_, _ = a.store.AppendMessage(thread.ID, "assistant", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "user", &bigContent, nil)

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")
	result := a.compressIfNeeded(context.Background(), thread.ID, msgs, "test-model", 256, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s", result.Outcome)
	}

	if len(fake.recorded) != 1 {
		t.Fatalf("expected 1 summarizer call, got %d", len(fake.recorded))
	}

	// Verify the summarizer input contains the prior summary text.
	var batchText strings.Builder
	for _, m := range fake.recorded[0].Messages {
		batchText.WriteString(m.Content)
	}
	if !strings.Contains(batchText.String(), "Prior summary about greetings.") {
		t.Errorf("summarizer input should contain prior summary text; got: %q", batchText.String())
	}

	// Verify the result contains a new compression summary, not the raw old one.
	foundNewSummary := false
	for _, im := range result.Messages {
		if im.Kind == "compression_summary" {
			if strings.Contains(im.Msg.Content, "Chained summary.") {
				foundNewSummary = true
			}
		}
	}
	if !foundNewSummary {
		t.Error("result should contain a new compression summary message")
	}
}

// TestRepeatedCompression_BuildMessagesUsesLatestSummary verifies that after
// two compressions, buildMessages injects the most recent summary.
func TestRepeatedCompression_BuildMessagesUsesLatestSummary(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Latest summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	bigContent := strings.Repeat("word ", 100)

	_, _ = a.store.AppendMessage(thread.ID, "user", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &bigContent, nil)
	m3, _ := a.store.AppendMessage(thread.ID, "user", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "assistant", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "user", &bigContent, nil)

	rec1 := memory.CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "Old summary.",
		FirstKeptSeq: m3.Seq,
	}
	if err := a.store.SaveCompression(rec1); err != nil {
		t.Fatalf("save first compression: %v", err)
	}

	_, _ = a.store.AppendMessage(thread.ID, "assistant", &bigContent, nil)
	_, _ = a.store.AppendMessage(thread.ID, "user", &bigContent, nil)

	msgs := mustBuildMessages(t, a, thread.ID, "", "web")
	result := a.compressIfNeeded(context.Background(), thread.ID, msgs, "test-model", 256, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s", result.Outcome)
	}

	// Now build messages again — it should use the NEW latest summary.
	msgs2 := mustBuildMessages(t, a, thread.ID, "", "web")
	if len(msgs2) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs2))
	}
	if msgs2[1].Kind != "compression_summary" {
		t.Fatalf("expected compression_summary at index 1, got %q", msgs2[1].Kind)
	}
	if !strings.Contains(msgs2[1].Msg.Content, "Latest summary.") {
		t.Errorf("buildMessages should inject the latest summary; got: %q", msgs2[1].Msg.Content)
	}
	if strings.Contains(msgs2[1].Msg.Content, "Old summary.") {
		t.Error("buildMessages should not inject the old summary")
	}
}

// TestCompressIfNeeded_ForceBelowBudget verifies the manual /compress path:
// with the force flag set, compression runs even when the thread is under the
// token budget (where the automatic path would no-op).
func TestCompressIfNeeded_ForceBelowBudget(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Summary of old stuff."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	content := strings.Repeat("word ", 10)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "System prompt."}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content}},
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content}},
		{Seq: 5, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current?"}},
	}

	// Generous context window → well under budget → automatic path no-ops.
	if r := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 128000, CompressionModeTurnStart); r.Outcome != CompressionOutcomeNone {
		t.Fatalf("under budget without force should be none, got %s", r.Outcome)
	}

	// With force, it compresses anyway and calls the summarizer.
	// But if the summary is larger than the original (tiny conversation),
	// the safety check correctly returns noChange instead of accepting it.
	r := a.compressIfNeeded(withForceCompression(context.Background()), "", msgs, "test-model", 128000, CompressionModeTurnStart)
	if r.Outcome != CompressionOutcomeCompressed && r.Outcome != CompressionOutcomeNone {
		t.Errorf("forced compression should compress or skip, got %s (fallback: %s)", r.Outcome, r.FallbackReason)
	}
	if len(fake.recorded) == 0 {
		t.Error("forced compression should call the summarizer")
	}
}

// ----------------------------------------------------------------------------
// Turn-start headroom
// ----------------------------------------------------------------------------

// TestCompressIfNeeded_TurnStart_CompressesToTargetNotBareBudget pins the
// headroom contract: turn-start compression selects its batch so the result
// lands at or below the post-compression target (post_compression_ratio),
// not merely under the hard budget. Budget-only selection landed a few tokens
// under budget, so the first tool outputs of the new turn immediately
// re-triggered mid-loop compression with nothing safe to summarize.
func TestCompressIfNeeded_TurnStart_CompressesToTargetNotBareBudget(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Headroom summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)

	// contextLength=100, threshold=0.80 -> budget=80, target=0.50*100=50.
	// Six ~35-token exchanges plus system and a final user turn total ~220
	// tokens. Stopping at the budget would keep everything from exchange 5
	// on; stopping at the target must also fold exchange 5 into the batch.
	bigContent := strings.Repeat("word ", 30)
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
	}
	for i := 1; i <= 6; i++ {
		role := openai.ChatMessageRoleUser
		if i%2 == 0 {
			role = openai.ChatMessageRoleAssistant
		}
		msgs = append(msgs, indexedMessage{Seq: i, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: role, Content: bigContent}})
	}
	msgs = append(msgs, indexedMessage{Seq: 7, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Current question?"}})

	result := a.compressIfNeeded(context.Background(), "", msgs, "test-model", 100, CompressionModeTurnStart)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected compressed, got %s (fallback: %s)", result.Outcome, result.FallbackReason)
	}
	if result.AfterTokens > 80 {
		t.Fatalf("after=%d exceeds budget=80", result.AfterTokens)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("summarizer calls: got %d, want 1", len(fake.recorded))
	}
	batch := formatMessagesForCompression(fake.recorded[0].Messages)
	exchangesInBatch := strings.Count(batch, bigContent)
	if exchangesInBatch < 5 {
		t.Fatalf("batch folded only %d of 6 exchanges; target-based selection must reach deeper than the budget stop (4)", exchangesInBatch)
	}
	if result.FirstKeptSeq != 7 {
		t.Fatalf("FirstKeptSeq=%d, want 7 (only the current user turn survives verbatim)", result.FirstKeptSeq)
	}
}

// ----------------------------------------------------------------------------
// Mid-loop escalation
// ----------------------------------------------------------------------------

// TestCompressIfNeeded_MidLoop_EscalatesToPrefixCompression reproduces the
// production failure shape: a huge durable prefix plus a tiny active turn.
// Mid-loop alone cannot checkpoint anything (the recent-tail floor protects
// the whole active turn), which used to degrade to blind truncation. It must
// instead escalate to the turn-start path: summarize the durable prefix,
// persist the record, and keep the current turn verbatim.
func TestCompressIfNeeded_MidLoop_EscalatesToPrefixCompression(t *testing.T) {
	fake := &fakeSummarizer{returnContent: "Escalated prefix summary."}
	a := newTestAgentWithFakeSummarizer(t, fake)
	a.cfg.Compression.Threshold = 0.70

	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// contextLength=65536, threshold=0.70 -> budget=45875. Eight durable
	// prefix exchanges of ~8K tokens (~64K total) plus a tiny active turn
	// (~200 tokens) after the latest user message.
	msgs := []indexedMessage{
		{Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "System prompt."}},
	}
	for i := 0; i < 8; i++ {
		role := openai.ChatMessageRoleUser
		if i%2 == 1 {
			role = openai.ChatMessageRoleAssistant
		}
		msgs = append(msgs, indexedMessage{
			Seq:  1 + i,
			Kind: "thread_message",
			Msg:  openai.ChatCompletionMessage{Role: role, Content: fmt.Sprintf("PREFIX-%02d ", i) + strings.Repeat("analysis ", 8000)},
		})
	}
	msgs = append(msgs, indexedMessage{Seq: 9, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "proceed"}})
	msgs = append(msgs, indexedMessage{Seq: 10, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "ACTIVE-VERBATIM " + strings.Repeat("result ", 200)}})

	result := a.compressIfNeeded(context.Background(), thread.ID, msgs, "test-model", 65536, CompressionModeMidLoop)
	if result.Outcome != CompressionOutcomeCompressed {
		t.Fatalf("expected escalated compression, got %s (fallback: %s, err: %v)", result.Outcome, result.FallbackReason, result.Err)
	}
	if result.FallbackUsed {
		t.Fatalf("escalation must not use fallback truncation: %+v", result)
	}
	// Exactly one summarizer call: the escalation. compressActiveTurn must
	// not have spent a call before giving up.
	if len(fake.recorded) != 1 {
		t.Fatalf("summarizer calls: got %d, want 1", len(fake.recorded))
	}
	batch := formatMessagesForCompression(fake.recorded[0].Messages)
	if !strings.Contains(batch, "PREFIX-00") {
		t.Fatalf("escalation batch did not include the durable prefix")
	}
	if strings.Contains(batch, "ACTIVE-VERBATIM") || strings.Contains(batch, "proceed") {
		t.Fatalf("escalation batch must not include the current turn")
	}
	// The current turn survives verbatim.
	foundUser, foundActive := false, false
	for _, im := range result.Messages {
		if im.Msg.Content == "proceed" {
			foundUser = true
		}
		if im.Msg.Content == msgs[len(msgs)-1].Msg.Content {
			foundActive = true
		}
	}
	if !foundUser || !foundActive {
		t.Fatalf("current turn not preserved verbatim: user=%v active=%v", foundUser, foundActive)
	}
	if err := validateProviderPayload(toRawMessages(result.Messages)); err != nil {
		t.Fatalf("escalated payload fails validation: %v", err)
	}
	// The durable checkpoint is persisted for future turns.
	rec, err := a.store.GetLatestCompression(thread.ID)
	if err != nil || rec == nil {
		t.Fatalf("escalation must persist a compression record: rec=%v err=%v", rec, err)
	}
	if rec.FirstKeptSeq < 2 || rec.FirstKeptSeq > 9 {
		t.Fatalf("FirstKeptSeq=%d must fall inside the durable prefix", rec.FirstKeptSeq)
	}
}
