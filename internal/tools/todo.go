package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aetherbird/sandbar/internal/memory"
)

// PlanStore is the durable subset of memory.Store used by the todo tool. The
// interface keeps tool tests lightweight while the production Agent installs
// its SQLite store on the Registry.
type PlanStore interface {
	ListTodos(threadID string) ([]memory.TodoItem, error)
	CreateTodos(threadID string, items []memory.TodoItem) ([]memory.TodoItem, error)
	UpdateTodos(threadID string, updates []memory.TodoUpdate) ([]memory.TodoItem, error)
}

type todoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed, cancelled
}

type todoInput struct {
	ID      string  `json:"id"`
	Content *string `json:"content"`
	Status  *string `json:"status"`
}

type todoState struct {
	mu    sync.Mutex
	items []todoItem
}

// todoStore holds per-thread todo lists keyed by thread id, so concurrent
// sessions sharing one process (the server) don't see each other's tasks.
// Calls with no thread id in context share the "" bucket, matching the prior
// single-list behavior of the one-process CLI.
var todoStore = struct {
	mu       sync.Mutex
	byThread map[string]*todoState
}{byThread: map[string]*todoState{}}

func todosFor(ctx context.Context) *todoState {
	tid := threadIDFromContext(ctx)
	todoStore.mu.Lock()
	defer todoStore.mu.Unlock()
	s := todoStore.byThread[tid]
	if s == nil {
		s = &todoState{}
		todoStore.byThread[tid] = s
	}
	return s
}

// TodoList manages the task list for the current thread.
func TodoList(ctx context.Context, args map[string]interface{}) (string, error) {
	return todoList(nil, ctx, args)
}

// todoList uses SQLite whenever a store and thread context are available. The
// in-memory fallback preserves standalone Registry/tool tests that intentionally
// execute without a conversation thread.
func todoList(store PlanStore, ctx context.Context, args map[string]interface{}) (string, error) {
	rawAction, ok := args["action"]
	if !ok || rawAction == nil {
		return "", fmt.Errorf(`todo requires "action" (create, update, complete, cancel, or list)`)
	}
	action, ok := rawAction.(string)
	if !ok || strings.TrimSpace(action) == "" {
		return "", fmt.Errorf(`todo "action" must be one of create, update, complete, cancel, or list`)
	}
	action = strings.TrimSpace(action)

	if action == "list" {
		if rawItems, present := args["items"]; present && rawItems != nil {
			return "", fmt.Errorf(`todo list does not accept "items"; omit that argument`)
		}
		if store != nil && threadIDFromContext(ctx) != "" {
			items, err := store.ListTodos(threadIDFromContext(ctx))
			if err != nil {
				return "", fmt.Errorf("list persisted plan: %w", err)
			}
			return formatTodos(fromPersistedTodos(items)), nil
		}
		st := todosFor(ctx)
		st.mu.Lock()
		defer st.mu.Unlock()
		return formatTodos(st.items), nil
	}
	if action != "create" && action != "update" && action != "complete" && action != "cancel" {
		return "", fmt.Errorf("unknown todo action %q (use create, update, complete, cancel, or list)", action)
	}

	items, err := parseTodoItems(action, args["items"])
	if err != nil {
		return "", err
	}

	if store != nil && threadIDFromContext(ctx) != "" {
		return mutatePersistedTodos(store, threadIDFromContext(ctx), action, items)
	}

	st := todosFor(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()

	switch action {
	case "create":
		created := make([]todoItem, len(items))
		for i, item := range items {
			created[i] = todoItem{
				ID:      fmt.Sprintf("%d", len(st.items)+i+1),
				Content: *item.Content,
				Status:  "pending",
			}
		}
		st.items = append(st.items, created...)
		return formatTodos(st.items), nil

	case "update":
		indices, err := resolveTodoIndices(st.items, action, items)
		if err != nil {
			return "", err
		}
		for i, item := range items {
			if item.Content != nil {
				st.items[indices[i]].Content = *item.Content
			}
			if item.Status != nil {
				st.items[indices[i]].Status = *item.Status
			}
		}
		return formatTodos(st.items), nil

	case "complete":
		indices, err := resolveTodoIndices(st.items, action, items)
		if err != nil {
			return "", err
		}
		for _, index := range indices {
			st.items[index].Status = "completed"
		}
		return formatTodos(st.items), nil

	case "cancel":
		indices, err := resolveTodoIndices(st.items, action, items)
		if err != nil {
			return "", err
		}
		for _, index := range indices {
			st.items[index].Status = "cancelled"
		}
		return formatTodos(st.items), nil
	}

	return "", fmt.Errorf("unsupported todo action %q", action)
}

func mutatePersistedTodos(store PlanStore, threadID, action string, items []todoInput) (string, error) {
	var (
		updated []memory.TodoItem
		err     error
	)
	switch action {
	case "create":
		created := make([]memory.TodoItem, len(items))
		for i, item := range items {
			created[i] = memory.TodoItem{Content: *item.Content, Status: memory.TodoPending}
		}
		updated, err = store.CreateTodos(threadID, created)
	case "update", "complete", "cancel":
		updates := make([]memory.TodoUpdate, len(items))
		for i, item := range items {
			updates[i].ID = item.ID
			if item.Content != nil {
				content := *item.Content
				updates[i].Content = &content
			}
			var status string
			switch action {
			case "complete":
				status = string(memory.TodoCompleted)
			case "cancel":
				status = string(memory.TodoCancelled)
			default:
				if item.Status != nil {
					status = *item.Status
				}
			}
			if status != "" {
				persistedStatus := memory.TodoStatus(status)
				updates[i].Status = &persistedStatus
			}
		}
		updated, err = store.UpdateTodos(threadID, updates)
	default:
		return "", fmt.Errorf("unsupported todo action %q", action)
	}
	if err != nil {
		if errors.Is(err, memory.ErrTodoNotFound) {
			return "", fmt.Errorf("todo %s references an unknown id; use action \"list\" to inspect valid IDs: %w", action, err)
		}
		return "", fmt.Errorf("persist todo %s: %w", action, err)
	}
	return formatTodos(fromPersistedTodos(updated)), nil
}

func fromPersistedTodos(items []memory.TodoItem) []todoItem {
	out := make([]todoItem, len(items))
	for i, item := range items {
		out[i] = todoItem{ID: item.ID, Content: item.Content, Status: string(item.Status)}
	}
	return out
}

func parseTodoItems(action string, raw interface{}) ([]todoInput, error) {
	if raw == nil {
		return nil, fmt.Errorf(`todo %s requires "items": a non-empty array of todo objects`, action)
	}

	var data []byte
	var err error
	if encoded, ok := raw.(string); ok {
		// Text-embedded tool-call dialects carry every parameter as text. Accept a
		// JSON-encoded items array here while native structured calls use an array.
		data = []byte(encoded)
	} else {
		data, err = json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf(`todo %s could not encode "items": %w`, action, err)
		}
	}

	var encodedItems []json.RawMessage
	if err := json.Unmarshal(data, &encodedItems); err != nil {
		return nil, fmt.Errorf(`todo %s "items" must be a non-empty array of objects: %w`, action, err)
	}
	if len(encodedItems) == 0 {
		return nil, fmt.Errorf(`todo %s "items" must contain at least one item`, action)
	}

	items := make([]todoInput, len(encodedItems))
	seenIDs := make(map[string]struct{}, len(encodedItems))
	for i, encodedItem := range encodedItems {
		decoder := json.NewDecoder(bytes.NewReader(encodedItem))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&items[i]); err != nil {
			return nil, fmt.Errorf("todo %s item %d must be an object containing only id, content, and status: %w", action, i+1, err)
		}
		if err := validateTodoInput(action, i+1, &items[i]); err != nil {
			return nil, err
		}
		if action != "create" {
			if _, duplicate := seenIDs[items[i].ID]; duplicate {
				return nil, fmt.Errorf("todo %s item %d repeats id %q", action, i+1, items[i].ID)
			}
			seenIDs[items[i].ID] = struct{}{}
		}
	}
	return items, nil
}

