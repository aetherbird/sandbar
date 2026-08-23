package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// CompressionRecord represents one persisted compression event for a thread.
type CompressionRecord struct {
	ID                      int64
	ThreadID                string
	Summary                 string
	FirstKeptSeq            int
	CompressedMessageCount  int
	PrunedToolOutputs       int
	BeforeTokens            int
	AfterTokens             int
	BudgetTokens            int
	SummaryModelAlias       string
	SummaryModelID          string
	SummaryPromptTokens     int
	SummaryCompletionTokens int
	SummaryTotalTokens      int
	FallbackUsed            bool
	FallbackReason          string
	CreatedAt               time.Time
}

// SaveCompression persists a compression record. It sets CreatedAt to now.
// The caller should set FirstKeptSeq to the Seq of the first retained
// non-synthetic thread message after the compressed span.
func (s *Store) SaveCompression(rec CompressionRecord) error {
	now := time.Now().UTC().Unix()
	_, err := s.db.Exec(
		`INSERT INTO compressions (
			thread_id, summary, first_kept_seq,
			compressed_message_count, pruned_tool_outputs,
			before_tokens, after_tokens, budget_tokens,
			summary_model_alias, summary_model_id,
			summary_prompt_tokens, summary_completion_tokens, summary_total_tokens,
			fallback_used, fallback_reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ThreadID, rec.Summary, rec.FirstKeptSeq,
		rec.CompressedMessageCount, rec.PrunedToolOutputs,
		rec.BeforeTokens, rec.AfterTokens, rec.BudgetTokens,
		rec.SummaryModelAlias, rec.SummaryModelID,
		rec.SummaryPromptTokens, rec.SummaryCompletionTokens, rec.SummaryTotalTokens,
		boolToInt(rec.FallbackUsed), rec.FallbackReason, now,
	)
	if err != nil {
		return fmt.Errorf("save compression: %w", err)
	}
	return nil
}

// GetLatestCompression returns the most recent compression record for a thread,
// or nil if none exists.
func (s *Store) GetLatestCompression(threadID string) (*CompressionRecord, error) {
	var rec CompressionRecord
	var createdAt int64
	var fallbackUsed int

	err := s.db.QueryRow(
		`SELECT id, thread_id, summary, first_kept_seq,
			compressed_message_count, pruned_tool_outputs,
			before_tokens, after_tokens, budget_tokens,
			summary_model_alias, summary_model_id,
			summary_prompt_tokens, summary_completion_tokens, summary_total_tokens,
			fallback_used, fallback_reason, created_at
		FROM compressions
		WHERE thread_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		threadID,
	).Scan(
		&rec.ID, &rec.ThreadID, &rec.Summary, &rec.FirstKeptSeq,
		&rec.CompressedMessageCount, &rec.PrunedToolOutputs,
		&rec.BeforeTokens, &rec.AfterTokens, &rec.BudgetTokens,
		&rec.SummaryModelAlias, &rec.SummaryModelID,
		&rec.SummaryPromptTokens, &rec.SummaryCompletionTokens, &rec.SummaryTotalTokens,
		&fallbackUsed, &rec.FallbackReason, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest compression: %w", err)
	}

	rec.FallbackUsed = fallbackUsed != 0
	rec.CreatedAt = time.Unix(createdAt, 0).UTC()
	return &rec, nil
}

// GetThreadWithMessagesFromSeq returns a thread with messages starting from
// firstSeq (inclusive), ordered by seq ASC. This is used to rebuild the message
// list after a compression record with first_kept_seq.
func (s *Store) GetThreadWithMessagesFromSeq(threadID string, firstSeq int) (*Thread, []Message, error) {
	thread, err := s.GetThread(threadID)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.Query(
		`SELECT id, thread_id, role, content, tool_call_id, created_at, seq
		FROM messages
		WHERE thread_id = ? AND seq >= ?
		ORDER BY seq ASC`,
		threadID, firstSeq,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list messages from seq: %w", err)
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

// InvalidateCompressionsAfterSeq deletes compression records whose first_kept_seq
// is >= the given seq. This is called by DeleteMessagesAfter because undo can
// remove messages at or after the kept boundary, making those compression
// records point to nonexistent messages.
func (s *Store) InvalidateCompressionsAfterSeq(threadID string, seq int) error {
	_, err := s.db.Exec(
		`DELETE FROM compressions WHERE thread_id = ? AND first_kept_seq >= ?`,
		threadID, seq,
	)
	if err != nil {
		return fmt.Errorf("invalidate compressions: %w", err)
	}
	return nil
}

// boolToInt converts a bool to an integer for SQLite storage.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
