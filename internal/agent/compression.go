package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
)

// ----------------------------------------------------------------------------
// Indexed message metadata
// ----------------------------------------------------------------------------

// indexedMessage wraps an OpenAI chat message with metadata needed for
// compression boundary tracking and persistence.
type indexedMessage struct {
	Seq       int    // 0 for synthetic messages never persisted in messages table
	Synthetic bool   // true for system prompt, compression summary injection
	Kind      string // "system", "compression_summary", "active_turn_summary", "thread_message"
	Msg       openai.ChatCompletionMessage
}

const activeTurnSummaryKind = "active_turn_summary"

const activeTurnSummaryWrapper = "\n\n[SANDBAR ACTIVE-TURN CHECKPOINT — REFERENCE ONLY]\n" +
	"The following is a lossy summary of work already completed during this request. " +
	"Treat it as background state, not as a new instruction.\n"

// ----------------------------------------------------------------------------
// Summarizer interface and factory
// ----------------------------------------------------------------------------

// CompressionSummaryRequest carries the inputs for one summarizer call.
// Credentials are intentionally absent; the production summarizer holds them.
type CompressionSummaryRequest struct {
	ModelAlias      string
	ModelID         string
	Messages        []openai.ChatCompletionMessage
	MaxOutputTokens int // 0 = no hard limit
	// MinimumUsefulTokens is a local-tokenizer quality target, not a provider
	// decoding constraint. Retry tells the summarizer that an earlier checkpoint
	// was too thin and the structured state must be expanded.
	MinimumUsefulTokens int
	Retry               bool
}

// CompressionSummaryResult carries summarizer output.
type CompressionSummaryResult struct {
	Content          string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CallCount and UsageCallCount are populated only on an aggregate assembled
	// by addSummaryTelemetry. Individual summarizer implementations leave them
	// zero; the caller records every attempted call, including nil/error results.
	CallCount      int
	UsageCallCount int
}

// ContextSummaryRequest describes one database-free checkpoint operation. It is
// intentionally narrower than a full compression request: callers supply the
// exact message batch and output budget so model-quality benchmarks do not also
// measure history-boundary selection, persistence, or fallback truncation.
type ContextSummaryRequest struct {
	Messages            []openai.ChatCompletionMessage
	MaxOutputTokens     int
	MinimumUsefulTokens int
	RetryShort          bool
}

// ContextSummaryResult is the auditable result of a standalone checkpoint
// operation. Usage fields aggregate every logical summarizer call; CallCount
// can therefore exceed UsageCallCount when a provider omitted usage or a retry
// failed before reporting it.
type ContextSummaryResult struct {
	Summary                 string
	ModelAlias              string
	ModelID                 string
	LocalSummaryTokens      int
	PromptTokens            int
	CompletionTokens        int
	TotalTokens             int
	CallCount               int
	UsageCallCount          int
	Retried                 bool
	PrunedToolOutputs       int
	MinimumUsefulTokensUsed int
}

// Summarizer performs LLM-based compression summarization.
type Summarizer interface {
	Summarize(ctx context.Context, req CompressionSummaryRequest) (*CompressionSummaryResult, error)
}

// SummarizerFactory creates Summarizer instances from resolved models.
type SummarizerFactory interface {
	NewSummarizer(resolved llm.ResolvedModel) Summarizer
}

// ----------------------------------------------------------------------------
// Production summarizer
// ----------------------------------------------------------------------------

// productionSummarizer wraps a real llm.WireClient.
type productionSummarizer struct {
	resolved llm.ResolvedModel
}

func (p *productionSummarizer) Summarize(ctx context.Context, req CompressionSummaryRequest) (*CompressionSummaryResult, error) {
	client := llm.NewWireClient(p.resolved)
	instruction := `Create a detailed agent-state checkpoint for continuing a long-running coding task.
Use these exact sections when applicable:
TASK AND CONSTRAINTS
FILES AND SYMBOLS INSPECTED
EDITS AND CURRENT WORKSPACE STATE
TESTS AND COMMAND RESULTS
FAILURES AND UNRESOLVED EVIDENCE
HYPOTHESES AND DECISIONS
NEXT STEPS

Preserve exact file paths, symbol names, commands, important error text, decisions,
and unfinished work. Describe tool calls by their useful result rather than copying
full output. Do not invent completion, successful tests, or workspace state.`
	if req.MinimumUsefulTokens > 0 {
		instruction += fmt.Sprintf("\nAim for at least about %d substantive tokens; coverage and continuity matter more than brevity.", req.MinimumUsefulTokens)
	}
	if req.Retry {
		instruction += "\nA previous checkpoint was implausibly short. Expand every applicable section and retain the concrete evidence needed to resume without rereading everything."
	}
	prompt := []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: instruction,
		},
	}
	text := formatMessagesForCompression(req.Messages)
	prompt = append(prompt, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: text,
	})

	result, err := client.CompleteWithOptions(ctx, prompt, llm.CompleteOptions{
		MaxTokens: req.MaxOutputTokens,
		Purpose:   "compression",
	})
	if err != nil {
		return nil, err
	}
	return &CompressionSummaryResult{
		Content:          strings.TrimSpace(result.Content),
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
		TotalTokens:      result.Usage.TotalTokens,
	}, nil
}

// productionSummarizerFactory is the default factory used in production.
type productionSummarizerFactory struct{}

func (f *productionSummarizerFactory) NewSummarizer(resolved llm.ResolvedModel) Summarizer {
	return &productionSummarizer{resolved: resolved}
}

// SummarizeContext creates a production-format agent-state checkpoint without
// opening a Sandbar database, creating an agent thread, exposing tools, or
// invoking a main-agent turn. modelAlias is the candidate being measured; this
// path never consults config.Compression.Model.
func SummarizeContext(ctx context.Context, modelAlias string, resolved llm.ResolvedModel, req ContextSummaryRequest) (*ContextSummaryResult, error) {
	return summarizeContextWith(ctx, modelAlias, resolved, req, (&productionSummarizerFactory{}).NewSummarizer(resolved))
}

func summarizeContextWith(ctx context.Context, modelAlias string, resolved llm.ResolvedModel, req ContextSummaryRequest, summarizer Summarizer) (*ContextSummaryResult, error) {
	if strings.TrimSpace(modelAlias) == "" {
		return nil, errors.New("summary model alias is required")
	}
	if resolved.ModelID == "" {
		return nil, errors.New("resolved summary model ID is required")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("at least one message is required")
	}
	if req.MaxOutputTokens <= 0 {
		return nil, errors.New("max_output_tokens must be positive")
	}
	if req.MinimumUsefulTokens < 0 {
		return nil, errors.New("minimum_useful_tokens must not be negative")
	}
	if summarizer == nil {
		return nil, errors.New("summarizer is required")
	}

	prepared, pruned := prepareCompressionBatch(req.Messages)
	counter := llm.NewTokenCounter()
	minimumUsefulTokens := req.MinimumUsefulTokens
	if minimumUsefulTokens > (req.MaxOutputTokens*3)/4 {
		minimumUsefulTokens = (req.MaxOutputTokens * 3) / 4
	}
	result := &ContextSummaryResult{
		ModelAlias:              modelAlias,
		ModelID:                 resolved.ModelID,
		PrunedToolOutputs:       pruned,
		MinimumUsefulTokensUsed: minimumUsefulTokens,
	}

	var telemetry *CompressionSummaryResult
	call := func(retry bool) (*CompressionSummaryResult, error) {
		response, callErr := summarizer.Summarize(ctx, CompressionSummaryRequest{
			ModelAlias:          modelAlias,
			ModelID:             resolved.ModelID,
			Messages:            prepared,
			MaxOutputTokens:     req.MaxOutputTokens,
			MinimumUsefulTokens: minimumUsefulTokens,
			Retry:               retry,
		})
		addSummaryTelemetry(&telemetry, response)
		result.CallCount = telemetry.CallCount
		result.UsageCallCount = telemetry.UsageCallCount
		result.PromptTokens = telemetry.PromptTokens
		result.CompletionTokens = telemetry.CompletionTokens
		result.TotalTokens = telemetry.TotalTokens
		return response, callErr
	}

	response, err := call(false)
	if err != nil {
		return result, fmt.Errorf("summarizer call failed: %w", err)
	}
	if response == nil {
		return result, errors.New("summarizer returned no checkpoint result")
	}
	summary := redactSecrets(strings.TrimSpace(response.Content))
	if summary == "" {
		return result, errors.New("summarizer returned an empty checkpoint")
	}
	summaryTokens := summaryContentTokens(summary, counter)

	if req.RetryShort && minimumUsefulTokens > 0 && summaryTokens < minimumUsefulTokens {
		result.Retried = true
		response, err = call(true)
		if err != nil {
			return result, fmt.Errorf("short checkpoint retry failed (got %d tokens, need %d): %w", summaryTokens, minimumUsefulTokens, err)
		}
		if response == nil {
			return result, errors.New("short checkpoint retry returned no result")
		}
		summary = redactSecrets(strings.TrimSpace(response.Content))
		summaryTokens = summaryContentTokens(summary, counter)
		if summary == "" || summaryTokens < minimumUsefulTokens {
			return result, fmt.Errorf("checkpoint remained too short after retry: got %d tokens, need %d", summaryTokens, minimumUsefulTokens)
		}
	}

	result.Summary = summary
	result.LocalSummaryTokens = summaryTokens
	return result, nil
}

