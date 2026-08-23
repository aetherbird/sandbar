package tools

import (
	"fmt"
	"strings"
)

// diffMaxLines caps the number of diff lines emitted in a tool result so that
// very large file changes don't flood the model's context or the terminal.
const diffMaxLines = 200

// maxLCSCells guards the LCS DP table allocation: when the changed region's
// n*m product exceeds this, we fall back to a coarse delete-then-insert diff
// instead of allocating a huge matrix. Typical edits are tiny after common
// prefix/suffix trimming, so this rarely triggers.
const maxLCSCells = 4_000_000

// diffOp is a single line operation in a line-oriented diff.
type diffOp struct {
	kind byte   // ' ' equal, '-' removed (from old), '+' added (in new)
	text string // the line without its trailing newline
}

// splitLines splits s into lines without introducing a phantom empty trailing
// element for content that ends in a newline. "a\nb\n" -> ["a","b"].
// "" -> nil.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i])
		s = s[i+1:]
		if s == "" {
			break
		}
	}
	return lines
}

// lcsOps returns a line-oriented diff op list transforming a into b. It first
// strips the common prefix and suffix (O(n)) so the expensive LCS DP only runs
// over the genuinely changed middle region.
func lcsOps(a, b []string) []diffOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	sa, sb := len(a), len(b)
	for sa > p && sb > p && a[sa-1] == b[sb-1] {
		sa--
		sb--
	}

	ops := make([]diffOp, 0, len(a)+len(b))
	for i := 0; i < p; i++ {
		ops = append(ops, diffOp{kind: ' ', text: a[i]})
	}

	midA := a[p:sa]
	midB := b[p:sb]
	switch {
	case len(midA) == 0 && len(midB) == 0:
		// no change in the middle
	case len(midA)*len(midB) <= maxLCSCells:
		ops = append(ops, lcsMiddle(midA, midB)...)
	default:
		// Region too large for an O(n*m) table: emit a coarse diff so we still
		// report what changed without risking a giant allocation.
		for _, l := range midA {
			ops = append(ops, diffOp{kind: '-', text: l})
		}
		for _, l := range midB {
			ops = append(ops, diffOp{kind: '+', text: l})
		}
	}

	for i := sa; i < len(a); i++ {
		ops = append(ops, diffOp{kind: ' ', text: a[i]})
	}
	return ops
}

// lcsMiddle computes the LCS-based diff between two line slices that have
// already had common prefix/suffix removed. Returns the op sequence.
func lcsMiddle(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = length of LCS of a[i:] and b[j:]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, diffOp{kind: ' ', text: a[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			ops = append(ops, diffOp{kind: '-', text: a[i]})
			i++
		} else {
			ops = append(ops, diffOp{kind: '+', text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: '-', text: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: '+', text: b[j]})
	}
	return ops
}

// hunk is one unified-diff hunk.
type hunk struct {
	oldStart, oldCount int // 0-based start, line counts
	newStart, newCount int
	lines              []diffOp // the op lines in this hunk (with kind prefixes)
}

// buildHunks groups ops into unified-diff hunks, each expanded by `context`
// lines of surrounding equal content. Adjacent change groups whose gap is at
// most 2*context are merged into a single hunk (matching `git diff` behavior).
func buildHunks(ops []diffOp, context int) []hunk {
	type rng struct{ start, end int } // half-open op indices
	var groups []rng
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i
		for i < len(ops) && ops[i].kind != ' ' {
			i++
		}
		groups = append(groups, rng{start, i})
	}
	if len(groups) == 0 {
		return nil
	}

	// Merge groups separated by few enough equal lines.
	merged := []rng{groups[0]}
	for _, g := range groups[1:] {
		prev := &merged[len(merged)-1]
		if g.start-prev.end <= 2*context {
			prev.end = g.end
		} else {
			merged = append(merged, g)
		}
	}

	hunks := make([]hunk, 0, len(merged))
	for _, g := range merged {
		hs := g.start - context
		if hs < 0 {
			hs = 0
		}
		he := g.end + context
		if he > len(ops) {
			he = len(ops)
		}
		hunks = append(hunks, makeHunk(ops, hs, he))
	}
	return hunks
}

// makeHunk builds a hunk spanning ops[start:end], computing line numbers.
func makeHunk(ops []diffOp, start, end int) hunk {
	var h hunk
	oldLine, newLine := 0, 0
	for k := 0; k < start; k++ {
		switch ops[k].kind {
		case ' ':
			oldLine++
			newLine++
		case '-':
			oldLine++
		case '+':
			newLine++
		}
	}
	h.oldStart = oldLine
	h.newStart = newLine
	for k := start; k < end; k++ {
		op := ops[k]
		switch op.kind {
		case ' ':
			oldLine++
			newLine++
			h.oldCount++
			h.newCount++
		case '-':
			oldLine++
			h.oldCount++
		case '+':
			newLine++
			h.newCount++
		}
		h.lines = append(h.lines, op)
	}
	return h
}

// rangeStr formats a unified-diff range header component. 0-based start is
// converted to 1-based; when the count is zero the start is shown as the line
// preceding the insertion point (GNU diff convention).
func rangeStr(start0, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start0)
	case 1:
		return fmt.Sprintf("%d", start0+1)
	default:
		return fmt.Sprintf("%d,%d", start0+1, count)
	}
}

// unifiedDiff returns a unified-diff body (with --- / +++ headers and @@
// hunks) describing the change from oldContent to newContent. Returns "" when
// the contents are identical.
func unifiedDiff(oldContent, newContent, path string) string {
	ops := lcsOps(splitLines(oldContent), splitLines(newContent))
	hunks := buildHunks(ops, 3)
	if len(hunks) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", path)
	fmt.Fprintf(&b, "+++ b/%s\n", path)
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", rangeStr(h.oldStart, h.oldCount), rangeStr(h.newStart, h.newCount))
		for _, ln := range h.lines {
			b.WriteByte(ln.kind)
			b.WriteString(ln.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// countAddsRemoves counts added (+) and removed (-) lines in a unified-diff
// body, ignoring the +++ / --- file headers.
func countAddsRemoves(diffBody string) (added, removed int) {
	for _, line := range strings.Split(diffBody, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return
}

// fileDiffSummary produces a tool-result string: a one-line summary header
// followed by the unified-diff body (capped at diffMaxLines). action is a short
// verb like "wrote", "appended", or "patched"; path is the display path.
func fileDiffSummary(action, path, oldContent, newContent string) string {
	body := unifiedDiff(oldContent, newContent, path)
	added, removed := countAddsRemoves(body)

	var b strings.Builder
	if added == 0 && removed == 0 {
		fmt.Fprintf(&b, "── %s %s (no changes) ──\n", action, path)
		b.WriteString("(no content changes)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "── %s %s (+%d -%d) ──\n", action, path, added, removed)

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) > diffMaxLines {
		for _, l := range lines[:diffMaxLines] {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "... (%d more lines truncated)\n", len(lines)-diffMaxLines)
	} else {
		b.WriteString(body)
	}
	return b.String()
}
