package memory

import (
	"strings"
	"testing"
)

func TestPlanModeMigrationAndRoundTrip(t *testing.T) {
	store := openTestStore(t)

	// The migration ran exactly once and added the column.
	var recorded int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE name = '0007_thread_plan_mode.sql'`,
	).Scan(&recorded); err != nil {
		t.Fatalf("read migration record: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("migration record count = %d, want 1", recorded)
	}
	var column string
	if err := store.DB().QueryRow(
		`SELECT name FROM pragma_table_info('threads') WHERE name = 'plan_mode'`,
	).Scan(&column); err != nil {
		t.Fatalf("plan_mode column missing: %v", err)
	}

	// New threads default to plan mode off, and lifecycle states round-trip
	// through both GetThread and ListThreads.
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.PlanMode != PlanModeOff {
		t.Fatalf("new thread plan mode = %q, want off", thread.PlanMode)
	}
	for _, state := range []string{PlanModePlanning, PlanModePendingApproval, PlanModeApproved, PlanModeOff} {
		if err := store.SetThreadPlanMode(thread.ID, state); err != nil {
			t.Fatalf("set plan mode %q: %v", state, err)
		}
		loaded, err := store.GetThread(thread.ID)
		if err != nil {
			t.Fatalf("get thread: %v", err)
		}
		if loaded.PlanMode != state {
			t.Fatalf("GetThread plan mode = %q, want %q", loaded.PlanMode, state)
		}
		threads, err := store.ListThreads()
		if err != nil {
			t.Fatalf("list threads: %v", err)
		}
		if len(threads) != 1 || threads[0].PlanMode != state {
			t.Fatalf("ListThreads plan mode = %+v, want state %q", threads, state)
		}
	}

	// Lifecycle transitions must not bump updated_at (not conversation activity).
	before, err := store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if err := store.SetThreadPlanMode(thread.ID, PlanModePlanning); err != nil {
		t.Fatalf("set plan mode: %v", err)
	}
	after, err := store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("plan-mode transition bumped updated_at: %v → %v", before.UpdatedAt, after.UpdatedAt)
	}

	// Unknown thread IDs are reported, not silently ignored.
	if err := store.SetThreadPlanMode("no-such-thread", PlanModePlanning); err == nil ||
		!strings.Contains(err.Error(), "thread not found") {
		t.Fatalf("set plan mode on missing thread: %v", err)
	}
}
