package memory

import (
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestCompressionsTableCreated(t *testing.T) {
	store := openTestStore(t)

	var name string
	err := store.DB().QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='compressions';").Scan(&name)
	if err != nil {
		t.Fatalf("compressions table not found: %v", err)
	}
	if name != "compressions" {
		t.Errorf("table name: got %q, want compressions", name)
	}
}

func TestCompressionMigrationIdempotent(t *testing.T) {
	// Open the store twice with the same database file. The second open re-runs
	// all migrations. Since they use IF NOT EXISTS, this must succeed.
	dbPath := t.TempDir() + "/test.db"

	store1, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	store1.Close()

	store2, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("second open (idempotent): %v", err)
	}
	defer store2.Close()

	// Verify the compressions table is usable after the second migration.
	var count int
	err = store2.DB().QueryRow("SELECT count(*) FROM compressions;").Scan(&count)
	if err != nil {
		t.Fatalf("select from compressions after idempotent migration: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows, got %d", count)
	}
}

func TestSaveAndGetLatestCompression(t *testing.T) {
	store := openTestStore(t)

	// Create a thread and some messages to establish seq values.
	thread, err := store.CreateThread(strPtr("test"), nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Append messages to get seq values.
	_, err = store.AppendMessage(thread.ID, "user", strPtr("hello"), nil)
	if err != nil {
		t.Fatalf("append message 1: %v", err)
	}
	_, err = store.AppendMessage(thread.ID, "assistant", strPtr("hi"), nil)
	if err != nil {
		t.Fatalf("append message 2: %v", err)
	}
	_, err = store.AppendMessage(thread.ID, "user", strPtr("do stuff"), nil)
	if err != nil {
		t.Fatalf("append message 3: %v", err)
	}

	// No compression record yet.
	rec, err := store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest (empty): %v", err)
	}
	if rec != nil {
		t.Error("expected nil record for thread with no compressions")
	}

	// Save a compression record.
	compRec := CompressionRecord{
		ThreadID:                thread.ID,
		Summary:                 "User asked about X and the assistant did Y.",
		FirstKeptSeq:            3, // messages 1-2 compressed, kept from seq 3
		CompressedMessageCount:  2,
		BeforeTokens:            5000,
		AfterTokens:             2000,
		BudgetTokens:            4000,
		SummaryModelAlias:       "deepseek/deepseek-v4-flash",
		SummaryModelID:          "deepseek/deepseek-v4-flash",
		SummaryPromptTokens:     3000,
		SummaryCompletionTokens: 500,
		SummaryTotalTokens:      3500,
	}
	if err := store.SaveCompression(compRec); err != nil {
		t.Fatalf("save compression: %v", err)
	}

	// Retrieve it.
	got, err := store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil record")
	}
	if got.Summary != compRec.Summary {
		t.Errorf("Summary: got %q, want %q", got.Summary, compRec.Summary)
	}
	if got.FirstKeptSeq != 3 {
		t.Errorf("FirstKeptSeq: got %d, want 3", got.FirstKeptSeq)
	}
	if got.CompressedMessageCount != 2 {
		t.Errorf("CompressedMessageCount: got %d, want 2", got.CompressedMessageCount)
	}
	if got.BeforeTokens != 5000 {
		t.Errorf("BeforeTokens: got %d, want 5000", got.BeforeTokens)
	}
	if got.AfterTokens != 2000 {
		t.Errorf("AfterTokens: got %d, want 2000", got.AfterTokens)
	}
	if got.SummaryModelAlias != "deepseek/deepseek-v4-flash" {
		t.Errorf("SummaryModelAlias: got %q", got.SummaryModelAlias)
	}
	if got.SummaryTotalTokens != 3500 {
		t.Errorf("SummaryTotalTokens: got %d, want 3500", got.SummaryTotalTokens)
	}
	if got.FallbackUsed {
		t.Error("FallbackUsed should be false")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
}

func TestGetLatestCompression_PicksMostRecent(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(strPtr("chain-test"), nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Save first compression.
	err = store.SaveCompression(CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "first summary",
		FirstKeptSeq: 5,
	})
	if err != nil {
		t.Fatalf("save first: %v", err)
	}

	// Save second compression (should become the latest).
	err = store.SaveCompression(CompressionRecord{
		ThreadID:     thread.ID,
		Summary:      "second summary",
		FirstKeptSeq: 10,
	})
	if err != nil {
		t.Fatalf("save second: %v", err)
	}

	got, err := store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got.Summary != "second summary" {
		t.Errorf("expected latest summary 'second summary', got %q", got.Summary)
	}
	if got.FirstKeptSeq != 10 {
		t.Errorf("expected FirstKeptSeq 10, got %d", got.FirstKeptSeq)
	}
}

func TestGetLatestCompression_DifferentThreads(t *testing.T) {
	store := openTestStore(t)
	t1, _ := store.CreateThread(strPtr("t1"), nil)
	t2, _ := store.CreateThread(strPtr("t2"), nil)

	store.SaveCompression(CompressionRecord{ThreadID: t1.ID, Summary: "t1 summary", FirstKeptSeq: 3})
	store.SaveCompression(CompressionRecord{ThreadID: t2.ID, Summary: "t2 summary", FirstKeptSeq: 7})

	got1, _ := store.GetLatestCompression(t1.ID)
	if got1.Summary != "t1 summary" {
		t.Errorf("t1: got %q", got1.Summary)
	}

	got2, _ := store.GetLatestCompression(t2.ID)
	if got2.Summary != "t2 summary" {
		t.Errorf("t2: got %q", got2.Summary)
	}
}

