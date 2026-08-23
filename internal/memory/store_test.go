package memory

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
)

func TestOpenAndWAL(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var journalMode string
	row := store.DB().QueryRow("PRAGMA journal_mode;")
	if err := row.Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode: got %q, want wal", journalMode)
	}
}

func TestEveryPooledConnectionEnforcesForeignKeys(t *testing.T) {
	store, err := OpenWithMigrations(t.TempDir()+"/pooled.db", "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "cascade across pooled connection"}}); err != nil {
		t.Fatalf("create persisted plan: %v", err)
	}

	ctx := context.Background()
	first, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("acquire first connection: %v", err)
	}
	defer first.Close()
	second, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("acquire second connection: %v", err)
	}
	defer second.Close()
	for i, conn := range []*sql.Conn{first, second} {
		var enabled int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
			t.Fatalf("read foreign_keys on connection %d: %v", i+1, err)
		}
		if enabled != 1 {
			t.Fatalf("foreign_keys on connection %d = %d, want 1", i+1, enabled)
		}
	}

	// Both known connections remain checked out, forcing DeleteThread onto a
	// newly opened pooled connection. Its foreign-key cascade must still apply.
	if err := store.DeleteThread(thread.ID); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	for _, table := range []string{"plans", "todos"} {
		var count int
		if err := first.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after pooled delete = %d, want 0", table, count)
		}
	}
}

func TestTablesCreated(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	tables := []string{"threads", "messages", "tool_calls", "compressions"}
	for _, tbl := range tables {
		var name string
		err := store.DB().QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?;", tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", tbl, err)
		}
	}
}

func TestRestartSafety(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store1, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store 1: %v", err)
	}
	store1.Close()

	store2, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store 2: %v", err)
	}
	defer store2.Close()

	var count int
	if err := store2.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table';").Scan(&count); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if count < 4 {
		t.Errorf("expected at least 4 tables after restart, got %d", count)
	}
}

// TestOpenEmbeddedMigrations verifies Open() builds a working schema from the
// migrations embedded in the binary — no migrations directory on disk needed.
func TestOpenEmbeddedMigrations(t *testing.T) {
	store, err := Open(t.TempDir() + "/fresh.db")
	if err != nil {
		t.Fatalf("Open with embedded migrations: %v", err)
	}
	defer store.Close()
	if _, err := store.CreateThread(nil, nil); err != nil {
		t.Errorf("schema not applied (CreateThread failed): %v", err)
	}
}

func TestLegacyDBUpgradesWithWorkspaceColumn(t *testing.T) {
	// Simulate a database created by an older binary: apply only migrations
	// 0001–0004 (the idempotent-era set), create a thread, then reopen with
	// the full migration set and verify the workspace column is added without
	// losing data, and that subsequent reopens are no-ops.
	dir := t.TempDir()
	dbPath := dir + "/legacy.db"

	real := os.DirFS("../../migrations")
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(real, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if e.Name() >= "0005" {
			continue // stop at the workspace migration — the legacy set
		}
		data, err := fs.ReadFile(real, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		old[e.Name()] = &fstest.MapFile{Data: data}
	}

	legacy, err := OpenWithMigrationsFS(dbPath, old)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	// Write the thread the way the old binary did — the INSERT predates the
	// workspace column, so it must go through raw SQL here.
	legacyID := "legacy-thread-1"
	if _, err := legacy.DB().Exec(
		`INSERT INTO threads (id, title, created_at, updated_at, model) VALUES (?, NULL, 1, 1, NULL)`,
		legacyID,
	); err != nil {
		t.Fatalf("insert legacy thread: %v", err)
	}
	legacy.Close()

	// Reopen with the full migration set (incl. 0005 ALTER TABLE).
	upgraded, err := OpenWithMigrationsFS(dbPath, real)
	if err != nil {
		t.Fatalf("open upgraded db: %v", err)
	}
	defer upgraded.Close()

	got, err := upgraded.GetThread(legacyID)
	if err != nil {
		t.Fatalf("thread lost during upgrade: %v", err)
	}
	if got.Workspace != "" {
		t.Errorf("legacy thread workspace: got %q, want %q (unknown)", got.Workspace, "")
	}
	var hasCol int
	if err := upgraded.DB().QueryRow(
		`SELECT count(*) FROM pragma_table_info('threads') WHERE name='workspace'`,
	).Scan(&hasCol); err != nil || hasCol != 1 {
		t.Errorf("workspace column missing after upgrade: hasCol=%d err=%v", hasCol, err)
	}

	// A second reopen must not re-run 0005 (the ALTER is not idempotent).
	again, err := OpenWithMigrationsFS(dbPath, real)
	if err != nil {
		t.Fatalf("second reopen after upgrade failed: %v", err)
	}
	again.Close()

	// Workspace-aware listing works on the upgraded db.
	listed, err := upgraded.ListThreadsByWorkspace("", true)
	if err != nil {
		t.Fatalf("list by workspace on upgraded db: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != legacyID {
		t.Errorf("upgraded listing: got %+v", listed)
	}
}
