package agent

import (
	"testing"

	"github.com/sashabaranov/go-openai"
)

func functionCall(id, name string) openai.ToolCall {
	return openai.ToolCall{
		ID:   id,
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      name,
			Arguments: `{}`,
		},
	}
}

func TestToolBatchCanRunConcurrently(t *testing.T) {
	parallelSafe := func(name string) bool {
		switch name {
		case "file_read", "search_files", "web_fetch", "delegate_task":
			return true
		default:
			return false
		}
	}
	tests := []struct {
		name  string
		calls []openai.ToolCall
		want  bool
	}{
		{
			name:  "independent reads and searches",
			calls: []openai.ToolCall{functionCall("1", "file_read"), functionCall("2", "search_files"), functionCall("3", "web_fetch")},
			want:  true,
		},
		{
			name:  "parallel delegations",
			calls: []openai.ToolCall{functionCall("1", "delegate_task"), functionCall("2", "delegate_task")},
			want:  true,
		},
		{
			name:  "single call has no batch benefit",
			calls: []openai.ToolCall{functionCall("1", "file_read")},
			want:  false,
		},
		{
			name:  "mixed mutation stays sequential",
			calls: []openai.ToolCall{functionCall("1", "file_read"), functionCall("2", "file_write")},
			want:  false,
		},
		{
			name:  "shell stays sequential",
			calls: []openai.ToolCall{functionCall("1", "shell_exec"), functionCall("2", "shell_exec")},
			want:  false,
		},
		{
			name: "unsupported call type stays sequential",
			calls: []openai.ToolCall{
				functionCall("1", "file_read"),
				{ID: "2", Type: "custom", Function: openai.FunctionCall{Name: "file_read", Arguments: `{}`}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolBatchCanRunConcurrently(tt.calls, parallelSafe); got != tt.want {
				t.Fatalf("toolBatchCanRunConcurrently() = %v, want %v", got, tt.want)
			}
		})
	}
}
