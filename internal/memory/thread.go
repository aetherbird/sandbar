package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Thread represents a conversation thread.
type Thread struct {
	ID        string    `json:"id"`
	Title     *string   `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Model     *string   `json:"model"`
	// Workspace is the directory the thread's tools run in. Empty means
	// unknown — threads created before migration 0005 have no record.
	Workspace string `json:"workspace"`
	// PlanMode is the thread's plan-approval lifecycle state (migration 0007):
	// one of the PlanMode* constants. Empty means plan mode is off.
	PlanMode string `json:"plan_mode"`
}

// Plan-mode lifecycle states for Thread.PlanMode.
const (
	// PlanModeOff means no plan turn is active or pending.
	PlanModeOff = ""
	// PlanModePlanning means a read-only plan turn was requested and is
	// running (or was interrupted before completing).
	PlanModePlanning = "planning"
	// PlanModePendingApproval means a plan turn completed and the presented
	// plan awaits the user's approve/reject decision.
	PlanModePendingApproval = "pending_approval"
	// PlanModeApproved means the user approved the pending plan; the next
	// turn injects the execution directive exactly once, then clears it.
	PlanModeApproved = "approved"
)

// Message represents a single message in a thread.
type Message struct {
	ID         int       `json:"id"`
	ThreadID   string    `json:"thread_id"`
	Role       string    `json:"role"`
	Content    *string   `json:"content"`
	ToolCallID *string   `json:"tool_call_id"`
	CreatedAt  time.Time `json:"created_at"`
	Seq        int       `json:"seq"`
}

// ToolCall represents an assistant tool invocation.
type ToolCall struct {
	ID        string `json:"id"`
	MessageID int    `json:"message_id"`
	ToolName  string `json:"tool_name"`
	Arguments string `json:"arguments"`
	Seq       int    `json:"seq"`
}

// AssistantToolCall is one provider tool call to persist with an assistant
// message. Slice order becomes the stored tool-call sequence.
type AssistantToolCall struct {
	ID        string
	ToolName  string
	Arguments string
}

// CreateThread creates a new thread with optional title and model.
func (s *Store) CreateThread(title, model *string) (*Thread, error) {
	return s.CreateThreadWithWorkspace(title, model, "")
}

// CreateThreadWithWorkspace creates a new thread and records the workspace
// directory its tools run in ("" for unknown/legacy).
func (s *Store) CreateThreadWithWorkspace(title, model *string, workspace string) (*Thread, error) {
	id := uuid.New().String()
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO threads (id, title, created_at, updated_at, model, workspace) VALUES (?, ?, ?, ?, ?, ?)`,
		id, title, now.Unix(), now.Unix(), model, workspace,
	)
	if err != nil {
		return nil, fmt.Errorf("create thread: %w", err)
	}
	return &Thread{
		ID:        id,
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
		Model:     model,
		Workspace: workspace,
	}, nil
}

// ListThreads returns all threads ordered by most recent update.
func (s *Store) ListThreads() ([]Thread, error) {
	return s.listThreads(``, nil)
}

// ListThreadsByWorkspace returns threads whose recorded workspace matches,
// ordered by most recent update. When includeLegacy is true, threads with an
// unknown workspace ("") are included too — they predate workspace tracking
// and can't be attributed, so they surface in the default view only.
func (s *Store) ListThreadsByWorkspace(workspace string, includeLegacy bool) ([]Thread, error) {
	where := `WHERE workspace = ?`
	if includeLegacy {
		where += ` OR workspace = ''`
	}
	return s.listThreads(where, []any{workspace})
}

