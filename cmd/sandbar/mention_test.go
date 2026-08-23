package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// ── fuzzy scoring ────────────────────────────────────────────────────────────

func TestSubsequenceMatch(t *testing.T) {
	cases := []struct {
		q, c string
		want []int
	}{
		{"ab", "a/b", []int{0, 2}},
		{"ab", "zzz", nil},
		{"", "abc", nil},
		{"abc", "aXXbXXc", []int{0, 3, 6}},
	}
	for _, tc := range cases {
		got := subsequenceMatch([]rune(tc.q), []rune(tc.c))
		if len(got) != len(tc.want) {
			t.Errorf("subsequenceMatch(%q,%q) = %v, want %v", tc.q, tc.c, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("subsequenceMatch(%q,%q) = %v, want %v", tc.q, tc.c, got, tc.want)
				break
			}
		}
	}
}

func TestFuzzyScoreRanksContiguousAndBasenameHigher(t *testing.T) {
	contiguous := fuzzyScore("abc", "abc.go")
	scattered := fuzzyScore("abc", "axbxc.go")
	if contiguous < 0 || scattered < 0 {
		t.Fatalf("both should match: contiguous=%d scattered=%d", contiguous, scattered)
	}
	if contiguous <= scattered {
		t.Errorf("contiguous match (%d) should outrank scattered (%d)", contiguous, scattered)
	}

	basename := mentionScore("main", "cmd/sandbar/main.go")
	dirname := mentionScore("main", "main/cmd/sandbar.go")
	if basename <= dirname {
		t.Errorf("basename match (%d) should outrank dirname match (%d)", basename, dirname)
	}
}

func TestFuzzyScoreNoMatch(t *testing.T) {
	if got := fuzzyScore("xyz", "abc"); got != -1 {
		t.Errorf("fuzzyScore(non-subsequence) = %d, want -1", got)
	}
}

func TestRuneOffset(t *testing.T) {
	// "héllo" — 'é' is two bytes.
	if got := runeOffset("héllo", 2); got != 3 {
		t.Errorf("runeOffset(héllo, 2) = %d, want 3", got)
	}
	if got := runeOffset("abc", 3); got != 3 {
		t.Errorf("runeOffset(abc, 3) = %d, want 3", got)
	}
}

// ── index walk ───────────────────────────────────────────────────────────────

func TestBuildMentionIndexSkips(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.go")
	mustWrite("sub/b.md")
	mustWrite(".hidden/skip.txt")
	mustWrite("node_modules/x/y.js")
	mustWrite("vendor/v.go")
	mustWrite("dist/out.bin")
	mustWrite("build/out.o")
	mustWrite("target/out")
	mustWrite(".git/config")

	// Hidden files at the root are indexed (only hidden DIRS are skipped);
	// everything under skipped dirs is absent.
	mustWrite(".env")
	got := buildMentionIndexAt(root)
	want := []string{".env", "a.go", "sub/b.md"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("index = %v, want %v", got, want)
	}
}

func TestBuildMentionIndexCaps(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < mentionMaxEntries+5; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := buildMentionIndexAt(root)
	if len(got) != mentionMaxEntries {
		t.Fatalf("index len = %d, want capped at %d", len(got), mentionMaxEntries)
	}
}

// ── trigger rules ────────────────────────────────────────────────────────────

func TestMentionWordTriggerRules(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})

	check := func(value string, row, col int, wantWord string, wantOK bool) {
		t.Helper()
		m.ta.SetValue(value)
		setCursor(t, &m, row, col)
		_, _, word, ok := m.mentionWordAtCursor()
		if ok != wantOK || word != wantWord {
			t.Errorf("value %q cursor (%d,%d): word=%q ok=%v, want word=%q ok=%v",
				value, row, col, word, ok, wantWord, wantOK)
		}
	}

	check("fix @mai", 0, 8, "@mai", true)
	check("fix @mai in build", 0, 8, "@mai", true) // cursor mid-word
	check("@", 0, 1, "", false)                    // bare @ never triggers
	check("read @sub/file.go", 0, 16, "", false)   // path-shaped: no picker
	check("plain text", 0, 10, "", false)
	check("line one\n@go", 1, 3, "@go", true) // mention on a later line
}

