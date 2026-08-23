package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/aetherbird/sandbar/internal/backend"
)

// ── content margin ───────────────────────────────────────────────────────────

// TestIndentGlamourOutputMarginsANSILines pins the indenter's contract: every
// non-empty (visible) line — styled or plain — gets the content margin, runs
// of blank lines collapse to one, and leading/trailing blanks are dropped.
func TestIndentGlamourOutputMarginsANSILines(t *testing.T) {
	in := "\nplain line\n\n" + "\x1b[1mstyled line\x1b[0m\n\n\n\x1b[0m\n\n"
	got := indentGlamourOutput(in)
	want := "  plain line\n\n  \x1b[1mstyled line\x1b[0m"
	if got != want {
		t.Fatalf("indented output = %q, want %q", got, want)
	}
}

// TestProseMarginFitsPrintWidth verifies the width accounting by construction:
// prose wrapped at printWidth()-contentMargin then prefixed with the margin
// yields lines that never exceed printWidth — including a 200-cell unbreakable
// token, which must be hard-broken rather than overflow.
func TestProseMarginFitsPrintWidth(t *testing.T) {
	m := appModel{width: 80}
	token := strings.Repeat("u", 200)
	text := "short intro paragraph that wraps nicely and a monster token " + token + " then more words"

	for name, out := range map[string]string{
		"plain prose (marginProse)":   m.marginProse(text),
		"stored transcript (glamour)": renderStoredAssistant(text, m.printWidth()),
	} {
		for i, line := range strings.Split(out, "\n") {
			if cells := lipgloss.Width(line); cells > m.printWidth() {
				t.Errorf("%s line %d is %d cells > printWidth %d: %q", name, i, cells, m.printWidth(), line)
			}
			if line != "" && !strings.HasPrefix(line, "  ") {
				t.Errorf("%s line %d missing content margin: %q", name, i, line)
			}
		}
		// Hard-broken chunks carry the margin between them, so count the token
		// cells rather than looking for the contiguous run.
		if got := strings.Count(out, "u"); got != len(token) {
			t.Errorf("%s lost token content: %d of %d u's", name, got, len(token))
		}
	}
}

// TestWrapForPrintHardBreaksLongToken pins the WrapPrint switch: a token wider
// than the budget is hard-broken mid-token (Wordwrap let it overflow, which
// desynced the inline renderer) without losing any content.
func TestWrapForPrintHardBreaksLongToken(t *testing.T) {
	got := wrapForPrint(strings.Repeat("x", 200), 40)
	for i, line := range strings.Split(got, "\n") {
		if cells := lipgloss.Width(line); cells > 40 {
			t.Errorf("line %d is %d cells > 40", i, cells)
		}
	}
	if joined := strings.ReplaceAll(got, "\n", ""); joined != strings.Repeat("x", 200) {
		t.Errorf("hard wrap lost content: %d chars", len(joined))
	}
}

// TestFlushTokensFinalOutputMargined checks the plain-streaming final flush:
// every non-blank printed line carries the content margin and fits printWidth.
// (executeTeaCommandText renders bubbletea's internal print message via fmt,
// wrapping the body in "{...}" — strip that envelope before asserting.)
func TestFlushTokensFinalOutputMargined(t *testing.T) {
	m := appModel{width: 80}
	m.tokBuf = []byte("assistant prose line one\n\nassistant prose line two")
	out := executeTeaCommandText(t, m.flushTokens(true))
	out = strings.TrimPrefix(out, "{")
	out = strings.TrimSuffix(out, "}")
	for i, line := range strings.Split(out, "\n") {
		if cells := lipgloss.Width(line); cells > m.printWidth() {
			t.Errorf("line %d is %d cells > printWidth %d: %q", i, cells, m.printWidth(), line)
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "  ") {
			t.Errorf("prose line %d missing content margin: %q", i, line)
		}
	}
}

// ── /noformat ────────────────────────────────────────────────────────────────