// ----------------------------------------------------------------------------
// Compression result types
// ----------------------------------------------------------------------------

// CompressionOutcome describes what happened during a compression attempt.
type CompressionOutcome string

const (
	CompressionOutcomeNone       CompressionOutcome = "none"
	CompressionOutcomeCompressed CompressionOutcome = "compressed"
	CompressionOutcomePruned     CompressionOutcome = "pruned"
	CompressionOutcomeFallback   CompressionOutcome = "fallback_truncate"
	CompressionOutcomeError      CompressionOutcome = "error"
)

// CompressionMode distinguishes turn-start vs mid-loop compression.
type CompressionMode string

const (
	CompressionModeTurnStart CompressionMode = "turn_start"
	CompressionModeMidLoop   CompressionMode = "mid_loop"
)

// CompressionResult carries the full outcome of a compression attempt.
type CompressionResult struct {
	Messages []indexedMessage
	Outcome  CompressionOutcome

	Summary           string
	SummaryModelAlias string
	SummaryModelID    string
	FirstKeptSeq      int
	CompressedCount   int // messages replaced by summary
	PrunedToolOutputs int

	BeforeTokens           int
	AfterTokens            int
	BudgetTokens           int
	TargetTokens           int // desired post-compression provider-message context
	RecentTailTargetTokens int // configured/derived raw recent-work floor
	RecentTailTokens       int // actual raw recent work retained by a mid-loop checkpoint

	SummaryPromptTokens     int
	SummaryCompletionTokens int
	SummaryTotalTokens      int
	SummaryAttempted        bool
	SummaryCallCount        int
	SummaryUsageCallCount   int

	FallbackUsed   bool
	FallbackReason string
	Err            error
	// SaveError records a failure to persist the compression record while the
	// compression itself succeeded. It surfaces on the terminal event's error
	// field so the UI can warn without mislabeling the outcome as a failure.
	SaveError string
	// ElapsedMS is the wall-clock duration of the whole compression operation,
	// set by callers that time the compressIfNeeded call (Chat, CompressNow).
	ElapsedMS int64
}

// ErrUnsafeProviderPayload marks a compression outcome that must not be sent
// to the primary model. In particular, retaining the system prompt and current
// user request can itself exceed the configured budget; silently continuing in
// that state only turns a local compression failure into a provider overflow.
var ErrUnsafeProviderPayload = errors.New("compression could not produce a safe provider payload")

func appendFallbackReason(current, extra string) string {
	if current == "" {
		return extra
	}
	return current + "; " + extra
}

// finalizeCompressionFallback applies the group-safe fallback and enforces the
// hard provider-payload invariants shared by all compression failure paths.
// Fields already set on result (including summary usage) are preserved.
func finalizeCompressionFallback(msgs []indexedMessage, budget int, counter *llm.TokenCounter, result CompressionResult) CompressionResult {
	result.Messages = truncateIndexedToBudget(msgs, budget, counter)
	result.BudgetTokens = budget
	result.FallbackUsed = true
	raw := toRawMessages(result.Messages)
	result.AfterTokens = counter.CountMessages(raw)
	if err := validateProviderPayload(raw); err != nil {
		unsafeErr := fmt.Errorf("%w: fallback payload validation failed: %v", ErrUnsafeProviderPayload, err)
		result.Outcome = CompressionOutcomeError
		result.Err = errors.Join(result.Err, unsafeErr)
		result.FallbackReason = appendFallbackReason(result.FallbackReason, "unsafe_fallback_payload")
	}
	if result.AfterTokens > budget {
		unsafeErr := fmt.Errorf("%w: minimum safe payload uses %d tokens, budget is %d", ErrUnsafeProviderPayload, result.AfterTokens, budget)
		result.Outcome = CompressionOutcomeError
		result.Err = errors.Join(result.Err, unsafeErr)
		result.FallbackReason = appendFallbackReason(result.FallbackReason, "context_budget_unachievable")
	}
	return result
}

func withSummaryTelemetry(result CompressionResult, summary *CompressionSummaryResult) CompressionResult {
	result.SummaryAttempted = true
	result.SummaryCallCount = 1
	if summary != nil {
		result.SummaryPromptTokens = summary.PromptTokens
		result.SummaryCompletionTokens = summary.CompletionTokens
		result.SummaryTotalTokens = summary.TotalTokens
		if summary.CallCount > 0 {
			result.SummaryCallCount = summary.CallCount
			result.SummaryUsageCallCount = summary.UsageCallCount
		} else if hasSummaryUsage(summary) {
			result.SummaryUsageCallCount = 1
		}
	}
	return result
}

func hasSummaryUsage(summary *CompressionSummaryResult) bool {
	return summary != nil && (summary.PromptTokens > 0 || summary.CompletionTokens > 0 || summary.TotalTokens > 0)
}

// ----------------------------------------------------------------------------
// Message group helpers for boundary-safe compression
// ----------------------------------------------------------------------------

// messageGroup identifies a contiguous group of messages that must not be split.
type messageGroup struct {
	Start int // inclusive index into msgs
	End   int // exclusive index into msgs
	Kind  string
}

// groupMessagesForCompression partitions a message list into indivisible groups.
// Each user/assistant+tool-results/standalone-tool forms one group so that
// compression boundaries never orphan tool result messages.
func groupMessagesForCompression(msgs []openai.ChatCompletionMessage) []messageGroup {
	var groups []messageGroup
	i := 0
	for i < len(msgs) {
		msg := msgs[i]
		switch msg.Role {
		case openai.ChatMessageRoleSystem:
			groups = append(groups, messageGroup{Start: i, End: i + 1, Kind: "system"})
			i++
		case openai.ChatMessageRoleUser:
			groups = append(groups, messageGroup{Start: i, End: i + 1, Kind: "user"})
			i++
		case openai.ChatMessageRoleAssistant:
			if len(msg.ToolCalls) > 0 {
				// Assistant with tool calls: group includes all following tool-result messages.
				end := i + 1
				for end < len(msgs) && msgs[end].Role == "tool" {
					end++
				}
				groups = append(groups, messageGroup{Start: i, End: end, Kind: "assistant_tool"})
				i = end
			} else {
				groups = append(groups, messageGroup{Start: i, End: i + 1, Kind: "assistant"})
				i++
			}
		default:
			// Orphan tool message or unknown role: treat as own group.
			groups = append(groups, messageGroup{Start: i, End: i + 1, Kind: "other"})
			i++
		}
	}
	return groups
}

// alignCompressionBoundary returns the message index that is safe to use as the
// exclusive end of the compression batch, snapped to a group boundary.
// It also ensures the latest user turn is never pulled into the compressed span.
func alignCompressionBoundary(groups []messageGroup, targetIndex int, msgs []openai.ChatCompletionMessage) int {
	// Find the last user message index so we can protect it.
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == openai.ChatMessageRoleUser {
			lastUserIdx = i
			break
		}
	}

	// Walk groups: accumulate until we'd exceed targetIndex or hit the last user turn.
	boundary := 0
	for _, g := range groups {
		if g.Start >= targetIndex {
			break
		}
		// Never consume the group containing the latest user turn.
		if lastUserIdx >= 0 && g.Start <= lastUserIdx && g.End > lastUserIdx {
			break
		}
		boundary = g.End
	}
	return boundary
}

