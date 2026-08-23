package memory

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"
)

func TestPlanMigrationCreatesConstrainedSchema(t *testing.T) {
	store := openTestStore(t)

	for _, table := range []string{"plans", "todos"} {
		var got string
		if err := store.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&got); err != nil {
			t.Fatalf("table %s not found: %v", table, err)
		}
	}
	var recorded int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE name = '0006_thread_plans.sql'`,
	).Scan(&recorded); err != nil {
		t.Fatalf("read migration record: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("migration record count = %d, want 1", recorded)
	}

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := store.DB().Exec(
		`INSERT INTO plans (thread_id, revision, next_todo_id, created_at, updated_at)
		 VALUES (?, 1, 2, 1, 1)`, thread.ID,
	); err != nil {
		t.Fatalf("insert raw plan: %v", err)
	}
	if _, err := store.DB().Exec(
		`INSERT INTO todos (thread_id, id, content, status, position, created_at, updated_at)
		 VALUES (?, '1', 'bad state', 'unknown', 1, 1, 1)`, thread.ID,
	); err == nil {
		t.Fatal("database accepted an invalid todo status")
	}
}

func TestLegacyDatabaseUpgradesWithPlanMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/legacy.db"
	real := os.DirFS("../../migrations")
	legacyFS := fstest.MapFS{}
	entries, err := fs.ReadDir(real, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0006" {
			continue
		}
		data, err := fs.ReadFile(real, entry.Name())
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		legacyFS[entry.Name()] = &fstest.MapFile{Data: data}
	}

	legacy, err := OpenWithMigrationsFS(dbPath, legacyFS)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	thread, err := legacy.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create legacy thread: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	upgraded, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	if _, err := upgraded.GetThread(thread.ID); err != nil {
		t.Fatalf("legacy thread was lost: %v", err)
	}
	if plan, err := upgraded.GetPlan(thread.ID); err != nil || plan != nil {
		t.Fatalf("new plan on legacy thread = %#v, %v; want nil, nil", plan, err)
	}
	if _, err := upgraded.CreateTodos(thread.ID, []TodoItem{{Content: "survives restart"}}); err != nil {
		t.Fatalf("use upgraded plan schema: %v", err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatalf("close upgraded database: %v", err)
	}

	reopened, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("reopen upgraded database: %v", err)
	}
	defer reopened.Close()
	items, err := reopened.ListTodos(thread.ID)
	if err != nil {
		t.Fatalf("list persisted todos: %v", err)
	}
	if len(items) != 1 || items[0].Content != "survives restart" {
		t.Fatalf("persisted todos = %#v", items)
	}
}

func TestEmbeddedPlanMigration(t *testing.T) {
	store, err := Open(t.TempDir() + "/embedded.db")
	if err != nil {
		t.Fatalf("open with embedded migrations: %v", err)
	}
	defer store.Close()
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	items, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "embedded"}})
	if err != nil {
		t.Fatalf("create todo through embedded schema: %v", err)
	}
	if len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("embedded plan items = %#v", items)
	}
}

func TestPlanCRUDAndThreadCascade(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	plan, err := store.GetPlan(thread.ID)
	if err != nil || plan != nil {
		t.Fatalf("initial plan = %#v, %v; want nil, nil", plan, err)
	}
	input := []TodoItem{
		{Content: "first"},
		{ID: "7", Content: "imported", Status: TodoCompleted},
		{Content: "third", Status: TodoInProgress},
	}
	plan, err = store.CreatePlan(thread.ID, input)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if plan.Revision != 1 {
		t.Fatalf("initial revision = %d, want 1", plan.Revision)
	}
	assertTodoShape(t, plan.Items, []string{"1", "7", "2"}, []string{"first", "imported", "third"})
	if plan.Items[0].Status != TodoPending || plan.Items[1].Status != TodoCompleted || plan.Items[2].Status != TodoInProgress {
		t.Fatalf("statuses = %#v", plan.Items)
	}
	if input[0].ID != "" || input[0].Status != "" {
		t.Fatalf("CreatePlan mutated caller input: %#v", input)
	}

	if _, err := store.CreatePlan(thread.ID, nil); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("duplicate CreatePlan error = %v, want ErrPlanExists", err)
	}
	listed, err := store.ListTodos(thread.ID)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	if !reflect.DeepEqual(listed, plan.Items) {
		t.Fatalf("listed todos differ:\n got %#v\nwant %#v", listed, plan.Items)
	}

	if err := store.DeletePlan(thread.ID); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	if err := store.DeletePlan(thread.ID); err != nil {
		t.Fatalf("idempotent delete plan: %v", err)
	}
	if got, err := store.ListTodos(thread.ID); err != nil || len(got) != 0 {
		t.Fatalf("todos after plan delete = %#v, %v", got, err)
	}

	if _, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "cascade me"}}); err != nil {
		t.Fatalf("recreate plan: %v", err)
	}
	if err := store.DeleteThread(thread.ID); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	for _, table := range []string{"plans", "todos"} {
		var count int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after thread delete = %d, want 0", table, count)
		}
	}
}

func TestTodoLifecycleUsesStableOrderAndMonotonicIDs(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	items, err := store.CreateTodos(thread.ID, []TodoItem{
		{Content: "design"},
		{Content: "build", Status: TodoInProgress},
	})
	if err != nil {
		t.Fatalf("create todos: %v", err)
	}
	assertTodoShape(t, items, []string{"1", "2"}, []string{"design", "build"})
	plan := mustGetPlan(t, store, thread.ID)
	if plan.Revision != 1 {
		t.Fatalf("revision after initial create = %d, want 1", plan.Revision)
	}

	items, err = store.CreateTodos(thread.ID, []TodoItem{{Content: "verify"}})
	if err != nil {
		t.Fatalf("append todo: %v", err)
	}
	assertTodoShape(t, items, []string{"1", "2", "3"}, []string{"design", "build", "verify"})

	done := TodoCompleted
	reworded := "implement"
	items, err = store.UpdateTodos(thread.ID, []TodoUpdate{
		{ID: "1", Status: &done},
		{ID: "2", Content: &reworded},
	})
	if err != nil {
		t.Fatalf("update todos: %v", err)
	}
	if items[0].Status != TodoCompleted || items[1].Content != "implement" {
		t.Fatalf("updated todos = %#v", items)
	}

	items, err = store.DeleteTodos(thread.ID, []string{"2"})
	if err != nil {
		t.Fatalf("delete todo: %v", err)
	}
	assertTodoShape(t, items, []string{"1", "3"}, []string{"design", "verify"})

	items, err = store.CreateTodos(thread.ID, []TodoItem{{Content: "ship"}})
	if err != nil {
		t.Fatalf("create after delete: %v", err)
	}
	assertTodoShape(t, items, []string{"1", "3", "4"}, []string{"design", "verify", "ship"})

	items, err = store.ReplaceTodos(thread.ID, []TodoItem{
		{ID: "3", Content: "verify harder", Status: TodoInProgress},
		{Content: "announce"},
	})
	if err != nil {
		t.Fatalf("replace todos: %v", err)
	}
	assertTodoShape(t, items, []string{"3", "5"}, []string{"verify harder", "announce"})
	if items[0].Status != TodoInProgress || items[1].Status != TodoPending {
		t.Fatalf("replacement statuses = %#v", items)
	}

	items, err = store.ReplaceTodos(thread.ID, nil)
	if err != nil {
		t.Fatalf("clear todos: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("cleared todos = %#v, want empty", items)
	}
	plan = mustGetPlan(t, store, thread.ID)
	if plan.Revision != 7 {
		t.Fatalf("final revision = %d, want 7", plan.Revision)
	}
}

func TestTodoValidationAndBatchesAreAtomic(t *testing.T) {
	store := openTestStore(t)
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "one"}, {Content: "two"}}); err != nil {
		t.Fatalf("seed todos: %v", err)
	}
	before := mustGetPlan(t, store, thread.ID)

	invalidStatus := TodoStatus("blocked")
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "create supplied ID",
			run: func() error {
				_, err := store.CreateTodos(thread.ID, []TodoItem{{ID: "99", Content: "bad"}})
				return err
			},
		},
		{
			name: "create blank content",
			run: func() error {
				_, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "  "}})
				return err
			},
		},
		{
			name: "create invalid status",
			run: func() error {
				_, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "bad", Status: invalidStatus}})
				return err
			},
		},
		{
			name: "update unknown after valid",
			run: func() error {
				changed := "must roll back"
				_, err := store.UpdateTodos(thread.ID, []TodoUpdate{
					{ID: "1", Content: &changed},
					{ID: "missing", Status: &invalidStatus},
				})
				return err
			},
		},
		{
			name: "replace duplicate ID",
			run: func() error {
				_, err := store.ReplaceTodos(thread.ID, []TodoItem{
					{ID: "1", Content: "first"},
					{ID: "1", Content: "duplicate"},
				})
				return err
			},
		},
		{
			name: "delete unknown after valid",
			run: func() error {
				_, err := store.DeleteTodos(thread.ID, []string{"1", "missing"})
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("operation unexpectedly succeeded")
			}
			after := mustGetPlan(t, store, thread.ID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("failed mutation changed plan:\n before %#v\n after  %#v", before, after)
			}
		})
	}

	valid := TodoCompleted
	_, err = store.UpdateTodos(thread.ID, []TodoUpdate{
		{ID: "1", Status: &valid},
		{ID: "missing", Status: &valid},
	})
	if !errors.Is(err, ErrTodoNotFound) {
		t.Fatalf("unknown update error = %v, want ErrTodoNotFound", err)
	}
	afterUnknown := mustGetPlan(t, store, thread.ID)
	if !reflect.DeepEqual(afterUnknown, before) {
		t.Fatalf("partially applied unknown-ID batch:\n before %#v\n after  %#v", before, afterUnknown)
	}
	if _, err := store.CreateTodos("missing-thread", []TodoItem{{Content: "orphan"}}); err == nil {
		t.Fatal("created todos for a missing thread")
	}
}

func TestPlansAreIsolatedByThread(t *testing.T) {
	store := openTestStore(t)
	threadA, _ := store.CreateThread(nil, nil)
	threadB, _ := store.CreateThread(nil, nil)

	if _, err := store.CreateTodos(threadA.ID, []TodoItem{{Content: "A"}}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := store.CreateTodos(threadB.ID, []TodoItem{{Content: "B"}}); err != nil {
		t.Fatalf("create B: %v", err)
	}
	itemsA, _ := store.ListTodos(threadA.ID)
	itemsB, _ := store.ListTodos(threadB.ID)
	if len(itemsA) != 1 || itemsA[0].ID != "1" || itemsA[0].Content != "A" {
		t.Fatalf("thread A items = %#v", itemsA)
	}
	if len(itemsB) != 1 || itemsB[0].ID != "1" || itemsB[0].Content != "B" {
		t.Fatalf("thread B items = %#v", itemsB)
	}

	done := TodoCompleted
	if _, err := store.UpdateTodos(threadA.ID, []TodoUpdate{{ID: "1", Status: &done}}); err != nil {
		t.Fatalf("update A: %v", err)
	}
	itemsB, _ = store.ListTodos(threadB.ID)
	if itemsB[0].Status != TodoPending {
		t.Fatalf("thread B status leaked from A: %q", itemsB[0].Status)
	}
}

func TestCreateTodosConcurrentAllocatesUniqueOrderedIDs(t *testing.T) {
	dbPath := t.TempDir() + "/plans.db"
	storeA, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	defer storeA.Close()
	thread, err := storeA.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	storeB, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	defer storeB.Close()

	const count = 32
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := storeA
			if i%2 == 1 {
				store = storeB
			}
			_, err := store.CreateTodos(thread.ID, []TodoItem{{Content: "item " + strconv.Itoa(i)}})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}

	plan := mustGetPlan(t, storeA, thread.ID)
	if len(plan.Items) != count {
		t.Fatalf("item count = %d, want %d", len(plan.Items), count)
	}
	if plan.Revision != count {
		t.Fatalf("revision = %d, want %d", plan.Revision, count)
	}
	for i, item := range plan.Items {
		want := strconv.Itoa(i + 1)
		if item.ID != want || item.Position != i+1 {
			t.Fatalf("item %d = ID %q position %d, want ID %q position %d", i, item.ID, item.Position, want, i+1)
		}
	}
}

func TestValidateTodoStatus(t *testing.T) {
	for _, status := range []TodoStatus{TodoPending, TodoInProgress, TodoCompleted, TodoCancelled} {
		if !status.Valid() {
			t.Errorf("%q.Valid() = false", status)
		}
		if err := ValidateTodoStatus(status); err != nil {
			t.Errorf("ValidateTodoStatus(%q): %v", status, err)
		}
	}
	if TodoStatus("blocked").Valid() {
		t.Fatal("blocked.Valid() = true")
	}
	if err := ValidateTodoStatus("blocked"); err == nil {
		t.Fatal("ValidateTodoStatus(blocked) succeeded")
	}
}

func mustGetPlan(t *testing.T, store *Store, threadID string) *Plan {
	t.Helper()
	plan, err := store.GetPlan(threadID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if plan == nil {
		t.Fatal("get plan returned nil")
	}
	return plan
}

func assertTodoShape(t *testing.T, items []TodoItem, ids, contents []string) {
	t.Helper()
	if len(items) != len(ids) || len(items) != len(contents) {
		t.Fatalf("item count = %d, want ids=%d contents=%d", len(items), len(ids), len(contents))
	}
	for i, item := range items {
		if item.ID != ids[i] || item.Content != contents[i] || item.Position != i+1 {
			t.Fatalf("item %d = {ID:%q Content:%q Position:%d}, want {ID:%q Content:%q Position:%d}",
				i, item.ID, item.Content, item.Position, ids[i], contents[i], i+1)
		}
		if item.ThreadID == "" || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
			t.Fatalf("item %d missing persisted metadata: %#v", i, item)
		}
	}
}
