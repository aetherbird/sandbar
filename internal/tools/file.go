package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxBytes  = 16 * 1024
	absoluteMaxBytes = 128 * 1024
)

// FileTools provides jailed file operations.
type FileTools struct {
	workspace  string
	subagents  SubagentStore // optional: resolves agent:// subagent transcripts
}

// NewFileTools creates file tools jailed to workspace.
func NewFileTools(workspace string) *FileTools {
	return &FileTools{workspace: workspace}
}

// SetSubagentStore enables agent:// transcript reads. Agent.New installs the
// SQLite store here; without it agent:// reports the missing wiring.
func (f *FileTools) SetSubagentStore(store SubagentStore) {
	f.subagents = store
}

// resolveWorkspace returns the per-request workspace carried by ctx, falling
// back to the workspace captured at construction.
func (f *FileTools) resolveWorkspace(ctx context.Context) string {
	workspace := f.workspace
	if contextual := WorkspaceFromContext(ctx); contextual != "" {
		workspace = contextual
	}
	return workspace
}

func (f *FileTools) validatePath(ctx context.Context, raw string) (string, error) {
	cleaned := filepath.Clean(raw)
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("path traversal detected: %s", raw)
	}

	// Absolute paths: use directly (full filesystem access).
	if filepath.IsAbs(cleaned) {
		return cleaned, nil
	}

	workspace := f.resolveWorkspace(ctx)

	// Relative paths: resolve within workspace.
	resolved := filepath.Join(workspace, cleaned)
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}

	// Ensure resolved path is within workspace.
	if !strings.HasPrefix(absResolved+string(filepath.Separator), absWorkspace+string(filepath.Separator)) &&
		absResolved != absWorkspace {
		return "", fmt.Errorf("path escapes workspace: %s", raw)
	}

	return absResolved, nil
}

// FileRead reads a file and includes the full-content SHA-256 in its output,
// including when the displayed content is truncated or suppressed as binary.
// Text content is stamped in hashline format: each line is prefixed with an
// 8-hex content hash ("hash│content"), which file_patch accepts as anchors
// when old_str is hashline-formatted.
func (f *FileTools) FileRead(ctx context.Context, args map[string]interface{}) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}

	// URL-like paths (pr://, issue:// via gh; agent:// from the store) are
	// resolved before the workspace jail — they are not files.
	if out, handled, err := readScheme(ctx, f.subagents, path); handled {
		return out, err
	}

	resolved, err := f.validatePath(ctx, path)
	if err != nil {
		return "", err
	}

	maxBytes := defaultMaxBytes
	if v, ok := args["max_bytes"]; ok {
		if n, ok := v.(float64); ok {
			maxBytes = int(n)
		} else if n, ok := v.(int); ok {
			maxBytes = n
		}
	}
	if maxBytes <= 0 {
		return "", fmt.Errorf("max_bytes must be greater than zero")
	}
	if maxBytes > absoluteMaxBytes {
		maxBytes = absoluteMaxBytes
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	hash := sha256Hex(data)

	// Binary / non-UTF-8 detection.
	if !utf8.Valid(data) {
		return fmt.Sprintf("[sha256: %s]\n[binary output suppressed: %d bytes]", hash, len(data)), nil
	}

	if len(data) > maxBytes {
		truncated := data[:maxBytes]
		// Ensure we don't cut a multi-byte UTF-8 rune.
		for !utf8.Valid(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
		return fmt.Sprintf("[sha256: %s]\n", hash) + formatHashlineOutput(string(truncated)) + fmt.Sprintf("\n[...truncated %d bytes; full output not shown to model...]", len(data)-len(truncated)), nil
	}

	return fmt.Sprintf("[sha256: %s]\n", hash) + formatHashlineOutput(string(data)), nil
}

// FileWrite atomically writes content when expected_sha256 still matches. Use
// ExpectedFileAbsent as the precondition when creating a new file.
func (f *FileTools) FileWrite(ctx context.Context, args map[string]interface{}) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}
	content, ok := args["content"].(string)
	if !ok {
		return "", fmt.Errorf("content must be a string")
	}
	expected, err := expectedSHA256(args)
	if err != nil {
		return "", err
	}

	resolved, err := f.validatePath(ctx, path)
	if err != nil {
		return "", err
	}

	before, newHash, err := mutateFile(ctx, resolved, path, expected, true, func(fileSnapshot) ([]byte, error) {
		return []byte(content), nil
	})
	if err != nil {
		return "", err
	}
	return mutationResult("wrote", path, string(before.content), content, newHash), nil
}

// FileAppend performs an atomic compare-and-swap read-modify-write.
func (f *FileTools) FileAppend(ctx context.Context, args map[string]interface{}) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}
	content, ok := args["content"].(string)
	if !ok {
		return "", fmt.Errorf("content must be a string")
	}
	expected, err := expectedSHA256(args)
	if err != nil {
		return "", err
	}

	resolved, err := f.validatePath(ctx, path)
	if err != nil {
		return "", err
	}

	var result string
	before, newHash, err := mutateFile(ctx, resolved, path, expected, true, func(snapshot fileSnapshot) ([]byte, error) {
		result = string(snapshot.content) + content
		return []byte(result), nil
	})
	if err != nil {
		return "", err
	}
	return mutationResult("appended", path, string(before.content), result, newHash), nil
}