// validateProviderPayload checks that a message list is safe to send to a provider.
// It returns an error describing the first violation found.
func validateProviderPayload(msgs []openai.ChatCompletionMessage) error {
	seenCallIDs := make(map[string]struct{})
	var pending map[string]bool
	pendingAssistant := -1

	closePending := func(nextIndex int) error {
		for id, closed := range pending {
			if !closed {
				return fmt.Errorf("assistant message at index %d has tool_call_id %q with no corresponding tool result before index %d", pendingAssistant, id, nextIndex)
			}
		}
		pending = nil
		pendingAssistant = -1
		return nil
	}

	for i := range msgs {
		msg := msgs[i]
		if msg.Role == "tool" {
			if msg.ToolCallID == "" {
				return fmt.Errorf("tool message at index %d has empty ToolCallID", i)
			}
			if pending == nil {
				return fmt.Errorf("tool message at index %d references ToolCallID %q without an immediately preceding assistant tool_calls group", i, msg.ToolCallID)
			}
			closed, ok := pending[msg.ToolCallID]
			if !ok {
				return fmt.Errorf("tool message at index %d references ToolCallID %q outside the preceding assistant group", i, msg.ToolCallID)
			}
			if closed {
				return fmt.Errorf("tool message at index %d duplicates result for ToolCallID %q", i, msg.ToolCallID)
			}
			pending[msg.ToolCallID] = true
			continue
		}

		if pending != nil {
			if err := closePending(i); err != nil {
				return err
			}
		}

		if msg.Role != openai.ChatMessageRoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}

		pending = make(map[string]bool, len(msg.ToolCalls))
		pendingAssistant = i
		for _, tc := range msg.ToolCalls {
			if tc.ID == "" {
				return fmt.Errorf("assistant message at index %d has an empty tool_call_id", i)
			}
			if _, duplicate := seenCallIDs[tc.ID]; duplicate {
				return fmt.Errorf("assistant message at index %d reuses tool_call_id %q", i, tc.ID)
			}
			if _, duplicate := pending[tc.ID]; duplicate {
				return fmt.Errorf("assistant message at index %d duplicates tool_call_id %q", i, tc.ID)
			}
			seenCallIDs[tc.ID] = struct{}{}
			pending[tc.ID] = false
		}
	}

	if pending != nil {
		return closePending(len(msgs))
	}
	return nil
}

// ----------------------------------------------------------------------------
// Core compression logic
// ----------------------------------------------------------------------------

// forceCompressionKey marks a context that requests compression even when the
// thread is under the token budget (used by the manual /compress command).
type forceCompressionKey struct{}

func withForceCompression(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceCompressionKey{}, true)
}

func forceCompression(ctx context.Context) bool {
	v, _ := ctx.Value(forceCompressionKey{}).(bool)
	return v
}

// CompressNow forces a context compression for threadID regardless of the token
// budget, persisting the summary like the automatic path. It returns the result
// (before/after tokens, outcome) for reporting. Used by the CLI /compress
// command. Compression still no-ops when the conversation is too short to
// compress (<= 2 messages).
func (a *Agent) CompressNow(ctx context.Context, threadID, modelAlias string) (CompressionResult, error) {
	resolved, err := a.registry.ResolveModel(modelAlias)
	if err != nil {
		return CompressionResult{}, fmt.Errorf("resolve model: %w", err)
	}
	msgs, err := a.buildMessages(threadID, a.cfg.Workspace, "cli", false, nil, false)
	if err != nil {
		return CompressionResult{}, fmt.Errorf("build messages: %w", err)
	}
	started := time.Now()
	comp := a.compressIfNeeded(withForceCompression(ctx), threadID, msgs, modelAlias, resolved.ContextLength, CompressionModeTurnStart)
	comp.ElapsedMS = time.Since(started).Milliseconds()
	return comp, comp.Err
}

