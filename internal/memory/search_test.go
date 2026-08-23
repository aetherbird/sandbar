package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSearchMessages(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Create two threads with messages.
	thread1, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread1: %v", err)
	}
	thread2, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread2: %v", err)
	}

	content1 := "The quick brown fox jumps over the lazy dog"
	content2 := "Golang testing patterns are important"
	content3 := "The fox is quick and brown"

	store.AppendMessage(thread1.ID, "user", &content1, nil)
	store.AppendMessage(thread1.ID, "assistant", &content2, nil)
	store.AppendMessage(thread2.ID, "user", &content3, nil)

	// Search for "fox".
	results, err := store.SearchMessages("fox", 10)
	if err != nil {
		t.Fatalf("search fox: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for 'fox', got %d", len(results))
	}

	// Verify snippets are populated.
	for _, r := range results {
		if r.Snippet == "" {
			t.Error("expected non-empty snippet")
		}
		if r.ThreadID == "" {
			t.Error("expected thread_id")
		}
		if r.Content == "" {
			t.Error("expected content")
		}
	}

	// Search for something that does not exist.
	results, err = store.SearchMessages("xyznonexistent", 10)
	if err != nil {
		t.Fatalf("search nonexistent: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for nonexistent term, got %d", len(results))
	}
}

func TestSearchMessagesLimit(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, _ := store.CreateThread(nil, nil)
	for i := 0; i < 5; i++ {
		text := "common word"
		store.AppendMessage(thread.ID, "user", &text, nil)
	}

	results, err := store.SearchMessages("common", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results with limit=2, got %d", len(results))
	}
}

func TestMakeSnippet(t *testing.T) {
	tests := []struct {
		text    string
		query   string
		maxLen  int
		wantPre string
	}{
		{"short text", "short", 100, "short text"},
		{"The quick brown fox jumps over the lazy dog", "fox", 20, "…brown fox jumps…"},
	}

	for _, tt := range tests {
		snippet := makeSnippet(tt.text, tt.query, tt.maxLen)
		// Snippet may include up to 6 extra bytes for leading+trailing ellipsis.
		if len(snippet) > tt.maxLen+6 {
			t.Errorf("snippet too long: %q (%d chars)", snippet, len(snippet))
		}
	}
}

func TestMakeSnippetUnicodeSafe(t *testing.T) {
	text := strings.Repeat("界", 40) + " Needle " + strings.Repeat("🙂", 40)
	snippet := makeSnippet(text, "needle", 24)
	if !utf8.ValidString(snippet) {
		t.Fatalf("snippet is not valid UTF-8: %q", snippet)
	}
	if !strings.Contains(snippet, "Needle") {
		t.Fatalf("snippet does not contain the match: %q", snippet)
	}
	if got := utf8.RuneCountInString(strings.Trim(snippet, "…")); got > 24 {
		t.Fatalf("snippet has %d content runes, want at most 24: %q", got, snippet)
	}
}

func TestEscapeFTS5Query(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", `"hello world"`},
		{"internal quote", `say "hi"`, `"say ""hi"""`},
		{"lone quote", `a"b`, `"a""b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeFTS5Query(tt.in); got != tt.want {
				t.Errorf("escapeFTS5Query(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSearchMessages_QuotedQuery verifies that a search containing a double
// quote does not produce an FTS5 syntax error (regression: internal quotes were
// not escaped, so any query with a `"` crashed the search).
func TestSearchMessages_QuotedQuery(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := OpenWithMigrations(tmpDir+"/test.db", "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	content := `he said "hello" to the room`
	store.AppendMessage(thread.ID, "user", &content, nil)

	if _, err := store.SearchMessages(`"hello"`, 10); err != nil {
		t.Fatalf("search with embedded quotes errored: %v", err)
	}
}

// TestSearchMessages_ExcludesCompressionSummaries verifies that compression
// summaries stored in the compressions table do NOT appear in FTS search
// results, per the robustness spec recommendation.
func TestSearchMessages_ExcludesCompressionSummaries(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Create a message with a unique word.
	msgContent := "The astronaut planted a flag on the moon"
	store.AppendMessage(thread.ID, "user", &msgContent, nil)

	// Save a compression summary containing the same unique word.
	err = store.SaveCompression(CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "Earlier context summarized: the astronaut went to space",
		FirstKeptSeq: 2,
	})
	if err != nil {
		t.Fatalf("save compression: %v", err)
	}

	// Search for the word that appears in both the message and the compression summary.
	results, err := store.SearchMessages("astronaut", 10)
	if err != nil {
		t.Fatalf("search astronaut: %v", err)
	}

	// Only the real message should match; the compression summary must not appear.
	if len(results) != 1 {
		t.Fatalf("expected 1 result (message only), got %d", len(results))
	}
	if results[0].Role != "user" {
		t.Errorf("expected result from messages table (role=user), got role=%q", results[0].Role)
	}
}
