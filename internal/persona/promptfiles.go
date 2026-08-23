package persona

import (
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// Prompt-file layering: SYSTEM.md / APPEND_SYSTEM.md / TITLE_SYSTEM.md.
//
// Discovery order per file: project scope first (config bases under the
// working directory), then user scope (sandbar's config dir, then the
// interop harness dirs). Within a scope the first config base holding the
// file wins. Discovery never walks ancestors — launch from the directory
// that owns the files, or put them in a user-level dir.
//
// SYSTEM.md replaces the base persona instructions only; the rest of the
// prompt assembly (environment block, project context, skills) still happens
// around it. APPEND_SYSTEM.md is appended after everything else.
// TITLE_SYSTEM.md is a title template rendered over the first user message —
// titles are generated locally, never via a model call.

// configBasesProject are the per-harness config directories checked under
// the working directory, in precedence order.
var configBasesProject = []string{".sandbar", ".claude", ".codex", ".agents"}

// configBasesUser are the per-harness config directories checked under the
// user's home (sandbar's native dir first, then interop harnesses).
func configBasesUser(configDir string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return []string{
		configDir,                      // native: ~/.config/sandbar
		filepath.Join(home, ".claude"), // interop
		filepath.Join(home, ".codex"),  // interop
		filepath.Join(home, ".agents"), // interop
	}
}

// UserConfigDir returns sandbar's user configuration directory
// ($XDG_CONFIG_HOME/sandbar, else ~/.config/sandbar), mirroring the search
// path config/resolve.go uses for config.yaml.
func UserConfigDir() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "sandbar")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config/sandbar"
	}
	return filepath.Join(home, ".config", "sandbar")
}

// PromptFileSet is the result of prompt-file discovery. An empty field means
// that file was not found in any scope.
type PromptFileSet struct {
	System string // SYSTEM.md content
	Append string // APPEND_SYSTEM.md content
	Title  string // TITLE_SYSTEM.md content
}

// promptVars are the template variables available in SYSTEM.md and
// APPEND_SYSTEM.md. They are exposed to templates as dot-less functions
// ({{cwd}}, {{date}}), the Handlebars-style surface the files use.
type promptVars struct {
	CWD  string
	Date string
}

// titleVars are the template variables available in TITLE_SYSTEM.md.
type titleVars struct {
	Message   string // the full first user message
	FirstLine string // its first line
	CWD       string
	Date      string
}

// promptFuncs exposes the dot-less variable names ({{cwd}}, {{date}}) as
// template functions.
func promptFuncs(vars promptVars) template.FuncMap {
	return template.FuncMap{
		"cwd":  func() string { return vars.CWD },
		"date": func() string { return vars.Date },
	}
}

// titleFuncs exposes the dot-less variable names ({{cwd}}, {{date}},
// {{message}}, {{firstLine}}) as template functions.
func titleFuncs(vars titleVars) template.FuncMap {
	return template.FuncMap{
		"cwd":       func() string { return vars.CWD },
		"date":      func() string { return vars.Date },
		"message":   func() string { return vars.Message },
		"firstLine": func() string { return vars.FirstLine },
	}
}

// DiscoverPromptFiles finds SYSTEM.md, APPEND_SYSTEM.md, and
// TITLE_SYSTEM.md for the given working directory and sandbar config dir,
// honoring project-over-user precedence and per-scope base order.
func DiscoverPromptFiles(cwd, configDir string) PromptFileSet {
	userBases := configBasesUser(configDir)
	var set PromptFileSet
	set.System = firstExisting(promptPaths(cwd, userBases, "SYSTEM.md"))
	set.Append = firstExisting(promptPaths(cwd, userBases, "APPEND_SYSTEM.md"))
	set.Title = firstExisting(promptPaths(cwd, userBases, "TITLE_SYSTEM.md"))
	return set
}

// promptPaths lists every candidate location for one prompt file, in
// precedence order: project bases first, then user bases.
func promptPaths(cwd string, userBases []string, name string) []string {
	out := make([]string, 0, len(configBasesProject)+len(userBases))
	for _, base := range configBasesProject {
		out = append(out, filepath.Join(cwd, base, name))
	}
	for _, base := range userBases {
		out = append(out, filepath.Join(base, name))
	}
	return out
}

// firstExisting returns the content of the first readable file in paths, or
// "" when none exists.
func firstExisting(paths []string) string {
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	return ""
}

// RenderPrompt renders a SYSTEM.md / APPEND_SYSTEM.md template with the cwd
// and date variables. A template syntax or execution error falls back to the
// raw content — a user file can never break the prompt.
func RenderPrompt(content string, cwd string) string {
	vars := promptVars{CWD: cwd, Date: time.Now().Format("2006-01-02")}
	tmpl, err := template.New("prompt").Funcs(promptFuncs(vars)).Option("missingkey=error").Parse(content)
	if err != nil {
		return strings.TrimSpace(content)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, vars); err != nil {
		return strings.TrimSpace(content)
	}
	return strings.TrimSpace(b.String())
}

// RenderTitle renders a TITLE_SYSTEM.md template over the first user
// message. On template error or empty output it falls back to the first-line
// heuristic so a malformed template still yields a usable title.
func RenderTitle(content, userText, cwd string) string {
	first := userText
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	vars := titleVars{
		Message:   strings.TrimSpace(userText),
		FirstLine: first,
		CWD:       cwd,
		Date:      time.Now().Format("2006-01-02"),
	}
	tmpl, err := template.New("title").Funcs(titleFuncs(vars)).Option("missingkey=error").Parse(content)
	if err != nil {
		return first
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, vars); err != nil {
		return first
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return first
	}
	return out
}
