package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilePatchSuccess(t *testing.T) {
	workspace := t.TempDir()
	fpath := filepath.Join(workspace, "main.go")
	oldContent := "func oldName() {\n    return 42\n}"
	os.WriteFile(fpath, []byte(oldContent), 0644)

	ft := NewFileTools(workspace)
	result, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "main.go",
		"search_string":   "func oldName() {\n    return 42\n}",
		"replace_string":  "func newName() {\n    return 43\n}",
		"expected_sha256": sha256Hex([]byte(oldContent)),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	data, _ := os.ReadFile(fpath)
	want := "func newName() {\n    return 43\n}"
	if string(data) != want {
		t.Errorf("patched content: got %q, want %q", string(data), want)
	}
	if !strings.Contains(result, "sha256: "+sha256Hex([]byte(want))) {
		t.Errorf("patch result missing new hash: %s", result)
	}
}

func TestFilePatchStaleHashDoesNotModifyFile(t *testing.T) {
	workspace := t.TempDir()
	fpath := filepath.Join(workspace, "main.go")
	content := "package main\n"
	if err := os.WriteFile(fpath, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTools(workspace)
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "main.go",
		"old_str":         "main",
		"new_str":         "other",
		"expected_sha256": sha256Hex([]byte("outdated")),
	})
	if !errors.Is(err, ErrFileConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	data, readErr := os.ReadFile(fpath)
	if readErr != nil || string(data) != content {
		t.Fatalf("stale patch changed file: %q, %v", data, readErr)
	}
	info, statErr := os.Stat(fpath)
	if statErr != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("stale patch changed mode: %v, %v", info, statErr)
	}
}

func TestFilePatchNotFound(t *testing.T) {
	workspace := t.TempDir()
	fpath := filepath.Join(workspace, "main.go")
	os.WriteFile(fpath, []byte("package main\n"), 0644)

	ft := NewFileTools(workspace)
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "main.go",
		"search_string":   "nonexistent",
		"replace_string":  "x",
		"expected_sha256": sha256Hex([]byte("package main\n")),
	})
	if err == nil {
		t.Fatal("expected error for missing search string")
	}
}

func TestFilePatchAmbiguous(t *testing.T) {
	workspace := t.TempDir()
	fpath := filepath.Join(workspace, "main.go")
	os.WriteFile(fpath, []byte("foo\nfoo\n"), 0644)

	ft := NewFileTools(workspace)
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "main.go",
		"search_string":   "foo",
		"replace_string":  "bar",
		"expected_sha256": sha256Hex([]byte("foo\nfoo\n")),
	})
	if err == nil {
		t.Fatal("expected error for ambiguous match")
	}
}

func TestFilePatchMissingPath(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"search_string":  "x",
		"replace_string": "y",
	})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestFilePatchTraversal(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "../outside.txt",
		"search_string":   "x",
		"replace_string":  "y",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestFilePatchMissingFile(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "nope.go",
		"search_string":   "x",
		"replace_string":  "y",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFilePatchRejectsNonStringReplacement(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "value.txt")
	content := "keep me"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := NewFileTools(workspace).FilePatch(context.Background(), map[string]interface{}{
		"path":            "value.txt",
		"old_str":         "keep",
		"new_str":         7,
		"expected_sha256": sha256Hex([]byte(content)),
	})
	if err == nil || !strings.Contains(err.Error(), "new_str must be a string") {
		t.Fatalf("non-string replacement error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != content {
		t.Fatalf("invalid patch changed file: %q, %v", got, readErr)
	}
}

func TestFilePatchRejectsAbsentPrecondition(t *testing.T) {
	workspace := t.TempDir()
	_, err := NewFileTools(workspace).FilePatch(context.Background(), map[string]interface{}{
		"path":            "missing.txt",
		"old_str":         "old",
		"new_str":         "new",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot use expected_sha256") {
		t.Fatalf("absent patch precondition error = %v", err)
	}
}

// TestFindClosestLineGuards pins the fuzzy-match guards (§4 item 11): the
// Levenshtein scan is skipped outright on files larger than fuzzyMatchMaxBytes
// and aborts on ctx cancellation; normal-sized files still get the hint.
func TestFindClosestLineGuards(t *testing.T) {
	// Small file: scan runs and finds the closest line.
	line, err := findClosestLine(context.Background(), "alpha\nbeta\ngamma\n", "bete")
	if err != nil || line != 2 {
		t.Errorf("small file: got line %d, err %v; want 2, nil", line, err)
	}

	// Oversized file: scan skipped, not-found reported without a hint.
	big := strings.Repeat("x", fuzzyMatchMaxBytes+1)
	line, err = findClosestLine(context.Background(), big, "absent-needle")
	if err != nil || line != 0 {
		t.Errorf("oversized file: got line %d, err %v; want 0, nil (scan skipped)", line, err)
	}

	// Cancelled ctx: scan aborts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = findClosestLine(ctx, "alpha\nbeta\n", "bete"); err == nil {
		t.Error("cancelled ctx should abort the scan")
	}
}

// TestFilePatchNotFoundLargeFileSkipsScan verifies that a miss in a file over
// fuzzyMatchMaxBytes returns the not-found error immediately (no closest-match
// line) instead of running the unbounded Levenshtein scan.
func TestFilePatchNotFoundLargeFileSkipsScan(t *testing.T) {
	workspace := t.TempDir()
	fpath := filepath.Join(workspace, "big.txt")
	content := strings.Repeat("x", fuzzyMatchMaxBytes+1)
	os.WriteFile(fpath, []byte(content), 0644)

	ft := NewFileTools(workspace)
	_, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path":            "big.txt",
		"search_string":   "absent-needle",
		"replace_string":  "y",
		"expected_sha256": sha256Hex([]byte(content)),
	})
	if err == nil {
		t.Fatal("expected error for missing search string")
	}
	if !strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "closest match around line") {
		t.Fatalf("expected immediate not-found error without closest-match hint, got %v", err)
	}
}
