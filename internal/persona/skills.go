package persona

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skills are folders of on-demand instructions discovered across scopes:
//
//	.sandbar/skills/<name>/SKILL.md   workspace (native)
//	.claude/skills/<name>/SKILL.md    workspace (interop)
//	.agents/skills/<name>/SKILL.md    workspace (interop)
//	~/.config/sandbar/skills/<name>/  user (native)
//	~/.claude/skills/<name>/          user (interop)
//	~/.agents/skills/<name>/          user (interop)
//
// SKILL.md starts with a small frontmatter-ish header whose `description:`
// line is the one-line summary advertised in the system prompt. The model is
// told the skill exists and reads the full file with file_read only when a
// task matches — skills never bloat the prompt with their bodies, and adding
// or editing one requires no restart or config change. A skill is named
// after its directory in kebab case; duplicate names across scopes are
// de-duplicated with the earlier (higher-precedence) scope winning.
const (
	skillsMasterFile  = "SKILL.md"
	maxSkills         = 20
	maxSkillDescRunes = 200
)

type Skill struct {
	Name        string
	Description string
	Path        string // path to SKILL.md: workspace-relative for project scopes, absolute for user scopes
}

// DiscoverSkills lists skills from the workspace and user scopes in stable
// name order. Missing or unreadable skills directories are not an error —
// an empty list means no skills section is rendered. The list is capped at
// maxSkills in discovery order (earlier scopes win); the cap is a
// prompt-budget guard, and hitting it is a workspace-authoring smell rather
// than a runtime condition to handle gracefully.
func DiscoverSkills(workspace string) []Skill {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	// Each scope pairs the directory to scan with the base of the advertised
	// path: workspace-relative for project scopes, absolute for user scopes.
	scopes := []struct{ dir, root string }{
		{filepath.Join(workspace, ".sandbar", "skills"), filepath.Join(".sandbar", "skills")},
		{filepath.Join(workspace, ".claude", "skills"), filepath.Join(".claude", "skills")},
		{filepath.Join(workspace, ".agents", "skills"), filepath.Join(".agents", "skills")},
		{filepath.Join(UserConfigDir(), "skills"), ""},
		{filepath.Join(home, ".claude", "skills"), ""},
		{filepath.Join(home, ".agents", "skills"), ""},
	}
	seen := make(map[string]bool)
	var skills []Skill
	for _, scope := range scopes {
		entries, err := os.ReadDir(scope.dir)
		if err != nil {
			continue
		}
		root := scope.root
		if root == "" {
			root = scope.dir // user scopes advertise absolute paths
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			master := filepath.Join(scope.dir, entry.Name(), skillsMasterFile)
			data, err := os.ReadFile(master)
			if err != nil {
				continue
			}
			name := kebab(entry.Name())
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			skills = append(skills, Skill{
				Name:        name,
				Description: skillDescription(string(data)),
				Path:        filepath.ToSlash(filepath.Join(root, entry.Name(), skillsMasterFile)),
			})
			if len(skills) >= maxSkills {
				sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
				return skills
			}
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills
}

// kebab lowercases name and collapses runs of anything outside [a-z0-9]
// into single dashes, trimmed from both ends. Skill directories are
// advertised and de-duplicated under this kebab name regardless of how the
// folder is spelled on disk.
func kebab(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// skillDescription pulls the `description:` value from the SKILL.md header.
// The first match wins; text beyond maxSkillDescRunes is truncated so one
// verbose skill cannot dominate the advertised list. Files without a
// description line are still listed with an empty summary.
func skillDescription(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "description:") {
			continue
		}
		desc := strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
		desc = strings.Trim(desc, "\"'")
		runes := []rune(desc)
		if len(runes) > maxSkillDescRunes {
			return string(runes[:maxSkillDescRunes]) + "…"
		}
		return desc
	}
	return ""
}

// renderSkillsSection formats the advertised skills list. It returns an empty
// string when there is nothing to advertise.
func renderSkillsSection(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skills\n")
	b.WriteString("Specialized instruction packs from this workspace or your user config. If a skill matches the current task, read its SKILL.md with file_read before proceeding; follow it unless it conflicts with the instructions above.\n")
	for _, skill := range skills {
		if skill.Description == "" {
			fmt.Fprintf(&b, "- %s (%s)\n", skill.Name, skill.Path)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s (%s)\n", skill.Name, skill.Description, skill.Path)
	}
	return strings.TrimRight(b.String(), "\n")
}
