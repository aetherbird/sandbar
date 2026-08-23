package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireRipgrep(t *testing.T) string {
	t.Helper()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	return rgPath
}

func writeSearchFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFilesUsesWorkspaceAndContentOptions(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "first.go"), "package first\n// unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, "second.go"), "package second\n// unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, "excluded.txt"), "unique-marker\n")

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern":   "unique-marker",
		"file_glob": "*.go",
		"limit":     float64(1),
	}, workspace, requireRipgrep(t))
	if err != nil {
		t.Fatalf("search files: %v", err)
	}
	if !strings.Contains(out, "Search results (1 shown; more available)") {
		t.Fatalf("limit was not applied: %s", out)
	}
	if strings.Contains(out, "excluded.txt") {
		t.Fatalf("file_glob was not applied: %s", out)
	}
}

func TestSearchFilesTreatsFilenamePatternLiterally(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "nested", "literal[report].md"), "report")
	writeSearchFixture(t, filepath.Join(workspace, "nested", "literalr.md"), "report")

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern": "[REPORT]",
		"target":  "files",
	}, workspace, requireRipgrep(t))
	if err != nil {
		t.Fatalf("search filenames: %v", err)
	}
	if !strings.Contains(out, filepath.Join("nested", "literal[report].md")) {
		t.Fatalf("expected literal, case-insensitive filename match: %s", out)
	}
	if strings.Contains(out, "literalr.md") {
		t.Fatalf("filename pattern was interpreted as a glob: %s", out)
	}
}

func TestSearchFilesReturnsMultipleMatchesPerFileAndStopsAtLimit(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "many.txt"), strings.Repeat("bounded-marker\n", 20))

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern": "bounded-marker",
		"limit":   float64(2),
	}, workspace, requireRipgrep(t))
	if err != nil {
		t.Fatalf("search content: %v", err)
	}
	if !strings.Contains(out, "Search results (2 shown; more available)") {
		t.Fatalf("search did not report a bounded result set: %s", out)
	}
	if got := strings.Count(out, "bounded-marker"); got != 2 {
		t.Fatalf("returned %d matches, want 2: %s", got, out)
	}
	if !strings.Contains(out, "many.txt:1:") || !strings.Contains(out, "many.txt:2:") {
		t.Fatalf("search did not return multiple matches from one file: %s", out)
	}
}

func TestSearchFilesFindsFilenamesWithRipgrep(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "nested", "special-report.md"), "report")
	writeSearchFixture(t, filepath.Join(workspace, "nested", "ordinary.md"), "special-report")

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern": "SPECIAL-REPORT",
		"target":  "files",
	}, workspace, requireRipgrep(t))
	if err != nil {
		t.Fatalf("search filenames: %v", err)
	}
	if !strings.Contains(out, filepath.Join("nested", "special-report.md")) {
		t.Fatalf("expected case-insensitive filename match: %s", out)
	}
	if strings.Contains(out, "ordinary.md") {
		t.Fatalf("filename search unexpectedly searched contents: %s", out)
	}
}

func TestRegistrySearchFilesDefaultsToConfiguredWorkspace(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "workspace.txt"), "registry-workspace-marker")
	r := NewRegistry(workspace, "", "", nil)

	out, err := r.Execute(context.Background(), "search_files", map[string]interface{}{
		"pattern": "registry-workspace-marker",
	})
	if err != nil {
		t.Fatalf("registry search: %v", err)
	}
	if !strings.Contains(out, "workspace.txt") {
		t.Fatalf("registry search did not use configured workspace: %s", out)
	}
}

func TestSearchFilesReportsMissingRipgrep(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "fallback.txt"), "needle-line\n")

	out, err := SearchFiles(context.Background(), map[string]interface{}{
		"pattern": "needle-line",
		"path":    workspace,
	})
	if err != nil {
		t.Fatalf("search without ripgrep: %v", err)
	}
	if !strings.Contains(out, "fallback.txt:1:needle-line") {
		t.Fatalf("pure-Go fallback result missing: %s", out)
	}
}

// requireNoRipgrep forces the pure-Go fallback by pointing rgExecutable at a
// nonexistent path (LookPath is only consulted for the empty param).
func requireNoRipgrep(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "rg-definitely-not-here")
}

func TestSearchFilesGoFallbackMatchesRipgrepFormat(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "first.go"), "package first\n// unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, "second.go"), "package second\n// unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, "excluded.txt"), "unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, ".hidden.go"), "unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, "node_modules", "junk.go"), "unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, ".git", "config"), "unique-marker\n")
	writeSearchFixture(t, filepath.Join(workspace, "binary.bin"), "unique-marker\x00binary\n")

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern":   "unique-marker",
		"file_glob": "*.go",
		"limit":     float64(10),
	}, workspace, requireNoRipgrep(t))
	if err != nil {
		t.Fatalf("fallback content search: %v", err)
	}
	for _, want := range []string{
		"Search results (2 of 2 shown):",
		"first.go:2:// unique-marker",
		"second.go:2:// unique-marker",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("fallback output missing %q: %s", want, out)
		}
	}
	for _, banned := range []string{"excluded.txt", ".hidden.go", "node_modules", ".git", "binary.bin"} {
		if strings.Contains(out, banned) {
			t.Fatalf("fallback searched excluded path %q: %s", banned, out)
		}
	}
}

func TestSearchFilesGoFallbackStopsAtLimit(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "many.txt"), strings.Repeat("bounded-marker\n", 20))

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern": "bounded-marker",
		"limit":   float64(2),
	}, workspace, requireNoRipgrep(t))
	if err != nil {
		t.Fatalf("fallback search: %v", err)
	}
	if !strings.Contains(out, "Search results (2 shown; more available)") {
		t.Fatalf("fallback limit was not applied: %s", out)
	}
	if got := strings.Count(out, "bounded-marker"); got != 2 {
		t.Fatalf("returned %d matches, want 2: %s", got, out)
	}
}

func TestSearchFilesGoFallbackFindsFilenames(t *testing.T) {
	workspace := t.TempDir()
	writeSearchFixture(t, filepath.Join(workspace, "nested", "literal[report].md"), "report")
	writeSearchFixture(t, filepath.Join(workspace, "nested", "literalr.md"), "report")

	out, err := searchFiles(context.Background(), map[string]interface{}{
		"pattern": "[REPORT]",
		"target":  "files",
	}, workspace, requireNoRipgrep(t))
	if err != nil {
		t.Fatalf("fallback filename search: %v", err)
	}
	if !strings.Contains(out, filepath.Join("nested", "literal[report].md")) {
		t.Fatalf("expected literal, case-insensitive filename match: %s", out)
	}
	if strings.Contains(out, "literalr.md") {
		t.Fatalf("filename pattern was interpreted as a glob: %s", out)
	}
}
