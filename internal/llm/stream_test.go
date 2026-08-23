package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamEventCanonicalJSON locks in the canonical JSON schema shared by
// Sandbar's --json mode — the shape scripting consumers parse. Field names
// and omitempty behavior are part of the contract.
func TestStreamEventCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		ev   StreamEvent
		want []string
		deny []string
	}{
		{
			name: "token",
			ev:   StreamEvent{Type: "token", Content: "hi"},
			want: []string{`"type":"token"`, `"content":"hi"`},
			deny: []string{"prompt_tokens", "tool_name", "thread_id"},
		},
		{
			name: "tool_call",
			ev:   StreamEvent{Type: "tool_call", ToolName: "file_read", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
			want: []string{`"type":"tool_call"`, `"tool_name":"file_read"`, `"arguments":{"path":"a.txt"}`},
			deny: []string{`"tool":`, `"content"`},
		},
		{
			name: "tool_result",
			ev:   StreamEvent{Type: "tool_result", ToolName: "file_read", Content: "file body"},
			want: []string{`"type":"tool_result"`, `"tool_name":"file_read"`, `"content":"file body"`},
			deny: []string{`"output":`},
		},
		{
			name: "usage",
			ev:   StreamEvent{Type: "usage", PromptTokens: 100, CompletionTokens: 25, TotalTokens: 125},
			want: []string{`"type":"usage"`, `"prompt_tokens":100`, `"completion_tokens":25`, `"total_tokens":125`},
		},
		{
			name: "done",
			ev:   StreamEvent{Type: "done", ThreadID: "thr-1", Content: "thr-1"},
			want: []string{`"type":"done"`, `"thread_id":"thr-1"`},
		},
		{
			name: "auxiliary_usage",
			ev:   StreamEvent{Type: "auxiliary_usage", UsagePurpose: "compression", PromptTokens: 9},
			want: []string{`"type":"auxiliary_usage"`, `"usage_purpose":"compression"`, `"prompt_tokens":9`},
			deny: []string{`"purpose":`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s := string(b)
			for _, w := range c.want {
				if !strings.Contains(s, w) {
					t.Errorf("want %q in %s", w, s)
				}
			}
			for _, d := range c.deny {
				if strings.Contains(s, d) {
					t.Errorf("did not want %q in %s", d, s)
				}
			}
		})
	}
}
