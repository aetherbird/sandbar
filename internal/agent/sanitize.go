package agent

import (
	"github.com/sashabaranov/go-openai"
)

// interruptedToolResult seals an assistant tool-call group whose turn ended
// before every call produced a result. Providers reject payloads where an
// assistant tool_call is not immediately followed by its tool result, so a
// turn interrupted mid-group would otherwise poison the thread.
const interruptedToolResult = "error: tool call was not completed because the agent turn was interrupted"

// sanitizeProviderMessages repairs msgs for provider pairing rules:
//   - every assistant tool_call gets a following tool result before the next
//     non-tool message (unanswered calls get a synthetic interrupted result),
//   - tool messages referencing no unanswered call in the immediately
//     preceding assistant group (orphans and duplicates) are dropped,
//   - assistant messages with empty content and no tool calls are dropped.
//
// Order is otherwise preserved; the input slice is not modified.
func sanitizeProviderMessages(msgs []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for i := 0; i < len(msgs); {
		msg := msgs[i]
		i++
		switch {
		case msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0:
			out = append(out, msg)
			pending, order := unansweredCallSet(msg.ToolCalls)
			// Keep only the first result per call ID among the following group.
			for i < len(msgs) && msgs[i].Role == openai.ChatMessageRoleTool {
				if answered, ok := pending[msgs[i].ToolCallID]; ok && !answered {
					pending[msgs[i].ToolCallID] = true
					out = append(out, msgs[i])
				}
				i++
			}
			// Seal the calls that never received a result.
			for _, id := range order {
				if !pending[id] {
					out = append(out, syntheticToolResult(id))
				}
			}
		case msg.Role == openai.ChatMessageRoleAssistant && msg.Content == "":
			// Empty assistant message: drop.
		case msg.Role == openai.ChatMessageRoleTool:
			// Tool result with no preceding assistant group: orphan, drop.
		default:
			out = append(out, msg)
		}
	}
	// omitempty in the wire JSON would drop an empty content key; providers
	// reject user/system/tool messages missing it.
	for i := range out {
		if out[i].Role != openai.ChatMessageRoleAssistant && out[i].Content == "" {
			out[i].Content = "(empty)"
		}
	}
	return out
}

// sanitizeIndexedMessages applies the same repair to an indexed message view.
// Inserted results are Synthetic with Seq 0: they patch the provider-facing
// view only and are never persisted, so compression bookkeeping must not
// anchor on them.
func sanitizeIndexedMessages(msgs []indexedMessage) []indexedMessage {
	out := make([]indexedMessage, 0, len(msgs))
	for i := 0; i < len(msgs); {
		im := msgs[i]
		i++
		msg := im.Msg
		switch {
		case msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0:
			out = append(out, im)
			pending, order := unansweredCallSet(msg.ToolCalls)
			for i < len(msgs) && msgs[i].Msg.Role == openai.ChatMessageRoleTool {
				if answered, ok := pending[msgs[i].Msg.ToolCallID]; ok && !answered {
					pending[msgs[i].Msg.ToolCallID] = true
					out = append(out, msgs[i])
				}
				i++
			}
			for _, id := range order {
				if !pending[id] {
					out = append(out, indexedMessage{
						Synthetic: true,
						Kind:      "thread_message",
						Msg:       syntheticToolResult(id),
					})
				}
			}
		case msg.Role == openai.ChatMessageRoleAssistant && msg.Content == "":
			// Empty assistant message: drop from the view.
		case msg.Role == openai.ChatMessageRoleTool:
			// Tool result with no preceding assistant group: orphan, drop.
		default:
			out = append(out, im)
		}
	}
	// omitempty in the wire JSON would drop an empty content key; providers
	// reject user/system/tool messages missing it.
	for i := range out {
		if out[i].Msg.Role != openai.ChatMessageRoleAssistant && out[i].Msg.Content == "" {
			out[i].Msg.Content = "(empty)"
		}
	}
	return out
}

// unansweredCallSet indexes a tool-call group's unique non-empty IDs; pending
// tracks which are answered, order preserves call order for deterministic sealing.
func unansweredCallSet(toolCalls []openai.ToolCall) (pending map[string]bool, order []string) {
	pending = make(map[string]bool, len(toolCalls))
	for _, tc := range toolCalls {
		if tc.ID == "" {
			continue
		}
		if _, seen := pending[tc.ID]; seen {
			continue
		}
		pending[tc.ID] = false
		order = append(order, tc.ID)
	}
	return pending, order
}

func syntheticToolResult(toolCallID string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{
		Role:       openai.ChatMessageRoleTool,
		ToolCallID: toolCallID,
		Content:    interruptedToolResult,
	}
}

// trailingUnansweredToolCalls returns the tool calls of the final assistant
// tool-call group lacking a following tool result, or nil if the view does not
// end with a (possibly partially answered) assistant tool-call group. Only
// persisted messages count; synthetic view entries are ignored.
func trailingUnansweredToolCalls(msgs []indexedMessage) []openai.ToolCall {
	i := len(msgs) - 1
	answered := map[string]bool{}
	for i >= 0 && msgs[i].Msg.Role == openai.ChatMessageRoleTool {
		answered[msgs[i].Msg.ToolCallID] = true
		i--
	}
	if i < 0 {
		return nil
	}
	last := msgs[i]
	if last.Synthetic || last.Msg.Role != openai.ChatMessageRoleAssistant || len(last.Msg.ToolCalls) == 0 {
		return nil
	}
	var missing []openai.ToolCall
	for _, tc := range last.Msg.ToolCalls {
		if tc.ID != "" && !answered[tc.ID] {
			missing = append(missing, tc)
		}
	}
	return missing
}
