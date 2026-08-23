package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandbar/internal/memory"
)

func TestGhArgs(t *testing.T) {
	tests := []struct {
		uri     string
		kind    string
		wantErr bool
		argv    []string
		label   string
	}{
		{uri: "pr://123", kind: "pr", argv: []string{"pr", "view", "123"}, label: "123"},
		{uri: "issue://42", kind: "issue", argv: []string{"issue", "view", "42"}, label: "42"},
		{uri: "pr://owner/repo/7", kind: "pr", argv: []string{"pr", "view", "7", "--repo", "owner/repo"}, label: "owner/repo/7"},
		{uri: "issue://a/b/9", kind: "issue", argv: []string{"issue", "view", "9", "--repo", "a/b"}, label: "a/b/9"},
		{uri: "pr://", kind: "pr", wantErr: true},
		{uri: "pr://abc", kind: "pr", wantErr: true},
		{uri: "pr://a/b/c/d", kind: "pr", wantErr: true},
		{uri: "file://x", kind: "pr", wantErr: true},
	}
	for _, tt := range tests {
		argv, label, err := ghArgs(tt.uri, tt.kind)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ghArgs(%q) = %v, want error", tt.uri, argv)
			}
			continue
		}
		if err != nil {
			t.Errorf("ghArgs(%q): %v", tt.uri, err)
			continue
		}
		if strings.Join(argv, " ") != strings.Join(tt.argv, " ") || label != tt.label {
			t.Errorf("ghArgs(%q) = %v %q, want %v %q", tt.uri, argv, label, tt.argv, tt.label)
		}
	}
}

// TestReadSchemeRouting: the scheme matrix routes pr://, issue:// and
// agent:// URIs; every other path (including colon-bearing ones) falls
// through to the normal file read.
func TestReadSchemeRouting(t *testing.T) {
	cases := []struct {
		path   string
		routed bool
	}{
		{"pr://123", true},
		{"issue://42", true},
		{"pr://owner/repo/7", true},
		{"agent://task-1", true},
		{"agent://", true},
		{"src/main.go", false},
		{"/etc/passwd", false},
		{"weird:name.txt", false},
		{"./relative:file", false},
	}
	for _, tc := range cases {
		_, routed, err := readScheme(context.Background(), nil, tc.path)
		if routed != tc.routed {
			t.Errorf("readScheme(%q) routed = %v (err %v), want %v", tc.path, routed, err, tc.routed)
		}
	}
}

// installFakeGH puts a fake gh executable first on PATH that records its
// arguments and prints a canned view body — the same stub pattern the ssh
// tests use.
func installFakeGH(t *testing.T) (callsFile string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GH_CALLS\"\nprintf 'fake gh view body\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("GH_CALLS", calls)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// TestReadSchemeGHResolvedViaStub: pr:// and issue:// paths resolve through
// the gh CLI (stubbed on PATH) and never touch the workspace jail.
func TestReadSchemeGHResolvedViaStub(t *testing.T) {
	calls := installFakeGH(t)
	workspace := t.TempDir()

	out, err := NewFileTools(workspace).FileRead(context.Background(), map[string]interface{}{"path": "pr://123"})
	if err != nil {
		t.Fatalf("pr:// read: %v", err)
	}
	if !strings.Contains(out, "pr (123)") || !strings.Contains(out, "fake gh view body") {
		t.Fatalf("pr:// output = %q", out)
	}
	got, readErr := os.ReadFile(calls)
	if readErr != nil || strings.TrimSpace(string(got)) != "pr view 123" {
		t.Fatalf("gh argv = %q (%v), want %q", got, readErr, "pr view 123")
	}

	out, err = NewFileTools(workspace).FileRead(context.Background(), map[string]interface{}{"path": "issue://own/rep/7"})
	if err != nil {
		t.Fatalf("issue:// read: %v", err)
	}
	if !strings.Contains(out, "issue (own/rep/7)") {
		t.Fatalf("issue:// output = %q", out)
	}
	got, readErr = os.ReadFile(calls)
	if readErr != nil || strings.TrimSpace(string(got)) != "pr view 123\nissue view 7 --repo own/rep" {
		t.Fatalf("gh argv = %q (%v)", got, readErr)
	}
}

// TestReadSchemeGHMissingBinary: with no gh on PATH the scheme read errors
// with a message naming gh rather than falling through to a file read.
func TestReadSchemeGHMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewFileTools(t.TempDir()).FileRead(context.Background(), map[string]interface{}{"path": "pr://123"})
	if err == nil || !strings.Contains(err.Error(), "gh") {
		t.Fatalf("pr:// read error = %v, want gh mention", err)
	}
}