// listThreads runs the thread listing with an optional WHERE clause.
func (s *Store) listThreads(where string, args []any) ([]Thread, error) {
	query := `SELECT id, title, created_at, updated_at, model, workspace, plan_mode FROM threads `
	if where != "" {
		query += where
	}
	query += ` ORDER BY updated_at DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		var createdAt, updatedAt int64
		if err := rows.Scan(&t.ID, &t.Title, &createdAt, &updatedAt, &t.Model, &t.Workspace, &t.PlanMode); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		t.CreatedAt = time.Unix(createdAt, 0).UTC()
		t.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// GetThread returns a thread by ID.
func (s *Store) GetThread(id string) (*Thread, error) {
	var t Thread
	var createdAt, updatedAt int64
	err := s.db.QueryRow(
		`SELECT id, title, created_at, updated_at, model, workspace, plan_mode FROM threads WHERE id = ?`,
		id,
	).Scan(&t.ID, &t.Title, &createdAt, &updatedAt, &t.Model, &t.Workspace, &t.PlanMode)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("thread not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	t.CreatedAt = time.Unix(createdAt, 0).UTC()
	t.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &t, nil
}

// GetThreadWithMessages returns a thread with its ordered messages.
func (s *Store) GetThreadWithMessages(id string) (*Thread, []Message, error) {
	thread, err := s.GetThread(id)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.Query(
		`SELECT id, thread_id, role, content, tool_call_id, created_at, seq FROM messages WHERE thread_id = ? ORDER BY seq ASC`,
		id,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Role, &m.Content, &m.ToolCallID, &createdAt, &m.Seq); err != nil {
			return nil, nil, fmt.Errorf("scan message: %w", err)
		}
		m.CreatedAt = time.Unix(createdAt, 0).UTC()
		messages = append(messages, m)
	}
	return thread, messages, rows.Err()
}

// AppendMessage adds a message to a thread and updates the thread timestamp.
//
// Uses BEGIN IMMEDIATE to acquire the write lock before reading, preventing
// the deferred-transaction lock-upgrade issue that causes SQLITE_BUSY.
// The UNIQUE(thread_id, seq) constraint (migration 0003) is the hard guarantee
// against duplicate seq values.
func (s *Store) AppendMessage(threadID, role string, content *string, toolCallID *string) (*Message, error) {
	return s.appendMessageWithToolCalls(threadID, role, content, toolCallID, nil)
}

// AppendAssistantMessageWithToolCalls atomically persists an assistant message
// and every tool call it contains. If any tool-call insert fails (including a
// globally duplicate call ID), the assistant message and all earlier calls in
// the same group are rolled back together.
func (s *Store) AppendAssistantMessageWithToolCalls(threadID string, content *string, toolCalls []AssistantToolCall) (*Message, error) {
	return s.appendMessageWithToolCalls(threadID, "assistant", content, nil, toolCalls)
}

func (s *Store) appendMessageWithToolCalls(
	threadID, role string,
	content *string,
	toolCallID *string,
	toolCalls []AssistantToolCall,
) (*Message, error) {
	now := time.Now().UTC()

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		msg, retry, err := s.appendMessageWithToolCallsOnce(threadID, role, content, toolCallID, toolCalls, now)
		if err == nil {
			return msg, nil
		}
		if retry && attempt < maxRetries-1 {
			continue
		}
		return nil, err
	}

	return nil, fmt.Errorf("append message: seq conflict persisted after %d retries", maxRetries)
}

func (s *Store) appendMessageWithToolCallsOnce(
	threadID, role string,
	content *string,
	toolCallID *string,
	toolCalls []AssistantToolCall,
	now time.Time,
) (msg *Message, retry bool, err error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	// Pin every statement to one connection. BEGIN IMMEDIATE acquires the
	// RESERVED write lock before sequence calculation, avoiding a deferred
	// transaction's read-to-write lock upgrade race.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, strings.Contains(err.Error(), "database is locked"), fmt.Errorf("begin immediate: %w", err)
	}
	inTransaction := true
	defer func() {
		if !inTransaction {
			return
		}
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
		}
	}()

	var seq int
	err = conn.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE thread_id = ?`,
		threadID,
	).Scan(&seq)
	if err != nil {
		return nil, false, fmt.Errorf("compute seq: %w", err)
	}

	res, err := conn.ExecContext(
		ctx,
		`INSERT INTO messages (thread_id, role, content, tool_call_id, created_at, seq) VALUES (?, ?, ?, ?, ?, ?)`,
		threadID, role, content, toolCallID, now.Unix(), seq,
	)
	if err != nil {
		return nil, isUniqueConstraintErr(err), fmt.Errorf("insert message: %w", err)
	}

	msgID, err := res.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("read inserted message id: %w", err)
	}
	for i, toolCall := range toolCalls {
		if _, err := conn.ExecContext(
			ctx,
			`INSERT INTO tool_calls (id, message_id, tool_name, arguments, seq) VALUES (?, ?, ?, ?, ?)`,
			toolCall.ID, msgID, toolCall.ToolName, toolCall.Arguments, i,
		); err != nil {
			return nil, false, fmt.Errorf("insert tool call %d (%q): %w", i, toolCall.ID, err)
		}
	}

	if _, err := conn.ExecContext(
		ctx,
		`UPDATE threads SET updated_at = ? WHERE id = ?`,
		now.Unix(), threadID,
	); err != nil {
		return nil, false, fmt.Errorf("update thread: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	inTransaction = false

	return &Message{
		ID:         int(msgID),
		ThreadID:   threadID,
		Role:       role,
		Content:    content,
		ToolCallID: toolCallID,
		CreatedAt:  now,
		Seq:        seq,
	}, false, nil
}

// isUniqueConstraintErr returns true if err is a SQLite UNIQUE-constraint
// violation. modernc.org/sqlite surfaces these as errors whose message contains
// "UNIQUE constraint failed".
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}