// compressIfNeeded is the main entry point called from the agent turn loop.
// Turn-start mode creates a durable prefix summary. Mid-loop mode creates a
// transient split-turn checkpoint that keeps the current user request intact.
func (a *Agent) compressIfNeeded(
	ctx context.Context,
	threadID string,
	msgs []indexedMessage,
	modelAlias string,
	contextLength int,
	mode CompressionMode,
) CompressionResult {
	noChange := CompressionResult{
		Messages: msgs,
		Outcome:  CompressionOutcomeNone,
	}

	if contextLength <= 0 {
		return noChange
	}

	counter := llm.NewTokenCounter()

	cfg := a.cfg.Compression
	threshold := cfg.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.80
	}
	budget := int(float64(contextLength) * threshold)
	rawMsgs := toRawMessages(msgs)
	total := counter.CountMessages(rawMsgs)
	if !cfg.Enabled {
		if total <= budget {
			return noChange
		}
		return finalizeCompressionFallback(msgs, budget, counter, CompressionResult{
			Outcome:        CompressionOutcomeFallback,
			BeforeTokens:   total,
			FallbackReason: "compression_disabled",
		})
	}

	if !forceCompression(ctx) && total <= budget {
		return noChange
	}
	if len(msgs) <= 2 {
		if total <= budget {
			return noChange
		}
		return finalizeCompressionFallback(msgs, budget, counter, CompressionResult{
			Outcome:        CompressionOutcomeError,
			BeforeTokens:   total,
			FallbackReason: "no_compressible_history",
			Err:            ErrUnsafeProviderPayload,
		})
	}

	beforeTokens := total

	// Mid-loop mode first performs the cheap old-output pruning pass. If the
	// active turn is still too large, summarize only complete work groups after
	// the latest persisted user message into a transient checkpoint.
	if mode == CompressionModeMidLoop {
		pruned, prunedCount := pruneOldToolOutputs(msgs)
		if prunedCount > 0 {
			afterRaw := toRawMessages(pruned)
			afterTokens := counter.CountMessages(afterRaw)
			// If pruning alone brought us under budget, return pruned result.
			if afterTokens <= budget {
				recentRawTokens := activeTurnRawTailTokens(pruned, counter)
				return CompressionResult{
					Messages:               pruned,
					Outcome:                CompressionOutcomePruned,
					BeforeTokens:           beforeTokens,
					AfterTokens:            afterTokens,
					BudgetTokens:           budget,
					TargetTokens:           activeTurnTargetBudget(contextLength, budget, cfg.PostCompressionRatio),
					RecentTailTargetTokens: activeTurnRequiredRecentTail(contextLength, cfg.RecentTailTokens, recentRawTokens),
					RecentTailTokens:       recentRawTokens,
					PrunedToolOutputs:      prunedCount,
				}
			}
		}
		res := a.compressActiveTurn(ctx, pruned, modelAlias, contextLength, beforeTokens, budget, prunedCount, counter)
		// A huge durable prefix with a small active turn leaves mid-loop
		// nothing to checkpoint (the recent-tail floor protects everything).
		// Escalate to a durable prefix compression — the same turn-start path,
		// which summarizes the prefix and persists the record — instead of
		// blindly truncating history. If escalation fails, the original
		// fallback truncation result stands.
		if res.Outcome == CompressionOutcomeFallback &&
			strings.Contains(res.FallbackReason, "mid_loop_no_old_prefix_with_recent_tail") &&
			latestPersistedUserIndex(msgs) >= 2 {
			if escalated := a.compressIfNeeded(ctx, threadID, msgs, modelAlias, contextLength, CompressionModeTurnStart); escalated.Outcome == CompressionOutcomeCompressed {
				return escalated
			}
		}
		return res
	}

	// Turn-start mode: attempt full LLM summarization.
	modelAliasCfg := cfg.Model
	if modelAliasCfg == "" {
		modelAliasCfg = modelAlias
	}

	resolved, err := a.registry.ResolveModel(modelAliasCfg)
	if err != nil {
		return finalizeCompressionFallback(msgs, budget, counter, CompressionResult{
			Outcome:        CompressionOutcomeError,
			BeforeTokens:   beforeTokens,
			FallbackReason: fmt.Sprintf("resolve_model_failed: %v", err),
			Err:            err,
		})
	}

	// Find safe compression boundary.
	const batchStart = 1 // skip system prompt at index 0
	targetIndex := batchStart

	// When forced (user ran /compress) or when under budget, the budget-based
	// loop below would stop immediately (total - batchTokens <= target is true
	// on the first iteration). That compresses almost nothing — the summary
	// ends up larger than the batch it replaced. Instead, compress everything
	// up to the latest user turn so the summary is meaningful.
	forced := forceCompression(ctx)
	if forced || total <= budget {
		targetIndex = len(rawMsgs) - 1 // compress everything except last message
	} else {
		// Compress toward the post-compression target, not merely under the
		// hard budget. Landing a few tokens under budget leaves no headroom
		// for the upcoming turn's tool outputs: the next mid-loop check then
		// fires with nothing safe to summarize and degrades to blind
		// truncation of history.
		target := activeTurnTargetBudget(contextLength, budget, cfg.PostCompressionRatio)
		batchTokens := 0
		for i := batchStart; i < len(rawMsgs)-1; i++ {
			batchTokens += estimateMessageTokens(&rawMsgs[i], counter)
			targetIndex = i + 1
			if total-batchTokens <= target {
				break
			}
		}
	}

	// Align to group boundary.
	rawForGroups := rawMsgs[batchStart:] // work with the non-system slice
	groups := groupMessagesForCompression(rawForGroups)
	// targetIndex is relative to rawMsgs; convert to rawForGroups index:
	relTarget := targetIndex - batchStart
	alignedEnd := alignCompressionBoundary(groups, relTarget, rawForGroups)
	batchEndInFull := batchStart + alignedEnd // absolute index in rawMsgs

	if batchEndInFull <= batchStart {
		return finalizeCompressionFallback(msgs, budget, counter, CompressionResult{
			Outcome:        CompressionOutcomeFallback,
			BeforeTokens:   beforeTokens,
			FallbackReason: "boundary_too_small",
		})
	}

	batch := rawMsgs[batchStart:batchEndInFull]
	// Prune oversized tool outputs before summarizing to reduce summarizer input.
	pruneCount := 0
	for i := range batch {
		if batch[i].Role == "tool" && len(batch[i].Content) > 2000 {
			origLen := len(batch[i].Content)
			batch[i].Content = batch[i].Content[:2000] + fmt.Sprintf("\n[tool output trimmed for compression: %d original chars]", origLen)
			pruneCount++
		}
	}
	// Redact secrets from the batch before it reaches the summarizer.
	for i := range batch {
		batch[i].Content = redactSecrets(batch[i].Content)
	}

	// Call the summarizer using injected factory.
	summarizerFactory := a.summarizers
	if summarizerFactory == nil {
		summarizerFactory = &productionSummarizerFactory{}
	}
	summarizer := summarizerFactory.NewSummarizer(resolved)

	// Use configured timeout, falling back to 120s if unset (e.g. test configs
	// that bypass Load()). 0 would create an immediately-expired context.
	timeoutSecs := cfg.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = 120
	}
	compressCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	// Compute max output tokens from target_ratio, clamped to configured bounds.
	maxOutput := int(cfg.TargetRatio * float64(total))
	if maxOutput < cfg.MinSummaryTokens {
		maxOutput = cfg.MinSummaryTokens
	}
	if cfg.MaxSummaryTokens > 0 && maxOutput > cfg.MaxSummaryTokens {
		maxOutput = cfg.MaxSummaryTokens
	}
	summaryReq := CompressionSummaryRequest{
		ModelAlias:      modelAliasCfg,
		ModelID:         resolved.ModelID,
		Messages:        batch,
		MaxOutputTokens: maxOutput,
	}

	summaryResult, err := summarizer.Summarize(compressCtx, summaryReq)
	if err != nil {
		result := withSummaryTelemetry(CompressionResult{
			Outcome:           CompressionOutcomeError,
			BeforeTokens:      beforeTokens,
			SummaryModelAlias: modelAliasCfg,
			SummaryModelID:    resolved.ModelID,
			FallbackReason:    fmt.Sprintf("summarizer_error: %v", err),
			Err:               err,
		}, summaryResult)
		return finalizeCompressionFallback(msgs, budget, counter, result)
	}

	summary := redactSecrets(strings.TrimSpace(summaryResult.Content))
	if summary == "" {
		result := withSummaryTelemetry(CompressionResult{
			Outcome:        CompressionOutcomeFallback,
			BeforeTokens:   beforeTokens,
			FallbackReason: "empty_summary",
		}, summaryResult)
		return finalizeCompressionFallback(msgs, budget, counter, result)
	}

	// Determine FirstKeptSeq from first non-synthetic retained thread message.
	firstKeptSeq := 0
	for _, im := range msgs[batchEndInFull:] {
		if !im.Synthetic && im.Seq > 0 {
			firstKeptSeq = im.Seq
			break
		}
	}

	// Build compressed result message list.
	compressed := make([]indexedMessage, 0, len(msgs)-len(batch)+2)
	// Keep system prompt.
	compressed = append(compressed, msgs[0])
	// Inject synthetic compression summary.
	compressed = append(compressed, indexedMessage{
		Seq:       0,
		Synthetic: true,
		Kind:      "compression_summary",
		Msg: openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: "[Compressed context from earlier: " + summary + "]",
		},
	})
	// Keep remaining messages after compressed span.
	compressed = append(compressed, msgs[batchEndInFull:]...)

	afterRaw := toRawMessages(compressed)

	// Defense-in-depth: verify the compressed payload has no orphan tool
	// messages or missing tool results. This catches edge cases that the
	// group-boundary alignment should prevent but doesn't guarantee.
	if err := validateProviderPayload(afterRaw); err != nil {
		result := withSummaryTelemetry(CompressionResult{
			Outcome:        CompressionOutcomeFallback,
			BeforeTokens:   beforeTokens,
			FallbackReason: fmt.Sprintf("payload_validation: %v", err),
		}, summaryResult)
		return finalizeCompressionFallback(msgs, budget, counter, result)
	}

	afterTokens := counter.CountMessages(afterRaw)

	// If the summary didn't actually reduce the token count, don't use it.
	// Return the original messages unchanged — no compression, no truncation.
	if afterTokens >= beforeTokens {
		result := withSummaryTelemetry(CompressionResult{
			Outcome:        CompressionOutcomeFallback,
			BeforeTokens:   beforeTokens,
			FallbackReason: "ineffective_summary",
		}, summaryResult)
		return finalizeCompressionFallback(msgs, budget, counter, result)
	}
	if afterTokens > budget {
		result := withSummaryTelemetry(CompressionResult{
			Outcome:        CompressionOutcomeFallback,
			BeforeTokens:   beforeTokens,
			FallbackReason: "summary_over_budget",
		}, summaryResult)
		return finalizeCompressionFallback(msgs, budget, counter, result)
	}

	result := withSummaryTelemetry(CompressionResult{
		Messages:          compressed,
		Outcome:           CompressionOutcomeCompressed,
		Summary:           summary,
		SummaryModelAlias: modelAliasCfg,
		SummaryModelID:    resolved.ModelID,
		FirstKeptSeq:      firstKeptSeq,
		CompressedCount:   len(batch),
		PrunedToolOutputs: pruneCount,
		BeforeTokens:      beforeTokens,
		AfterTokens:       afterTokens,
		BudgetTokens:      budget,
	}, summaryResult)

	// Persist compression record so future turns can reuse the summary.
	if threadID != "" && a.store != nil && result.FirstKeptSeq > 0 {
		rec := memory.CompressionRecord{
			ThreadID:                threadID,
			Summary:                 result.Summary,
			FirstKeptSeq:            result.FirstKeptSeq,
			CompressedMessageCount:  result.CompressedCount,
			PrunedToolOutputs:       result.PrunedToolOutputs,
			BeforeTokens:            result.BeforeTokens,
			AfterTokens:             result.AfterTokens,
			BudgetTokens:            result.BudgetTokens,
			SummaryModelAlias:       result.SummaryModelAlias,
			SummaryModelID:          result.SummaryModelID,
			SummaryPromptTokens:     result.SummaryPromptTokens,
			SummaryCompletionTokens: result.SummaryCompletionTokens,
			SummaryTotalTokens:      result.SummaryTotalTokens,
		}
		if err := a.store.SaveCompression(rec); err != nil {
			// The summary is already in effect for this turn; only durable
			// reuse across future turns failed. Keep the compressed outcome
			// but surface the persistence failure on the terminal event so
			// the UI shows it instead of silently losing the record.
			result.SaveError = fmt.Sprintf("persist summary: %v", err)
			result.FallbackUsed = true
			result.FallbackReason = appendFallbackReason(result.FallbackReason, result.SaveError)
		}
	}

	return result
}

const activeTurnPostCompressionRatio = 0.50

// activeTurnRecentTailTarget returns the desired minimum amount of recent raw
// work to preserve after a mid-loop checkpoint. The automatic value scales on
// small contexts and lands near 10K for a 65K context, capped at 12K so it does
// not consume all available headroom on larger models.
func activeTurnRecentTailTarget(contextLength, configured int) int {
	if configured > 0 {
		return configured
	}
	if contextLength <= 0 {
		return 0
	}
	target := int(float64(contextLength) * 0.15)
	if contextLength >= 65536 {
		if target < 8192 {
			target = 8192
		}
		if target > 12288 {
			target = 12288
		}
		return target
	}
	if target < 64 {
		target = 64
	}
	if target > 8192 {
		target = 8192
	}
	return target
}

