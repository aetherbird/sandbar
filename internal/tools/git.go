package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type GitTools struct {
	workspace string
}

func NewGitTools(workspace string) *GitTools {
	return &GitTools{workspace: workspace}
}

func (g *GitTools) resolveRepoPath(repoPath string) (string, error) {
	if filepath.IsAbs(repoPath) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", repoPath)
	}
	cleaned := filepath.Clean(repoPath)
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("path traversal detected: %s", repoPath)
	}
	resolved := filepath.Join(g.workspace, cleaned)
	return resolved, nil
}

func (g *GitTools) runGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// GitStatus returns the git status for a repository.
func (g *GitTools) GitStatus(ctx context.Context, args map[string]interface{}) (string, error) {
	repoPath, _ := args["repo_path"].(string)
	if repoPath == "" {
		repoPath = "."
	}
	resolved, err := g.resolveRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	out, err := g.runGit(ctx, resolved, "status")
	if err != nil {
		return "", fmt.Errorf("git status: %w\n%s", err, out)
	}
	return out, nil
}

func (g *GitTools) GitDiff(ctx context.Context, args map[string]interface{}) (string, error) {
	repoPath, _ := args["repo_path"].(string)
	if repoPath == "" {
		repoPath = "."
	}
	resolved, err := g.resolveRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	gitArgs := []string{"diff"}
	if staged, _ := args["staged"].(bool); staged {
		gitArgs = append(gitArgs, "--staged")
	}
	out, err := g.runGit(ctx, resolved, gitArgs...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w\n%s", err, out)
	}
	return out, nil
}

// GitAdd stages specific paths.
func (g *GitTools) GitAdd(ctx context.Context, args map[string]interface{}) (string, error) {
	repoPath, _ := args["repo_path"].(string)
	if repoPath == "" {
		repoPath = "."
	}
	resolved, err := g.resolveRepoPath(repoPath)
	if err != nil {
		return "", err
	}

	pathsRaw, ok := args["paths"].([]interface{})
	if !ok || len(pathsRaw) == 0 {
		return "", fmt.Errorf("paths is required and must be a non-empty array")
	}
	var paths []string
	for _, p := range pathsRaw {
		s, ok := p.(string)
		if !ok || s == "" {
			return "", fmt.Errorf("paths must be non-empty strings")
		}
		paths = append(paths, s)
	}

	out, err := g.runGit(ctx, resolved, append([]string{"add"}, paths...)...)
	if err != nil {
		return "", fmt.Errorf("git add: %w\n%s", err, out)
	}
	return "Staged: " + strings.Join(paths, ", "), nil
}

func (g *GitTools) GitCommit(ctx context.Context, args map[string]interface{}) (string, error) {
	repoPath, _ := args["repo_path"].(string)
	if repoPath == "" {
		repoPath = "."
	}
	resolved, err := g.resolveRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	message, _ := args["message"].(string)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	out, err := g.runGit(ctx, resolved, "commit", "-m", message)
	if err != nil {
		return "", fmt.Errorf("git commit: %w\n%s", err, out)
	}
	return out, nil
}
