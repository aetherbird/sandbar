package cliui

import (
	"encoding/json"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"

	uxtheme "github.com/aetherbird/sandbar/internal/ui/theme"
)

// markdownRendererKey identifies the exact renderer configuration: the theme
// id plus the JSON-encoded style config (empty for the fixed ASCII config).
// The JSON is the configuration Glamour actually consumes — the color profile
// only matters through the palette colors already downsampled into it — so
// caching on the JSON keeps renderers correct across profiles without carrying
// the profile dimension Glamour v2 no longer knows about. The ascii flag
// selects the zero-ANSI structural renderer for the ASCII profile.
type markdownRendererKey struct {
	themeID   string
	styleJSON string
	ascii     bool
}

// markdownTermRenderer is the minimal surface of a glamour term renderer; the
// interface lets tests inject a failing renderer to pin the render-error
// fallback guard.
type markdownTermRenderer interface {
	Render(string) (string, error)
}

// MarkdownRenderer caches Glamour renderers by concrete style configuration.
// It is safe for streaming and UI goroutines to share.
// Glamour renders unwrapped (WithWordWrap(0)): its reflow-based wrapper
// breaks words at hyphens — splitting flags, paths, and hyphenated words —
// so all line breaking is delegated to WrapPrint, which only breaks at
// whitespace.
type MarkdownRenderer struct {
	mu       sync.Mutex
	key      markdownRendererKey
	renderer markdownTermRenderer
	// build constructs the cached Glamour renderer; overridable in tests to
	// exercise the construction- and render-error fallbacks.
	build func(*Styles) (markdownTermRenderer, error)
}

func defaultMarkdownBuild(s *Styles) (markdownTermRenderer, error) {
	return buildMarkdown(s, "terminal16m")
}

// buildASCIIStyle is the zero-ANSI structural renderer for the ASCII profile:
// no colors, no SGR escapes, but markdown delimiters still render into plain
// structure (headings without # markers, bold/italic/code de-marked, list
// bullets normalized) instead of passing raw source through. Glamour's stock
// ASCII config re-emits the delimiters (it targets no-TTY logs that mirror
// markdown), so the emphasis/code/heading prefixes are cleared here.
func buildASCIIStyle() glamouransi.StyleConfig {
	style := styles.ASCIIStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	empty := glamouransi.StylePrimitive{}
	style.Emph = empty
	style.Strong = empty
	style.Strikethrough = empty
	style.Code.StylePrimitive = empty
	style.Heading.StylePrimitive = glamouransi.StylePrimitive{BlockSuffix: "\n"}
	style.H1.StylePrimitive = glamouransi.StylePrimitive{}
	style.H2.StylePrimitive = glamouransi.StylePrimitive{}
	style.H3.StylePrimitive = glamouransi.StylePrimitive{}
	style.H4.StylePrimitive = glamouransi.StylePrimitive{}
	style.H5.StylePrimitive = glamouransi.StylePrimitive{}
	style.H6.StylePrimitive = glamouransi.StylePrimitive{}
	return style
}

// buildMarkdownStyle constructs a term renderer from an explicit style config
// and chroma formatter.
func buildMarkdownStyle(style glamouransi.StyleConfig, chromaFormatter string) (*glamour.TermRenderer, error) {
	return glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(0),
		glamour.WithChromaFormatter(chromaFormatter),
	)
}

func buildMarkdown(s *Styles, chromaFormatter string) (*glamour.TermRenderer, error) {
	return buildMarkdownStyle(markdownStyle(s), chromaFormatter)
}

// looksLikeMarkdown reports whether text carries markdown structure worth
// rendering. Plain conversational text passes through verbatim so output
// keeps its historical shape.
func looksLikeMarkdown(text string) bool {
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "#"), // headings
			strings.HasPrefix(t, "```"), // fenced code
			strings.HasPrefix(t, ">"),   // blockquote
			strings.HasPrefix(t, "- "),  // list bullets
			strings.HasPrefix(t, "* "),
			strings.HasPrefix(t, "1. "), // ordered lists (1. covers 10. too)
			strings.HasPrefix(t, "- ["), // task list
			strings.Contains(t, "|---"): // table separator
			return true
		}
	}
	return false
}