// The hard floor is enabled automatically for realistic long-context models.
// Tiny synthetic contexts retain historical behavior so tests, embeddings, and
// deliberately small local models can still summarize a single oversized group.
// An explicit recent_tail_tokens value opts any context size into the hard floor.
func activeTurnRequiredRecentTail(contextLength, configured, totalRaw int) int {
	if configured <= 0 && contextLength < 65536 {
		return 0
	}
	required := activeTurnRecentTailTarget(contextLength, configured)
	if totalRaw < required {
		return totalRaw
	}
	return required
}

// activeTurnTargetBudget returns the desired post-compression message-context
// size: turn-start compression selects its batch to land at or below it, and a
// mid-loop checkpoint treats it as the target for the retained raw tail plus
// fixed prefix. Leaving headroom below the hard budget is the point — a
// turn-start compression that lands just under budget leaves no room for the
// turn's tool outputs and immediately re-triggers mid-loop compression.
func activeTurnTargetBudget(contextLength, hardBudget int, configuredRatio float64) int {
	ratio := configuredRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = activeTurnPostCompressionRatio
	}
	target := int(float64(contextLength) * ratio)
	if target <= 0 || target > hardBudget {
		return hardBudget
	}
	return target
}

// activeTurnSummaryReserve estimates room for the new checkpoint. It is a
// planning reserve only; provider tokenizers and actual output sizes differ.
func activeTurnSummaryReserve(contextLength, targetBudget int, cfg config.CompressionConfig) int {
	reserve := int(float64(contextLength) * 0.06)
	if reserve < cfg.MinSummaryTokens {
		reserve = cfg.MinSummaryTokens
	}
	if cfg.MaxSummaryTokens > 0 && reserve > cfg.MaxSummaryTokens {
		reserve = cfg.MaxSummaryTokens
	}
	// Tiny contexts used by tests (and unusually small production contexts)
	// must still leave room for the system prompt, request, and a raw tail.
	cap := targetBudget / 4
	if reserve > cap {
		reserve = cap
	}
	if reserve < 0 {
		return 0
	}
	return reserve
}

func messageRangeTokens(msgs []indexedMessage, counter *llm.TokenCounter) int {
	total := 0
	for i := range msgs {
		total += estimateMessageTokens(&msgs[i].Msg, counter)
	}
	return total
}

// activeTurnRawTailTokens counts only persisted assistant/tool work after the
// current user. Synthetic checkpoints are intentionally excluded: they are not
// a substitute for the verbatim recent-work floor.
func activeTurnRawTailTokens(msgs []indexedMessage, counter *llm.TokenCounter) int {
	userIdx := latestPersistedUserIndex(msgs)
	if userIdx < 0 {
		return 0
	}
	total := 0
	for i := userIdx + 1; i < len(msgs); i++ {
		if msgs[i].Synthetic {
			continue
		}
		total += estimateMessageTokens(&msgs[i].Msg, counter)
	}
	return total
}

// Provider completion usage may use a different tokenizer or include reasoning
// tokens, so checkpoint quality is measured with Sandbar's local counter.
func minimumUsefulActiveSummaryTokens(batchTokens int) int {
	if batchTokens < 4096 {
		return 0
	}
	minimum := batchTokens / 50 // about 2% of the bounded summarizer input
	if minimum < 768 {
		minimum = 768
	}
	if minimum > 2048 {
		minimum = 2048
	}
	return minimum
}

func summaryContentTokens(summary string, counter *llm.TokenCounter) int {
	if counter == nil || summary == "" {
		return 0
	}
	withContent := counter.CountMessage(&openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: summary})
	empty := counter.CountMessage(&openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser})
	if withContent <= empty {
		return 0
	}
	return withContent - empty
}

func addSummaryTelemetry(total **CompressionSummaryResult, next *CompressionSummaryResult) {
	if *total == nil {
		*total = &CompressionSummaryResult{}
	}
	(*total).CallCount++
	if next == nil {
		return
	}
	(*total).Content = next.Content
	if !hasSummaryUsage(next) {
		return
	}
	(*total).UsageCallCount++
	(*total).PromptTokens += next.PromptTokens
	(*total).CompletionTokens += next.CompletionTokens
	(*total).TotalTokens += next.TotalTokens
}

