package llm

import "encoding/json"

// StreamEvent represents a single event from the LLM stream. Its JSON form is
// the canonical, machine-readable event schema emitted by Sandbar's surfaces
// (the --json scripting mode), so downstream consumers — scripts and
// benchmark harnesses — can parse one stable shape. The "type" field is
// the discriminator; the remaining fields are populated per type and omitted
// when empty.
type StreamEvent struct {
	Type    string `json:"type"` // thinking, token, tool_call, tool_result, error, done, usage, compression_start, compression_end, compression_error, auxiliary_usage, user_message, and subagent_* variants
	Content string `json:"content,omitempty"`

	// Tool call fields (populated when Type == "tool_call" or "tool_result").
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`

	// Subagent lifecycle fields. TaskID is stable across all events from one
	// delegated run, while ToolCallID continues to identify the parent model's
	// delegate_task invocation. Frontends can therefore maintain a compact live
	// task roster without scraping display text or retaining token deltas.
	TaskID     string `json:"task_id,omitempty"`
	TaskGoal   string `json:"task_goal,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`

	// Approval carries an interactive authorization request when Type is
	// "approval_required". The decision is submitted out-of-band so an SSE
	// stream can remain one-way and the exact approved arguments stay bound to
	// the pending request ID.
	Approval *ApprovalEvent `json:"approval,omitempty"`

	// Usage fields (populated when Type == "usage" — provider's token counts).
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// Cache token columns (populated when Type == "usage" by providers that
	// report prompt caching, e.g. the Anthropic Messages wire; 0 elsewhere).
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// UsageCallCount is the number of calls covered by an aggregated auxiliary
	// usage event. It is normally 1, and is 2 when a compression retry's usage
	// is combined into one event.
	UsageCallCount int `json:"usage_call_count,omitempty"`

	// Compression fields (populated when Type == "compression_start", "compression_end", or "compression_error").
	Compression *CompressionEvent `json:"compression,omitempty"`

	// UsagePurpose indicates the purpose of a usage event: "main", "compression", "title", "subagent".
	UsagePurpose string `json:"usage_purpose,omitempty"`

	// ThreadID is populated on "thread" events (announced at turn start, even
	// for turns that are later interrupted) and on "done" events.
	ThreadID string `json:"thread_id,omitempty"`
}

// ApprovalEvent is the transport-safe form of a centralized tool approval.
// Arguments is the post-resolution argument set that will execute on approval.
type ApprovalEvent struct {
	ID         string          `json:"id"`
	Capability string          `json:"capability"`
	Tool       string          `json:"tool"`
	Tier       string          `json:"tier"`
	Action     string          `json:"action,omitempty"`
	Resource   string          `json:"resource,omitempty"`
	Summary    string          `json:"summary,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	ThreadID   string          `json:"thread_id,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Workspace  string          `json:"workspace,omitempty"`
}

// CompressionEvent carries metadata about a compression operation for SSE events.
type CompressionEvent struct {
	Outcome                 string `json:"outcome,omitempty"`
	SummaryAttempted        bool   `json:"summary_attempted,omitempty"`
	ModelAlias              string `json:"model_alias,omitempty"`
	ModelID                 string `json:"model_id,omitempty"`
	BeforeTokens            int    `json:"before_tokens,omitempty"`
	AfterTokens             int    `json:"after_tokens,omitempty"`
	BudgetTokens            int    `json:"budget_tokens,omitempty"`
	ThresholdTokens         int    `json:"threshold_tokens,omitempty"`
	TargetTokens            int    `json:"target_tokens,omitempty"`
	RecentTailTargetTokens  int    `json:"recent_tail_target_tokens,omitempty"`
	RecentTailTokens        int    `json:"recent_tail_tokens,omitempty"`
	MessageCount            int    `json:"message_count,omitempty"`
	CompressedMessageCount  int    `json:"compressed_message_count,omitempty"`
	PrunedToolOutputs       int    `json:"pruned_tool_outputs,omitempty"`
	SummaryPromptTokens     int    `json:"summary_prompt_tokens,omitempty"`
	SummaryCompletionTokens int    `json:"summary_completion_tokens,omitempty"`
	SummaryTotalTokens      int    `json:"summary_total_tokens,omitempty"`
	SummaryCallCount        int    `json:"summary_call_count,omitempty"`
	SummaryUsageCallCount   int    `json:"summary_usage_call_count,omitempty"`
	FallbackUsed            bool   `json:"fallback_used,omitempty"`
	FallbackReason          string `json:"fallback_reason,omitempty"`
	Error                   string `json:"error,omitempty"`
	// ElapsedMS is the wall-clock duration of the whole compression operation
	// (including the summarizer call), in milliseconds. It is populated on the
	// terminal compression_end / compression_error events, mirroring the
	// standalone --summarize-context contract's elapsed_ms field.
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`
}

// thinkParser tracks whether we are inside <think>...</think> blocks.
type thinkParser struct {
	inThink bool
	buf     string
}

func newThinkParser() *thinkParser {
	return &thinkParser{}
}

// feed processes incoming text and returns events.
func (p *thinkParser) feed(text string) []StreamEvent {
	p.buf += text
	var events []StreamEvent

	for {
		if p.inThink {
			idx := findTag(p.buf, "</think>")
			if idx == -1 {
				// No closing tag yet; emit safe part, keeping potential partial tag.
				safe, keep := splitPartial(p.buf, "</think>")
				if len(safe) > 0 {
					events = append(events, StreamEvent{Type: "thinking", Content: safe})
				}
				p.buf = keep
				break
			}
			if idx > 0 {
				events = append(events, StreamEvent{Type: "thinking", Content: p.buf[:idx]})
			}
			p.buf = p.buf[idx+len("</think>"):]
			p.inThink = false
		} else {
			idx := findTag(p.buf, "<think>")
			if idx == -1 {
				// No opening tag yet; emit safe part, keeping potential partial tag.
				safe, keep := splitPartial(p.buf, "<think>")
				if len(safe) > 0 {
					events = append(events, StreamEvent{Type: "token", Content: safe})
				}
				p.buf = keep
				break
			}
			if idx > 0 {
				events = append(events, StreamEvent{Type: "token", Content: p.buf[:idx]})
			}
			p.buf = p.buf[idx+len("<think>"):]
			p.inThink = true
		}
	}
	return events
}

func findTag(s, tag string) int {
	for i := 0; i <= len(s)-len(tag); i++ {
		if s[i:i+len(tag)] == tag {
			return i
		}
	}
	return -1
}

// splitPartial returns the safe-to-emit prefix and the suffix that might be a partial tag.
func splitPartial(s, tag string) (safe, keep string) {
	for i := len(tag) - 1; i > 0; i-- {
		if len(s) >= i && s[len(s)-i:] == tag[:i] {
			return s[:len(s)-i], s[len(s)-i:]
		}
	}
	return s, ""
}