// UpdateThreadTitle sets the thread title unless it was already set (conditional for auto-title).
func (s *Store) UpdateThreadTitle(id, title string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE threads SET title = ?, updated_at = ? WHERE id = ? AND title IS NULL`,
		title, now.Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("update title: %w", err)
	}
	return nil
}

// SetThreadTitle unconditionally overwrites the thread title (for manual rename or regeneration).
func (s *Store) SetThreadTitle(id, title string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE threads SET title = ?, updated_at = ? WHERE id = ?`,
		title, now.Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("set title: %w", err)
	}
	return nil
}

// SetThreadPlanMode sets the thread's plan-approval lifecycle state (one of
// the PlanMode* constants). It deliberately does not touch updated_at: a
// lifecycle transition is not conversation activity and must not reorder the
// session list.
func (s *Store) SetThreadPlanMode(id, mode string) error {
	res, err := s.db.Exec(
		`UPDATE threads SET plan_mode = ? WHERE id = ?`,
		mode, id,
	)
	if err != nil {
		return fmt.Errorf("set plan mode: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("thread not found: %s", id)
	}
	return nil
}

// ForkThread creates a new thread that copies the source thread's title, model,
// messages, assistant tool calls, and current persisted plan. Tool-call IDs are globally unique, so the
// fork receives fresh IDs and all copied tool-result messages are rewritten to
// reference them. The entire copy is one transaction: a failed fork never leaves
// behind a partial thread.
func (s *Store) ForkThread(srcID string) (newID string, err error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("fork: acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return "", fmt.Errorf("fork: begin immediate: %w", err)
	}
	inTransaction := true
	defer func() {
		if !inTransaction {
			return
		}
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("fork: rollback: %w", rollbackErr))
		}
	}()

	var title, model *string
	var workspace string
	if err := conn.QueryRowContext(
		ctx,
		`SELECT title, model, workspace FROM threads WHERE id = ?`,
		srcID,
	).Scan(&title, &model, &workspace); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("fork: source thread not found: %s", srcID)
		}
		return "", fmt.Errorf("fork: load source thread: %w", err)
	}

	type forkMessage struct {
		id         int
		role       string
		content    *string
		toolCallID *string
		createdAt  int64
		seq        int
	}
	var messages []forkMessage
	rows, err := conn.QueryContext(
		ctx,
		`SELECT id, role, content, tool_call_id, created_at, seq
		 FROM messages WHERE thread_id = ? ORDER BY seq ASC`,
		srcID,
	)
	if err != nil {
		return "", fmt.Errorf("fork: load source messages: %w", err)
	}
	for rows.Next() {
		var msg forkMessage
		if err := rows.Scan(&msg.id, &msg.role, &msg.content, &msg.toolCallID, &msg.createdAt, &msg.seq); err != nil {
			rows.Close()
			return "", fmt.Errorf("fork: scan source message: %w", err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("fork: iterate source messages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("fork: close source messages: %w", err)
	}

	toolCallsByMessage := make(map[int][]ToolCall)
	toolCallIDs := make(map[string]string)
	rows, err = conn.QueryContext(
		ctx,
		`SELECT tc.id, tc.message_id, tc.tool_name, tc.arguments, tc.seq
		 FROM tool_calls tc
		 JOIN messages m ON m.id = tc.message_id
		 WHERE m.thread_id = ?
		 ORDER BY m.seq ASC, tc.seq ASC`,
		srcID,
	)
	if err != nil {
		return "", fmt.Errorf("fork: load source tool calls: %w", err)
	}
	for rows.Next() {
		var toolCall ToolCall
		if err := rows.Scan(&toolCall.ID, &toolCall.MessageID, &toolCall.ToolName, &toolCall.Arguments, &toolCall.Seq); err != nil {
			rows.Close()
			return "", fmt.Errorf("fork: scan source tool call: %w", err)
		}
		toolCallsByMessage[toolCall.MessageID] = append(toolCallsByMessage[toolCall.MessageID], toolCall)
		toolCallIDs[toolCall.ID] = uuid.New().String()
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("fork: iterate source tool calls: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("fork: close source tool calls: %w", err)
	}

	newID = uuid.New().String()
	now := time.Now().UTC().Unix()
	if _, err := conn.ExecContext(
		ctx,
		`INSERT INTO threads (id, title, created_at, updated_at, model, workspace) VALUES (?, ?, ?, ?, ?, ?)`,
		newID, title, now, now, model, workspace,
	); err != nil {
		return "", fmt.Errorf("fork: create thread: %w", err)
	}

	for _, msg := range messages {
		toolCallID := msg.toolCallID
		if toolCallID != nil {
			remappedID, ok := toolCallIDs[*toolCallID]
			if !ok {
				return "", fmt.Errorf("fork: message seq %d references missing tool call %q", msg.seq, *toolCallID)
			}
			toolCallID = &remappedID
		}

		res, err := conn.ExecContext(
			ctx,
			`INSERT INTO messages (thread_id, role, content, tool_call_id, created_at, seq)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			newID, msg.role, msg.content, toolCallID, msg.createdAt, msg.seq,
		)
		if err != nil {
			return "", fmt.Errorf("fork: copy message seq %d: %w", msg.seq, err)
		}
		messageID, err := res.LastInsertId()
		if err != nil {
			return "", fmt.Errorf("fork: read copied message id at seq %d: %w", msg.seq, err)
		}

		for _, toolCall := range toolCallsByMessage[msg.id] {
			remappedID := toolCallIDs[toolCall.ID]
			if _, err := conn.ExecContext(
				ctx,
				`INSERT INTO tool_calls (id, message_id, tool_name, arguments, seq)
				 VALUES (?, ?, ?, ?, ?)`,
				remappedID, messageID, toolCall.ToolName, toolCall.Arguments, toolCall.Seq,
			); err != nil {
				return "", fmt.Errorf("fork: copy tool call %q: %w", toolCall.ID, err)
			}
		}
	}

	// A plan is conversation state, not merely historical tool output. Copy its
	// current snapshot so the fork's next todo list agrees with the todo results
	// already present in the copied transcript. Subsequent mutations remain
	// independent because every row is keyed to the new thread.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO plans (thread_id, revision, next_todo_id, created_at, updated_at)
		 SELECT ?, revision, next_todo_id, created_at, updated_at
		 FROM plans WHERE thread_id = ?`,
		newID, srcID,
	); err != nil {
		return "", fmt.Errorf("fork: copy plan: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO todos (thread_id, id, content, status, position, created_at, updated_at)
		 SELECT ?, id, content, status, position, created_at, updated_at
		 FROM todos WHERE thread_id = ? ORDER BY position`,
		newID, srcID,
	); err != nil {
		return "", fmt.Errorf("fork: copy todo items: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", fmt.Errorf("fork: commit: %w", err)
	}
	inTransaction = false
	return newID, nil
}

// DeleteThread removes a thread and cascades messages + tool_calls.
func (s *Store) DeleteThread(id string) error {
	_, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete thread: %w", err)
	}
	return nil
}

// AppendToolCall records a tool call for an assistant message.
func (s *Store) AppendToolCall(messageID int, callID, toolName, arguments string, seq int) error {
	_, err := s.db.Exec(
		`INSERT INTO tool_calls (id, message_id, tool_name, arguments, seq) VALUES (?, ?, ?, ?, ?)`,
		callID, messageID, toolName, arguments, seq,
	)
	if err != nil {
		return fmt.Errorf("append tool call: %w", err)
	}
	return nil
}

// GetToolCallsForMessage returns all tool calls for a given message, ordered by seq.
func (s *Store) GetToolCallsForMessage(messageID int) ([]ToolCall, error) {
	rows, err := s.db.Query(
		`SELECT id, message_id, tool_name, arguments, seq FROM tool_calls WHERE message_id = ? ORDER BY seq ASC`,
		messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("get tool calls: %w", err)
	}
	defer rows.Close()

	var toolCalls []ToolCall
	for rows.Next() {
		var tc ToolCall
		if err := rows.Scan(&tc.ID, &tc.MessageID, &tc.ToolName, &tc.Arguments, &tc.Seq); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		toolCalls = append(toolCalls, tc)
	}
	return toolCalls, rows.Err()
}

// DeleteMessagesAfter removes all messages in a thread from seq onward (inclusive).
// It also invalidates any compression records whose first_kept_seq >= seq,
// because undo can remove the messages that the compression boundary references.
func (s *Store) DeleteMessagesAfter(threadID string, seq int) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE thread_id = ? AND seq >= ?`, threadID, seq)
	if err != nil {
		return fmt.Errorf("delete messages after: %w", err)
	}
	// Invalidate compression records that now point to deleted messages.
	if invErr := s.InvalidateCompressionsAfterSeq(threadID, seq); invErr != nil {
		return fmt.Errorf("invalidate compressions after delete: %w", invErr)
	}
	return nil
}
