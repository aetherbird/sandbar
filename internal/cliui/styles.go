// Package cliui owns Sandbar's terminal presentation primitives.
//
// It deliberately has no Bubble Tea model or application state: cmd/sandbar owns
// interaction, while this package maps shared theme tokens onto terminal
// styles and renderers.
package cliui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"sandbar/internal/config"
	uxtheme "sandbar/internal/ui/theme"
)

// Semantic roles let callers describe intent without embedding terminal color
// numbers or palette-specific values in application code.
const (
	RoleAccent       = "accent"
	RoleAccentStrong = "accent-strong"
	RoleMuted        = "muted"
	RoleText         = "text"
	RoleWarning      = "warning"
	RoleDanger       = "danger"
	RoleSecondary    = "secondary"
	RoleThinking     = "thinking"
	RoleSuccess      = "success"
	RoleBorder       = "border"
	RoleSurface      = "surface"
)

// Styles is an immutable terminal presentation specification. The Lip Gloss
// renderer is configured once at construction, so a runtime theme change swaps
// the complete *Styles value instead of mutating colors piecemeal.
type Styles struct {
	requestedTheme string
	palette        uxtheme.Palette
	colorMode      string
	colors         bool
	darkBackground bool
	renderer       *lipgloss.Renderer
}

// PreferredTheme returns the first configured theme in documented precedence:
// explicit flag, environment override, persisted preference, then system.
func PreferredTheme(flagValue, envValue, configured string) string {
	for _, candidate := range []string{flagValue, envValue, configured, uxtheme.System} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return uxtheme.System
}

// TerminalSupportsColor applies the CLI's explicit color mode and conventional
// automatic terminal checks. An explicit "always" overrides NO_COLOR; auto
// honors it.
func TerminalSupportsColor(mode string, output *os.File) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case config.ColorModeAlways:
		return true
	case config.ColorModeNever:
		return false
	}
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return output != nil && term.IsTerminal(int(output.Fd()))
}

// DetectDarkBackground asks Lip Gloss which system base palette best matches
// the output terminal.
func DetectDarkBackground(output *os.File) bool {
	var writer io.Writer = io.Discard
	if output != nil {
		writer = output
	}
	return lipgloss.NewRenderer(writer).HasDarkBackground()
}

// NewStyles resolves a shared Sandbar palette and configures a terminal
// renderer for the requested color capability.
func NewStyles(name, colorMode string, darkBackground bool, output *os.File) (*Styles, error) {
	requested := strings.ToLower(strings.TrimSpace(name))
	if requested == "" {
		requested = uxtheme.System
	}
	palette, err := uxtheme.Resolve(requested, darkBackground)
	if err != nil {
		return nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(colorMode))
	switch mode {
	case config.ColorModeAlways, config.ColorModeNever:
	default:
		mode = config.ColorModeAuto
	}

	var writer io.Writer = io.Discard
	if output != nil {
		writer = output
	}
	renderer := lipgloss.NewRenderer(writer)
	detectedProfile := renderer.ColorProfile()
	if mode == config.ColorModeAlways {
		// Explicit color overrides NO_COLOR, but must not override the terminal's
		// actual 16/256/truecolor capability.
		detectedProfile = renderer.Output().ColorProfile()
	}
	colors := TerminalSupportsColor(mode, output)
	renderer.SetColorProfile(profileForColorMode(mode, detectedProfile, colors))
	renderer.SetHasDarkBackground(darkBackground)

	return &Styles{
		requestedTheme: requested,
		palette:        palette,
		colorMode:      mode,
		colors:         colors,
		darkBackground: darkBackground,
		renderer:       renderer,
	}, nil
}

// DefaultStyles returns the catalog-validated automatic dark terminal style.
// It is used only as a defensive bootstrap before main loads client settings.
func DefaultStyles(output *os.File) *Styles {
	s, err := NewStyles(uxtheme.System, config.ColorModeAuto, true, output)
	if err != nil {
		panic(fmt.Sprintf("invalid built-in CLI theme catalog: %v", err))
	}
	return s
}

func (s *Styles) RequestedTheme() string        { return s.requestedTheme }
func (s *Styles) Palette() uxtheme.Palette      { return s.palette }
func (s *Styles) ColorMode() string             { return s.colorMode }
func (s *Styles) ColorsEnabled() bool           { return s.colors }
func (s *Styles) DarkBackground() bool          { return s.darkBackground }
func (s *Styles) ColorProfile() termenv.Profile { return s.renderer.ColorProfile() }

// profileForColorMode preserves real terminal capability in both auto and
// always modes. When color is explicitly forced to a non-TTY, detection can
// only report ASCII; ANSI256 is the documented compatibility fallback instead
// of assuming TrueColor support.
func profileForColorMode(mode string, detected termenv.Profile, colors bool) termenv.Profile {
	if !colors {
		return termenv.Ascii
	}
	if mode == config.ColorModeAlways && detected == termenv.Ascii {
		return termenv.ANSI256
	}
	return detected
}

func (s *Styles) color(role string) string {
	t := s.palette.Tokens
	switch role {
	case RoleAccent, RoleAccentStrong:
		return t.Accent
	case RoleMuted:
		return t.Text3
	case RoleText:
		return t.Text1
	case RoleWarning:
		return t.Warning
	case RoleDanger:
		return t.Danger
	case RoleSecondary, RoleThinking:
		return t.Text2
	case RoleSuccess:
		return t.Success
	case RoleBorder:
		return t.Border2
	case RoleSurface:
		return t.Surface1
	default:
		return role
	}
}

// Color resolves a semantic role to the active palette value.
func (s *Styles) Color(role string) string { return s.color(role) }

// Style returns a foreground style for a semantic role.
func (s *Styles) Style(role string) lipgloss.Style {
	base := s.renderer.NewStyle()
	if color := s.color(role); color != "" {
		base = base.Foreground(lipgloss.Color(color))
	}
	return base
}

// Background returns a background style for a semantic role.
func (s *Styles) Background(role string) lipgloss.Style {
	base := s.renderer.NewStyle()
	if color := s.color(role); color != "" {
		base = base.Background(lipgloss.Color(color))
	}
	return base
}

// ApplyTextarea gives all textarea states the same semantic palette.
func (s *Styles) ApplyTextarea(ta *textarea.Model) {
	if ta == nil {
		return
	}
	base := s.Style(RoleText)
	ta.FocusedStyle.Base = base
	ta.FocusedStyle.Text = s.Style(RoleText)
	ta.FocusedStyle.Prompt = s.Style(RoleAccent).Bold(true)
	ta.FocusedStyle.Placeholder = s.Style(RoleMuted).Italic(true)
	ta.FocusedStyle.CursorLine = base
	ta.FocusedStyle.EndOfBuffer = s.Style(RoleMuted)
	ta.BlurredStyle = ta.FocusedStyle
	ta.Cursor.Style = s.Style(RoleAccent)
}

// FormatThemeList returns a stable, script-friendly theme catalog.
func FormatThemeList() string {
	var b strings.Builder
	b.WriteString("system\tSystem (Auto)\n")
	for _, p := range uxtheme.List() {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", p.ID, p.Label, p.Group)
	}
	return strings.TrimRight(b.String(), "\n")
}
