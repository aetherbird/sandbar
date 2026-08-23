package memory

import (
	"fmt"
	"strings"
	"time"
)

// SearchResult represents a single message match from FTS5 search.
type SearchResult struct {
	MessageID   int       `json:"message_id"`
	ThreadID    string    `json:"thread_id"`
	ThreadTitle *string   `json:"thread_title"`
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	Snippet     string    `json:"snippet"`
	CreatedAt   time.Time `json:"created_at"`
}

// SearchMessages performs a full-text search across all messages.
// Uses SQLite FTS5 with the bm25 ranking function.
//
// NOTE: Compression summaries are intentionally excluded from FTS search.
// They live in the `compressions` table, not `messages`, and are not indexed
// into `messages_fts`. This follows the robustness spec recommendation:
// do not index synthetic compression summaries into user-facing search unless
// clearly labeled as summaries.
func (s *Store) SearchMessages(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// Escape FTS5 special characters in the query to prevent syntax errors.
	safeQuery := escapeFTS5Query(query)

	sql := `
		SELECT
			m.id,
			m.thread_id,
			t.title,
			m.role,
			m.content,
			m.created_at
		FROM messages_fts
		JOIN messages m ON messages_fts.rowid = m.id
		JOIN threads t ON m.thread_id = t.id
		WHERE messages_fts MATCH ?
		ORDER BY bm25(messages_fts)
		LIMIT ?
	`

	rows, err := s.db.Query(sql, safeQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var createdAt int64
		if err := rows.Scan(&r.MessageID, &r.ThreadID, &r.ThreadTitle, &r.Role, &r.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		r.CreatedAt = time.Unix(createdAt, 0).UTC()
		r.Snippet = makeSnippet(r.Content, query, 120)
		results = append(results, r)
	}
	return results, rows.Err()
}

// escapeFTS5Query sanitizes user input for SQLite FTS5 MATCH.
// Wraps the whole query in double quotes to treat it as a single phrase
// token, escaping any internal double quotes.
func escapeFTS5Query(q string) string {
	// Replace internal double quotes with two double quotes (FTS5 escape),
	// then wrap the whole query so it is matched as a single phrase token.
	q = strings.ReplaceAll(q, `"`, `""`)
	return `"` + q + `"`
}

// makeSnippet returns a short snippet of text centered around the first
// occurrence of any word from the query.
func makeSnippet(text, query string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	// Find first occurrence of query substring (case-insensitive).
	idx := indexOfIgnoreCase(text, query)
	if idx == -1 {
		// Fallback: just truncate from start.
		return string(runes[:maxLen]) + "…"
	}
	start := idx - maxLen/4
	if start < 0 {
		start = 0
	}
	end := start + maxLen
	if end > len(runes) {
		end = len(runes)
		start = end - maxLen
		if start < 0 {
			start = 0
		}
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet = snippet + "…"
	}
	return snippet
}

func indexOfIgnoreCase(s, substr string) int {
	ls := []rune(strings.ToLower(s))
	lsub := []rune(strings.ToLower(substr))
	if len(lsub) == 0 {
		return 0
	}
	for i := 0; i <= len(ls)-len(lsub); i++ {
		matched := true
		for j := range lsub {
			if ls[i+j] != lsub[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}
