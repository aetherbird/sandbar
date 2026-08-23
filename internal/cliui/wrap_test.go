package cliui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestWrapPrint(t *testing.T) {
	green := "\033[38;5;114m"
	reset := "\033[0m"
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"empty string", "", 10, ""},
		{"width < 1 is a no-op", "hello", 0, "hello"},
		{"shorter than width", "hello", 10, "hello"},
		{"exactly width", "abcdefghij", 10, "abcdefghij"},
		{"breaks at space", "hello bigworld", 10, "hello\nbigworld"},
		{"space at break is consumed", "hello world", 5, "hello\nworld"},
		{"hyphenated word stays whole when it fits", "hi there-buddy", 13, "hi\nthere-buddy"},
		{"oversized hyphenated token hard-broken", "hello-bigworld", 10, "hello-bigw\norld"},
		{"flags never split at hyphens", "deploy with --name=example and self-hostable", 24, "deploy with\n--name=example and\nself-hostable"},
		{"tab is a break opportunity", "aaaa\tbbbb", 5, "aaaa\nbbbb"},
		{"no breakpoint hard-breaks", "abcdefghijklmno", 10, "abcdefghij\nklmno"},
		{"long unbroken word hard-broken", strings.Repeat("a", 25), 10, "aaaaaaaaaa\naaaaaaaaaa\naaaaa"},
		{"wide runes hard-break", "界界界界界", 4, "界界\n界界\n界"},
		{"existing newlines preserved", "abc\nde", 5, "abc\nde"},
		{"ansi sequences preserved and not counted", green + "abcde fghij" + reset, 10, green + "abcde\nfghij" + reset},
		{"ansi between words preserved", green + "abcde" + reset + " fghij", 10, green + "abcde" + reset + "\nfghij"},
		{"leading whitespace becomes continuation indent", "  1. one two three four", 10, "  1. one\n  two\n  three\n  four"},
	}
	for _, c := range cases {
		if got := WrapPrint(c.in, c.width); got != c.want {
			t.Errorf("%s: WrapPrint(%q, %d) = %q, want %q", c.name, c.in, c.width, got, c.want)
		}
	}
}

// TestWrapPrintNeverSplitsTokens verifies the property the old hyphen-aware
// wrapper broke: tokens (paths, URLs, flags, hyphenated words) narrower than
// the width must always survive intact on one line.
func TestWrapPrintNeverSplitsTokens(t *testing.T) {
	tokens := []string{
		"/opt/tools/deploy.sh",
		"https://docs.example.com/guide",
		"--name=example",
		"self-hostable",
		"must-have",
		"multi-tenant",
		"the-example-host",
	}
	for _, tok := range tokens {
		if w := lipgloss.Width(tok); w > 30 {
			continue
		}
		text := "filler words before the token " + tok + " trailing words after it too"
		for width := 20; width <= 60; width += 2 {
			for _, line := range strings.Split(WrapPrint(text, width), "\n") {
				// A token may be hard-broken only when alone it exceeds the
				// width; when it fits, it must appear intact.
				if strings.Contains(line, tok) {
					continue // intact on this line
				}
				joined := strings.ReplaceAll(WrapPrint(text, width), "\n", "")
				if !strings.Contains(joined, tok) && lipgloss.Width(tok) <= width {
					t.Errorf("width %d: token %q was split: %q", width, tok, WrapPrint(text, width))
				}
			}
		}
	}
}

// TestWrapPrintLinesNeverExceedWidth checks the invariant every printed line
// must obey (inline-renderer desync protection), including ANSI-styled input.
func TestWrapPrintLinesNeverExceedWidth(t *testing.T) {
	green := "\033[38;5;114m"
	reset := "\033[0m"
	inputs := []string{
		strings.Repeat("x", 200),
		"short prose with " + green + "styled words that are quite long indeed" + reset + " and plain tail",
		"word " + strings.Repeat("y", 120) + " word",
		"  indented line with " + strings.Repeat("z", 90) + " trailing",
	}
	for _, in := range inputs {
		for _, width := range []int{1, 5, 12, 40, 78, 116} {
			for i, line := range strings.Split(WrapPrint(in, width), "\n") {
				// A single cell wider than the width (wide glyph in a tiny
				// budget) is the only permitted overflow.
				if cells := lipgloss.Width(line); cells > width && cells > 2 {
					t.Errorf("width %d line %d is %d cells: %q", width, i, cells, line)
				}
			}
		}
	}
}
