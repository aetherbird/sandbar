package cliui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// wrapCell is one atomic unit of the print wrapper: a single rune or an ANSI
// escape sequence, with its display width (zero for escapes). At rune
// granularity, not grapheme clusters: glamorous output escapes span whole
// styled runs, and combining sequences are not split because escapes never
// open a wrap opportunity.
type wrapCell struct {
	str string
	w   int
}

// wrapTokenize splits s into wrapCells, treating ANSI escape sequences as
// zero-width atoms so they pass through wrapping intact.
func wrapTokenize(s string) []wrapCell {
	cells := make([]wrapCell, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			if j < len(s) {
				j++ // consume the final byte of the escape
			}
			cells = append(cells, wrapCell{str: s[i:j]})
			i = j
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		w := ansi.StringWidth(s[i : i+size])
		if s[i] == '\t' {
			w = 1
		}
		cells = append(cells, wrapCell{str: s[i : i+size], w: w})
		i += size
	}
	return cells
}

// wrapSeg is a run of same-class cells: a whitespace run (break opportunity)
// or a word (cells that must reach the next line together, escapes included).
type wrapSeg struct {
	cells []wrapCell
	w     int
	sep   bool
}

func wrapSepCell(c wrapCell) bool { return c.str == " " || c.str == "\t" }

func wrapBuildSegs(cells []wrapCell) []wrapSeg {
	var segs []wrapSeg
	for i := 0; i < len(cells); {
		sep := wrapSepCell(cells[i])
		j, w := i, 0
		for j < len(cells) && wrapSepCell(cells[j]) == sep {
			w += cells[j].w
			j++
		}
		segs = append(segs, wrapSeg{cells: cells[i:j], w: w, sep: sep})
		i = j
	}
	return segs
}

func wrapJoin(cells []wrapCell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteString(c.str)
	}
	return b.String()
}

// wrapChunk hard-breaks an oversized token into chunks of at most width
// cells. A single cell wider than width (a wide glyph in a narrow budget)
// is emitted alone; zero-width escapes never open a chunk boundary.
func wrapChunk(cells []wrapCell, width int) []string {
	var out []string
	var cur strings.Builder
	curW := 0
	for _, c := range cells {
		if curW > 0 && curW+c.w > width {
			out = append(out, cur.String())
			cur.Reset()
			curW = 0
		}
		cur.WriteString(c.str)
		curW += c.w
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// wrapLineCells packs one source line's cells into lines of at most width
// cells, breaking only at whitespace. A word wider than width moves to its
// own line and is hard-chunked. The line's leading whitespace becomes the
// hanging indent for its wrapped continuations (list markers, code indent).
func wrapLineCells(cells []wrapCell, width int) []string {
	total, hasWord := 0, false
	for _, c := range cells {
		total += c.w
		if !wrapSepCell(c) {
			hasWord = true
		}
	}
	if total <= width || !hasWord {
		return []string{wrapJoin(cells)}
	}

	lead := 0
	for _, c := range cells {
		if !wrapSepCell(c) {
			break
		}
		lead++
	}
	indentW := lead
	if half := width / 2; indentW > half {
		indentW = half
	}
	indent := strings.Repeat(" ", indentW)
	avail := width - indentW
	if avail < 1 {
		avail = 1
	}

	var (
		out       []string
		line      = wrapJoin(cells[:lead])
		lineW     = lead
		lineDirty = false
		pendSep   string
		pendSepW  int
	)
	for _, seg := range wrapBuildSegs(cells) {
		if seg.sep {
			if lineDirty {
				pendSep, pendSepW = wrapJoin(seg.cells), seg.w
			}
			continue
		}
		word, wordW := wrapJoin(seg.cells), seg.w
		switch {
		case lineW+pendSepW+wordW <= width:
			if lineDirty {
				line += pendSep
				lineW += pendSepW
			}
			line += word
			lineW += wordW
			lineDirty = true
		case wordW <= avail:
			if line != "" {
				out = append(out, line)
			}
			line, lineW, lineDirty = indent+word, indentW+wordW, true
		default: // token wider than a whole line: hard-chunk it
			if line != "" {
				out = append(out, line)
			}
			for _, chunk := range wrapChunk(seg.cells, avail) {
				out = append(out, indent+chunk)
			}
			line, lineW, lineDirty = "", 0, false
		}
		pendSep, pendSepW = "", 0
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// WrapPrint wraps every line of s to at most width display cells, breaking
// lines only at whitespace. Words — hyphenated compounds, file paths, URLs,
// CLI flags — move to the next line whole; only a token wider than width
// itself is hard-broken mid-token, so no printed line can overflow. Existing
// newlines are preserved, ANSI escape sequences pass through intact and do
// not count toward the width, and a wrapped line's leading whitespace is
// reused as the continuation indent.
func WrapPrint(s string, width int) string {
	if s == "" || width < 1 {
		return s
	}
	src := strings.Split(s, "\n")
	out := make([]string, 0, len(src))
	for _, line := range src {
		out = append(out, wrapLineCells(wrapTokenize(line), width)...)
	}
	return strings.Join(out, "\n")
}