// markdownStyle builds a glamour style config from the active palette. Every
// color already carries the active profile's fidelity: hex on truecolor
// terminals, the nearest xterm-256 index on 256-color ones.
func markdownStyle(s *Styles) glamouransi.StyleConfig {
	palette := s.Palette()
	style := styles.DarkStyleConfig
	if palette.Scheme == uxtheme.SchemeLight {
		style = styles.LightStyleConfig
	}
	zero := uint(0)
	style.Document.Margin = &zero
	t := palette.Tokens
	color := func(hex string) *string {
		if hex == "" {
			return nil
		}
		v := s.downsampleColor(hex)
		return &v
	}
	style.Document.Color = color(t.Text1)
	style.Text.Color = color(t.Text1)
	style.Heading.Color = color(t.Accent)
	style.H1.Color = color(t.Accent)
	style.H2.Color = color(t.Accent)
	style.H3.Color = color(t.Accent)
	style.H4.Color = color(t.Accent)
	style.H5.Color = color(t.Accent)
	style.H6.Color = color(t.Accent)
	style.Link.Color = color(t.Accent)
	style.LinkText.Color = color(t.Accent)
	style.Emph.Color = color(t.Text2)
	style.Strong.Color = color(t.Text1)
	style.BlockQuote.Color = color(t.Text2)
	// Inline code is foreground-only: a Surface2 background chip reads as a
	// white box washing out on light-theme (paper/cream) terminals, and
	// 256-color downsampling can shift it to pure white. The colored text is
	// distinct from body copy without relying on the terminal background.
	style.Code.Color = color(t.Success)
	style.Code.BackgroundColor = nil
	// Fenced blocks keep their panel background: they are full-width rows
	// whose fg/bg pair is theme-controlled, not chips on the terminal bg.
	style.CodeBlock.BackgroundColor = color(t.Surface1)
	style.HorizontalRule.Color = color(t.Border2)
	return style
}

// Reset discards the cached Glamour renderer.
func (m *MarkdownRenderer) Reset() {
	m.mu.Lock()
	m.key = markdownRendererKey{}
	m.renderer = nil
	m.mu.Unlock()
}

// Render renders Markdown with the supplied immutable presentation style.
// The output is unwrapped; callers wrap it with WrapPrint at their own
// printable width.
//
// Guards, in order: empty or non-markdown text passes through verbatim; the
// ASCII profile renders structurally through a zero-ANSI Glamour config, so
// --color never output carries no SGR but never shows raw markdown source
// either; renderer construction and render errors both fall back to the raw
// text so rendering never loses the message.
func (m *MarkdownRenderer) Render(s *Styles, text string) string {
	if text == "" || !looksLikeMarkdown(text) {
		return text
	}
	ascii := !s.ColorsEnabled()
	key := markdownRendererKey{ascii: ascii}
	var build func(*Styles) (markdownTermRenderer, error)
	if ascii {
		key.themeID = "ascii"
		build = func(*Styles) (markdownTermRenderer, error) {
			return buildMarkdownStyle(buildASCIIStyle(), "terminal16m")
		}
	} else {
		key.themeID = s.Palette().ID
		if styleJSON, err := json.Marshal(markdownStyle(s)); err == nil {
			key.styleJSON = string(styleJSON)
		}
		build = m.build
		if build == nil {
			build = defaultMarkdownBuild
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.renderer == nil || m.key != key {
		renderer, err := build(s)
		if err != nil {
			// Fail soft with no renderer; callers degrade to verbatim text.
			m.renderer = nil
			m.key = markdownRendererKey{}
			return text
		}
		m.renderer = renderer
		m.key = key
	}
	rendered, err := m.renderer.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(rendered, "\n")
}
