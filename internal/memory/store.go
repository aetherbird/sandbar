package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"sandbar/migrations"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the SQLite database at path and applies the migrations
// embedded in the binary. This works regardless of the working directory or
// where the config file lives.
func Open(path string) (*Store, error) {
	return OpenWithMigrationsFS(path, migrations.FS)
}

// OpenWithMigrations opens the database and applies migrations from a directory
// on disk. Kept for tests that point at the repo's migrations/ folder.
func OpenWithMigrations(path, migrationsDir string) (*Store, error) {
	return OpenWithMigrationsFS(path, os.DirFS(migrationsDir))
}

// OpenWithMigrationsFS opens the database and applies migrations read from fsys.
func OpenWithMigrationsFS(path string, fsys fs.FS) (*Store, error) {
	// SQLite pragmas such as foreign_keys and busy_timeout are connection-local.
	// Put them in the driver DSN so database/sql applies them to every pooled
	// connection, including connections opened after startup under server load.
	db, err := sql.Open("sqlite", sqliteConnectionDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(fsys); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func sqliteConnectionDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator +
		"_pragma=foreign_keys%281%29" +
		"&_pragma=busy_timeout%285000%29" +
		"&_pragma=synchronous%28NORMAL%29"
}

// migrate reads and executes all numbered .sql migrations in order from fsys,
// recording each applied migration in schema_migrations so it runs exactly once
// per database. Earlier migrations were written to be idempotent (CREATE TABLE
// IF NOT EXISTS) because the old runner re-executed everything on every open;
// on a database created by an older binary, the first run with this runner
// re-applies them (harmless — they are still idempotent), records them, and
// never runs them again. Newer migrations may use non-idempotent statements
// such as ALTER TABLE.
func (s *Store) migrate(fsys fs.FS) error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := s.applyMigration(fsys, name); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration in a BEGIN IMMEDIATE transaction, checking
// and recording it in schema_migrations inside the same transaction. The
// immediate lock serializes concurrent first-opens of a fresh database: the
// second process blocks until the first commits, then sees the migration as
// applied and skips it — so two processes racing to open the same new database
// can't both run a non-idempotent statement like ALTER TABLE ADD COLUMN.
func (s *Store) applyMigration(fsys fs.FS, name string) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration %s: acquire connection: %w", name, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("migration %s: begin immediate: %w", name, err)
	}
	inTransaction := true
	defer func() {
		if !inTransaction {
			return
		}
		if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("migration %s: rollback: %w", name, rollbackErr))
		}
	}()

	var applied int
	err = conn.QueryRowContext(ctx,
		`SELECT 1 FROM schema_migrations WHERE name = ?`, name,
	).Scan(&applied)
	if err == nil {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("migration %s: commit: %w", name, err)
		}
		inTransaction = false
		return nil // already applied — skip
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("migration %s: check applied: %w", name, err)
	}

	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}
	if _, err := conn.ExecContext(ctx, string(data)); err != nil {
		return fmt.Errorf("exec migration %s: %w", name, err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations (name) VALUES (?)`, name,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	inTransaction = false
	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying sql.DB for tests.
func (s *Store) DB() *sql.DB {
	return s.db
}