func validateTodoInput(action string, position int, item *todoInput) error {
	item.ID = strings.TrimSpace(item.ID)
	if item.Content != nil {
		if strings.TrimSpace(*item.Content) == "" {
			return fmt.Errorf(`todo %s item %d has an empty "content"; provide non-blank todo text`, action, position)
		}
	}
	if item.Status != nil {
		status := strings.TrimSpace(*item.Status)
		item.Status = &status
		if !validTodoStatus(status) {
			return fmt.Errorf(`todo %s item %d has invalid status %q (use pending, in_progress, completed, or cancelled)`, action, position, status)
		}
	}

	switch action {
	case "create":
		if item.Content == nil {
			return fmt.Errorf(`todo create item %d requires a non-empty "content" string`, position)
		}
	case "update":
		if item.ID == "" {
			return fmt.Errorf(`todo update item %d requires an existing "id"`, position)
		}
		if item.Content == nil && item.Status == nil {
			return fmt.Errorf(`todo update item %d requires "content" and/or "status"`, position)
		}
	case "complete", "cancel":
		if item.ID == "" {
			return fmt.Errorf(`todo %s item %d requires an existing "id"`, action, position)
		}
	}
	return nil
}

func validTodoStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed", "cancelled":
		return true
	default:
		return false
	}
}

func resolveTodoIndices(existing []todoItem, action string, items []todoInput) ([]int, error) {
	byID := make(map[string]int, len(existing))
	for i, item := range existing {
		byID[item.ID] = i
	}
	indices := make([]int, len(items))
	for i, item := range items {
		index, ok := byID[item.ID]
		if !ok {
			return nil, fmt.Errorf(`todo %s item %d references unknown id %q; use action "list" to inspect valid IDs`, action, i+1, item.ID)
		}
		indices[i] = index
	}
	return indices, nil
}

func formatTodos(items []todoItem) string {
	if len(items) == 0 {
		return "(no items)"
	}
	return "Task list:\n" + renderTodoLines(items)
}

// RenderTodos renders a persisted todo list one item per line, using the same
// status icons as the todo tool's list output but without its header. It
// returns "" for an empty list so callers can skip empty reminders.
func RenderTodos(items []memory.TodoItem) string {
	return renderTodoLines(fromPersistedTodos(items))
}

func renderTodoLines(items []todoItem) string {
	var sb strings.Builder
	for _, item := range items {
		icon := map[string]string{
			"pending":     "[ ]",
			"in_progress": "[>]",
			"completed":   "[✓]",
			"cancelled":   "[-]",
		}[item.Status]
		if icon == "" {
			icon = "[ ]"
		}
		sb.WriteString(fmt.Sprintf("  %s %s %s\n", icon, item.ID, item.Content))
	}
	return sb.String()
}
