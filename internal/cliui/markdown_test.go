package cliui

import (
	"errors"
	"strings"
	"testing"

	glamouransi "charm.land/glamour/v2/ansi"

	"github.com/aetherbird/sandbar/internal/config"
	uxtheme "github.com/aetherbird/sandbar/internal/ui/theme"
)

// stylesFor builds a Styles value with an explicit profile so render tests do
// not depend on the test runner's terminal environment.
func stylesFor(t *testing.T, themeID string, dark bool, profile Profile) *Styles {
	t.Helper()
	palette, err := uxtheme.Resolve(themeID, dark)
	if err != nil {
		t.Fatal(err)
	}
	return &Styles{
		requestedTheme: themeID,
		palette:        palette,
		colorMode:      config.ColorModeAlways,
		colors:         profile != ProfileAscii,
		darkBackground: dark,
		profile:        profile,
	}
}

func TestMarkdownRendererCachesPerStyleConfig(t *testing.T) {
	light := stylesFor(t, "light", false, ProfileANSI256)
	dark := stylesFor(t, "dark", true, ProfileANSI256)
	plain := stylesFor(t, "dark", true, ProfileAscii)

	var renderer MarkdownRenderer
	lightOutput := renderer.Render(light, "# Sandbar\n\n`code`")
	lightRenderer := renderer.renderer
	if renderer.key.themeID != "light" || renderer.key.styleJSON == "" {
		t.Fatalf("light cache key = %+v", renderer.key)
	}

	darkOutput := renderer.Render(dark, "# Sandbar\n\n`code`")
	darkRenderer := renderer.renderer
	if renderer.key.themeID != "dark" {
		t.Fatalf("dark cache key = %+v", renderer.key)
	}
	if darkRenderer == lightRenderer {
		t.Fatal("light and dark themes reused the same Glamour renderer")
	}
	if darkOutput == lightOutput {
		t.Fatal("light and dark Markdown output should use different palette escapes")
	}

	// The ASCII profile passes text through verbatim and must not touch the
	// cache at all.
	plainOutput := renderer.Render(plain, "# Sandbar\n\n`code`")
	if plainOutput != "# Sandbar\n\n`code`" {
		t.Fatalf("ASCII profile must pass markdown through verbatim, got %q", plainOutput)
	}
	if renderer.renderer != darkRenderer {
		t.Fatal("ASCII passthrough rebuilt the cached renderer")
	}
	if strings.Contains(plainOutput, "\x1b[") {
		t.Fatalf("ASCII Markdown contains SGR output: %q", plainOutput)
	}
}

// TestMarkdownRendererDownsampledPalette pins the exact 256-color escapes for
// known theme slots: the dark palette's accent (#60a5fa) downsamples to xterm
// 75 and the default success (#10b981) to xterm 36. Truecolor profiles keep
// the exact hex escape. The same theme id across the two profiles must rebuild
// the renderer, because the palette colors differ per profile.
func TestMarkdownRendererDownsampledPalette(t *testing.T) {
	dark256 := stylesFor(t, "dark", true, ProfileANSI256)
	darkTC := stylesFor(t, "dark", true, ProfileTrueColor)

	var renderer MarkdownRenderer
	out256 := renderer.Render(dark256, "# Sandbar\n\n`code`")
	outTC := renderer.Render(darkTC, "# Sandbar\n\n`code`")

	if !strings.Contains(out256, "38;5;75") {
		t.Fatalf("256-profile heading lacks downsampled accent escape: %q", out256)
	}
	if !strings.Contains(out256, "38;5;36") {
		t.Fatalf("256-profile code lacks downsampled success escape: %q", out256)
	}
	if !strings.Contains(outTC, "38;2;96;165;250") {
		t.Fatalf("truecolor heading lacks exact accent hex: %q", outTC)
	}
	if strings.Contains(outTC, "38;5;75") {
		t.Fatalf("truecolor output carries a 256-color escape: %q", outTC)
	}
	if strings.Contains(out256, "38;2;96;165;250") {
		t.Fatalf("256-profile output carries a truecolor escape: %q", out256)
	}
}

func TestMarkdownRendererPlainTextPassthrough(t *testing.T) {
	dark := stylesFor(t, "dark", true, ProfileANSI256)
	var renderer MarkdownRenderer
	text := "plain conversational answer, no markdown structure at all"
	if got := renderer.Render(dark, text); got != text {
		t.Fatalf("non-markdown text must pass through verbatim, got %q", got)
	}
	if renderer.renderer != nil {
		t.Fatal("non-markdown text must not build a Glamour renderer")
	}
}

// TestMarkdownRendererConstructionErrorFallsBackToRaw pins the fail-soft
// guard: when the renderer cannot be built, Render returns the raw text and
// leaves no half-built renderer behind.
func TestMarkdownRendererConstructionErrorFallsBackToRaw(t *testing.T) {
	dark := stylesFor(t, "dark", true, ProfileANSI256)
	var renderer MarkdownRenderer
	renderer.build = func(*Styles) (markdownTermRenderer, error) {
		return nil, errors.New("boom")
	}
	text := "# Sandbar"
	if got := renderer.Render(dark, text); got != text {
		t.Fatalf("construction failure must fall back to raw text, got %q", got)
	}
	if renderer.renderer != nil || renderer.key != (markdownRendererKey{}) {
		t.Fatalf("failed build must not stick in the cache: key=%+v", renderer.key)
	}
}

// errRenderer fails every Render call; it pins the render-error fallback.
type errRenderer struct{ err error }

func (e errRenderer) Render(string) (string, error) { return "", e.err }

// TestMarkdownRendererRenderErrorFallsBackToRaw pins the render-error guard:
// when the cached renderer fails, Render returns the raw text.
func TestMarkdownRendererRenderErrorFallsBackToRaw(t *testing.T) {
	dark := stylesFor(t, "dark", true, ProfileANSI256)
	var renderer MarkdownRenderer
	renderer.build = func(*Styles) (markdownTermRenderer, error) {
		return errRenderer{err: errors.New("boom")}, nil
	}
	text := "# Sandbar\n\n```go\nfunc main() {}\n```"
	if got := renderer.Render(dark, text); got != text {
		t.Fatalf("render failure must fall back to the raw text, got %q", got)
	}
}

// TestMarkdownHeadingsShareAccentColor pins H4-H6 theming at the style-config
// level: every heading level must reference the palette accent, so glamour's
// stock non-theme heading colors can't leak through.
func TestMarkdownHeadingsShareAccentColor(t *testing.T) {
	dark := stylesFor(t, "dark", true, ProfileTrueColor)
	style := markdownStyle(dark)
	accent := dark.Palette().Tokens.Accent
	for name, block := range map[string]*glamouransi.StyleBlock{
		"heading": &style.Heading, "h1": &style.H1, "h2": &style.H2,
		"h3": &style.H3, "h4": &style.H4, "h5": &style.H5, "h6": &style.H6,
	} {
		if block.Color == nil || *block.Color != accent {
			t.Errorf("%s heading color = %v, want palette accent %s", name, block.Color, accent)
		}
	}
}
