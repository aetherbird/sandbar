package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// TodoStatus is the durable lifecycle state of one plan item.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
	TodoCancelled  TodoStatus = "cancelled"
)

var (
	// ErrPlanExists is returned when CreatePlan is called for a thread that
	// already owns a plan.
	ErrPlanExists = errors.New("plan already exists")
	// ErrTodoNotFound is wrapped when a todo mutation names an unknown ID.
	ErrTodoNotFound = errors.New("todo not found")
)

// Valid reports whether status is one of the persisted todo states.
func (status TodoStatus) Valid() bool {
	switch status {
	case TodoPending, TodoInProgress, TodoCompleted, TodoCancelled:
		return true
	default:
		return false
	}
}

// ValidateTodoStatus returns a descriptive error for an unsupported state.
func ValidateTodoStatus(status TodoStatus) error {
	if status.Valid() {
		return nil
	}
	return fmt.Errorf("invalid todo status %q (use pending, in_progress, completed, or cancelled)", status)
}

// Plan is the current durable task plan for a conversation thread. Revision is
// incremented once for each successful batch mutation, making snapshots safe to
// reconcile in a CLI or web client.
type Plan struct {
	ThreadID  string     `json:"thread_id"`
	Revision  int64      `json:"revision"`
	Items     []TodoItem `json:"items"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// TodoItem is one ordered step in a thread's plan. IDs are monotonic decimal
// strings generated per plan. Position is one-based and follows slice order.
type TodoItem struct {
	ID        string     `json:"id"`
	ThreadID  string     `json:"thread_id"`
	Content   string     `json:"content"`
	Status    TodoStatus `json:"status"`
	Position  int        `json:"position"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// TodoUpdate changes one existing item. A nil field is left unchanged.
type TodoUpdate struct {
	ID      string      `json:"id"`
	Content *string     `json:"content,omitempty"`
	Status  *TodoStatus `json:"status,omitempty"`
}

// CreatePlan creates the sole plan belonging to threadID. Empty plans are
// valid. Items with an empty ID receive deterministic IDs starting at "1";
// supplied IDs are useful when importing a snapshot.
func (s *Store) CreatePlan(threadID string, items []TodoItem) (plan *Plan, err error) {
	threadID, err = validatePlanThreadID(threadID)
	if err != nil {
		return nil, err
	}
	items = append([]TodoItem(nil), items...)

	now := time.Now().UTC()
	prepared, nextID, err := prepareReplacement(threadID, items, 1, nil, now)
	if err != nil {
		return nil, err
	}

	err = s.withPlanWrite(func(ctx context.Context, conn *sql.Conn) error {
		if err := requirePlanThread(ctx, conn, threadID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO plans (thread_id, revision, next_todo_id, created_at, updated_at)
			 VALUES (?, 1, ?, ?, ?)`,
			threadID, nextID, now.Unix(), now.Unix(),
		); err != nil {
			if isUniqueConstraintErr(err) {
				return fmt.Errorf("%w for thread %q", ErrPlanExists, threadID)
			}
			return fmt.Errorf("create plan: %w", err)
		}
		if err := insertTodos(ctx, conn, prepared); err != nil {
			return err
		}
		plan, err = loadPlan(ctx, conn, threadID)
		return err
	})
	return plan, err
}

// GetPlan returns the current plan and its ordered items. A thread without a
// plan returns (nil, nil).
func (s *Store) GetPlan(threadID string) (*Plan, error) {
	threadID, err := validatePlanThreadID(threadID)
	if err != nil {
		return nil, err
	}
	return loadPlan(context.Background(), s.db, threadID)
}

// DeletePlan deletes a thread's plan and all its todos. It is idempotent.
func (s *Store) DeletePlan(threadID string) error {
	threadID, err := validatePlanThreadID(threadID)
	if err != nil {
		return err
	}
	return s.withPlanWrite(func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM plans WHERE thread_id = ?`, threadID); err != nil {
			return fmt.Errorf("delete plan: %w", err)
		}
		return nil
	})
}

// ListTodos returns a thread's todo items in stable plan order. A thread with
// no plan has an empty list.
func (s *Store) ListTodos(threadID string) ([]TodoItem, error) {
	plan, err := s.GetPlan(threadID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return []TodoItem{}, nil
	}
	return plan.Items, nil
}

// CreateTodos appends items to a thread's plan, creating the plan on demand.
// Input IDs and positions must be empty: IDs are allocated atomically and
// positions are derived from the existing list. Empty status defaults to
// pending. The returned slice is the complete updated list.
func (s *Store) CreateTodos(threadID string, items []TodoItem) (updated []TodoItem, err error) {
	threadID, err = validatePlanThreadID(threadID)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("create todos requires at least one item")
	}
	items = append([]TodoItem(nil), items...)
	for i := range items {
		if strings.TrimSpace(items[i].ID) != "" {
			return nil, fmt.Errorf("create todo item %d must not supply an ID", i+1)
		}
		if items[i].Position != 0 {
			return nil, fmt.Errorf("create todo item %d must not supply a position", i+1)
		}
		if err := validateTodoDraft(threadID, i, &items[i]); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	err = s.withPlanWrite(func(ctx context.Context, conn *sql.Conn) error {
		if err := requirePlanThread(ctx, conn, threadID); err != nil {
			return err
		}
		plan, err := loadPlan(ctx, conn, threadID)
		if err != nil {
			return err
		}

		createdPlan := plan == nil
		if createdPlan {
			plan = &Plan{ThreadID: threadID, Revision: 1, Items: []TodoItem{}, CreatedAt: now, UpdatedAt: now}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO plans (thread_id, revision, next_todo_id, created_at, updated_at)
				 VALUES (?, 1, 1, ?, ?)`,
				threadID, now.Unix(), now.Unix(),
			); err != nil {
				return fmt.Errorf("create plan for todos: %w", err)
			}
		}

		var nextID int64
		if err := conn.QueryRowContext(ctx,
			`SELECT next_todo_id FROM plans WHERE thread_id = ?`, threadID,
		).Scan(&nextID); err != nil {
			return fmt.Errorf("read next todo ID: %w", err)
		}
		used := make(map[string]struct{}, len(plan.Items)+len(items))
		for _, item := range plan.Items {
			used[item.ID] = struct{}{}
		}

		created := make([]TodoItem, len(items))
		for i := range items {
			id, next, err := allocateTodoID(nextID, used)
			if err != nil {
				return err
			}
			nextID = next
			used[id] = struct{}{}
			status := items[i].Status
			if status == "" {
				status = TodoPending
			}
			created[i] = TodoItem{
				ID:        id,
				ThreadID:  threadID,
				Content:   items[i].Content,
				Status:    status,
				Position:  len(plan.Items) + i + 1,
				CreatedAt: now,
				UpdatedAt: now,
			}
		}
		if err := insertTodos(ctx, conn, created); err != nil {
			return err
		}

		if createdPlan {
			if _, err := conn.ExecContext(ctx,
				`UPDATE plans SET next_todo_id = ?, updated_at = ? WHERE thread_id = ?`,
				nextID, now.Unix(), threadID,
			); err != nil {
				return fmt.Errorf("finalize new plan: %w", err)
			}
		} else if _, err := conn.ExecContext(ctx,
			`UPDATE plans
			 SET revision = revision + 1, next_todo_id = ?, updated_at = ?
			 WHERE thread_id = ?`,
			nextID, now.Unix(), threadID,
		); err != nil {
			return fmt.Errorf("advance plan revision: %w", err)
		}

		plan, err = loadPlan(ctx, conn, threadID)
		if err == nil {
			updated = plan.Items
		}
		return err
	})
	return updated, err
}

// ReplaceTodos atomically replaces a plan's complete item list. Slice order is
// authoritative. Existing IDs may be retained; blank IDs receive fresh,
// monotonic IDs. An empty slice clears the list but keeps the plan.
func (s *Store) ReplaceTodos(threadID string, items []TodoItem) (updated []TodoItem, err error) {
	threadID, err = validatePlanThreadID(threadID)
	if err != nil {
		return nil, err
	}
	items = append([]TodoItem(nil), items...)
	now := time.Now().UTC()

	err = s.withPlanWrite(func(ctx context.Context, conn *sql.Conn) error {
		if err := requirePlanThread(ctx, conn, threadID); err != nil {
			return err
		}
		current, err := loadPlan(ctx, conn, threadID)
		if err != nil {
			return err
		}
		createdPlan := current == nil
		if createdPlan {
			current = &Plan{ThreadID: threadID, Revision: 1, Items: []TodoItem{}, CreatedAt: now, UpdatedAt: now}
		}

		nextID := int64(1)
		createdAtByID := make(map[string]time.Time, len(current.Items))
		for _, item := range current.Items {
			createdAtByID[item.ID] = item.CreatedAt
		}
		if !createdPlan {
			if err := conn.QueryRowContext(ctx,
				`SELECT next_todo_id FROM plans WHERE thread_id = ?`, threadID,
			).Scan(&nextID); err != nil {
				return fmt.Errorf("read next todo ID: %w", err)
			}
		}
		prepared, nextID, err := prepareReplacement(threadID, items, nextID, createdAtByID, now)
		if err != nil {
			return err
		}

		if createdPlan {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO plans (thread_id, revision, next_todo_id, created_at, updated_at)
				 VALUES (?, 1, ?, ?, ?)`,
				threadID, nextID, now.Unix(), now.Unix(),
			); err != nil {
				return fmt.Errorf("create replacement plan: %w", err)
			}
		} else {
			if _, err := conn.ExecContext(ctx, `DELETE FROM todos WHERE thread_id = ?`, threadID); err != nil {
				return fmt.Errorf("clear todos: %w", err)
			}
		}
		if err := insertTodos(ctx, conn, prepared); err != nil {
			return err
		}
		if !createdPlan {
			if _, err := conn.ExecContext(ctx,
				`UPDATE plans
				 SET revision = revision + 1, next_todo_id = ?, updated_at = ?
				 WHERE thread_id = ?`,
				nextID, now.Unix(), threadID,
			); err != nil {
				return fmt.Errorf("advance replaced plan: %w", err)
			}
		}

		plan, err := loadPlan(ctx, conn, threadID)
		if err == nil {
			updated = plan.Items
		}
		return err
	})
	return updated, err
}