func TestMentionSuggestionsRankAndCap(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"main.go", "cmd/main.go", "go.mod", "go.sum", "docs/main.md"}
	m.ta.SetValue("fix @go")
	m.ta.CursorEnd()

	sugg := m.mentionSuggestions()
	if len(sugg) == 0 {
		t.Fatal("expected suggestions for @go")
	}
	if len(sugg) > mentionMaxRows {
		t.Fatalf("suggestions capped at %d rows, got %d", mentionMaxRows, len(sugg))
	}
	best := 0
	for _, p := range m.mentionIdx {
		if s := mentionScore("go", p); s > best {
			best = s
		}
	}
	prev := 0
	for i, s := range sugg {
		if !strings.HasPrefix(s, "@") {
			t.Fatalf("suggestion %d = %q, want @-prefixed path", i, s)
		}
		score := mentionScore("go", strings.TrimPrefix(s, "@"))
		if score < 0 {
			t.Fatalf("suggestion %q does not match query", s)
		}
		if i > 0 && score > prev {
			t.Fatalf("suggestions not score-ordered: %d > %d at index %d", score, prev, i)
		}
		prev = score
	}
	if got := mentionScore("go", strings.TrimPrefix(sugg[0], "@")); got != best {
		t.Fatalf("top suggestion score = %d, want best %d", got, best)
	}
}

func TestMentionDismissedSuppressesPopup(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"go.mod"}
	m.ta.SetValue("fix @go")
	m.ta.CursorEnd()
	if len(m.mentionSuggestions()) == 0 {
		t.Fatal("precondition: popup should be live")
	}
	m.mentionDismissed = true
	if len(m.mentionSuggestions()) != 0 {
		t.Fatal("dismissed popup must stay hidden")
	}
}

// ── acceptance and insertion ─────────────────────────────────────────────────

func TestMentionTabAcceptReplacesPartialWord(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"go.mod"}
	m.ta.SetValue("fix @go")
	m.ta.CursorEnd()

	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "fix @go.mod" {
		t.Fatalf("value after Tab accept = %q, want %q", got, "fix @go.mod")
	}
	if !m.mentionDismissed {
		t.Fatal("accept should suppress the popup until the next text change")
	}
	if len(m.mentionSuggestions()) != 0 {
		t.Fatal("popup should be dismissed right after accept")
	}
	// Typing re-arms it.
	upd, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = upd.(appModel)
	if m.mentionDismissed {
		t.Fatal("text change should re-arm the popup")
	}
}

func TestMentionEnterAcceptsWithoutSending(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"go.mod"}
	m.ta.SetValue("fix @go")
	m.ta.CursorEnd()

	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "fix @go.mod" {
		t.Fatalf("value after Enter accept = %q, want %q", got, "fix @go.mod")
	}
	if m.streaming {
		t.Fatal("accepting a mention must not start a turn")
	}
}

func TestMentionEscDismissesUntilTextChange(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"go.mod"}
	m.ta.SetValue("fix @go")
	m.ta.CursorEnd()

	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = upd.(appModel)
	if !m.mentionDismissed {
		t.Fatal("esc should dismiss the mention popup")
	}
	if m.ta.Value() != "fix @go" {
		t.Fatalf("esc must not modify the input, got %q", m.ta.Value())
	}
	if len(m.mentionSuggestions()) != 0 {
		t.Fatal("dismissed popup should be hidden")
	}
}

func TestMentionInsertionMidValue(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("fix @mai in build")
	setCursor(t, &m, 0, 8) // just past "@mai"
	m.replaceMention("@main.go")
	if got := m.ta.Value(); got != "fix @main.go in build" {
		t.Fatalf("value after replaceMention = %q, want %q", got, "fix @main.go in build")
	}
	if m.ta.Line() != 0 || m.ta.Column() != 12 {
		t.Fatalf("cursor = (%d,%d), want (0,12)", m.ta.Line(), m.ta.Column())
	}
}

func TestMentionArrowSelectionAndAccept(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"alpha.go", "alpine.go"}
	m.ta.SetValue("fix @alp")
	m.ta.CursorEnd()

	sugg := m.mentionSuggestions()
	if len(sugg) != 2 {
		t.Fatalf("suggestions = %v, want 2", sugg)
	}
	// ↓ moves to the second row; Tab accepts the highlighted one.
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = upd.(appModel)
	if m.mentionSel != 1 {
		t.Fatalf("mentionSel after down = %d, want 1", m.mentionSel)
	}
	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = upd.(appModel)
	if got := m.ta.Value(); got != "fix "+sugg[1] {
		t.Fatalf("value = %q, want %q", got, "fix "+sugg[1])
	}
}

func TestMentionPopupDoesNotFireMidPath(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.mentionBuilt = true
	m.mentionIdx = []string{"sub/file.go"}
	m.ta.SetValue("read @sub/fi")
	m.ta.CursorEnd()
	if len(m.mentionSuggestions()) != 0 {
		t.Fatal("path-shaped @word must not open the mention picker")
	}
}
