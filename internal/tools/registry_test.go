package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)

	// List should have default tools.
	names := r.List()
	if len(names) == 0 {
		t.Fatal("expected registered tools")
	}

	// Get existing.
	for _, name := range names {
		tool, err := r.Get(name)
		if err != nil {
			t.Errorf("get %s: %v", name, err)
		}
		if tool.Name != name {
			t.Errorf("tool name mismatch: got %q, want %q", tool.Name, name)
		}
	}

	// Get unknown.
	_, err := r.Get("nonexistent")
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestRegistryWithoutSubagentsOmitsBothEntryPoints(t *testing.T) {
	normal := NewRegistry(t.TempDir(), "", "", nil)
	for _, name := range []string{"delegate_task", "resume_task"} {
		if _, err := normal.Get(name); err != nil {
			t.Fatalf("default registry missing %s: %v", name, err)
		}
	}

	restricted := NewRegistry(t.TempDir(), "", "", nil, WithoutSubagents())
	names := strings.Join(restricted.List(), ",")
	for _, name := range []string{"delegate_task", "resume_task"} {
		if strings.Contains(","+names+",", ","+name+",") {
			t.Errorf("restricted registry advertised %s: %v", name, restricted.List())
		}
		if _, err := restricted.Get(name); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("Get(%q) error = %v, want unknown tool", name, err)
		}
		if _, err := restricted.Execute(context.Background(), name, map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("Execute(%q) error = %v, want unknown tool", name, err)
		}
	}
	if _, err := restricted.Get("file_read"); err != nil {
		t.Fatalf("restricted registry removed unrelated tool: %v", err)
	}
}

func TestRegistryCancelActiveTool(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	// Cancel when nothing is active should not error.
	if err := r.CancelActiveTool(); err != nil {
		t.Errorf("cancel with no active tool: %v", err)
	}
}

func TestRegistryParallelSafetyIsExplicit(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	for _, name := range []string{"file_read", "search_files", "web_search", "web_fetch", "delegate_task", "vision_analyze"} {
		if !r.CanRunConcurrently(name) {
			t.Errorf("%s should opt into concurrent batches", name)
		}
	}
	for _, name := range []string{"file_write", "file_append", "file_patch", "shell_exec", "git", "todo", "image_generate", "unknown"} {
		if r.CanRunConcurrently(name) {
			t.Errorf("%s should default to sequential execution", name)
		}
	}
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	r.Register(Tool{
		Name:        "custom",
		Description: "custom tool",
		Schema:      map[string]interface{}{"type": "object"},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			return "ok", nil
		},
	})
	tool, err := r.Get("custom")
	if err != nil {
		t.Fatalf("get custom: %v", err)
	}
	if tool.Name != "custom" {
		t.Errorf("name: got %q, want custom", tool.Name)
	}
}

func TestRegistryExecuteEnforcesRequiredArguments(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	r.Clear()
	called := false
	r.Register(Tool{
		Name: "required_test",
		Schema: map[string]interface{}{
			"type":     "object",
			"required": []string{"first", "second"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			called = true
			return "ok", nil
		},
	})

	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{name: "nil arguments", args: nil, want: "first, second"},
		{name: "one missing", args: map[string]interface{}{"first": "set"}, want: "second"},
		{name: "null is missing", args: map[string]interface{}{"first": "set", "second": nil}, want: "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			_, err := r.Execute(context.Background(), "required_test", tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute error = %v, want missing %s", err, tc.want)
			}
			if called {
				t.Fatal("executor ran with missing required arguments")
			}
		})
	}

	out, err := r.Execute(context.Background(), "required_test", map[string]interface{}{
		"first":  "set",
		"second": "set",
	})
	if err != nil || out != "ok" || !called {
		t.Fatalf("valid Execute = %q, %v, called=%v", out, err, called)
	}
}

func TestTodoSchemaAdvertisesItemsContract(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	tool, err := r.Get("todo")
	if err != nil {
		t.Fatal(err)
	}
	properties, ok := tool.Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("todo properties = %#v", tool.Schema["properties"])
	}
	action, ok := properties["action"].(map[string]interface{})
	if !ok {
		t.Fatalf("todo action schema = %#v", properties["action"])
	}
	wantActions := []string{"create", "update", "complete", "cancel", "list"}
	if got := action["enum"]; !reflect.DeepEqual(got, wantActions) {
		t.Fatalf("todo action enum = %#v, want %#v", got, wantActions)
	}

	items, ok := properties["items"].(map[string]interface{})
	if !ok || items["type"] != "array" || items["minItems"] != 1 {
		t.Fatalf("todo items schema = %#v", properties["items"])
	}
	itemSchema, ok := items["items"].(map[string]interface{})
	if !ok || itemSchema["type"] != "object" {
		t.Fatalf("todo item schema = %#v", items["items"])
	}
	itemProperties, ok := itemSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("todo item properties = %#v", itemSchema["properties"])
	}
	for _, field := range []string{"id", "content", "status"} {
		if _, ok := itemProperties[field]; !ok {
			t.Errorf("todo item schema is missing %q", field)
		}
	}
}

