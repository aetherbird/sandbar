package agent

import (
	"encoding/json"
	"strings"
	"sync"
)

// fileTracker records file operations performed by a sub-agent. It is safe
// for concurrent use so the tool-execution callback and the goroutine that
// serialises results can access it without racing.
type fileTracker struct {
	mu           sync.Mutex
	readFiles    map[string]bool // path -> seen
	writtenFiles map[string]bool // path -> seen
	commandsRun  []string        // shell commands (first 200 chars each)
	urlsFetched  []string        // web_fetch URLs
}

func newFileTracker() *fileTracker {
	return &fileTracker{
		readFiles:    make(map[string]bool),
		writtenFiles: make(map[string]bool),
	}
}

// trackFileOp records a file operation from a tool call's arguments.
// toolName is the tool that was called; args holds the parsed arguments.
func (ft *fileTracker) trackFileOp(toolName string, args map[string]interface{}) {
	path, _ := args["path"].(string)
	if path == "" {
		return
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()

	switch toolName {
	case "file_read", "search_files", "file_patch":
		// file_patch reads the file before applying the edit.
		ft.readFiles[path] = true
	case "file_write", "file_append":
		ft.writtenFiles[path] = true
	}
}

// trackShellCmd records a shell command (truncated to 200 chars).
func (ft *fileTracker) trackShellCmd(cmd string) {
	if cmd == "" {
		return
	}
	if len(cmd) > 200 {
		cmd = cmd[:200] + "..."
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.commandsRun = append(ft.commandsRun, cmd)
}

// trackURL records a web_fetch URL.
func (ft *fileTracker) trackURL(url string) {
	if url == "" {
		return
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.urlsFetched = append(ft.urlsFetched, url)
}

// Manifest returns a human-readable summary of all tracked file activity.
// It is model-visible and designed to be appended to the subagent's result.
func (ft *fileTracker) Manifest() string {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	if len(ft.readFiles) == 0 && len(ft.writtenFiles) == 0 && len(ft.commandsRun) == 0 && len(ft.urlsFetched) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n--- Subagent File Activity ---\n")

	if len(ft.readFiles) > 0 {
		b.WriteString("Files read:\n")
		for f := range ft.readFiles {
			b.WriteString("  - " + f + "\n")
		}
	}
	if len(ft.writtenFiles) > 0 {
		b.WriteString("Files modified:\n")
		for f := range ft.writtenFiles {
			b.WriteString("  - " + f + "\n")
		}
	}
	if len(ft.commandsRun) > 0 {
		b.WriteString("Commands run:\n")
		for _, c := range ft.commandsRun {
			b.WriteString("  - " + c + "\n")
		}
	}
	if len(ft.urlsFetched) > 0 {
		b.WriteString("URLs fetched:\n")
		for _, u := range ft.urlsFetched {
			b.WriteString("  - " + u + "\n")
		}
	}
	return b.String()
}

// FilesRead returns a JSON array of read file paths.
func (ft *fileTracker) FilesRead() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	out := make([]string, 0, len(ft.readFiles))
	for f := range ft.readFiles {
		out = append(out, f)
	}
	return out
}

// FilesWritten returns a JSON array of written file paths.
func (ft *fileTracker) FilesWritten() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	out := make([]string, 0, len(ft.writtenFiles))
	for f := range ft.writtenFiles {
		out = append(out, f)
	}
	return out
}

// CommandsRun returns the list of shell commands that were run.
func (ft *fileTracker) CommandsRun() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	out := make([]string, len(ft.commandsRun))
	copy(out, ft.commandsRun)
	return out
}

// ── Prompt-injection scanning ────────────────────────────────────────────────

// injectionPatterns are substrings that indicate a possible prompt-injection
// attempt when they appear at the start of a line in a subagent's output.
// If any of these patterns is found at the start of a line, that line is
// escaped to prevent the parent model from interpreting it as a role
// assignment, system instruction, or tool definition.
var injectionLinePrefixes = []string{
	"Human:",
	"Assistant:",
	"System:",
	"User:",
	"<system>",
	"<system-reminder>",
	"<system_prompt>",
	"<function=",
	"<tool_call>",
	"<tool_result>",
	"<thinking>",
	"```system",
	"```user",
	"```assistant",
	"```tool",
}

// sanitizeSubagentOutput scans a subagent's result text for lines that could
// be interpreted by the parent model as conversation-role injections, system
// prompt overrides, or tool call definitions. Suspect lines are prefixed with
// a visible [escaped] marker so the parent model sees them as data from the
// subagent rather than as instructions.
func sanitizeSubagentOutput(result string) string {
	if result == "" {
		return result
	}

	lines := strings.Split(result, "\n")
	var clean []string
	hadInjection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			clean = append(clean, line)
			continue
		}

		needsEscape := false
		for _, prefix := range injectionLinePrefixes {
			if strings.HasPrefix(trimmed, prefix) || strings.HasPrefix(trimmed, strings.ToLower(prefix)) {
				needsEscape = true
				break
			}
		}

		if needsEscape {
			hadInjection = true
			// Escape the line by prefixing with a visible marker.
			clean = append(clean, "[escaped] "+line)
		} else {
			clean = append(clean, line)
		}
	}

	if !hadInjection {
		return result
	}

	result = strings.Join(clean, "\n")
	result = "[Injection-safe: potentially unsafe lines in subagent output have been escaped.]\n" + result
	return result
}

// marshalStringsJSON is a small helper that serialises a []string to a JSON
// array literal. We hand-roll a tiny one to keep the dependency on encoding/json
// optional (we already import it elsewhere, so it is not a new import).
func marshalStringsJSON(src []string) string {
	if len(src) == 0 {
		return "[]"
	}
	data, err := json.Marshal(src)
	if err != nil {
		return "[]"
	}
	return string(data)
}
