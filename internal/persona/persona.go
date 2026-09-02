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

// projectContextFiles defines the filenames scanned in the workspace, in
// priority order.
var projectContextFiles = []string{
	".sandbar.md",
	"AGENTS.md",
	".cursorrules",
	"CLAUDE.md",
}

// ancestorContextFiles is scanned in ancestor directories of the workspace,
// up to the git repo root. Workspace-local conventions (.sandbar.md,
// .cursorrules) deliberately stay workspace-only.
var ancestorContextFiles = []string{
	"AGENTS.md",
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

	// Environment block: where the agent is and what it is running as.
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

// loadProjectContext assembles workspace project-context sections: the
// workspace's own instruction files (all four names, priority order), plus
// AGENTS.md/CLAUDE.md from ancestor directories up to the git repo root —
// outermost first, so the closest (workspace) instructions come last and read
// as the most specific layer. Context files are the owner's own instructions
// and are trusted as-is (an injection scan was removed after its false
// positives dropped legitimate files wholesale).
func loadProjectContext(workspace string) string {
	abs := workspace
	if a, err := filepath.Abs(workspace); err == nil {
		abs = a
	}
	var sections []string
	for _, dir := range contextDirs(workspace) {
		names := ancestorContextFiles
		if dir == abs {
			names = projectContextFiles
		}
		for _, filename := range names {
			path := filepath.Join(dir, filename)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			content := string(data)
			if strings.TrimSpace(content) == "" {
				continue
			}
			header := filename
			if dir != abs {
				if rel, relErr := filepath.Rel(abs, path); relErr == nil {
					header = rel // e.g. "../../AGENTS.md"
				}
			}
			sections = append(sections, fmt.Sprintf("### %s\n\n%s", header, content))
		}
	}
	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// contextDirs returns the directories to scan for project context, outermost
// first: every ancestor of workspace up to and including the topmost ancestor
// containing .git. Outside a git repo the workspace itself is the only entry —
// the walk never leaves the project on the workspace's own.
func contextDirs(workspace string) []string {
	if workspace == "" {
		return nil
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return []string{workspace}
	}
	top := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
			top = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	if top == "" || top == abs {
		return []string{abs}
	}
	// Collect the upward chain from the workspace to the repo root, then
	// reverse it so the scan runs outermost-first.
	var chain []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		chain = append(chain, dir)
		if dir == top || dir == filepath.Dir(dir) {
			break
		}
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// NearestDirectoryInstructions finds the closest instruction file governing
// dir: AGENTS.md then CLAUDE.md, checked in dir and each ancestor, stopping
// just before stopExclusive (the workspace, whose own files are already in the
// system prompt). It never looks outside the workspace chain: a dir that is
// not stopExclusive or beneath it finds nothing. Returns the file's absolute
// path and content, or ok=false.
func NearestDirectoryInstructions(dir, stopExclusive string) (path, content string, ok bool) {
	if dir == "" || stopExclusive == "" {
		return "", "", false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", false
	}
	absStop, err := filepath.Abs(stopExclusive)
	if err != nil {
		return "", "", false
	}
	if absDir != absStop && !strings.HasPrefix(absDir, absStop+string(filepath.Separator)) {
		return "", "", false
	}
	for d := absDir; d != absStop && d != filepath.Dir(d); d = filepath.Dir(d) {
		for _, name := range ancestorContextFiles {
			p := filepath.Join(d, name)
			data, readErr := os.ReadFile(p)
			if readErr != nil || strings.TrimSpace(string(data)) == "" {
				continue
			}
			return p, string(data), true
		}
	}
	return "", "", false
}