func TestToolSchemasAdvertiseHandlerArguments(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	tests := []struct {
		tool       string
		properties []string
	}{
		{tool: "shell_exec", properties: []string{"command", "host", "timeout_seconds", "async"}},
		{tool: "job", properties: []string{"action", "job_id", "max_bytes", "timeout_seconds"}},
		{tool: "git", properties: []string{"action", "repo_path", "staged", "paths", "message"}},
		{tool: "search_files", properties: []string{"pattern", "path", "target", "file_glob", "limit"}},
		{tool: "web_fetch", properties: []string{"url", "max_chars"}},
	}

	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			tool, err := r.Get(tc.tool)
			if err != nil {
				t.Fatal(err)
			}
			properties, ok := tool.Schema["properties"].(map[string]interface{})
			if !ok {
				t.Fatalf("properties = %#v", tool.Schema["properties"])
			}
			if len(properties) != len(tc.properties) {
				t.Fatalf("properties = %#v, want exactly %v", properties, tc.properties)
			}
			for _, name := range tc.properties {
				if _, ok := properties[name]; !ok {
					t.Errorf("schema is missing %q", name)
				}
			}
			if additional, ok := tool.Schema["additionalProperties"].(bool); !ok || additional {
				t.Errorf("additionalProperties = %#v, want false", tool.Schema["additionalProperties"])
			}
		})
	}
}

func TestExpandedToolSchemaConstraints(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)

	gitTool, _ := r.Get("git")
	gitProperties := gitTool.Schema["properties"].(map[string]interface{})
	gitAction := gitProperties["action"].(map[string]interface{})
	if got, want := gitAction["enum"], []string{"status", "diff", "add", "commit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("git action enum = %#v, want %#v", got, want)
	}
	paths := gitProperties["paths"].(map[string]interface{})
	if paths["type"] != "array" || paths["minItems"] != 1 {
		t.Errorf("git paths schema = %#v", paths)
	}

	searchTool, _ := r.Get("search_files")
	searchProperties := searchTool.Schema["properties"].(map[string]interface{})
	target := searchProperties["target"].(map[string]interface{})
	if got, want := target["enum"], []string{"content", "files"}; !reflect.DeepEqual(got, want) {
		t.Errorf("search target enum = %#v, want %#v", got, want)
	}
	limit := searchProperties["limit"].(map[string]interface{})
	if limit["type"] != "integer" || limit["minimum"] != 1 || limit["maximum"] != 200 {
		t.Errorf("search limit schema = %#v", limit)
	}

	shellTool, _ := r.Get("shell_exec")
	shellProperties := shellTool.Schema["properties"].(map[string]interface{})
	if timeout := shellProperties["timeout_seconds"].(map[string]interface{}); timeout["type"] != "integer" || timeout["minimum"] != 1 {
		t.Errorf("shell timeout schema = %#v", timeout)
	}

	webTool, _ := r.Get("web_fetch")
	webProperties := webTool.Schema["properties"].(map[string]interface{})
	if maxChars := webProperties["max_chars"].(map[string]interface{}); maxChars["type"] != "integer" || maxChars["minimum"] != 1000 || maxChars["maximum"] != 200000 {
		t.Errorf("web max_chars schema = %#v", maxChars)
	}
}

func TestRegistryBackgroundJobLifecycle(t *testing.T) {
	workspace := t.TempDir()
	registry := NewRegistry(workspace, "", "", nil)
	ctx := WithWorkspace(WithThreadID(context.Background(), "thread-registry-job"), workspace)

	startedJSON, err := registry.Execute(ctx, "shell_exec", map[string]interface{}{
		"command": "printf registry-job",
		"async":   true,
	})
	if err != nil {
		t.Fatalf("start background job through registry: %v", err)
	}
	var started JobSnapshot
	if err := json.Unmarshal([]byte(startedJSON), &started); err != nil {
		t.Fatalf("decode background job result %q: %v", startedJSON, err)
	}

	finishedJSON, err := registry.Execute(ctx, "job", map[string]interface{}{
		"action":          "wait",
		"job_id":          started.JobID,
		"timeout_seconds": 2,
	})
	if err != nil {
		t.Fatalf("wait for background job through registry: %v", err)
	}
	var finished JobSnapshot
	if err := json.Unmarshal([]byte(finishedJSON), &finished); err != nil {
		t.Fatalf("decode completed job result %q: %v", finishedJSON, err)
	}
	if finished.State != JobCompleted || finished.StdoutTail != "registry-job" {
		t.Fatalf("completed job = %+v, want completed with registry-job output", finished)
	}
}

