package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestResumeSubagentCompletedTaskReturnsStoredResult pins the fixed behavior:
// resume_task on a COMPLETED task returns the stored result as a done event
// instead of erroring. The old error made models re-delegate identical work —
// observed live when a parent tried to fetch a finished subagent's findings.
func TestResumeSubagentCompletedTaskReturnsStoredResult(t *testing.T) {
	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()

	now := time.Now().Unix()
	_, err := a.store.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, turn, max_turns, status, result, files_read, files_written, commands_run, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"task-done-1", "review the code", "ctx", "test-model", "[]", 1, 30, "completed",
		"three defects found: A, B, C", "[]", "[]", "[]", now, now,
	)
	if err != nil {
		t.Fatalf("seed completed task: %v", err)
	}

	events, err := a.ResumeSubagent(context.Background(), "task-done-1")
	if err != nil {
		t.Fatalf("resume completed task must not error: %v", err)
	}

	var result string
	var gotDone bool
	for ev := range events {
		if ev.Type == "done" {
			gotDone = true
			result = ev.Content
		}
	}
	if !gotDone {
		t.Fatal("no done event for completed task")
	}
	if !strings.Contains(result, "three defects found") {
		t.Fatalf("stored result not returned: %q", result)
	}
	if !strings.Contains(result, "already completed") {
		t.Fatalf("result should note the task was already completed: %q", result)
	}
}

// TestResumeSubagentEmptyCompletedResult covers the empty-result edge: the
// model still gets a done event, with an explicit no-output note.
func TestResumeSubagentEmptyCompletedResult(t *testing.T) {
	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()

	now := time.Now().Unix()
	_, err := a.store.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, turn, max_turns, status, result, files_read, files_written, commands_run, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"task-done-2", "goal", "ctx", "test-model", "[]", 1, 30, "completed",
		"", "[]", "[]", "[]", now, now,
	)
	if err != nil {
		t.Fatalf("seed completed task: %v", err)
	}

	events, err := a.ResumeSubagent(context.Background(), "task-done-2")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	var result string
	for ev := range events {
		if ev.Type == "done" {
			result = ev.Content
		}
	}
	if !strings.Contains(result, "no output") {
		t.Fatalf("empty completed result should carry the no-output note: %q", result)
	}
}