// compressActiveTurn summarizes only the oldest completed assistant/tool groups
// from the currently executing user turn. A model/context-aware suffix of recent
// groups remains verbatim. The checkpoint is transient: raw messages remain in
// SQLite, while a prior checkpoint is safely folded into a later prefix summary.
func (a *Agent) compressActiveTurn(
	ctx context.Context,
	msgs []indexedMessage,
	modelAlias string,
	contextLength int,
	beforeTokens int,
	budget int,
	prunedCount int,
	counter *llm.TokenCounter,
) CompressionResult {
	var summaryTelemetry *CompressionSummaryResult
	var summaryModelAlias, summaryModelID string
	var targetTokens, requiredRecentTail int
	fallback := func(reason string, cause error) CompressionResult {
		outcome := CompressionOutcomeFallback
		if cause != nil {
			outcome = CompressionOutcomeError
		}
		result := CompressionResult{
			Outcome:           outcome,
			BeforeTokens:      beforeTokens,
			FallbackReason:    reason,
			PrunedToolOutputs: prunedCount,
			SummaryModelAlias: summaryModelAlias,
			SummaryModelID:    summaryModelID,
			Err:               cause,
		}
		if summaryTelemetry != nil && summaryTelemetry.CallCount > 0 {
			result = withSummaryTelemetry(result, summaryTelemetry)
		}
		result = finalizeCompressionFallback(msgs, budget, counter, result)
		result.TargetTokens = targetTokens
		result.RecentTailTargetTokens = requiredRecentTail
		result.RecentTailTokens = activeTurnRawTailTokens(result.Messages, counter)
		if requiredRecentTail > 0 && result.RecentTailTokens < requiredRecentTail {
			// Never turn a recoverable compression failure into a deceptively
			// tiny but provider-valid prompt. Preserve the original in-memory
			// state and stop before the next provider request instead.
			tailErr := fmt.Errorf("%w: recent raw tail would fall from %d required tokens to %d", ErrUnsafeProviderPayload, requiredRecentTail, result.RecentTailTokens)
			result.Messages = msgs
			result.AfterTokens = beforeTokens
			result.Outcome = CompressionOutcomeError
			result.Err = errors.Join(result.Err, tailErr)
			result.FallbackReason = appendFallbackReason(result.FallbackReason, "recent_tail_floor_violation")
			result.RecentTailTokens = activeTurnRawTailTokens(msgs, counter)
		}
		return result
	}

	providerMsgs := toRawMessages(msgs)
	if err := validateProviderPayload(providerMsgs); err != nil {
		return fallback(fmt.Sprintf("mid_loop_invalid_payload: %v", err), err)
	}

	userIdx := latestPersistedUserIndex(msgs)
	if userIdx < 0 || userIdx+1 >= len(msgs) {
		return fallback("mid_loop_no_complete_active_groups", nil)
	}

	// A previous active checkpoint is synthetic. Require at least one persisted
	// assistant/tool message so we never spend a model call summarizing only the
	// previous checkpoint.
	hasNewWork := false
	for _, im := range msgs[userIdx+1:] {
		if !im.Synthetic {
			hasNewWork = true
			break
		}
	}
	if !hasNewWork {
		return fallback("mid_loop_no_new_complete_groups", nil)
	}

	// A previous checkpoint belongs to the old prefix, not the raw recent suffix.
	// Folding it into the replacement checkpoint preserves recursive state while
	// ensuring the provider receives exactly one checkpoint wrapper.
	workStart := userIdx + 1
	if workStart < len(msgs) && msgs[workStart].Kind == activeTurnSummaryKind {
		workStart++
	}

	workRaw := toRawMessages(msgs[workStart:])
	groups := groupMessagesForCompression(workRaw)
	if len(groups) == 0 {
		return fallback("mid_loop_no_complete_work_groups", nil)
	}

	targetBudget := activeTurnTargetBudget(contextLength, budget, a.cfg.Compression.PostCompressionRatio)
	targetTokens = targetBudget
	fixedTokens := counter.CountMessages(toRawMessages(msgs[:userIdx+1]))
	reserve := activeTurnSummaryReserve(contextLength, targetBudget, a.cfg.Compression)
	recentTarget := activeTurnRecentTailTarget(contextLength, a.cfg.Compression.RecentTailTokens)
	totalRawWorkTokens := messageRangeTokens(msgs[workStart:], counter)
	requiredRecentTail = activeTurnRequiredRecentTail(contextLength, a.cfg.Compression.RecentTailTokens, totalRawWorkTokens)
	recentLimit := targetBudget - fixedTokens - reserve
	maxRecentAtHardBudget := budget - fixedTokens - reserve
	if maxRecentAtHardBudget < 0 {
		maxRecentAtHardBudget = 0
	}
	if recentLimit < recentTarget {
		recentLimit = recentTarget
		if recentLimit > maxRecentAtHardBudget {
			recentLimit = maxRecentAtHardBudget
		}
	}
	if recentLimit < 0 {
		recentLimit = 0
	}

	groupTokens := make([]int, len(groups))
	for i, group := range groups {
		groupTokens[i] = messageRangeTokens(msgs[workStart+group.Start:workStart+group.End], counter)
	}
	protectedGroup := len(groups)
	protectedTokens := 0
	if requiredRecentTail > 0 {
		for i := len(groups) - 1; i >= 0; i-- {
			protectedGroup = i
			protectedTokens += groupTokens[i]
			if protectedTokens >= requiredRecentTail {
				break
			}
		}
	}

	retainedRawTokens := totalRawWorkTokens
	batchEnd := workStart
	for i, group := range groups {
		if retainedRawTokens <= recentLimit {
			break
		}
		// Never cross the precomputed whole-group suffix that satisfies the
		// raw recent-work floor, even when an uneven boundary group is large.
		if i >= protectedGroup {
			break
		}
		retainedRawTokens -= groupTokens[i]
		batchEnd = workStart + group.End
	}
	if batchEnd == workStart {
		return fallback("mid_loop_no_old_prefix_with_recent_tail", nil)
	}

	// The provider payload validator proves each selected assistant/tool group is
	// complete. Include the current user for summarizer orientation, but replace
	// only the prior checkpoint and selected oldest work prefix.
	batch := make([]openai.ChatCompletionMessage, 0, batchEnd-userIdx)
	batch = append(batch, msgs[userIdx].Msg)
	for _, im := range msgs[userIdx+1 : batchEnd] {
		msg := im.Msg
		if im.Kind == activeTurnSummaryKind {
			msg.Role = openai.ChatMessageRoleUser
			msg.Content = "Prior active-turn checkpoint:\n" + msg.Content
		}
		batch = append(batch, msg)
	}
	batch, batchPruned := prepareCompressionBatch(batch)
	prunedCount += batchPruned
	// The quality floor describes the actual redacted/trimmed payload sent to
	// the summarizer, not the potentially enormous raw tool output it replaced.
	batchTokens := counter.CountMessages(batch)
	minimumUsefulTokens := minimumUsefulActiveSummaryTokens(batchTokens)

	cfg := a.cfg.Compression
	modelAliasCfg := cfg.Model
	if modelAliasCfg == "" {
		modelAliasCfg = modelAlias
	}
	resolved, err := a.registry.ResolveModel(modelAliasCfg)
	if err != nil {
		return fallback(fmt.Sprintf("resolve_model_failed: %v", err), err)
	}
	summaryModelAlias = modelAliasCfg
	summaryModelID = resolved.ModelID

	summarizerFactory := a.summarizers
	if summarizerFactory == nil {
		summarizerFactory = &productionSummarizerFactory{}
	}
	summarizer := summarizerFactory.NewSummarizer(resolved)
	timeoutSecs := cfg.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = 120
	}
	compressCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	maxOutput := int(cfg.TargetRatio * float64(beforeTokens))
	if maxOutput < cfg.MinSummaryTokens {
		maxOutput = cfg.MinSummaryTokens
	}
	if cfg.MaxSummaryTokens > 0 && maxOutput > cfg.MaxSummaryTokens {
		maxOutput = cfg.MaxSummaryTokens
	}
	if maxOutput > 0 && minimumUsefulTokens > (maxOutput*3)/4 {
		minimumUsefulTokens = (maxOutput * 3) / 4
	}

	summaryResult, err := summarizer.Summarize(compressCtx, CompressionSummaryRequest{
		ModelAlias:          modelAliasCfg,
		ModelID:             resolved.ModelID,
		Messages:            batch,
		MaxOutputTokens:     maxOutput,
		MinimumUsefulTokens: minimumUsefulTokens,
	})
	addSummaryTelemetry(&summaryTelemetry, summaryResult)
	if err != nil {
		return fallback(fmt.Sprintf("summarizer_error: %v", err), err)
	}
	summary := redactSecrets(strings.TrimSpace(summaryResult.Content))
	if summary == "" {
		return fallback("empty_summary", nil)
	}
	if got := summaryContentTokens(summary, counter); minimumUsefulTokens > 0 && got < minimumUsefulTokens {
		retryResult, retryErr := summarizer.Summarize(compressCtx, CompressionSummaryRequest{
			ModelAlias:          modelAliasCfg,
			ModelID:             resolved.ModelID,
			Messages:            batch,
			MaxOutputTokens:     maxOutput,
			MinimumUsefulTokens: minimumUsefulTokens,
			Retry:               true,
		})
		addSummaryTelemetry(&summaryTelemetry, retryResult)
		if retryErr != nil {
			return fallback(fmt.Sprintf("summary_too_short_then_retry_failed: got %d, need %d: %v", got, minimumUsefulTokens, retryErr), retryErr)
		}
		summaryResult = retryResult
		summary = redactSecrets(strings.TrimSpace(summaryResult.Content))
		retryTokens := summaryContentTokens(summary, counter)
		if summary == "" || retryTokens < minimumUsefulTokens {
			return fallback(fmt.Sprintf("summary_too_short_after_retry: got %d, need %d", retryTokens, minimumUsefulTokens), nil)
		}
	}

	compressed := make([]indexedMessage, 0, userIdx+2)
	compressed = append(compressed, msgs[:userIdx+1]...)
	compressed = append(compressed, indexedMessage{
		Synthetic: true,
		Kind:      activeTurnSummaryKind,
		Msg: openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: summary,
		},
	})
	compressed = append(compressed, msgs[batchEnd:]...)

	afterRaw := toRawMessages(compressed)
	if err := validateProviderPayload(afterRaw); err != nil {
		return fallback(fmt.Sprintf("payload_validation: %v", err), err)
	}
	afterTokens := counter.CountMessages(afterRaw)
	if afterTokens >= beforeTokens {
		return fallback("ineffective_active_turn_summary", nil)
	}

	fallbackUsed := false
	fallbackReason := ""
	if afterTokens > budget {
		return fallback("active_turn_summary_over_budget", nil)
	}
	recentRawTokens := activeTurnRawTailTokens(compressed, counter)
	if recentRawTokens < requiredRecentTail {
		return fallback("active_turn_recent_tail_floor_violation", fmt.Errorf("%w: retained %d raw recent tokens, require %d", ErrUnsafeProviderPayload, recentRawTokens, requiredRecentTail))
	}
	if err := validateProviderPayload(afterRaw); err != nil {
		return fallback(fmt.Sprintf("payload_validation_after_fallback: %v", err), err)
	}

	return withSummaryTelemetry(CompressionResult{
		Messages:               compressed,
		Outcome:                CompressionOutcomeCompressed,
		Summary:                summary,
		SummaryModelAlias:      modelAliasCfg,
		SummaryModelID:         resolved.ModelID,
		CompressedCount:        batchEnd - userIdx - 1,
		PrunedToolOutputs:      prunedCount,
		BeforeTokens:           beforeTokens,
		AfterTokens:            afterTokens,
		BudgetTokens:           budget,
		TargetTokens:           targetBudget,
		RecentTailTargetTokens: requiredRecentTail,
		RecentTailTokens:       recentRawTokens,
		FallbackUsed:           fallbackUsed,
		FallbackReason:         fallbackReason,
	}, summaryTelemetry)
}

func latestPersistedUserIndex(msgs []indexedMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].Synthetic && msgs[i].Msg.Role == openai.ChatMessageRoleUser {
			return i
		}
	}
	return -1
}

