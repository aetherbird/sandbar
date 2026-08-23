package persona

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Persona builds the system prompt for the agent.
type Persona struct {
	Name         string
	SystemPrompt string
}

// projectContextFiles defines the filenames to scan for, in priority order.
var projectContextFiles = []string{
	".sandbar.md",
	"AGENTS.md",
	".cursorrules",
	"CLAUDE.md",
}

// BuildSystemPrompt returns the final system prompt with dynamic injection:
// the base persona, an environment block (workspace/git/platform/date/model),
// and workspace project context. Every model receives the identical prompt;
// there is no per-family guidance.
//
// The static identity/tool/working-style block stays first and unchanged across
// turns so provider prompt-caching of the prefix keeps working; the injected
// blocks below it vary with the workspace/model.
func (p *Persona) BuildSystemPrompt(workspace, model string) string {
	var b strings.Builder
	b.WriteString(p.SystemPrompt)

	// Environment block — mirrors what opencode and codex inject at runtime so
	// the model can reason about where it is and what it is running as.
	b.WriteString("\n\n# Environment\n")
	if workspace != "" {
		b.WriteString(fmt.Sprintf("- Working directory: %s\n", workspace))
	}
	b.WriteString(fmt.Sprintf("- Is directory a git repo: %s\n", yesNo(isGitRepo(workspace))))
	b.WriteString(fmt.Sprintf("- Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH))
	// Day resolution (UTC) keeps the environment block stable across turns so
	// provider prefix caching of the system prompt keeps working.
	b.WriteString(fmt.Sprintf("- Today's date: %s\n", time.Now().UTC().Format("2006-01-02")))
	if model != "" {
		b.WriteString(fmt.Sprintf("- Model: %s\n", model))
	}

	if projectCtx := loadProjectContext(workspace); projectCtx != "" {
		b.WriteString("\n\n## Project Context\n\n" + projectCtx)
	}

	if skills := DiscoverSkills(workspace); len(skills) > 0 {
		b.WriteString("\n\n" + renderSkillsSection(skills))
	}
	return b.String()
}

// isGitRepo reports whether workspace (or any ancestor directory) contains a
// .git entry. A non-existent path is reported as not a git repo.
func isGitRepo(workspace string) bool {
	if workspace == "" {
		return false
	}
	dir := workspace
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// loadProjectContext scans the workspace for known project instruction files
// and returns their concatenated contents. Files are loaded in priority order;
// if multiple exist, all are included with headers. Context files are the
// owner's own instructions and are trusted as-is (a heuristic injection scan
// was removed 2026-08-14 on owner decision — its false positives dropped
// legitimate files wholesale).
func loadProjectContext(workspace string) string {
	var sections []string
	for _, filename := range projectContextFiles {
		path := filepath.Join(workspace, filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		if strings.TrimSpace(content) == "" {
			continue
		}
		sections = append(sections, fmt.Sprintf("### %s\n\n%s", filename, content))
	}
	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}