// TestNoformatPrintsRawText pins the /noformat contract: a styled one-line
// header, then the raw markdown source with no ANSI styling, no content
// margin, and no blank-line collapsing — only hard wrapping at printWidth.
func TestNoformatPrintsRawText(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	token := strings.Repeat("u", 200)
	m.lastResponseRaw = "# Heading\n\n\n\nsome **bold** text and `code`, plus a monster token " + token

	out := executeTeaCommandText(t, runNoformatCommand(&m, slashInvocation{}))
	if !strings.Contains(stripANSI(out), "previous response, unformatted:") {
		t.Fatalf("missing header:\n%s", out)
	}

	lines := strings.Split(out, "\n")
	var body []string
	for i, line := range lines {
		if strings.Contains(stripANSI(line), "previous response, unformatted:") {
			body = lines[i+1:]
			break
		}
	}
	if len(body) == 0 {
		t.Fatalf("no body after header:\n%s", out)
	}
	raw := strings.Join(body, "\n")
	for _, want := range []string{"# Heading", "**bold**", "`code`"} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw markdown source missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "\x1b[") {
		t.Errorf("unformatted body contains ANSI styling:\n%q", raw)
	}
	// No blank-line collapsing: the three blank source lines survive verbatim.
	if !strings.Contains(raw, "\n\n\n") {
		t.Errorf("blank lines were collapsed in unformatted output:\n%q", raw)
	}
	for i, line := range body {
		if cells := lipgloss.Width(line); cells > m.printWidth() {
			t.Errorf("body line %d is %d cells > printWidth %d (renderer desync risk): %q", i, cells, m.printWidth(), line)
		}
		if strings.TrimLeft(line, " ") != line {
			t.Errorf("body line %d must be flush-left (no margin): %q", i, line)
		}
	}
	if !strings.Contains(strings.ReplaceAll(raw, "\n", ""), token) {
		t.Error("hard wrap dropped token content")
	}
}

// TestNoformatEmptyHint verifies the empty case prints the muted hint instead
// of an empty body.
func TestNoformatEmptyHint(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	out := executeTeaCommandText(t, runNoformatCommand(&m, slashInvocation{}))
	if !strings.Contains(stripANSI(out), "no previous response in this session") {
		t.Fatalf("missing empty-case hint:\n%s", out)
	}
	if strings.Contains(stripANSI(out), "unformatted") {
		t.Errorf("empty case must not print the header:\n%s", out)
	}
}

// TestLastResponseRawLifecycle covers the raw-text tracking: appended per
// token during a turn, reset when the next turn starts, and recovered from
// transcript replay of the last assistant message.
func TestLastResponseRawLifecycle(t *testing.T) {
	m := newModel(&session{backend: &fakeCLIBackend{}})
	m.width = 80
	m.streamGen = 1
	m.streamCh = make(chan streamItem, 8)

	upd, _ := m.Update(streamItem{gen: 1, kind: "token", content: "hello "})
	m = upd.(appModel)
	upd, _ = m.Update(streamItem{gen: 1, kind: "token", content: "raw"})
	m = upd.(appModel)
	if m.lastResponseRaw != "hello raw" {
		t.Fatalf("lastResponseRaw after tokens = %q, want %q", m.lastResponseRaw, "hello raw")
	}

	// Starting the next user turn clears the stale response.
	m.startStream("next question", nil)
	if m.lastResponseRaw != "" {
		t.Fatalf("lastResponseRaw after startStream = %q, want empty", m.lastResponseRaw)
	}

	// Transcript replay of the last assistant message recovers it.
	m2 := newModel(&session{backend: &fakeCLIBackend{}})
	m2.width = 80
	msgs := []backend.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "old a"},
		{Role: "user", Content: "last q"},
		{Role: "assistant", Content: "last a"},
	}
	m2.renderLastExchange(msgs)
	if m2.lastResponseRaw != "last a" {
		t.Fatalf("lastResponseRaw after replay = %q, want %q", m2.lastResponseRaw, "last a")
	}
}
