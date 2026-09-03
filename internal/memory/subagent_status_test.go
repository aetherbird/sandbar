package memory

import (
	"testing"
)

// TestSubagentTaskStatus pins the background-delegation polling contract:
// status and result reflect the persisted lifecycle row, unknown IDs error,
// and a terminal update is what pollers see as "done".
func TestSubagentTaskStatus(t *testing.T) {
	s, err := OpenWithMigrations(t.TempDir()+"/test.db", "../../migrations")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	id := "bg-task-1"
	_, err = s.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, turn, max_turns, status, result, files_read, files_written, commands_run, created_at, updated_at)
		 VALUES (?, 'map the repo', '', 'm', '[]', 0, 5, 'running', '', '[]', '[]', '[]', 0, 0)`, id,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	status, result, err := s.SubagentTaskStatus(id)
	if err != nil || status != "running" || result != "" {
		t.Fatalf("running row = (%q, %q, %v)", status, result, err)
	}

	if _, err := s.DB().Exec(
		`UPDATE subagent_tasks SET status = 'completed', result = 'summary of findings' WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	status, result, err = s.SubagentTaskStatus(id)
	if err != nil || status != "completed" || result != "summary of findings" {
		t.Fatalf("completed row = (%q, %q, %v)", status, result, err)
	}

	if _, _, err := s.SubagentTaskStatus("no-such-task"); err == nil {
		t.Fatal("unknown task must error")
	}
}