// prepareCompressionBatch returns a redacted copy with oversized tool output
// bounded for the auxiliary model. The beginning and end are retained because
// errors and command summaries often appear at opposite ends of output.
func prepareCompressionBatch(batch []openai.ChatCompletionMessage) ([]openai.ChatCompletionMessage, int) {
	out := append([]openai.ChatCompletionMessage(nil), batch...)
	pruned := 0
	for i := range out {
		if out[i].Role == "tool" && len(out[i].Content) > 2000 {
			originalLen := len(out[i].Content)
			out[i].Content = out[i].Content[:1500] +
				fmt.Sprintf("\n[tool output trimmed for compression: %d original chars]\n", originalLen) +
				out[i].Content[originalLen-500:]
			pruned++
		}
		out[i].Content = redactSecrets(out[i].Content)
	}
	return out, pruned
}

// ----------------------------------------------------------------------------
// Event helpers
// ----------------------------------------------------------------------------

// buildCompressionStartEvent creates a CompressionEvent for the start of
// compression from the pre-work snapshot, so the payload is honest at emission
// time: the summarizer has not run yet, so after/outcome fields stay unset.
func buildCompressionStartEvent(pre compressionPreWork) *llm.CompressionEvent {
	return &llm.CompressionEvent{
		ModelAlias:      pre.modelAlias,
		BeforeTokens:    pre.beforeTokens,
		BudgetTokens:    pre.budgetTokens,
		ThresholdTokens: pre.thresholdTokens,
		TargetTokens:    pre.targetTokens,
	}
}

func buildCompressionEndEvent(result CompressionResult) *llm.CompressionEvent {
	ev := &llm.CompressionEvent{
		Outcome:                 string(result.Outcome),
		SummaryAttempted:        result.SummaryAttempted,
		ModelAlias:              result.SummaryModelAlias,
		ModelID:                 result.SummaryModelID,
		BeforeTokens:            result.BeforeTokens,
		AfterTokens:             result.AfterTokens,
		BudgetTokens:            result.BudgetTokens,
		TargetTokens:            result.TargetTokens,
		RecentTailTargetTokens:  result.RecentTailTargetTokens,
		RecentTailTokens:        result.RecentTailTokens,
		CompressedMessageCount:  result.CompressedCount,
		PrunedToolOutputs:       result.PrunedToolOutputs,
		SummaryPromptTokens:     result.SummaryPromptTokens,
		SummaryCompletionTokens: result.SummaryCompletionTokens,
		SummaryTotalTokens:      result.SummaryTotalTokens,
		SummaryCallCount:        result.SummaryCallCount,
		SummaryUsageCallCount:   result.SummaryUsageCallCount,
		FallbackUsed:            result.FallbackUsed,
		FallbackReason:          result.FallbackReason,
		Error:                   result.SaveError,
		ElapsedMS:               result.ElapsedMS,
	}
	return ev
}

func buildCompressionErrorEvent(result CompressionResult) *llm.CompressionEvent {
	ev := buildCompressionEndEvent(result)
	if result.Err != nil {
		ev.Error = result.Err.Error()
	}
	return ev
}

// buildCompressionAuxiliaryUsageEvent emits one aggregate usage event for all
// summary calls whose provider usage was reported. The explicit coverage count
// lets consumers reject a partial aggregate after a retry/error.
func buildCompressionAuxiliaryUsageEvent(result CompressionResult) *llm.StreamEvent {
	if result.SummaryUsageCallCount <= 0 {
		return nil
	}
	return &llm.StreamEvent{
		Type:             "auxiliary_usage",
		UsagePurpose:     "compression",
		PromptTokens:     result.SummaryPromptTokens,
		CompletionTokens: result.SummaryCompletionTokens,
		TotalTokens:      result.SummaryTotalTokens,
		UsageCallCount:   result.SummaryUsageCallCount,
	}
}

// compressionPreWork is the pre-work snapshot a compression_start event needs:
// the token counts the compressor will act on plus the summarizer model alias,
// computed before compressIfNeeded runs so the event payload is honest at
// emission time.
type compressionPreWork struct {
	willAttempt     bool
	beforeTokens    int
	budgetTokens    int
	thresholdTokens int
	targetTokens    int
	modelAlias      string
}

// preCompressionSnapshot decides whether a compression attempt is imminent and
// captures the before/budget/threshold/target values the compressor will see.
// It mirrors compressIfNeeded's own gating (enabled, over budget), so when
// willAttempt is true the subsequent compressIfNeeded call is guaranteed to
// produce a terminal compression event.
func preCompressionSnapshot(msgs []indexedMessage, modelAlias string, contextLength int, cfg config.CompressionConfig) compressionPreWork {
	if contextLength <= 0 || !cfg.Enabled {
		return compressionPreWork{}
	}
	counter := llm.NewTokenCounter()
	before := counter.CountMessages(toRawMessages(msgs))
	threshold := cfg.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.80
	}
	budget := int(float64(contextLength) * threshold)
	if before <= budget {
		return compressionPreWork{}
	}
	modelAliasCfg := cfg.Model
	if modelAliasCfg == "" {
		modelAliasCfg = modelAlias
	}
	return compressionPreWork{
		willAttempt:     true,
		beforeTokens:    before,
		budgetTokens:    budget,
		thresholdTokens: budget,
		targetTokens:    activeTurnTargetBudget(contextLength, budget, cfg.PostCompressionRatio),
		modelAlias:      modelAliasCfg,
	}
}

// shouldAttemptCompression returns true when the thread exceeds its
// compression budget — the same pre-work gate preCompressionSnapshot derives
// its payload from (kept as a thin wrapper for tests and callers that only
// need the boolean).
func shouldAttemptCompression(msgs []indexedMessage, contextLength int, cfg config.CompressionConfig) bool {
	return preCompressionSnapshot(msgs, "", contextLength, cfg).willAttempt
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

// pruneOldToolOutputs caps oversized tool outputs in messages older than the
// latest user turn; the latest turn and everything after it are "recent" and
// left alone, as is the system prompt (index 0).
// Returns the modified message list and the count of pruned tool outputs.
func pruneOldToolOutputs(msgs []indexedMessage) ([]indexedMessage, int) {
	const maxOutputLen = 2000

	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Msg.Role == openai.ChatMessageRoleUser {
			lastUserIdx = i
			break
		}
	}

	// No user turn means everything is recent — prune nothing.
	if lastUserIdx < 0 {
		result := make([]indexedMessage, len(msgs))
		copy(result, msgs)
		return result, 0
	}

	result := make([]indexedMessage, len(msgs))
	copy(result, msgs)
	pruned := 0

	for i := range result {
		if i == 0 {
			continue
		}
		// Skip recent messages (latest user turn and everything after).
		if lastUserIdx >= 0 && i >= lastUserIdx {
			continue
		}
		if result[i].Msg.Role != "tool" {
			continue
		}
		content := result[i].Msg.Content
		if len(content) <= maxOutputLen {
			continue
		}

		truncated := content[:maxOutputLen]
		lines := strings.Count(content, "\n") + 1
		// Resolve the tool name from the preceding assistant group's call ID.
		toolName := "tool"
		if i > 0 && result[i-1].Msg.Role == openai.ChatMessageRoleAssistant {
			toolCallID := result[i].Msg.ToolCallID
			for _, tc := range result[i-1].Msg.ToolCalls {
				if tc.ID == toolCallID && tc.Function.Name != "" {
					toolName = tc.Function.Name
					break
				}
			}
		}

		metadata := fmt.Sprintf("\n[%s output pruned: %d → %d chars (%d lines)]",
			toolName, len(content), maxOutputLen, lines)
		result[i].Msg.Content = truncated + metadata
		pruned++
	}
	return result, pruned
}

// toRawMessages strips indexedMessage metadata to get provider-facing OpenAI
// messages. Active-turn summaries are merged into the immediately preceding
// user message, preserving a conventional user -> assistant/tool sequence while
// leaving the persisted user message itself untouched.
func toRawMessages(msgs []indexedMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Kind == activeTurnSummaryKind {
			if len(out) > 0 && out[len(out)-1].Role == openai.ChatMessageRoleUser {
				out[len(out)-1].Content += activeTurnSummaryWrapper + m.Msg.Content + "\n[END SANDBAR ACTIVE-TURN CHECKPOINT]"
				continue
			}
			// A misplaced checkpoint is retained as a user message so runtime
			// validation can reject any surrounding malformed tool sequence.
			checkpoint := m.Msg
			checkpoint.Role = openai.ChatMessageRoleUser
			checkpoint.Content = activeTurnSummaryWrapper + checkpoint.Content + "\n[END SANDBAR ACTIVE-TURN CHECKPOINT]"
			out = append(out, checkpoint)
			continue
		}
		out = append(out, m.Msg)
	}
	return out
}

