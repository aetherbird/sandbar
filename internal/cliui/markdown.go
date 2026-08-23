package cliui

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/muesli/termenv"

	uxtheme "github.com/aetherbird/sandbar/internal/ui/theme"
)

type markdownRendererKey struct {
	themeID string
	profile termenv.Profile
}

// MarkdownRenderer caches Glamour renderers by concrete palette and terminal
// color profile. It is safe for streaming and UI goroutines to share.
// Glamour renders unwrapped (WithWordWrap(0)): its reflow-based wrapper
// breaks words at hyphens — splitting flags, paths, and hyphenated words —
// so all line breaking is delegated to WrapPrint, which only breaks at
// whitespace.
type MarkdownRenderer struct {
	mu       sync.Mutex
	key      markdownRendererKey
	renderer *glamour.TermRenderer
}

func markdownStyle(s *Styles) glamouransi.StyleConfig {
	if !s.ColorsEnabled() {
		style := styles.NoTTYStyleConfig
		zero := uint(0)
		style.Document.Margin = &zero
		return style
	}
	palette := s.Palette()
	style := styles.DarkStyleConfig
	if palette.Scheme == uxtheme.SchemeLight {
		style = styles.LightStyleConfig
	}
	zero := uint(0)
	style.Document.Margin = &zero
	t := palette.Tokens
	style.Document.Color = &t.Text1
	style.Text.Color = &t.Text1
	style.Heading.Color = &t.Accent
	style.H1.Color = &t.Accent
	style.H2.Color = &t.Accent
	style.H3.Color = &t.Accent
	style.H4.Color = &t.Accent
	style.H5.Color = &t.Accent
	style.H6.Color = &t.Accent
	style.Link.Color = &t.Accent
	style.LinkText.Color = &t.Accent
	style.Emph.Color = &t.Text2
	style.Strong.Color = &t.Text1
	style.BlockQuote.Color = &t.Text2
	style.Code.Color = &t.Success
	style.Code.BackgroundColor = &t.Surface2
	style.CodeBlock.BackgroundColor = &t.Surface1
	style.HorizontalRule.Color = &t.Border2
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
func (m *MarkdownRenderer) Render(s *Styles, text string) string {
	if text == "" {
		return text
	}
	profile := s.ColorProfile()
	key := markdownRendererKey{themeID: s.Palette().ID, profile: profile}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.renderer == nil || m.key != key {
		renderer, err := glamour.NewTermRenderer(
			glamour.WithStyles(markdownStyle(s)),
			glamour.WithWordWrap(0),
		)
		if err != nil {
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
