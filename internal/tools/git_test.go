package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGitRepo(t *testing.T, dir string) {
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = dir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "Test")
	cmd.Dir = dir
	cmd.Run()
}

func TestGitStatus(t *testing.T) {
	workspace := t.TempDir()
	initGitRepo(t, workspace)
	gt := NewGitTools(workspace)
	out, err := gt.GitStatus(context.Background(), map[string]interface{}{"repo_path": "."})
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(out, "On branch") {
		t.Errorf("unexpected status output: %s", out)
	}
}

func TestGitStatusTraversal(t *testing.T) {
	gt := NewGitTools(t.TempDir())
	_, err := gt.GitStatus(context.Background(), map[string]interface{}{"repo_path": "../outside"})
	if err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestGitDispatchResolvesRepoPathWithinWorkspaceOnce(t *testing.T) {
	workspace := t.TempDir()
	repoPath := filepath.Join(workspace, "nested", "repo")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoPath)

	out, err := GitDispatch(context.Background(), map[string]interface{}{
		"action":    "status",
		"workspace": workspace,
		"repo_path": filepath.Join("nested", "repo"),
	})
	if err != nil {
		t.Fatalf("git status in nested repository: %v", err)
	}
	if !strings.Contains(out, "On branch") {
		t.Fatalf("unexpected status output: %s", out)
	}

	r := NewRegistry(workspace, "", "", nil)
	out, err = r.Execute(context.Background(), "git", map[string]interface{}{
		"action":    "status",
		"repo_path": filepath.Join("nested", "repo"),
	})
	if err != nil {
		t.Fatalf("registry git status in nested repository: %v", err)
	}
	if !strings.Contains(out, "On branch") {
		t.Fatalf("unexpected registry status output: %s", out)
	}
}

func TestGitDispatchTrustedContextOverridesWorkspaceArgument(t *testing.T) {
	trusted := t.TempDir()
	modelSupplied := t.TempDir()
	repoPath := filepath.Join(trusted, "nested", "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoPath)

	ctx := WithWorkspace(context.Background(), trusted)
	out, err := GitDispatch(ctx, map[string]interface{}{
		"action": "status", "workspace": modelSupplied, "repo_path": filepath.Join("nested", "repo"),
	})
	if err != nil {
		t.Fatalf("git status ignored trusted workspace context: %v", err)
	}
	if !strings.Contains(out, "On branch") {
		t.Fatalf("unexpected git status output: %q", out)
	}

	registry := NewRegistry(trusted, "", "", nil)
	defer func() { _ = registry.Close(context.Background()) }()
	_, err = registry.Execute(context.Background(), "git", map[string]interface{}{
		"action": "status", "workspace": modelSupplied, "repo_path": filepath.Join("nested", "repo"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported fields: workspace") {
		t.Fatalf("registry workspace override error = %v", err)
	}
}

func TestGitAddAndCommit(t *testing.T) {
	workspace := t.TempDir()
	initGitRepo(t, workspace)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("A"), 0644)

	gt := NewGitTools(workspace)
	_, err := gt.GitAdd(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"paths":     []interface{}{"a.txt"},
	})
	if err != nil {
		t.Fatalf("git add: %v", err)
	}

	out, err := gt.GitCommit(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"message":   "initial commit",
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if !strings.Contains(out, "initial commit") && !strings.Contains(out, "1 file changed") {
		t.Errorf("unexpected commit output: %s", out)
	}
}

func TestGitAddEmptyPaths(t *testing.T) {
	workspace := t.TempDir()
	gt := NewGitTools(workspace)
	_, err := gt.GitAdd(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"paths":     []interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for empty paths")
	}
}

func TestGitCommitMissingMessage(t *testing.T) {
	workspace := t.TempDir()
	gt := NewGitTools(workspace)
	_, err := gt.GitCommit(context.Background(), map[string]interface{}{
		"repo_path": ".",
	})
	if err == nil {
		t.Fatal("expected error for missing message")
	}
}

func TestGitDiff(t *testing.T) {
	workspace := t.TempDir()
	initGitRepo(t, workspace)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("A"), 0644)
	gt := NewGitTools(workspace)
	gt.GitAdd(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"paths":     []interface{}{"a.txt"},
	})
	gt.GitCommit(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"message":   "first",
	})
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("B"), 0644)

	out, err := gt.GitDiff(context.Background(), map[string]interface{}{
		"repo_path": ".",
	})
	if err != nil {
		t.Fatalf("git diff: %v", err)
	}
	if !strings.Contains(out, "-A") || !strings.Contains(out, "+B") {
		t.Errorf("unexpected diff output: %s", out)
	}
}

func TestGitDiffStaged(t *testing.T) {
	workspace := t.TempDir()
	initGitRepo(t, workspace)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("A"), 0644)
	gt := NewGitTools(workspace)
	gt.GitAdd(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"paths":     []interface{}{"a.txt"},
	})

	out, err := gt.GitDiff(context.Background(), map[string]interface{}{
		"repo_path": ".",
		"staged":    true,
	})
	if err != nil {
		t.Fatalf("git diff staged: %v", err)
	}
	if !strings.Contains(out, "A") {
		t.Errorf("unexpected staged diff output: %s", out)
	}
}
