package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aetherbird/sandbar/internal/memory"
)

// TestTodoPerThreadIsolation verifies that todo lists are scoped per thread, so
// concurrent sessions on a shared process don't see each other's tasks.
func TestTodoPerThreadIsolation(t *testing.T) {
	ctxA := WithThreadID(context.Background(), "thread-A")
	ctxB := WithThreadID(context.Background(), "thread-B")

	out, err := TodoList(ctxA, map[string]interface{}{
		"action": "create",
		"items":  []interface{}{map[string]interface{}{"content": "task for A"}},
	})
	if err != nil {
		t.Fatalf("create in A: %v", err)
	}
	if !strings.Contains(out, "task for A") {
		t.Fatalf("create A did not record the task: %q", out)
	}

	// Thread B must not see thread A's task.
	outB, _ := TodoList(ctxB, map[string]interface{}{"action": "list"})
	if strings.Contains(outB, "task for A") {
		t.Errorf("thread B leaked thread A's todo: %q", outB)
	}
	if !strings.Contains(outB, "no items") {
		t.Errorf("thread B should be empty, got %q", outB)
	}

	// Thread A still has its task.
	outA, _ := TodoList(ctxA, map[string]interface{}{"action": "list"})
	if !strings.Contains(outA, "task for A") {
		t.Errorf("thread A lost its todo: %q", outA)
	}
}

func TestTodoMutationActionsRequireNonEmptyItems(t *testing.T) {
	ctx := WithThreadID(context.Background(), t.Name())
	for _, action := range []string{"create", "update", "complete", "cancel"} {
		t.Run(action+" missing", func(t *testing.T) {
			_, err := TodoList(ctx, map[string]interface{}{"action": action})
			if err == nil || !strings.Contains(err.Error(), "requires \"items\"") {
				t.Fatalf("error = %v, want actionable missing-items error", err)
			}
		})
		t.Run(action+" empty", func(t *testing.T) {
			_, err := TodoList(ctx, map[string]interface{}{
				"action": action,
				"items":  []interface{}{},
			})
			if err == nil || !strings.Contains(err.Error(), "at least one item") {
				t.Fatalf("error = %v, want actionable empty-items error", err)
			}
		})
	}
}

