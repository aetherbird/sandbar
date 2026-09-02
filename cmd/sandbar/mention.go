// @-mentions: fzf-style fuzzy file completion in prompts. Typing "@query"
// surfaces a subsequence-ranked popup of working-directory files; Tab/Enter
// accepts the highlighted path (replacing the partial @text), ↑/↓ move,
// Esc dismisses. A word that already contains a "/" is a direct @path
// reference, not a mention, so the popup stays out of its way.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mentionMaxEntries caps the cached file index; a repo bigger than this just
// drops the deepest entries.
const mentionMaxEntries = 2000

// mentionMaxRows caps the suggestion popup.
const mentionMaxRows = 6

// buildMentionIndexAt walks root once and returns a bounded list of relative
// file paths, skipping VCS, dependency, build-output, and hidden dirs. Pure
// so tests can point it at a temp tree.
func buildMentionIndexAt(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(out) >= mentionMaxEntries {
			if len(out) >= mentionMaxEntries {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || isSkipDir(d.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out
}

// buildMentionIndex walks the working directory once and caches the index.
func (m *appModel) buildMentionIndex() {
	if m.mentionBuilt {
		return
	}
	m.mentionBuilt = true
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	m.mentionIdx = buildMentionIndexAt(cwd)
}

func isSkipDir(name string) bool {
	switch name {
	case ".git", ".svn", ".hg", "node_modules", "vendor", "dist", "build", "target":
		return true
	}
	return false
}

// mentionWordAtCursor returns the byte range and text of the whitespace-
// delimited word containing the cursor when it is a mention trigger: a plain
// "@query" word (len >= 2 so a bare "@" doesn't trigger a walk, and no "/" —
// a path-shaped word is a direct @file reference, not a mention).
func (m *appModel) mentionWordAtCursor() (start, end int, word string, ok bool) {
	start, end, word = m.wordAtCursor()
	if !strings.HasPrefix(word, "@") || len(word) < 2 || strings.ContainsRune(word, '/') {
		return 0, 0, "", false
	}
	return start, end, word, true
}

// mentionSuggestions returns subsequence-ranked @-mention candidates for the
// current input, or nil when no mention is being typed.
func (m *appModel) mentionSuggestions() []string {
	if m.mentionDismissed {
		return nil
	}
	_, _, word, ok := m.mentionWordAtCursor()
	if !ok {
		return nil
	}
	query := strings.TrimPrefix(word, "@")
	if query == "" {
		return nil
	}
	m.buildMentionIndex()

	type scored struct {
		path  string
		score int
	}
	matches := make([]scored, 0, 64)
	for _, path := range m.mentionIdx {
		if s := mentionScore(query, path); s >= 0 {
			matches = append(matches, scored{path, s})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].path < matches[j].path
	})
	if len(matches) > mentionMaxRows {
		matches = matches[:mentionMaxRows]
	}
	out := make([]string, len(matches))
	for i, sc := range matches {
		out[i] = "@" + sc.path
	}
	return out
}

// clampedMentionSel keeps the highlighted row in range as the list filters.
func (m appModel) clampedMentionSel(n int) int {
	s := m.mentionSel
	if s >= n {
		s = n - 1
	}
	if s < 0 {
		s = 0
	}
	return s
}

// acceptMention swaps the @-word at the cursor for the highlighted candidate.
func (m *appModel) acceptMention(sugg []string) {
	sel := m.clampedMentionSel(len(sugg))
	if sel < 0 || sel >= len(sugg) {
		return
	}
	m.replaceMention(sugg[sel])
	m.syncInputHeight()
}

// mentionSuggestView renders the fuzzy picker popup shown above the input.
func (m appModel) mentionSuggestView(sugg []string) string {
	sel := m.clampedMentionSel(len(sugg))
	// Fixed-height body (see pathSuggestView): the frame must not change
	// height as the filter narrows, or the inline renderer burns stale popup
	// rows into scrollback.
	const maxShow = 10
	start := 0
	if sel > maxShow-1 {
		start = sel - maxShow + 1
	}
	end := start + maxShow
	if end > len(sugg) {
		end = len(sugg)
	}
	var b strings.Builder
	shown := 0
	for i := start; i < end; i++ {
		path := sugg[i]
		if i == sel {
			b.WriteString(sty(cAccent).Render("  ▸ "+path) + "\n")
		} else {
			b.WriteString(sty(cMuted).Render("    "+path) + "\n")
		}
		shown++
	}
	for ; shown < maxShow; shown++ {
		b.WriteString("\n")
	}
	hint := "    ↑↓ move · Tab/Enter accept · Esc dismiss"
	if hidden := len(sugg) - (end - start); hidden > 0 {
		hint += fmt.Sprintf(" · %d more…", hidden)
	}
	b.WriteString(sty(cMuted).Render(hint))
	return b.String()
}

// replaceMention swaps the @-word at the cursor for the completed path. The
// insertion is one undoable step.
func (m *appModel) replaceMention(replacement string) {
	start, end, _ := m.wordAtCursor()
	v := m.ta.Value()
	off := start + len(replacement)
	m.pushUndoState()
	m.ta.SetValue(v[:start] + replacement + v[end:])
	m.repositionToOffset(off)
}

// wordAtCursor returns the byte range and text of the whitespace-delimited
// word containing the cursor.
func (m *appModel) wordAtCursor() (start, end int, word string) {
	v := m.ta.Value()
	off := m.cursorOffset()
	start = off
	for start > 0 && !isWordSpace(v[start-1]) {
		start--
	}
	end = off
	for end < len(v) && !isWordSpace(v[end]) {
		end++
	}
	return start, end, v[start:end]
}

// cursorOffset is the cursor's byte offset within the full value.
func (m *appModel) cursorOffset() int {
	v := m.ta.Value()
	line, col := m.ta.Line(), m.ta.Column()
	lines := strings.Split(v, "\n")
	off := 0
	for i := 0; i < line && i < len(lines); i++ {
		off += len(lines[i]) + 1
	}
	if line < len(lines) {
		off += runeOffset(lines[line], col)
	}
	return off
}

// repositionToOffset moves the cursor to a byte offset within the value.
func (m *appModel) repositionToOffset(off int) {
	v := m.ta.Value()
	if off > len(v) {
		off = len(v)
	}
	lines := strings.Split(v[:off], "\n")
	m.reposition(len(lines)-1, len([]rune(lines[len(lines)-1])))
}

// runeOffset converts a rune index into s to a byte offset.
func runeOffset(s string, col int) int {
	n := 0
	for i := range s {
		if n == col {
			return i
		}
		n++
	}
	return len(s)
}

func isWordSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' }

// --- fuzzy scoring ---------------------------------------------------------

// subsequenceMatch returns the candidate indexes where query matches, or nil
// when query is not a subsequence of candidate.
func subsequenceMatch(query, candidate []rune) []int {
	if len(query) == 0 {
		return nil
	}
	pos := make([]int, 0, len(query))
	qi := 0
	for ci, r := range candidate {
		if r == query[qi] {
			pos = append(pos, ci)
			qi++
			if qi == len(query) {
				return pos
			}
		}
	}
	return nil
}

// mentionScore scores query against a file path for the @-mention popup. It
// takes the better of scoring the full path and the basename, so a typed
// filename matches its basename contiguously even when an earlier path
// segment would pull a greedy leftmost match astray.
func mentionScore(query, path string) int {
	s := fuzzyScore(query, path)
	if b := fuzzyScore(query, filepath.Base(path)); b > s {
		s = b
	}
	return s
}

// fuzzyScore scores how well query matches candidate as a subsequence:
// higher is better, -1 means no match. Contiguous runs and matches at path
// boundaries score higher; longer candidates and later first matches score
// lower. Ranking is heuristic (fzf-like) but deterministic.
func fuzzyScore(query, candidate string) int {
	q := []rune(strings.ToLower(query))
	c := []rune(strings.ToLower(candidate))
	pos := subsequenceMatch(q, c)
	if pos == nil {
		return -1
	}
	score := 0
	for i, p := range pos {
		switch {
		case i == 0:
			score += 12
		case p == pos[i-1]+1:
			score += 20 // contiguous
		default:
			score -= (p - pos[i-1] - 1) * 4 // gap penalty
		}
		if p == 0 || c[p-1] == '/' || c[p-1] == '.' || c[p-1] == '-' || c[p-1] == '_' {
			score += 15 // boundary bonus
		}
	}
	// Basename bonus: all matches land in the last path segment.
	lastSlash := -1
	for i, r := range c {
		if r == '/' {
			lastSlash = i
		}
	}
	if pos[0] > lastSlash {
		score += 25
	}
	score -= len(c) / 8 // prefer shorter paths
	score -= pos[0] / 6 // prefer an earlier first match
	return score
}
