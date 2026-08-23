package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseDSML(t *testing.T) {
	content := "<DSML｜invoke name=\"read\">\n<DSML｜parameter name=\"path\" string=\"true\">/work/foo</DSML｜parameter>\n</DSML｜invoke>"

	tcs := parseDSML(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "file_read" {
		t.Errorf("got %q, want file_read", tcs[0].Function.Name)
	}
}

func TestParseDSMLNoPrefixVariant(t *testing.T) {
	// <｜invoke> format (no DSML prefix)
	content := "<｜invoke name=\"bash\">\n<｜parameter name=\"command\" string=\"true\">ls</｜parameter>\n</｜invoke>"

	tcs := parseDSML(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "shell_exec" {
		t.Errorf("got %q, want shell_exec", tcs[0].Function.Name)
	}
}

func TestParseDSMLMultiple(t *testing.T) {
	content := "<DSML｜invoke name=\"read\">\n<DSML｜parameter name=\"path\">/a</DSML｜parameter>\n</DSML｜invoke>\n<DSML｜invoke name=\"bash\">\n<DSML｜parameter name=\"command\">ls</DSML｜parameter>\n</DSML｜invoke>"

	tcs := parseDSML(content)
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "file_read" {
		t.Errorf("first: got %q", tcs[0].Function.Name)
	}
	if tcs[1].Function.Name != "shell_exec" {
		t.Errorf("second: got %q", tcs[1].Function.Name)
	}
}

func TestParseDSMLNoMatch(t *testing.T) {
	tcs := parseDSML("Hello, how can I help?")
	if len(tcs) != 0 {
		t.Errorf("expected 0 tool calls from plain text, got %d", len(tcs))
	}
}

func TestParseXMLToolCalls(t *testing.T) {
	content := `<function_call>{"name":"shell_exec","arguments":{"command":"ls"}}</function_call>`
	tcs := parseXMLToolCalls(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "shell_exec" {
		t.Errorf("got %q, want shell_exec", tcs[0].Function.Name)
	}
}

func TestToolNameMapping(t *testing.T) {
	tests := map[string]string{
		"read":       "file_read",
		"bash":       "shell_exec",
		"shell_exec": "shell_exec",
		"file_read":  "file_read",
		"search":     "search_files",
		"todo":       "todo",
		"todo_write": "todo",
		"unknown":    "unknown",
	}
	for in, want := range tests {
		got := resolveToolName(in)
		if got != want {
			t.Errorf("resolveToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFunctionEqualsSingle(t *testing.T) {
	// Exact format emitted by some open-weights models.
	content := "<tool_call>\n<function=file_read>\n<parameter=path>\n/some/file.go\n</parameter>\n</function>\n</tool_call>"

	tcs := parseFunctionEquals(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "file_read" {
		t.Errorf("name: got %q, want file_read", tcs[0].Function.Name)
	}
	if !strings.Contains(tcs[0].Function.Arguments, "/some/file.go") {
		t.Errorf("args: expected path, got %s", tcs[0].Function.Arguments)
	}
}

func TestParseFunctionEqualsMultiple(t *testing.T) {
	// Two consecutive calls, each wrapped in <tool_call>.
	content := "<tool_call>\n<function=file_read>\n<parameter=path>\n/a.go\n</parameter>\n</function>\n</tool_call>\n<tool_call>\n<function=file_read>\n<parameter=path>\n/b.go\n</parameter>\n</function>\n</tool_call>"

	tcs := parseFunctionEquals(content)
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "file_read" || tcs[1].Function.Name != "file_read" {
		t.Errorf("names: %s, %s", tcs[0].Function.Name, tcs[1].Function.Name)
	}
	if !strings.Contains(tcs[0].Function.Arguments, "/a.go") {
		t.Errorf("first args: %s", tcs[0].Function.Arguments)
	}
	if !strings.Contains(tcs[1].Function.Arguments, "/b.go") {
		t.Errorf("second args: %s", tcs[1].Function.Arguments)
	}
}

func TestParseFunctionEqualsNoWrapper(t *testing.T) {
	// Without the <tool_call> wrapper.
	content := "<function=shell_exec>\n<parameter=command>\necho hello\n</parameter>\n</function>"

	tcs := parseFunctionEquals(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "shell_exec" {
		t.Errorf("got %q, want shell_exec", tcs[0].Function.Name)
	}
}

func TestParseFunctionEqualsMultiParam(t *testing.T) {
	content := "<function=file_patch>\n<parameter=path>\n/test.go\n</parameter>\n<parameter=old_string>\nfoo\n</parameter>\n<parameter=new_string>\nbar\n</parameter>\n</function>"

	tcs := parseFunctionEquals(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if !strings.Contains(tcs[0].Function.Arguments, "foo") {
		t.Errorf("expected 'foo' in args: %s", tcs[0].Function.Arguments)
	}
	if !strings.Contains(tcs[0].Function.Arguments, "bar") {
		t.Errorf("expected 'bar' in args: %s", tcs[0].Function.Arguments)
	}
}

func TestParseFunctionEqualsNoFalsePositive(t *testing.T) {
	// Plain text containing "function=" but not a real tool call.
	tcs := parseFunctionEquals("The function= keyword is reserved in Go.")
	if len(tcs) != 0 {
		t.Errorf("expected 0 false positives, got %d", len(tcs))
	}
}

func TestParseFunctionEqualsUniqueIDs(t *testing.T) {
	content := "<function=file_read>\n<parameter=path>\n/a\n</parameter>\n</function>\n<function=file_read>\n<parameter=path>\n/b\n</parameter>\n</function>"

	tcs := parseFunctionEquals(content)
	if len(tcs) != 2 {
		t.Fatalf("expected 2, got %d", len(tcs))
	}
	if tcs[0].ID == tcs[1].ID {
		t.Errorf("IDs must be unique: both %s", tcs[0].ID)
	}
}

func TestParseFunctionCallsInvokeObservedHaikuShape(t *testing.T) {
	content := `<function_calls>
<invoke name="bash">
<parameter name="tool">bash</parameter>
<parameter name="arguments">
<parameter name="command">cat > /workspace/RELEASES.md << 'EOF'
# Release Plan
EOF
</parameter>
</invoke>
</function_calls>
<function_calls>
<invoke name="bash">
<parameter name="tool">bash</parameter>
<parameter name="arguments">
<parameter name="command">cat /workspace/RELEASES.md</parameter>
</invoke>
</function_calls>`

	tcs := parseFunctionCallsInvoke(content)
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	for i, tc := range tcs {
		if tc.Type != "function" || tc.Function.Name != "shell_exec" {
			t.Fatalf("call %d: got type=%q name=%q", i, tc.Type, tc.Function.Name)
		}
		var args map[string]string
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			t.Fatalf("call %d arguments are not JSON: %v", i, err)
		}
		if len(args) != 1 || args["command"] == "" {
			t.Fatalf("call %d arguments leaked wrapper metadata: %#v", i, args)
		}
	}
	if !strings.Contains(tcs[0].Function.Arguments, "# Release Plan") {
		t.Fatalf("first command was truncated: %s", tcs[0].Function.Arguments)
	}
	if tcs[0].ID == tcs[1].ID {
		t.Fatalf("tool call IDs must be unique: %q", tcs[0].ID)
	}
}

func TestParseFunctionCallsInvokeDecodesAndRestrictsArguments(t *testing.T) {
	content := `<function_calls>
<invoke name="bash">
<parameter name="tool">bash</parameter>
<parameter name="arguments">
<parameter name="workspace">/outside</parameter>
<parameter name="command">printf &quot;%s&quot; &apos;A &amp; B&apos; &gt; result.txt</parameter>
</invoke>
</function_calls>`

	tcs := parseTextToolCalls(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "shell_exec" {
		t.Fatalf("got %q, want shell_exec", tcs[0].Function.Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tcs[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v", err)
	}
	want := `printf "%s" 'A & B' > result.txt`
	if len(args) != 1 || args["command"] != want {
		t.Fatalf("arguments: got %#v, want command %q only", args, want)
	}
}

func TestParseFunctionCallsInvokeRejectsUnknownOrMismatchedTool(t *testing.T) {
	tests := []string{
		`<function_calls><invoke name="not_registered"><parameter name="tool">not_registered</parameter><parameter name="arguments"><parameter name="command">echo unsafe</parameter></invoke></function_calls>`,
		`<function_calls><invoke name="bash"><parameter name="tool">read</parameter><parameter name="arguments"><parameter name="command">echo unsafe</parameter></invoke></function_calls>`,
		`<function_calls><invoke name="bash"><parameter name="arguments"><parameter name="command">echo unsafe</parameter></invoke></function_calls>`,
	}
	for _, content := range tests {
		if got := parseFunctionCallsInvoke(content); len(got) != 0 {
			t.Fatalf("unsafe wrapper parsed as tool call: %+v", got)
		}
	}
}

func TestParseFunctionCallsInvokeTodoUsesRegistryNameAndKeepsItems(t *testing.T) {
	content := `<function_calls>
<invoke name="todo_write">
<parameter name="tool">todo_write</parameter>
<parameter name="arguments">
<parameter name="action">create</parameter>
<parameter name="items">[{"content":"inspect the regression"}]</parameter>
</invoke>
</function_calls>`

	tcs := parseFunctionCallsInvoke(content)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 todo call, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "todo" {
		t.Fatalf("todo alias resolved to %q, want registry name todo", tcs[0].Function.Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tcs[0].Function.Arguments), &args); err != nil {
		t.Fatalf("todo arguments are not JSON: %v", err)
	}
	if args["action"] != "create" || args["items"] != `[{"content":"inspect the regression"}]` {
		t.Fatalf("todo arguments = %#v", args)
	}
}

func TestParseFunctionCallsInvokeKeepsModernToolArguments(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	content := `<function_calls>
<invoke name="file_write">
<parameter name="tool">file_write</parameter>
<parameter name="arguments">
<parameter name="path">notes.txt</parameter>
<parameter name="content">updated</parameter>
<parameter name="expected_sha256">` + digest + `</parameter>
</invoke>
<invoke name="shell_exec">
<parameter name="tool">shell_exec</parameter>
<parameter name="arguments">
<parameter name="command">sleep 1</parameter>
<parameter name="timeout_seconds">1.5</parameter>
<parameter name="async">true</parameter>
</invoke>
<invoke name="job">
<parameter name="tool">job</parameter>
<parameter name="arguments">
<parameter name="action">tail</parameter>
<parameter name="job_id">job_123</parameter>
<parameter name="max_bytes">2048</parameter>
</invoke>
</function_calls>`

	calls := parseFunctionCallsInvoke(content)
	if len(calls) != 3 {
		t.Fatalf("parsed calls = %d, want 3", len(calls))
	}
	var writeArgs map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &writeArgs); err != nil {
		t.Fatalf("decode file_write arguments: %v", err)
	}
	if writeArgs["expected_sha256"] != digest {
		t.Fatalf("file_write expected_sha256 = %#v", writeArgs["expected_sha256"])
	}
	var shellArgs map[string]interface{}
	if err := json.Unmarshal([]byte(calls[1].Function.Arguments), &shellArgs); err != nil {
		t.Fatalf("decode shell arguments: %v", err)
	}
	if shellArgs["async"] != true || shellArgs["timeout_seconds"] != 1.5 {
		t.Fatalf("typed shell arguments = %#v", shellArgs)
	}
	var jobArgs map[string]interface{}
	if err := json.Unmarshal([]byte(calls[2].Function.Arguments), &jobArgs); err != nil {
		t.Fatalf("decode job arguments: %v", err)
	}
	if jobArgs["max_bytes"] != float64(2048) || jobArgs["job_id"] != "job_123" {
		t.Fatalf("typed job arguments = %#v", jobArgs)
	}
}
