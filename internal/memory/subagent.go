package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// SubagentTranscript renders the persisted transcript of one subagent task
// (migration 0004's subagent_tasks row) for agent:// reads: goal header,
// role sections, tool calls and results, in stored order. It resolves an
// unknown id as a not-found error so file_read can surface it.
func (s *Store) SubagentTranscript(taskID string) (string, error) {
	var goal, contextStr, modelAlias, status, result, messagesJSON string
	err := s.db.QueryRow(
		`SELECT goal, context, model_alias, status, result, messages_json
		 FROM subagent_tasks WHERE id = ?`, taskID,
	).Scan(&goal, &contextStr, &modelAlias, &status, &result, &messagesJSON)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("subagent task not found: %s", taskID)
	}
	if err != nil {
		return "", fmt.Errorf("load subagent task %s: %w", taskID, err)
	}

	// messages_json is the agent's serialized []openai.ChatCompletionMessage;
	// only the transcript-relevant fields are decoded here so memory does not
	// depend on the provider wire types.
	var messages []subagentMessage
	if err := json.Unmarshal([]byte(messagesJSON), &messages); err != nil {
		return "", fmt.Errorf("deserialize subagent messages: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "subagent task %s (status: %s", taskID, status)
	if modelAlias != "" {
		fmt.Fprintf(&b, ", model: %s", modelAlias)
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "goal: %s\n", strings.TrimSpace(goal))
	if strings.TrimSpace(contextStr) != "" {
		fmt.Fprintf(&b, "context: %s\n", strings.TrimSpace(contextStr))
	}
	b.WriteString("\n")
	for _, m := range messages {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "## user\n%s\n\n", strings.TrimSpace(m.Content))
		case "assistant":
			b.WriteString("## assistant\n")
			if text := strings.TrimSpace(m.Content); text != "" {
				fmt.Fprintf(&b, "%s\n", text)
			}
			for _, call := range m.ToolCalls {
				fmt.Fprintf(&b, "▸ tool call %s %s\n", call.Function.Name, call.Function.Arguments)
			}
			b.WriteString("\n")
		case "tool":
			mark := "✓"
			if strings.Contains(strings.ToLower(m.Content), "error") {
				mark = "✗"
			}
			fmt.Fprintf(&b, "%s tool result %s: %s\n\n", mark, m.Name, strings.TrimSpace(m.Content))
		}
	}
	if strings.TrimSpace(result) != "" {
		fmt.Fprintf(&b, "## result\n%s\n", strings.TrimSpace(result))
	}
	return strings.TrimSpace(b.String()), nil
}

// subagentMessage mirrors the transcript-relevant subset of the serialized
// openai.ChatCompletionMessage rows stored in subagent_tasks.messages_json.
type subagentMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	Name       string             `json:"name,omitempty"`
	ToolCalls  []subagentToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

type subagentToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}