// UpdateTodos atomically applies a batch of patches and returns the complete
// ordered list. Every ID is checked before the first write, so an unknown ID or
// invalid patch leaves the plan unchanged.
func (s *Store) UpdateTodos(threadID string, updates []TodoUpdate) (updated []TodoItem, err error) {
	threadID, err = validatePlanThreadID(threadID)
	if err != nil {
		return nil, err
	}
	updates = append([]TodoUpdate(nil), updates...)
	if err := validateTodoUpdates(updates); err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	err = s.withPlanWrite(func(ctx context.Context, conn *sql.Conn) error {
		plan, err := loadPlan(ctx, conn, threadID)
		if err != nil {
			return err
		}
		byID := make(map[string]TodoItem, len(planItems(plan)))
		for _, item := range planItems(plan) {
			byID[item.ID] = item
		}
		for i := range updates {
			updates[i].ID = strings.TrimSpace(updates[i].ID)
			if _, ok := byID[updates[i].ID]; !ok {
				return fmt.Errorf("%w: %q", ErrTodoNotFound, updates[i].ID)
			}
		}

		for _, update := range updates {
			var content any
			if update.Content != nil {
				content = *update.Content
			}
			var status any
			if update.Status != nil {
				status = string(*update.Status)
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE todos
				 SET content = COALESCE(?, content), status = COALESCE(?, status), updated_at = ?
				 WHERE thread_id = ? AND id = ?`,
				content, status, now.Unix(), threadID, update.ID,
			); err != nil {
				return fmt.Errorf("update todo %q: %w", update.ID, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE plans SET revision = revision + 1, updated_at = ? WHERE thread_id = ?`,
			now.Unix(), threadID,
		); err != nil {
			return fmt.Errorf("advance updated plan: %w", err)
		}
		plan, err = loadPlan(ctx, conn, threadID)
		if err == nil {
			updated = plan.Items
		}
		return err
	})
	return updated, err
}

// DeleteTodos atomically removes the named items, compacts positions, and
// returns the remaining ordered list. All IDs must exist.
func (s *Store) DeleteTodos(threadID string, ids []string) (updated []TodoItem, err error) {
	threadID, err = validatePlanThreadID(threadID)
	if err != nil {
		return nil, err
	}
	cleanIDs, err := validateTodoIDs(ids)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	err = s.withPlanWrite(func(ctx context.Context, conn *sql.Conn) error {
		plan, err := loadPlan(ctx, conn, threadID)
		if err != nil {
			return err
		}
		byID := make(map[string]struct{}, len(planItems(plan)))
		for _, item := range planItems(plan) {
			byID[item.ID] = struct{}{}
		}
		for _, id := range cleanIDs {
			if _, ok := byID[id]; !ok {
				return fmt.Errorf("%w: %q", ErrTodoNotFound, id)
			}
		}
		for _, id := range cleanIDs {
			if _, err := conn.ExecContext(ctx,
				`DELETE FROM todos WHERE thread_id = ? AND id = ?`, threadID, id,
			); err != nil {
				return fmt.Errorf("delete todo %q: %w", id, err)
			}
		}

		remaining, err := loadPlan(ctx, conn, threadID)
		if err != nil {
			return err
		}
		for i, item := range planItems(remaining) {
			position := i + 1
			if item.Position == position {
				continue
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE todos SET position = ?, updated_at = ? WHERE thread_id = ? AND id = ?`,
				position, now.Unix(), threadID, item.ID,
			); err != nil {
				return fmt.Errorf("compact todo %q: %w", item.ID, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE plans SET revision = revision + 1, updated_at = ? WHERE thread_id = ?`,
			now.Unix(), threadID,
		); err != nil {
			return fmt.Errorf("advance pruned plan: %w", err)
		}
		plan, err = loadPlan(ctx, conn, threadID)
		if err == nil {
			updated = plan.Items
		}
		return err
	})
	return updated, err
}

type planQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadPlan(ctx context.Context, queryer planQueryer, threadID string) (*Plan, error) {
	rows, err := queryer.QueryContext(ctx,
		`SELECT p.thread_id, p.revision, p.created_at, p.updated_at,
		        t.id, t.content, t.status, t.position, t.created_at, t.updated_at
		 FROM plans p
		 LEFT JOIN todos t ON t.thread_id = p.thread_id
		 WHERE p.thread_id = ?
		 ORDER BY t.position ASC, t.id ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("load plan: %w", err)
	}
	defer rows.Close()

	var plan *Plan
	for rows.Next() {
		var (
			gotThreadID string
			revision    int64
			planCreated int64
			planUpdated int64
			id          sql.NullString
			content     sql.NullString
			status      sql.NullString
			position    sql.NullInt64
			itemCreated sql.NullInt64
			itemUpdated sql.NullInt64
		)
		if err := rows.Scan(
			&gotThreadID, &revision, &planCreated, &planUpdated,
			&id, &content, &status, &position, &itemCreated, &itemUpdated,
		); err != nil {
			return nil, fmt.Errorf("scan plan: %w", err)
		}
		if plan == nil {
			plan = &Plan{
				ThreadID:  gotThreadID,
				Revision:  revision,
				Items:     []TodoItem{},
				CreatedAt: time.Unix(planCreated, 0).UTC(),
				UpdatedAt: time.Unix(planUpdated, 0).UTC(),
			}
		}
		if !id.Valid {
			continue
		}
		plan.Items = append(plan.Items, TodoItem{
			ID:        id.String,
			ThreadID:  gotThreadID,
			Content:   content.String,
			Status:    TodoStatus(status.String),
			Position:  int(position.Int64),
			CreatedAt: time.Unix(itemCreated.Int64, 0).UTC(),
			UpdatedAt: time.Unix(itemUpdated.Int64, 0).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate plan: %w", err)
	}
	return plan, nil
}

func (s *Store) withPlanWrite(fn func(context.Context, *sql.Conn) error) (err error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("plan write: acquire connection: %w", err)
	}
	defer conn.Close()

	// These pragmas are connection-local. Store.Open applies them eagerly, but
	// database/sql may grow the pool later; repeat them on the exact connection
	// used for this write so concurrent CLI/server sessions wait for the writer
	// and always enforce the thread/plan foreign keys.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return fmt.Errorf("plan write: set connection pragmas: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("plan write: begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("plan write: rollback: %w", rollbackErr))
		}
	}()

	if err := fn(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("plan write: commit: %w", err)
	}
	committed = true
	return nil
}

func requirePlanThread(ctx context.Context, conn *sql.Conn, threadID string) error {
	var exists int
	if err := conn.QueryRowContext(ctx,
		`SELECT 1 FROM threads WHERE id = ?`, threadID,
	).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("plan thread not found: %s", threadID)
		}
		return fmt.Errorf("check plan thread: %w", err)
	}
	return nil
}

func insertTodos(ctx context.Context, conn *sql.Conn, items []TodoItem) error {
	for i, item := range items {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO todos (thread_id, id, content, status, position, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			item.ThreadID, item.ID, item.Content, string(item.Status), item.Position,
			item.CreatedAt.Unix(), item.UpdatedAt.Unix(),
		); err != nil {
			return fmt.Errorf("insert todo item %d (%q): %w", i+1, item.ID, err)
		}
	}
	return nil
}

func prepareReplacement(
	threadID string,
	items []TodoItem,
	nextID int64,
	createdAtByID map[string]time.Time,
	now time.Time,
) ([]TodoItem, int64, error) {
	if nextID < 1 {
		nextID = 1
	}
	allocationNext := nextID
	finalNext := nextID
	prepared := make([]TodoItem, len(items))
	used := make(map[string]struct{}, len(items))

	for i := range items {
		if err := validateTodoDraft(threadID, i, &items[i]); err != nil {
			return nil, 0, err
		}
		id := strings.TrimSpace(items[i].ID)
		if id == "" {
			continue
		}
		if _, duplicate := used[id]; duplicate {
			return nil, 0, fmt.Errorf("todo item %d repeats ID %q", i+1, id)
		}
		used[id] = struct{}{}
		if numeric, err := strconv.ParseInt(id, 10, 64); err == nil && numeric > 0 {
			if numeric == math.MaxInt64 {
				return nil, 0, fmt.Errorf("todo item %d ID %q is too large", i+1, id)
			}
			if numeric >= finalNext {
				finalNext = numeric + 1
			}
		}
	}

	for i := range items {
		id := strings.TrimSpace(items[i].ID)
		if id == "" {
			var err error
			id, allocationNext, err = allocateTodoID(allocationNext, used)
			if err != nil {
				return nil, 0, err
			}
			if allocationNext > finalNext {
				finalNext = allocationNext
			}
			used[id] = struct{}{}
		}
		status := items[i].Status
		if status == "" {
			status = TodoPending
		}
		createdAt := now
		if prior, ok := createdAtByID[id]; ok {
			createdAt = prior
		}
		prepared[i] = TodoItem{
			ID:        id,
			ThreadID:  threadID,
			Content:   items[i].Content,
			Status:    status,
			Position:  i + 1,
			CreatedAt: createdAt,
			UpdatedAt: now,
		}
	}
	return prepared, finalNext, nil
}

func validateTodoDraft(threadID string, index int, item *TodoItem) error {
	if item.ThreadID != "" && strings.TrimSpace(item.ThreadID) != threadID {
		return fmt.Errorf("todo item %d belongs to thread %q, not %q", index+1, item.ThreadID, threadID)
	}
	if strings.TrimSpace(item.Content) == "" {
		return fmt.Errorf("todo item %d has empty content", index+1)
	}
	if item.Status != "" {
		status := TodoStatus(strings.TrimSpace(string(item.Status)))
		if err := ValidateTodoStatus(status); err != nil {
			return fmt.Errorf("todo item %d: %w", index+1, err)
		}
		item.Status = status
	}
	return nil
}

func validateTodoUpdates(updates []TodoUpdate) error {
	if len(updates) == 0 {
		return errors.New("update todos requires at least one item")
	}
	seen := make(map[string]struct{}, len(updates))
	for i := range updates {
		id := strings.TrimSpace(updates[i].ID)
		if id == "" {
			return fmt.Errorf("todo update item %d has empty ID", i+1)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("todo update item %d repeats ID %q", i+1, id)
		}
		seen[id] = struct{}{}
		updates[i].ID = id
		if updates[i].Content == nil && updates[i].Status == nil {
			return fmt.Errorf("todo update item %d has no changes", i+1)
		}
		if updates[i].Content != nil && strings.TrimSpace(*updates[i].Content) == "" {
			return fmt.Errorf("todo update item %d has empty content", i+1)
		}
		if updates[i].Status != nil {
			status := TodoStatus(strings.TrimSpace(string(*updates[i].Status)))
			if err := ValidateTodoStatus(status); err != nil {
				return fmt.Errorf("todo update item %d: %w", i+1, err)
			}
			updates[i].Status = &status
		}
	}
	return nil
}

func validateTodoIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("delete todos requires at least one ID")
	}
	clean := make([]string, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for i, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("todo ID %d is empty", i+1)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("todo ID %d repeats %q", i+1, id)
		}
		seen[id] = struct{}{}
		clean[i] = id
	}
	return clean, nil
}

func allocateTodoID(nextID int64, used map[string]struct{}) (string, int64, error) {
	if nextID < 1 {
		nextID = 1
	}
	for {
		id := strconv.FormatInt(nextID, 10)
		if _, exists := used[id]; !exists {
			if nextID == math.MaxInt64 {
				return "", 0, errors.New("todo ID space exhausted")
			}
			return id, nextID + 1, nil
		}
		if nextID == math.MaxInt64 {
			return "", 0, errors.New("todo ID space exhausted")
		}
		nextID++
	}
}

func validatePlanThreadID(threadID string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", errors.New("plan thread ID must not be empty")
	}
	return threadID, nil
}

func planItems(plan *Plan) []TodoItem {
	if plan == nil {
		return nil
	}
	return plan.Items
}