// truncateIndexedMessages applies group-safe fallback truncation using the
// historical 80% budget. Compression paths with an explicit configured budget
// call truncateIndexedToBudget directly.
func truncateIndexedMessages(msgs []indexedMessage, contextLength int) []indexedMessage {
	if contextLength <= 0 {
		return msgs
	}
	counter := llm.NewTokenCounter()
	return truncateIndexedToBudget(msgs, int(float64(contextLength)*0.8), counter)
}

// truncateIndexedToBudget drops whole semantic groups, never individual members
// of an assistant/tool-result exchange. The latest persisted user message is
// always retained. Work before it is discarded first, then complete current-turn
// groups oldest-first. A generated active checkpoint is retained until last.
func truncateIndexedToBudget(msgs []indexedMessage, budget int, counter *llm.TokenCounter) []indexedMessage {
	if len(msgs) == 0 || budget <= 0 || counter == nil {
		return msgs
	}
	if counter.CountMessages(toRawMessages(msgs)) <= budget {
		return msgs
	}

	if err := validateProviderPayload(toRawMessages(msgs)); err != nil {
		return minimalSafeIndexedPayload(msgs)
	}

	internalRaw := make([]openai.ChatCompletionMessage, len(msgs))
	for i := range msgs {
		internalRaw[i] = msgs[i].Msg
	}
	groups := groupMessagesForCompression(internalRaw)
	keep := make([]bool, len(msgs))
	for i := range keep {
		keep[i] = true
	}
	userIdx := latestPersistedUserIndex(msgs)
	activeSummaryIdx := -1
	if userIdx >= 0 && userIdx+1 < len(msgs) && msgs[userIdx+1].Kind == activeTurnSummaryKind {
		activeSummaryIdx = userIdx + 1
	}

	var beforeUser, afterUser []messageGroup
	for _, g := range groups {
		if g.Start == 0 && msgs[g.Start].Msg.Role == openai.ChatMessageRoleSystem {
			continue
		}
		if userIdx >= g.Start && userIdx < g.End {
			continue
		}
		if activeSummaryIdx >= g.Start && activeSummaryIdx < g.End {
			continue
		}
		if userIdx < 0 || g.End <= userIdx {
			beforeUser = append(beforeUser, g)
		} else {
			afterUser = append(afterUser, g)
		}
	}

	rebuild := func() []indexedMessage {
		out := make([]indexedMessage, 0, len(msgs))
		for i := range msgs {
			if keep[i] {
				out = append(out, msgs[i])
			}
		}
		return out
	}
	dropGroup := func(g messageGroup) {
		for i := g.Start; i < g.End; i++ {
			keep[i] = false
		}
	}

	for _, candidates := range [][]messageGroup{beforeUser, afterUser} {
		for _, g := range candidates {
			if counter.CountMessages(toRawMessages(rebuild())) <= budget {
				break
			}
			dropGroup(g)
		}
	}

	result := rebuild()
	if counter.CountMessages(toRawMessages(result)) > budget && activeSummaryIdx >= 0 {
		keep[activeSummaryIdx] = false
		result = rebuild()
	}
	if err := validateProviderPayload(toRawMessages(result)); err != nil {
		return minimalSafeIndexedPayload(msgs)
	}
	return result
}

func minimalSafeIndexedPayload(msgs []indexedMessage) []indexedMessage {
	result := make([]indexedMessage, 0, 3)
	if len(msgs) > 0 && msgs[0].Msg.Role == openai.ChatMessageRoleSystem {
		result = append(result, msgs[0])
	}
	userIdx := latestPersistedUserIndex(msgs)
	if userIdx >= 0 {
		result = append(result, msgs[userIdx])
		if userIdx+1 < len(msgs) && msgs[userIdx+1].Kind == activeTurnSummaryKind {
			result = append(result, msgs[userIdx+1])
		}
	}
	return result
}

// wrapMessages wraps plain OpenAI messages as indexedMessages without Seq metadata.
func wrapMessages(msgs []openai.ChatCompletionMessage) []indexedMessage {
	out := make([]indexedMessage, len(msgs))
	for i, m := range msgs {
		kind := "thread_message"
		if m.Role == openai.ChatMessageRoleSystem {
			kind = "system"
		}
		out[i] = indexedMessage{
			Seq:       0,
			Synthetic: m.Role == openai.ChatMessageRoleSystem,
			Kind:      kind,
			Msg:       m,
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Secret redaction
// ----------------------------------------------------------------------------

var redactionPatterns = []struct {
	re      *regexp.Regexp
	replace string
}{
	// Private key blocks (PEM format).
	{regexp.MustCompile(`-----BEGIN\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE\s+KEY-----[\s\S]*?-----END\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE\s+KEY-----`), `[REDACTED PRIVATE KEY]`},
	// Authorization: Bearer <token>
	{regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)\S+`), `${1}[REDACTED]`},
	// Authorization: Basic <base64>
	{regexp.MustCompile(`(?i)(Authorization:\s*Basic\s+)\S+`), `${1}[REDACTED]`},
	// .env style assignments: FOO_API_KEY=secret, SECRET_TOKEN=secret, etc.
	{regexp.MustCompile(`(?i)(\b(?:API_KEY|APIKEY|SECRET|TOKEN|PWD|PASSWORD|ACCESS_KEY|ACCESSKEY|PRIVATE_KEY|PRIVATEKEY|AUTH_TOKEN|BEARER_TOKEN|REFRESH_TOKEN|OAUTH_TOKEN|CLIENT_SECRET|CLIENT_ID|OPENAI_API_KEY|ANTHROPIC_API_KEY|GOOGLE_API_KEY|BRAVE_API_KEY|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY)\s*=\s*)[^\s]*`), `${1}[REDACTED]`},
	// JSON/YAML style: "api_key": "secret" or api_key: secret
	{regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|secret|token|password|access[_-]?key|private[_-]?key|auth[_-]?token|bearer[_-]?token|refresh[_-]?token|client[_-]?secret|client[_-]?id)["']?\s*[:=]\s*["']?)[^\s"',}\]]+`), `${1}[REDACTED]`},
	// OpenAI-style API keys: sk-..., sk-proj-..., sk-test-...
	{regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{20,}\b`), `[REDACTED]`},
	// AWS access key IDs
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), `[REDACTED]`},
	// GitHub personal access tokens
	{regexp.MustCompile(`\bghp_[a-zA-Z0-9]{36,}\b`), `[REDACTED]`},
	{regexp.MustCompile(`\bgho_[a-zA-Z0-9]{36,}\b`), `[REDACTED]`},
	{regexp.MustCompile(`\bgithub_pat_[a-zA-Z0-9_]{22,}\b`), `[REDACTED]`},
	// OAuth / Google tokens (ya29.)
	{regexp.MustCompile(`\bya29\.[a-zA-Z0-9_\-\.]+\b`), `[REDACTED]`},
	// URL-embedded credentials: scheme://user:pass@host
	{regexp.MustCompile(`(://)[^:]+:[^@]+@`), `${1}[REDACTED]:[REDACTED]@`},
	// Generic "Bearer <token>" outside of Authorization header
	{regexp.MustCompile(`(?i)(Bearer\s+)\S+`), `${1}[REDACTED]`},
}

// redactSecrets removes common credential patterns from text so that
// summarizer input, persisted summaries, and emitted events do not leak secrets.
// It preserves enough shape to be useful (e.g., OPENAI_API_KEY=[REDACTED]).
func redactSecrets(text string) string {
	for _, p := range redactionPatterns {
		text = p.re.ReplaceAllString(text, p.replace)
	}
	return text
}

func formatMessagesForCompression(msgs []openai.ChatCompletionMessage) string {
	var sb strings.Builder
	for _, m := range msgs {
		content := redactSecrets(m.Content)

		// If assistant had tool calls, note them.
		if len(m.ToolCalls) > 0 {
			var toolNames []string
			for _, tc := range m.ToolCalls {
				if tc.Type == openai.ToolTypeFunction {
					toolNames = append(toolNames, tc.Function.Name)
				}
			}
			if len(toolNames) > 0 {
				content += fmt.Sprintf("\n[used tools: %s]", strings.Join(toolNames, ", "))
			}
		}

		sb.WriteString(fmt.Sprintf("%s: %s\n", m.Role, content))
	}
	return sb.String()
}

func estimateMessageTokens(msg *openai.ChatCompletionMessage, counter *llm.TokenCounter) int {
	// Per-message estimate without the per-request reply primer, so summing
	// these across a batch does not over-count by 2 tokens per message.
	return counter.CountMessage(msg)
}
