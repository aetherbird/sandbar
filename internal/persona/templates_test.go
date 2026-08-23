package persona

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// isolateUserScope points HOME and XDG_CONFIG_HOME at fresh temp dirs so
// user-scope discovery never sees the real machine's config.
func isolateUserScope(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func writeTemplate(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverTemplates(t *testing.T) {
	isolateUserScope(t)
	root := t.TempDir()
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", "review.md"),
		"---\ndescription: Review a change\nargument-hint: [file]\n---\n\nReview $1 carefully.\n")
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", "crlf.md"),
		"---\r\ndescription: CRLF template\r\n---\r\n\r\nDo the thing.\r\n")
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", "plain.md"), "\n\n# Plain fallback\nBody line.\n")
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", "notes.txt"), "not markdown")
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", ".hidden.md"), "---\ndescription: hidden\n---\n")
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", "sub", "inner.md"), "---\ndescription: dir\n---\n")

	want := []Template{
		{Name: "crlf", Description: "CRLF template", Body: "Do the thing.\r\n"},
		{Name: "plain", Description: "# Plain fallback", Body: "# Plain fallback\nBody line.\n"},
		{Name: "review", Description: "Review a change", ArgHint: "[file]", Body: "Review $1 carefully.\n"},
	}
	templates := DiscoverTemplates(root)
	if !reflect.DeepEqual(templates, want) {
		t.Errorf("DiscoverTemplates =\n%+v\nwant\n%+v", templates, want)
	}
}

func TestDiscoverTemplatesWorkspaceShadowsUser(t *testing.T) {
	home := isolateUserScope(t)
	ws := t.TempDir()
	writeTemplate(t, filepath.Join(ws, ".sandbar", "prompts", "review.md"), "---\ndescription: workspace\n---\nws body")
	writeTemplate(t, filepath.Join(home, ".config", "sandbar", "prompts", "review.md"), "---\ndescription: user\n---\nuser body")
	writeTemplate(t, filepath.Join(home, ".config", "sandbar", "prompts", "other.md"), "other body")

	templates := DiscoverTemplates(ws)
	review, ok := FindTemplate(templates, "review")
	if !ok {
		t.Fatal("review template missing")
	}
	if review.Description != "workspace" || review.Body != "ws body" {
		t.Errorf("review = %+v, want workspace body", review)
	}
	if len(templates) != 2 {
		t.Errorf("got %d templates, want 2", len(templates))
	}
}

func TestDiscoverTemplatesUserScopeFallback(t *testing.T) {
	home := isolateUserScope(t)
	ws := t.TempDir() // no .sandbar/prompts
	writeTemplate(t, filepath.Join(home, ".config", "sandbar", "prompts", "commit.md"), "---\ndescription: Commit staged work\n---\nCommit it.")

	templates := DiscoverTemplates(ws)
	if len(templates) != 1 || templates[0].Name != "commit" || templates[0].Description != "Commit staged work" {
		t.Fatalf("templates = %+v, want user-scope commit", templates)
	}
}

func TestDiscoverTemplatesMissingDirs(t *testing.T) {
	isolateUserScope(t)
	if templates := DiscoverTemplates(t.TempDir()); len(templates) != 0 {
		t.Errorf("got %+v, want none", templates)
	}
}

func TestFindTemplate(t *testing.T) {
	templates := []Template{{Name: "review"}, {Name: "commit"}}
	if tt, ok := FindTemplate(templates, "commit"); !ok || tt.Name != "commit" {
		t.Errorf("FindTemplate(commit) = %+v, %v", tt, ok)
	}
	if _, ok := FindTemplate(templates, "Commit"); ok {
		t.Error("FindTemplate(Commit) matched; names are exact")
	}
	if _, ok := FindTemplate(templates, "nope"); ok {
		t.Error("FindTemplate(nope) matched")
	}
}