func TestGetThreadWithMessagesFromSeq(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(strPtr("from-seq"), nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Append 5 messages.
	for i := 0; i < 5; i++ {
		_, err := store.AppendMessage(thread.ID, "user", strPtr("msg"), nil)
		if err != nil {
			t.Fatalf("append message %d: %v", i, err)
		}
	}

	// Load from seq 3 onward: should get seq 3, 4, 5.
	_, msgs, err := store.GetThreadWithMessagesFromSeq(thread.ID, 3)
	if err != nil {
		t.Fatalf("from seq: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages from seq 3, got %d", len(msgs))
	}
	if msgs[0].Seq != 3 {
		t.Errorf("first msg seq: got %d, want 3", msgs[0].Seq)
	}
	if msgs[2].Seq != 5 {
		t.Errorf("last msg seq: got %d, want 5", msgs[2].Seq)
	}

	// Load from seq 1 (equivalent to full load).
	_, allMsgs, err := store.GetThreadWithMessagesFromSeq(thread.ID, 1)
	if err != nil {
		t.Fatalf("from seq 1: %v", err)
	}
	if len(allMsgs) != 5 {
		t.Errorf("expected 5 messages from seq 1, got %d", len(allMsgs))
	}

	// Load from seq beyond all messages.
	_, empty, err := store.GetThreadWithMessagesFromSeq(thread.ID, 99)
	if err != nil {
		t.Fatalf("from seq 99: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected 0 messages from seq 99, got %d", len(empty))
	}
}

func TestInvalidateCompressionsAfterSeq(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(strPtr("invalidate"), nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Save two compression records.
	store.SaveCompression(CompressionRecord{ThreadID: thread.ID, Summary: "early", FirstKeptSeq: 3})
	store.SaveCompression(CompressionRecord{ThreadID: thread.ID, Summary: "late", FirstKeptSeq: 7})

	// Invalidate records with first_kept_seq >= 7 (undo deletes from seq 7).
	err = store.InvalidateCompressionsAfterSeq(thread.ID, 7)
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	got, err := store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	// The "late" record (first_kept_seq=7) should be gone; "early" (3) should remain.
	if got == nil {
		t.Fatal("expected early record to remain")
	}
	if got.Summary != "early" {
		t.Errorf("expected 'early' summary, got %q", got.Summary)
	}
	if got.FirstKeptSeq != 3 {
		t.Errorf("expected FirstKeptSeq 3, got %d", got.FirstKeptSeq)
	}
}

func TestInvalidateCompressionsAfterSeq_RemovesBoth(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(strPtr("invalidate-both"), nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	store.SaveCompression(CompressionRecord{ThreadID: thread.ID, Summary: "a", FirstKeptSeq: 5})
	store.SaveCompression(CompressionRecord{ThreadID: thread.ID, Summary: "b", FirstKeptSeq: 10})

	// Undo from seq 3: both records have first_kept_seq >= 3, so both are invalidated.
	err = store.InvalidateCompressionsAfterSeq(thread.ID, 3)
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	got, err := store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after invalidating all records, got summary %q", got.Summary)
	}
}

func TestCompressionCascadeOnThreadDelete(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(strPtr("cascade"), nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	store.SaveCompression(CompressionRecord{ThreadID: thread.ID, Summary: "to be cascaded", FirstKeptSeq: 3})

	// Delete the thread.
	err = store.DeleteThread(thread.ID)
	if err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	// Compression record should be gone via ON DELETE CASCADE.
	var count int
	err = store.DB().QueryRow("SELECT count(*) FROM compressions WHERE thread_id = ?", thread.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count compressions: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 compression records after thread delete, got %d", count)
	}
}

func TestSaveCompression_WithFallback(t *testing.T) {
	store := openTestStore(t)
	thread, _ := store.CreateThread(strPtr("fallback"), nil)

	rec := CompressionRecord{
		ThreadID:       thread.ID,
		Summary:        "",
		FirstKeptSeq:   5,
		FallbackUsed:   true,
		FallbackReason: "summarizer_error: timeout",
		BeforeTokens:   8000,
		AfterTokens:    6400,
	}
	if err := store.SaveCompression(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.GetLatestCompression(thread.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.FallbackUsed {
		t.Error("expected FallbackUsed=true")
	}
	if got.FallbackReason != "summarizer_error: timeout" {
		t.Errorf("FallbackReason: got %q", got.FallbackReason)
	}
}

func TestSaveCompression_SetsCreatedAt(t *testing.T) {
	store := openTestStore(t)
	thread, _ := store.CreateThread(strPtr("ts"), nil)

	before := time.Now().UTC().Add(-time.Second)
	store.SaveCompression(CompressionRecord{ThreadID: thread.ID, Summary: "ts-test", FirstKeptSeq: 1})
	after := time.Now().UTC().Add(time.Second)

	got, _ := store.GetLatestCompression(thread.ID)
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v not between %v and %v", got.CreatedAt, before, after)
	}
}
