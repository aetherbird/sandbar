package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func mkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func mkfiles(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"foo/", "foobar"}, "foo"},
		{[]string{"a", "ab", "abc"}, "a"},
		{[]string{"x", "y"}, ""},
		{[]string{"same", "same"}, "same"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := longestCommonPrefix(c.in); got != c.want {
			t.Errorf("longestCommonPrefix(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPathTabCompletesSingleMatch: one candidate replaces the filename
// portion outright.
func TestPathTabCompletesSingleMatch(t *testing.T) {
	dir := t.TempDir()
	mkfiles(t, dir, "unique.txt")
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("read " + dir + "/uni")
	m.ta.CursorEnd()

	if sugg := m.pathSuggestions(); len(sugg) != 1 || sugg[0] != "unique.txt" {
		t.Fatalf("suggestions = %v, want [unique.txt]", sugg)
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "read "+dir+"/unique.txt" {
		t.Fatalf("value after Tab = %q", got)
	}
}

// TestPathTabFillsLongestCommonPrefix: several candidates fill their shared
// prefix (bash behavior) instead of jumping to the highlighted row; Enter
// still selects the highlighted row.
func TestPathTabFillsLongestCommonPrefix(t *testing.T) {
	dir := t.TempDir()
	mkfiles(t, dir, "foo1.txt", "foo2.txt")
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("read " + dir + "/fo")
	m.ta.CursorEnd()

	if sugg := m.pathSuggestions(); len(sugg) != 2 {
		t.Fatalf("suggestions = %v, want 2", sugg)
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "read "+dir+"/foo" {
		t.Fatalf("value after Tab = %q, want LCP fill %q", got, "read "+dir+"/foo")
	}
	// The popup survives (still two candidates on the narrowed prefix).
	if sugg := m.pathSuggestions(); len(sugg) != 2 {
		t.Fatalf("suggestions after LCP fill = %v, want 2", sugg)
	}
	// Enter completes the highlighted row.
	m.pathSel = 1
	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "read "+dir+"/foo2.txt" {
		t.Fatalf("value after Enter = %q, want highlighted row %q", got, "read "+dir+"/foo2.txt")
	}
}

// TestPathSuggestionsDirTrailingSlash: directories keep a trailing "/" so the
// user can keep drilling in.
func TestPathSuggestionsDirTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "sub")
	mkfiles(t, dir, "readme.md")
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("read " + dir + "/s")
	m.ta.CursorEnd()

	sugg := m.pathSuggestions()
	found := false
	for _, s := range sugg {
		if s == "sub/" {
			found = true
		}
		if strings.HasSuffix(s, "/") && !strings.HasPrefix(s, "sub") {
			t.Fatalf("unexpected dir suggestion %q", s)
		}
	}
	if !found {
		t.Fatalf("suggestions = %v, want sub/ with trailing slash", sugg)
	}
}

// TestPathTildePreservedOnComplete: "~" resolves for the filesystem read but
// stays displayed as "~" in the input and the completed result.
func TestPathTildePreservedOnComplete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mkfiles(t, home, "notes.md", "other/thing.md")
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("read ~/no")
	m.ta.CursorEnd()

	if sugg := m.pathSuggestions(); len(sugg) != 1 || sugg[0] != "notes.md" {
		t.Fatalf("suggestions = %v, want [notes.md]", sugg)
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "read ~/notes.md" {
		t.Fatalf("value after Tab = %q, want tilde preserved", got)
	}
}
