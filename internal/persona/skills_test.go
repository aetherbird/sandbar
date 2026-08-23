package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillAt writes a SKILL.md under an explicit skills directory.
func writeSkillAt(t *testing.T, dir, name, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSkill(t *testing.T, workspace, name, body string) {
	t.Helper()
	writeSkillAt(t, filepath.Join(workspace, ".sandbar", "skills"), name, body)
}

func TestDiscoverSkills(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeSkill(t, ws, "deploy", "---\ndescription: Deploy the web service to prod-host\n---\nStep 1…")
	writeSkill(t, ws, "release-notes", "description: Draft release notes from the quest log\nBody.")
	// A folder without SKILL.md is not a skill.
	if err := os.MkdirAll(filepath.Join(ws, ".sandbar", "skills", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	skills := DiscoverSkills(ws)
	if len(skills) != 2 {
		t.Fatalf("skills = %+v, want 2", skills)
	}
	if skills[0].Name != "deploy" || skills[0].Description != "Deploy the web service to prod-host" {
		t.Errorf("deploy skill = %+v", skills[0])
	}
	if skills[0].Path != ".sandbar/skills/deploy/SKILL.md" {
		t.Errorf("deploy path = %q", skills[0].Path)
	}
	if skills[1].Name != "release-notes" {
		t.Errorf("order not stable: %+v", skills)
	}
}

func TestDiscoverSkillsMissingDir(t *testing.T) {
	isolateUserScope(t)
	if skills := DiscoverSkills(t.TempDir()); skills != nil {
		t.Errorf("missing skills dir must yield no skills, got %+v", skills)
	}
}

// TestDiscoverSkillsInteropScopes proves .claude/skills and .agents/skills
// under the workspace are discovered after the native dir, with the earlier
// scope shadowing later ones on name collisions.
func TestDiscoverSkillsInteropScopes(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeSkill(t, ws, "deploy", "description: native deploy")
	writeSkillAt(t, filepath.Join(ws, ".claude", "skills"), "deploy", "description: claude deploy (should lose)")
	writeSkillAt(t, filepath.Join(ws, ".claude", "skills"), "review", "description: review a diff")
	writeSkillAt(t, filepath.Join(ws, ".agents", "skills"), "sync", "description: sync agents")

	skills := DiscoverSkills(ws)
	if len(skills) != 3 {
		t.Fatalf("skills = %+v, want 3", skills)
	}
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	if byName["deploy"].Description != "native deploy" {
		t.Errorf("deploy = %+v, want the native scope to shadow .claude", byName["deploy"])
	}
	if byName["review"].Path != ".claude/skills/review/SKILL.md" {
		t.Errorf("review path = %q", byName["review"].Path)
	}
	if byName["sync"].Path != ".agents/skills/sync/SKILL.md" {
		t.Errorf("sync path = %q", byName["sync"].Path)
	}
}

// TestDiscoverSkillsUserScope proves user-scope dirs (~/.config/sandbar,
// ~/.claude, ~/.agents) are discovered with absolute paths, and that the
// workspace shadows the user scope on name collisions.
func TestDiscoverSkillsUserScope(t *testing.T) {
	home := isolateUserScope(t)
	ws := t.TempDir()
	writeSkill(t, ws, "deploy", "description: workspace deploy")
	writeSkillAt(t, filepath.Join(home, ".config", "sandbar", "skills"), "deploy", "description: user deploy (should lose)")
	writeSkillAt(t, filepath.Join(home, ".config", "sandbar", "skills"), "journal", "description: keep a journal")
	writeSkillAt(t, filepath.Join(home, ".claude", "skills"), "frontmatter", "description: claude interop")

	skills := DiscoverSkills(ws)
	if len(skills) != 3 {
		t.Fatalf("skills = %+v, want 3", skills)
	}
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	if byName["deploy"].Description != "workspace deploy" {
		t.Errorf("deploy = %+v, want the workspace to shadow user scope", byName["deploy"])
	}
	if !filepath.IsAbs(filepath.FromSlash(byName["journal"].Path)) {
		t.Errorf("journal path = %q, want absolute user-scope path", byName["journal"].Path)
	}
	if !strings.HasSuffix(byName["frontmatter"].Path, "/.claude/skills/frontmatter/SKILL.md") {
		t.Errorf("frontmatter path = %q, want the ~/.claude interop dir", byName["frontmatter"].Path)
	}
}

// TestDiscoverSkillsKebabNames proves directory names are advertised and
// de-duplicated in kebab case, whatever their on-disk spelling.
func TestDiscoverSkillsKebabNames(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeSkillAt(t, filepath.Join(ws, ".claude", "skills"), "Code_Review", "description: review code")
	writeSkillAt(t, filepath.Join(ws, ".agents", "skills"), "code-review", "description: shadowed twin")

	skills := DiscoverSkills(ws)
	if len(skills) != 1 {
		t.Fatalf("skills = %+v, want kebab names to collide and de-duplicate", skills)
	}
	if skills[0].Name != "code-review" || skills[0].Description != "review code" {
		t.Errorf("skill = %+v, want the earlier scope under a kebab name", skills[0])
	}
}

func TestSkillDescriptionTruncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := skillDescription("description: " + long)
	if len([]rune(got)) != 201 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncation = %d runes, suffix %q", len([]rune(got)), got[len(got)-3:])
	}
}

func TestSkillDescriptionMissing(t *testing.T) {
	if got := skillDescription("no header here"); got != "" {
		t.Errorf("missing description = %q", got)
	}
}

func TestBuildSystemPromptIncludesSkillsSection(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	writeSkill(t, ws, "deploy", "description: Deploy the server\nSteps…")

	p := Persona{Name: "Sandbar", SystemPrompt: "You are a test assistant."}
	prompt := p.BuildSystemPrompt(ws, "")
	for _, want := range []string{
		"# Skills",
		"deploy: Deploy the server",
		"(.sandbar/skills/deploy/SKILL.md)",
		"read its SKILL.md with file_read",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildSystemPromptWithoutSkillsHasNoSection(t *testing.T) {
	isolateUserScope(t)
	p := Persona{Name: "Sandbar", SystemPrompt: "You are a test assistant."}
	if prompt := p.BuildSystemPrompt(t.TempDir(), ""); strings.Contains(prompt, "# Skills") {
		t.Error("skills section rendered with no skills present")
	}
}

func TestDiscoverSkillsCapsList(t *testing.T) {
	isolateUserScope(t)
	ws := t.TempDir()
	for i := 0; i < maxSkills+5; i++ {
		writeSkill(t, ws, "s"+strings.Repeat("a", i+1), "description: filler\nbody")
	}
	if skills := DiscoverSkills(ws); len(skills) != maxSkills {
		t.Errorf("cap not enforced: %d skills", len(skills))
	}
}