func TestRestrictToRemovesUnadvertisedTools(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	before := r.List()
	if len(before) < 4 {
		t.Fatalf("expected a populated registry, got %v", before)
	}
	if err := r.RestrictTo([]string{"file_read", "shell_exec"}); err != nil {
		t.Fatalf("RestrictTo: %v", err)
	}
	after := r.List()
	if len(after) != 2 {
		t.Fatalf("expected exactly the two allowed tools, got %v", after)
	}
	for _, name := range after {
		if name != "file_read" && name != "shell_exec" {
			t.Fatalf("unexpected tool %q survived restriction", name)
		}
	}
	if _, err := r.Get("web_search"); err == nil {
		t.Fatal("restricted tool still resolvable via Get")
	}
}

func TestRestrictToRejectsUnknownName(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	err := r.RestrictTo([]string{"file_read", "typo_tool"})
	if err == nil || !strings.Contains(err.Error(), "typo_tool") {
		t.Fatalf("expected fail-closed unknown-tool error, got %v", err)
	}
	// A failed restriction must leave the registry untouched.
	if got := r.List(); len(got) == 0 {
		t.Fatal("failed restriction emptied the registry")
	}
}

func TestRestrictToEmptyRemovesAll(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	if err := r.RestrictTo([]string{}); err != nil {
		t.Fatalf("RestrictTo(empty): %v", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("expected empty registry, got %v", got)
	}
}

func TestPlanModeBlocksWriteAndExecTiers(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	ctx := WithPlanMode(context.Background())

	// Read tier passes.
	if _, err := r.Execute(ctx, "file_read", map[string]interface{}{"path": "nope.txt"}); err != nil {
		t.Logf("file_read on missing file errored (expected benign): %v", err)
	}

	// Write tier is denied with the plan-mode message, not a side effect.
	_, err := r.Execute(ctx, "file_write", map[string]interface{}{"path": "x.txt", "content": "x", "expected_sha256": "absent"})
	if err == nil || !strings.Contains(err.Error(), "plan mode is active") {
		t.Fatalf("file_write under plan mode: %v", err)
	}

	// Exec tier (shell) is denied even for harmless commands — planning turns
	// must not run state-changing shells, and read-only inspection has
	// dedicated tools.
	_, err = r.Execute(ctx, "shell_exec", map[string]interface{}{"command": "echo hi"})
	if err == nil || !strings.Contains(err.Error(), "plan mode is active") {
		t.Fatalf("shell_exec under plan mode: %v", err)
	}

	// Without the context flag everything runs as before.
	if _, err := r.Execute(context.Background(), "shell_exec", map[string]interface{}{"command": "echo hi"}); err != nil {
		t.Fatalf("shell_exec outside plan mode: %v", err)
	}
}

func TestPlanModeArgumentAwareTierEscalation(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	ctx := WithPlanMode(context.Background())
	// git's base tier resolves per action: a read-tier status call passes
	// (it may still fail benignly — e.g. not a git repo — but never with the
	// plan-mode denial), while add/commit escalates to write and must be denied.
	if _, err := r.Execute(ctx, "git", map[string]interface{}{"action": "status"}); err != nil && strings.Contains(err.Error(), "plan mode is active") {
		t.Fatalf("git status under plan mode wrongly denied: %v", err)
	}
	_, err := r.Execute(ctx, "git", map[string]interface{}{"action": "add", "paths": []string{"x"}})
	if err == nil || !strings.Contains(err.Error(), "plan mode is active") {
		t.Fatalf("git add under plan mode: %v", err)
	}
}

func TestPlanModeAllowsTodoMutations(t *testing.T) {
	r := NewRegistry(t.TempDir(), "", "", nil)
	ctx := WithPlanMode(context.Background())

	// Todo state is conversation metadata, not a filesystem write, so every
	// todo action — including the write-tiered mutations — runs in plan mode.
	if _, err := r.Execute(ctx, "todo", map[string]interface{}{
		"action": "create",
		"items":  []interface{}{map[string]interface{}{"content": "draft the plan"}},
	}); err != nil {
		t.Fatalf("todo create under plan mode: %v", err)
	}
	if _, err := r.Execute(ctx, "todo", map[string]interface{}{
		"action": "update",
		"items":  []interface{}{map[string]interface{}{"id": "1", "status": "in_progress"}},
	}); err != nil {
		t.Fatalf("todo update under plan mode: %v", err)
	}
	out, err := r.Execute(ctx, "todo", map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatalf("todo list under plan mode: %v", err)
	}
	if !strings.Contains(out, "draft the plan") {
		t.Fatalf("todo list output missing created item: %q", out)
	}

	// Actual file writes are still denied.
	if _, err := r.Execute(ctx, "file_write", map[string]interface{}{"path": "x.txt", "content": "x", "expected_sha256": "absent"}); err == nil || !strings.Contains(err.Error(), "plan mode is active") {
		t.Fatalf("file_write under plan mode should still be denied: %v", err)
	}
}
