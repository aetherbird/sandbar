package cliui

import (
	"strings"
	"testing"

	glamouransi "github.com/charmbracelet/glamour/ansi"
	"github.com/muesli/termenv"

	"sandbar/internal/config"
)

func TestMarkdownRendererCacheTracksThemeAndColorProfile(t *testing.T) {
	light, err := NewStyles("light", config.ColorModeAlways, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	dark, err := NewStyles("dark", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := NewStyles("dark", config.ColorModeNever, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	var renderer MarkdownRenderer
	lightOutput := renderer.Render(light, "# Sandbar\n\n`code`")
	lightRenderer := renderer.renderer
	if renderer.key.themeID != "light" || renderer.key.profile != termenv.ANSI256 {
		t.Fatalf("light cache key = %+v", renderer.key)
	}

	darkOutput := renderer.Render(dark, "# Sandbar\n\n`code`")
	darkRenderer := renderer.renderer
	if renderer.key.themeID != "dark" || renderer.key.profile != termenv.ANSI256 {
		t.Fatalf("dark cache key = %+v", renderer.key)
	}
	if darkRenderer == lightRenderer {
		t.Fatal("light and dark themes reused the same Glamour renderer")
	}
	if darkOutput == lightOutput {
		t.Fatal("light and dark Markdown output should use different palette escapes")
	}

	plainOutput := renderer.Render(plain, "# Sandbar\n\n`code`")
	if renderer.key.themeID != "dark" || renderer.key.profile != termenv.Ascii {
		t.Fatalf("plain cache key = %+v", renderer.key)
	}
	if renderer.renderer == darkRenderer {
		t.Fatal("profile change reused the color Glamour renderer")
	}
	if strings.Contains(plainOutput, "\x1b[") {
		t.Fatalf("ASCII Markdown contains SGR output: %q", plainOutput)
	}
}

func TestForcedColorProfilePreservesTerminalCapability(t *testing.T) {
	tests := []struct {
		name     string
		detected termenv.Profile
		colors   bool
		want     termenv.Profile
	}{
		{name: "disabled", detected: termenv.TrueColor, colors: false, want: termenv.Ascii},
		{name: "ansi", detected: termenv.ANSI, colors: true, want: termenv.ANSI},
		{name: "ansi256", detected: termenv.ANSI256, colors: true, want: termenv.ANSI256},
		{name: "truecolor", detected: termenv.TrueColor, colors: true, want: termenv.TrueColor},
		{name: "non tty fallback", detected: termenv.Ascii, colors: true, want: termenv.ANSI256},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profileForColorMode(config.ColorModeAlways, tt.detected, tt.colors); got != tt.want {
				t.Fatalf("profile = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMarkdownHeadingsShareAccentColor pins H4-H6 theming at the style-config
// level: every heading level must reference the palette accent, so glamour's
// stock non-theme heading colors can't leak through.
func TestMarkdownHeadingsShareAccentColor(t *testing.T) {
	dark, err := NewStyles("dark", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
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
