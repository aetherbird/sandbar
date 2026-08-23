package main

import (
	"regexp"
	"strings"

	"sandbar/internal/cliui"
)

var markdownPresentation cliui.MarkdownRenderer

func resetMarkdownRenderer() { markdownPresentation.Reset() }

func renderMarkdown(text string) string {
	return markdownPresentation.Render(currentStyles(), text)
}

// contentMargin is the left gutter applied to assistant prose (rendered
// markdown, plain streamed text, transcript assistant text). Prose is wrapped
// at printWidth()-contentMargin so the gutter plus a wrapped line can never
// exceed printWidth and desync the inline renderer.
const contentMargin = 2

var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

// indentGlamourOutput retains its historical name for call-site compatibility.
// It collapses Glamour's styled blank separators and indents every remaining
// non-empty line by contentMargin, giving assistant prose its left gutter.
func indentGlamourOutput(s string) string {
	lines := strings.Split(s, "\n")
	prefix := strings.Repeat(" ", contentMargin)
	var out []string
	prevBlank := true
	for _, line := range lines {
		visible := strings.TrimSpace(ansiSeq.ReplaceAllString(line, ""))
		if visible == "" {
			if !prevBlank && len(out) > 0 {
				out = append(out, "")
			}
			prevBlank = true
			continue
		}
		out = append(out, prefix+line)
		prevBlank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// prefixContentMargin indents every non-empty line of s by contentMargin
// without collapsing blank lines — for plain (non-Glamour) assistant prose
// that must keep its own spacing.
func prefixContentMargin(s string) string {
	prefix := strings.Repeat(" ", contentMargin)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// proseWidth is the wrap width for assistant prose: printWidth narrowed by the
// content margin so a wrapped line plus its gutter stays within printWidth.
func (m appModel) proseWidth() int {
	w := m.printWidth() - contentMargin
	if w < 1 {
		w = 1
	}
	return w
}

// marginProse wraps plain assistant prose at proseWidth and prefixes the
// content margin: the plain-streaming counterpart of the margined markdown
// path. Wraps at whitespace only, with a hard-wrap fallback for tokens wider
// than the line (URLs, long paths).
func (m appModel) marginProse(s string) string {
	return prefixContentMargin(cliui.WrapPrint(s, m.proseWidth()))
}