// seedSubagentTask inserts one persisted subagent task row.
func seedSubagentTask(t *testing.T, store *memory.Store, id string) {
	t.Helper()
	messages := `[
		{"role":"user","content":"inspect the auth module"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"search_files","arguments":"{\"pattern\":\"auth\"}"}}]},
		{"role":"tool","name":"search_files","tool_call_id":"c1","content":"auth.go:3: package auth"},
		{"role":"assistant","content":"The auth module is self-contained."}
	]`
	if _, err := store.DB().Exec(
		`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, status, result, created_at, updated_at)
		 VALUES (?, ?, '', 'test-model', ?, 'completed', 'done summary', 0, 0)`,
		id, "check auth", messages,
	); err != nil {
		t.Fatal(err)
	}
}

// TestReadAgentScheme: agent:// renders the persisted subagent transcript
// from the store.
func TestReadAgentScheme(t *testing.T) {
	store, err := memory.OpenWithMigrations(filepath.Join(t.TempDir(), "test.db"), "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	seedSubagentTask(t, store, "task-1")
	ft := NewFileTools(t.TempDir())
	ft.SetSubagentStore(store)

	out, err := ft.FileRead(context.Background(), map[string]interface{}{"path": "agent://task-1"})
	if err != nil {
		t.Fatalf("agent:// read: %v", err)
	}
	for _, want := range []string{
		"subagent task task-1", "status: completed",
		"goal: check auth", "inspect the auth module",
		"▸ tool call search_files", "package auth",
		"The auth module is self-contained.", "done summary",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript missing %q: %q", want, out)
		}
	}
}

// TestReadAgentSchemeNoStore: without the store wired, agent:// explains the
// missing wiring instead of failing as an unknown file.
func TestReadAgentSchemeNoStore(t *testing.T) {
	_, err := NewFileTools(t.TempDir()).FileRead(context.Background(), map[string]interface{}{"path": "agent://someid"})
	if err == nil || !strings.Contains(err.Error(), "session store") {
		t.Fatalf("agent:// without store error = %v", err)
	}
}

func TestReadAgentSchemeMissingID(t *testing.T) {
	_, err := NewFileTools(t.TempDir()).FileRead(context.Background(), map[string]interface{}{"path": "agent://"})
	if err == nil || !strings.Contains(err.Error(), "agent://") {
		t.Fatalf("missing id error = %v", err)
	}
}

func TestReadAgentSchemeUnknownTask(t *testing.T) {
	store, err := memory.OpenWithMigrations(filepath.Join(t.TempDir(), "test.db"), "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ft := NewFileTools(t.TempDir())
	ft.SetSubagentStore(store)
	_, err = ft.FileRead(context.Background(), map[string]interface{}{"path": "agent://ffffffff"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown task error = %v", err)
	}
}

// TestReadSchemeFallbackToFile: a normal path (even one containing a colon)
// must fall through to the jailed file read, not be treated as a scheme.
func TestReadSchemeFallbackToFile(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "plain.txt")
	if err := os.WriteFile(path, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewFileTools(workspace).FileRead(context.Background(), map[string]interface{}{"path": "plain.txt"})
	if err != nil {
		t.Fatalf("plain read failed: %v", err)
	}
	if !strings.Contains(out, "content") {
		t.Fatalf("file content missing: %q", out)
	}
}
