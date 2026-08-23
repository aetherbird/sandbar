package tools

import (
	"strings"
	"testing"
)

func TestSplitLines(t *testing.T) {
	cases := map[string][]string{
		"":         nil,
		"a":        {"a"},
		"a\n":      {"a"},
		"a\nb":     {"a", "b"},
		"a\nb\n":   {"a", "b"},
		"a\n\nb\n": {"a", "", "b"},
		"\n":       {""},
	}
	for in, want := range cases {
		got := splitLines(in)
		if len(got) != len(want) {
			t.Errorf("splitLines(%q) = %v (len %d), want %v (len %d)", in, got, len(got), want, len(want))
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	if got := unifiedDiff("a\nb\nc\n", "a\nb\nc\n", "f.txt"); got != "" {
		t.Errorf("identical content should produce empty diff, got:\n%s", got)
	}
}

func TestUnifiedDiffSimpleReplace(t *testing.T) {
	old := "line1\nline2\nline3\nline4\nline5\n"
	new := "line1\nCHANGED\nline3\nline4\nline5\n"
	got := unifiedDiff(old, new, "f.txt")

	// Must have file headers.
	if !strings.HasPrefix(got, "--- a/f.txt\n") {
		t.Errorf("missing old file header:\n%s", got)
	}
	if !strings.Contains(got, "+++ b/f.txt\n") {
		t.Errorf("missing new file header:\n%s", got)
	}
	// Must reference a hunk.
	if !strings.Contains(got, "@@ ") {
		t.Errorf("missing hunk header:\n%s", got)
	}
	// Removed and added lines present.
	if !strings.Contains(got, "-line2") {
		t.Errorf("missing removed line -line2:\n%s", got)
	}
	if !strings.Contains(got, "+CHANGED") {
		t.Errorf("missing added line +CHANGED:\n%s", got)
	}
	// Context lines around the change.
	if !strings.Contains(got, " line1") {
		t.Errorf("missing context line ' line1':\n%s", got)
	}
	if !strings.Contains(got, " line3") {
		t.Errorf("missing trailing context line ' line3':\n%s", got)
	}
}

func TestUnifiedDiffAddOnly(t *testing.T) {
	old := "a\nb\n"
	new := "a\nb\nc\nd\n"
	got := unifiedDiff(old, new, "f.txt")
	if !strings.Contains(got, "+c") || !strings.Contains(got, "+d") {
		t.Errorf("expected additions +c and +d:\n%s", got)
	}
	added, removed := countAddsRemoves(got)
	if removed != 0 {
		t.Errorf("pure addition should have no deletions, got -%d:\n%s", removed, got)
	}
	if added != 2 {
		t.Errorf("expected 2 additions, got +%d", added)
	}
}

func TestUnifiedDiffDeleteOnly(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nc\n"
	got := unifiedDiff(old, new, "f.txt")
	if !strings.Contains(got, "-b") {
		t.Errorf("expected deletion -b:\n%s", got)
	}
	added, removed := countAddsRemoves(got)
	if added != 0 {
		t.Errorf("pure deletion should have no additions, got +%d:\n%s", added, got)
	}
	if removed != 1 {
		t.Errorf("expected 1 deletion, got -%d", removed)
	}
}

func TestUnifiedDiffNewFile(t *testing.T) {
	got := unifiedDiff("", "hello\nworld\n", "new.txt")
	if !strings.Contains(got, "+hello") || !strings.Contains(got, "+world") {
		t.Errorf("new file should be all additions:\n%s", got)
	}
	// Hunk for an all-added file: old range count is 0.
	if !strings.Contains(got, ",0 +") {
		t.Errorf("expected old range with 0 count for new file:\n%s", got)
	}
}

func TestUnifiedDiffHunkLineNumbers(t *testing.T) {
	// Change a single line in a 10-line file; hunk should start at line ~3
	// (1-based) with 3 lines of context around the change at line 5.
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = "row" + string(rune('0'+i))
	}
	old := strings.Join(lines, "\n") + "\n"
	lines[4] = "X"
	new := strings.Join(lines, "\n") + "\n"
	got := unifiedDiff(old, new, "f.txt")
	// Expect a single hunk header with a sensible old range starting at 2.
	if !strings.Contains(got, "@@ -2,") {
		t.Errorf("expected hunk starting around old line 2 (3 context before line 5):\n%s", got)
	}
}

func TestRangeStr(t *testing.T) {
	cases := []struct {
		start, count int
		want         string
	}{
		{0, 0, "0,0"},
		{4, 0, "4,0"},
		{4, 1, "5"},
		{4, 3, "5,3"},
	}
	for _, c := range cases {
		if got := rangeStr(c.start, c.count); got != c.want {
			t.Errorf("rangeStr(%d,%d) = %q, want %q", c.start, c.count, got, c.want)
		}
	}
}

func TestCountAddsRemoves(t *testing.T) {
	body := "--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n-old\n+new\n ctx\n"
	added, removed := countAddsRemoves(body)
	if added != 1 || removed != 1 {
		t.Errorf("countAddsRemoves = (+%d -%d), want (+1 -1)", added, removed)
	}
}

func TestFileDiffSummary(t *testing.T) {
	out := fileDiffSummary("patched", "src/main.go", "a\nb\n", "a\nB\n")
	if !strings.HasPrefix(out, "── patched src/main.go (+1 -1) ──") {
		t.Errorf("unexpected summary header: %q", firstLine(out))
	}
	if !strings.Contains(out, "--- a/src/main.go") || !strings.Contains(out, "+++ b/src/main.go") {
		t.Errorf("summary should include diff body:\n%s", out)
	}
}

func TestFileDiffSummaryNoChanges(t *testing.T) {
	out := fileDiffSummary("wrote", "f.txt", "same\n", "same\n")
	if !strings.Contains(out, "(no changes)") {
		t.Errorf("expected no-change summary, got:\n%s", out)
	}
}

func TestFileDiffSummaryTruncates(t *testing.T) {
	// Generate a large change exceeding diffMaxLines.
	old := strings.Repeat("old\n", diffMaxLines+50)
	new := strings.Repeat("new\n", diffMaxLines+50)
	out := fileDiffSummary("wrote", "big.txt", old, new)
	if !strings.Contains(out, "truncated)") {
		t.Errorf("expected truncation marker for huge diff:\n%s", firstLine(out))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