func TestExpandTemplate(t *testing.T) {
	tests := []struct {
		name string
		body string
		args string
		want string
	}{
		{"positional", "fix $1 in $2", "bug auth", "fix bug in auth"},
		{"all nine", "$1$2$3$4$5$6$7$8$9", "1 2 3 4 5 6 7 8 9", "123456789"},
		{"missing positional is empty", "[$4]", "a", "[]"},
		{"dollar-at", "run $@ now", "a b", "run a b now"},
		{"ARGUMENTS", "run $ARGUMENTS now", "a b", "run a b now"},
		{"ARGUMENTS at eof", "$ARGUMENTS", "x y", "x y"},
		{"brace dollar-at", "run ${@} now", "a b", "run a b now"},
		{"brace ARGUMENTS", "${ARGUMENTS}", "x", "x"},
		{"adjacent forms", "$1$2$@", "a b", "aba b"},
		{"slice from N", "${@:2}", "a b c", "b c"},
		{"slice from first", "${@:1}", "a b", "a b"},
		{"slice beyond end", "${@:9}", "a", ""},
		{"slice N for L", "${@:2:1}", "a b c", "b"},
		{"slice L clamps", "${@:1:99}", "a b", "a b"},
		{"slice zero L", "${@:1:0}", "a b", ""},
		{"slice middle", "${@:2:2}", "a b c d", "b c"},
		{"unknown word", "pay $foo", "a", "pay $foo"},
		{"zero is not positional", "$0", "a", "$0"},
		{"ten is one then literal", "$10", "a b", "a0"},
		{"ARGUMENTS word boundary", "$ARGUMENTSX", "a", "$ARGUMENTSX"},
		{"unknown brace", "${name:2}", "", "${name:2}"},
		{"unknown brace keeps inner dollars", "${a$1}", "v", "${a$1}"},
		{"slice N must be >= 1", "${@:0}", "a", "${@:0}"},
		{"slice N numeric", "${@:x}", "a", "${@:x}"},
		{"slice L numeric", "${@:2:x}", "a b", "${@:2:x}"},
		{"unterminated brace", "a ${@:2 b", "a", "a ${@:2 b"},
		{"trailing dollar", "pay $", "", "pay $"},
		{"double dollar before arg", "$$1", "v", "$v"},
		{"empty args join to empty", "<$@|$ARGUMENTS>", "", "<|>"},
		{"args are whitespace-split", "fix $1 in $2", "  bug   auth  ", "fix bug in auth"},
		{"plain body", "plain text", "a", "plain text"},
		{"empty body", "", "a", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := Template{Name: "t", Body: tt.body}
			if got := ExpandTemplate(tmpl, tt.args); got != tt.want {
				t.Errorf("ExpandTemplate(%q, %q) = %q, want %q", tt.body, tt.args, got, tt.want)
			}
		})
	}
}

func TestTemplateFlow(t *testing.T) {
	// Discover → find → expand, the slash-command pipeline end to end.
	isolateUserScope(t)
	root := t.TempDir()
	writeTemplate(t, filepath.Join(root, ".sandbar", "prompts", "fix.md"),
		"---\ndescription: Fix a lint error\nargument-hint: <file> <line>\n---\n\nFix $1 at $2 in ${@:3}.\n")
	templates := DiscoverTemplates(root)
	tmpl, ok := FindTemplate(templates, "fix")
	if !ok {
		t.Fatal("fix template missing")
	}
	if tmpl.ArgHint != "<file> <line>" {
		t.Errorf("ArgHint = %q", tmpl.ArgHint)
	}
	if got := ExpandTemplate(tmpl, "auth.go 42 main tests"); got != "Fix auth.go at 42 in main tests.\n" {
		t.Errorf("ExpandTemplate = %q", got)
	}
	if !strings.HasPrefix(tmpl.Description, "Fix a lint error") {
		t.Errorf("Description = %q", tmpl.Description)
	}
}
