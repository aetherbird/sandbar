package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/memory"
)

// insertSubagentTask inserts an interrupted subagent task row directly so resume
// behavior can be tested without a full spawn run.
func insertSubagentTask(t *testing.T, store *memory.Store, id string, maxTurns, turn int) {
	t.Helper()
	msgs, _ := json.Marshal([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sub"},
		{Role: openai.ChatMessageRoleUser, Content: "goal"},
	})
	now := time.Now().Unix()
	_, err := store.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, turn, max_turns, status, result, files_read, files_written, commands_run, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "goal", "", "test-model", string(msgs), turn, maxTurns, "interrupted", "", "[]", "[]", "[]", now, now,
	)
	if err != nil {
		t.Fatalf("insert subagent task: %v", err)
	}
}

func TestResumeSubagentUnlimitedStaysUnlimited(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if callCount == 1 {
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"rc1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"hello.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprint(w, `{"id":"c2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"resumed and finished"},"finish_reason":"stop"}]}`)
		}
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"

	if err := os.WriteFile(agent.cfg.Workspace+"/hello.txt", []byte("world"), 0644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	const taskID = "unlimited-resume-1"
	insertSubagentTask(t, store, taskID, 0, 7) // max_turns=0 => unlimited, already 7 turns in

	events, err := agent.ResumeSubagent(context.Background(), taskID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	var sawDone, sawError bool
	for ev := range events {
		if ev.Type == "done" {
			sawDone = true
		}
		if ev.Type == "error" {
			sawError = true
			t.Logf("resume error: %s", ev.Content)
		}
	}
	if sawError {
		t.Fatal("unlimited resume hit a turn cap and errored")
	}
	if !sawDone {
		t.Fatal("unlimited resume did not complete")
	}
	if callCount != 2 {
		t.Fatalf("provider calls: got %d, want 2 (tool turn + terminal turn)", callCount)
	}
}

func TestResumeSubagentTurnCappedExhaustionGetsFreshBudget(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if callCount == 1 {
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"rc1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"hello.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprint(w, `{"id":"c2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done after fresh budget"},"finish_reason":"stop"}]}`)
		}
	}))
	defer ts.Close()

	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Providers[0].BaseURL = ts.URL
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"

	if err := os.WriteFile(agent.cfg.Workspace+"/hello.txt", []byte("world"), 0644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	const taskID = "capped-resume-1"
	insertSubagentTask(t, store, taskID, 5, 5) // capped at 5, already exhausted

	events, err := agent.ResumeSubagent(context.Background(), taskID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	var sawDone, sawError bool
	for ev := range events {
		if ev.Type == "done" {
			sawDone = true
		}
		if ev.Type == "error" {
			sawError = true
			t.Logf("resume error: %s", ev.Content)
		}
	}
	if sawError {
		t.Fatal("exhausted turn-capped resume errored instead of getting a fresh budget")
	}
	if !sawDone {
		t.Fatal("exhausted turn-capped resume did not complete")
	}
	if callCount != 2 {
		t.Fatalf("provider calls: got %d, want 2 (tool turn + terminal turn)", callCount)
	}
}

func TestResumeSubagentConcurrentGuard(t *testing.T) {
	agent, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	agent.cfg.Workspace = t.TempDir()
	agent.cfg.Subagent.Model = "test-model"

	const taskID = "guard-1"
	insertSubagentTask(t, store, taskID, 10, 0)

	// Simulate an in-flight resume by holding the guard, then confirm a second
	// resume of the same task is rejected rather than re-executing work.
	agent.resumeMu.Lock()
	if agent.resuming == nil {
		agent.resuming = make(map[string]bool)
	}
	agent.resuming[taskID] = true
	agent.resumeMu.Unlock()

	_, err := agent.ResumeSubagent(context.Background(), taskID)
	if err == nil || !strings.Contains(err.Error(), "already resuming") {
		t.Fatalf("expected 'already resuming' error, got %v", err)
	}
}