func TestTodoRejectsMalformedItems(t *testing.T) {
	ctx := WithThreadID(context.Background(), t.Name())
	tests := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{
			name: "missing action",
			args: map[string]interface{}{},
			want: "requires \"action\"",
		},
		{
			name: "items is not array",
			args: map[string]interface{}{"action": "create", "items": map[string]interface{}{"content": "task"}},
			want: "must be a non-empty array",
		},
		{
			name: "item is not object",
			args: map[string]interface{}{"action": "create", "items": []interface{}{"task"}},
			want: "must be an object",
		},
		{
			name: "unknown item field",
			args: map[string]interface{}{"action": "create", "items": []interface{}{map[string]interface{}{"text": "task"}}},
			want: "unknown field",
		},
		{
			name: "create missing content",
			args: map[string]interface{}{"action": "create", "items": []interface{}{map[string]interface{}{}}},
			want: "requires a non-empty \"content\"",
		},
		{
			name: "create blank content",
			args: map[string]interface{}{"action": "create", "items": []interface{}{map[string]interface{}{"content": "  "}}},
			want: "empty \"content\"",
		},
		{
			name: "update missing id",
			args: map[string]interface{}{"action": "update", "items": []interface{}{map[string]interface{}{"content": "changed"}}},
			want: "requires an existing \"id\"",
		},
		{
			name: "update missing changes",
			args: map[string]interface{}{"action": "update", "items": []interface{}{map[string]interface{}{"id": "1"}}},
			want: "requires \"content\" and/or \"status\"",
		},
		{
			name: "update invalid status",
			args: map[string]interface{}{"action": "update", "items": []interface{}{map[string]interface{}{"id": "1", "status": "done"}}},
			want: "invalid status",
		},
		{
			name: "complete missing id",
			args: map[string]interface{}{"action": "complete", "items": []interface{}{map[string]interface{}{}}},
			want: "requires an existing \"id\"",
		},
		{
			name: "cancel missing id",
			args: map[string]interface{}{"action": "cancel", "items": []interface{}{map[string]interface{}{}}},
			want: "requires an existing \"id\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TodoList(ctx, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestTodoLifecycleAndUnknownIDs(t *testing.T) {
	ctx := WithThreadID(context.Background(), t.Name())
	out, err := TodoList(ctx, map[string]interface{}{
		"action": "create",
		"items": []interface{}{
			map[string]interface{}{"content": "first task"},
			map[string]interface{}{"content": "second task"},
		},
	})
	if err != nil || !strings.Contains(out, "[ ] 1 first task") || !strings.Contains(out, "[ ] 2 second task") {
		t.Fatalf("create = %q, %v", out, err)
	}

	out, err = TodoList(ctx, map[string]interface{}{
		"action": "update",
		"items": []interface{}{
			map[string]interface{}{"id": "1", "content": "revised task", "status": "in_progress"},
		},
	})
	if err != nil || !strings.Contains(out, "[>] 1 revised task") {
		t.Fatalf("update = %q, %v", out, err)
	}

	out, err = TodoList(ctx, map[string]interface{}{
		"action": "complete",
		"items":  []interface{}{map[string]interface{}{"id": "2"}},
	})
	if err != nil || !strings.Contains(out, "[✓] 2 second task") {
		t.Fatalf("complete = %q, %v", out, err)
	}

	for _, action := range []string{"update", "complete", "cancel"} {
		item := map[string]interface{}{"id": "999"}
		if action == "update" {
			item["status"] = "pending"
		}
		_, err := TodoList(ctx, map[string]interface{}{
			"action": action,
			"items":  []interface{}{item},
		})
		if err == nil || !strings.Contains(err.Error(), "unknown id") || !strings.Contains(err.Error(), "list") {
			t.Fatalf("%s unknown ID error = %v", action, err)
		}
	}
}

func TestTodoInvalidBatchDoesNotPartiallyMutate(t *testing.T) {
	ctx := WithThreadID(context.Background(), t.Name())
	_, err := TodoList(ctx, map[string]interface{}{
		"action": "create",
		"items":  []interface{}{map[string]interface{}{"content": "original"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = TodoList(ctx, map[string]interface{}{
		"action": "update",
		"items": []interface{}{
			map[string]interface{}{"id": "1", "content": "changed"},
			map[string]interface{}{"id": "999", "content": "missing"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown id") {
		t.Fatalf("invalid batch error = %v", err)
	}
	out, err := TodoList(ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(out, "original") || strings.Contains(out, "changed") {
		t.Fatalf("invalid batch mutated list: %q, %v", out, err)
	}
}

func TestTodoAcceptsJSONEncodedItemsFromTextToolCalls(t *testing.T) {
	ctx := WithThreadID(context.Background(), t.Name())
	out, err := TodoList(ctx, map[string]interface{}{
		"action": "create",
		"items":  `[{"content":"text transport task"}]`,
	})
	if err != nil || !strings.Contains(out, "text transport task") {
		t.Fatalf("JSON-encoded text-tool items = %q, %v", out, err)
	}
}

func TestTodoPersistsAcrossStoreRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "plans.db")
	store, err := memory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithThreadID(context.Background(), thread.ID)
	if _, err := todoList(store, ctx, map[string]interface{}{
		"action": "create",
		"items":  []interface{}{map[string]interface{}{"content": "survives restart"}},
	}); err != nil {
		t.Fatalf("create persistent todo: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := memory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	out, err := todoList(reopened, ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(out, "survives restart") {
		t.Fatalf("reopened list = %q, %v", out, err)
	}
	if _, err := todoList(reopened, ctx, map[string]interface{}{
		"action": "complete",
		"items":  []interface{}{map[string]interface{}{"id": "1"}},
	}); err != nil {
		t.Fatalf("complete persistent todo: %v", err)
	}
	plan, err := reopened.GetPlan(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.Revision != 2 || len(plan.Items) != 1 || plan.Items[0].Status != memory.TodoCompleted {
		t.Fatalf("unexpected persisted plan: %#v", plan)
	}
}
