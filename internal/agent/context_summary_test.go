package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/llm"
)

type contextSummaryFake struct {
	responses []*CompressionSummaryResult
	errors    []error
	requests  []CompressionSummaryRequest
}

func (f *contextSummaryFake) Summarize(_ context.Context, req CompressionSummaryRequest) (*CompressionSummaryResult, error) {
	f.requests = append(f.requests, req)
	i := len(f.requests) - 1
	var response *CompressionSummaryResult
	if i < len(f.responses) {
		response = f.responses[i]
	}
	var err error
	if i < len(f.errors) {
		err = f.errors[i]
	}
	return response, err
}

func TestSummarizeContextWithPreparesRetriesAndAggregates(t *testing.T) {
	longToolOutput := "API_KEY=not-a-real-key\nHEAD\n" + strings.Repeat("middle evidence ", 300) + "\nTAIL"
	fake := &contextSummaryFake{responses: []*CompressionSummaryResult{
		{Content: "tiny", PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		{Content: strings.Repeat("expanded checkpoint evidence ", 100) + "API_KEY=still-not-real", PromptTokens: 20, CompletionTokens: 100, TotalTokens: 120},
	}}
	resolved := llm.ResolvedModel{ModelID: "candidate-model-id"}
	result, err := summarizeContextWith(context.Background(), "provider/candidate", resolved, ContextSummaryRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "continue the task"},
			{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: longToolOutput},
		},
		MaxOutputTokens:     200,
		MinimumUsefulTokens: 30,
		RetryShort:          true,
	}, fake)
	if err != nil {
		t.Fatalf("summarizeContextWith: %v", err)
	}
	if len(fake.requests) != 2 || fake.requests[0].Retry || !fake.requests[1].Retry {
		t.Fatalf("retry requests = %+v", fake.requests)
	}
	for _, request := range fake.requests {
		if request.ModelAlias != "provider/candidate" || request.ModelID != "candidate-model-id" {
			t.Fatalf("candidate routing changed: %+v", request)
		}
		if request.MaxOutputTokens != 200 || request.MinimumUsefulTokens != 30 {
			t.Fatalf("summary budgets changed: %+v", request)
		}
		prepared := request.Messages[1].Content
		if strings.Contains(prepared, "not-a-real-key") || !strings.Contains(prepared, "API_KEY=[REDACTED]") {
			t.Fatalf("prepared input was not redacted: %q", prepared)
		}
		if !strings.Contains(prepared, "HEAD") || !strings.Contains(prepared, "TAIL") || !strings.Contains(prepared, "tool output trimmed") {
			t.Fatalf("prepared tool output lost its bounded head/tail form: %q", prepared)
		}
	}
	if !result.Retried || result.CallCount != 2 || result.UsageCallCount != 2 {
		t.Fatalf("retry telemetry = %+v", result)
	}
	if result.PromptTokens != 30 || result.CompletionTokens != 101 || result.TotalTokens != 131 {
		t.Fatalf("usage aggregate = %+v", result)
	}
	if result.PrunedToolOutputs != 1 || result.LocalSummaryTokens < 30 {
		t.Fatalf("summary result = %+v", result)
	}
	if strings.Contains(result.Summary, "still-not-real") || !strings.Contains(result.Summary, "API_KEY=[REDACTED]") {
		t.Fatalf("returned summary was not redacted: %q", result.Summary)
	}
}

func TestSummarizeContextWithStillShortReturnsAuditableFailure(t *testing.T) {
	fake := &contextSummaryFake{responses: []*CompressionSummaryResult{
		{Content: "tiny", PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		{Content: "still tiny", PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
	}}
	result, err := summarizeContextWith(context.Background(), "provider/candidate", llm.ResolvedModel{ModelID: "candidate"}, ContextSummaryRequest{
		Messages:            []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("context ", 100)}},
		MaxOutputTokens:     200,
		MinimumUsefulTokens: 30,
		RetryShort:          true,
	}, fake)
	if err == nil || !strings.Contains(err.Error(), "remained too short") {
		t.Fatalf("error = %v, want too-short failure", err)
	}
	if result == nil || !result.Retried || result.CallCount != 2 || result.UsageCallCount != 2 || result.TotalTokens != 23 {
		t.Fatalf("partial failure telemetry = %+v", result)
	}
	if result.Summary != "" {
		t.Fatalf("failed checkpoint leaked as success: %q", result.Summary)
	}
}

func TestSummarizeContextUsesProductionPromptAndExplicitModel(t *testing.T) {
	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"summary-1","object":"chat.completion","created":1,"model":"candidate-model-id","choices":[{"index":0,"message":{"role":"assistant","content":"TASK AND CONSTRAINTS\nKeep working."},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":8,"total_tokens":50}}`)
	}))
	defer server.Close()

	result, err := SummarizeContext(context.Background(), "provider/candidate", llm.ResolvedModel{
		BaseURL: server.URL + "/v1",
		APIKey:  "test",
		ModelID: "candidate-model-id",
	}, ContextSummaryRequest{
		Messages:        []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "inspect src/app.go"}},
		MaxOutputTokens: 321,
	})
	if err != nil {
		t.Fatalf("SummarizeContext: %v", err)
	}
	if result.ModelAlias != "provider/candidate" || result.ModelID != "candidate-model-id" || result.TotalTokens != 50 {
		t.Fatalf("result = %+v", result)
	}
	if captured["model"] != "candidate-model-id" || captured["max_tokens"] != float64(321) {
		t.Fatalf("provider routing/budget = %#v", captured)
	}
	messages, ok := captured["messages"].([]interface{})
	if !ok || len(messages) != 2 {
		t.Fatalf("provider messages = %#v", captured["messages"])
	}
	system := messages[0].(map[string]interface{})["content"].(string)
	for _, section := range []string{"TASK AND CONSTRAINTS", "FILES AND SYMBOLS INSPECTED", "NEXT STEPS"} {
		if !strings.Contains(system, section) {
			t.Fatalf("production checkpoint prompt missing %q: %q", section, system)
		}
	}
	user := messages[1].(map[string]interface{})["content"].(string)
	if !strings.Contains(user, "user: inspect src/app.go") {
		t.Fatalf("message formatting changed: %q", user)
	}
}
