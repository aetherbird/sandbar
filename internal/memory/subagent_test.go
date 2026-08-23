package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSubagentTranscriptRendersPersistedTask(t *testing.T) {
	store, err := OpenWithMigrations(filepath.Join(t.TempDir(), "test.db"), "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	messages := `[
		{"role":"user","content":"audit the parser"},
		{"role":"assistant","content":"starting","tool_calls":[{"id":"c1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"parser.go\"}"}}]},
		{"role":"tool","name":"file_read","tool_call_id":"c1","content":"parser.go contents"},
		{"role":"assistant","content":"parser is fine"},
		{"role":"tool","name":"shell_exec","tool_call_id":"c2","content":"error: boom"}
	]`
	if _, err := store.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, status, result, created_at, updated_at)
		 VALUES ('t1', 'audit parser', 'focus on errors', 'p/m', ?, 'completed', 'all good', 1, 1)`, messages,
	); err != nil {
		t.Fatal(err)
	}

	out, err := store.SubagentTranscript("t1")
	if err != nil {
		t.Fatalf("SubagentTranscript: %v", err)
	}
	for _, want := range []string{
		"subagent task t1", "status: completed", "model: p/m",
		"goal: audit parser", "context: focus on errors",
		"## user\naudit the parser",
		"starting", "▸ tool call file_read", "✓ tool result file_read: parser.go contents",
		"✗ tool result shell_exec: error: boom", "parser is fine",
		"## result\nall good",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
}

func TestSubagentTranscriptUnknownTask(t *testing.T) {
	store, err := OpenWithMigrations(filepath.Join(t.TempDir(), "test.db"), "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SubagentTranscript("missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown task error = %v", err)
	}
}

func TestSubagentTranscriptRejectsCorruptMessages(t *testing.T) {
	store, err := OpenWithMigrations(filepath.Join(t.TempDir(), "test.db"), "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, model_alias, messages_json, status, created_at, updated_at)
		 VALUES ('t2', 'g', 'm', 'not json', 'failed', 1, 1)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubagentTranscript("t2"); err == nil {
		t.Fatal("corrupt messages_json should error")
	}
}
