// Package cliui owns Sandbar's terminal presentation primitives.
//
// It deliberately has no Bubble Tea model or application state: cmd/sandbar owns
// interaction, while this package maps shared theme tokens onto terminal
// styles and renderers.
package cliui

import (
	"fmt"
	"image/color"
	"os"
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/aetherbird/sandbar/internal/config"
	uxtheme "github.com/aetherbird/sandbar/internal/ui/theme"
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

// Profile is the terminal color capability styles render into. It mirrors the
// three-rung ladder the legacy codebase validated: truecolor, 256-color, or
// no color at all.
type Profile int

const (
	ProfileTrueColor Profile = iota
	ProfileANSI256
	ProfileAscii
)

func (p Profile) String() string {
	switch p {
	case ProfileTrueColor:
		return "truecolor"
	case ProfileANSI256:
		return "256"
	default:
		return "ascii"
	}
}

// Style wraps a lipgloss v2 style with the color capability of the Styles set
// that produced it. Callers chain attributes (Bold, Italic) exactly as they
// did with lipgloss styles; Render already carries the downsampled palette for
// the active profile. When color is disabled every attribute is a no-op, so
// `--color never` output contains no SGR at all.
type Style struct {
	lg    lipgloss.Style
	plain bool
}

// Render renders styled text. With color disabled it returns the plain text
// joined exactly as lipgloss joins block strings (single spaces).
func (s Style) Render(strs ...string) string {
	if s.plain {
		return strings.Join(strs, " ")
	}
	return s.lg.Render(strs...)
}

// Bold enables bold text; a no-op when color is disabled.
func (s Style) Bold(v bool) Style {
	if !s.plain {
		s.lg = s.lg.Bold(v)
	}
	return s
}

// Italic enables italic text; a no-op when color is disabled.
func (s Style) Italic(v bool) Style {
	if !s.plain {
		s.lg = s.lg.Italic(v)
	}
	return s
}

// Styles is an immutable terminal presentation specification. A runtime theme
// change swaps the complete *Styles value instead of mutating colors
// piecemeal.
type Styles struct {
	requestedTheme string
	palette        uxtheme.Palette
	colorMode      string
	colors         bool
	darkBackground bool
	profile        Profile
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

// detectProfile resolves the terminal color capability for a color mode. It is
// pure so tests can exercise the full environment matrix without a pty:
//
//   - never: ProfileAscii — no SGR anywhere.
//   - always: at least ProfileANSI256, even off a TTY or on a dumb terminal.
//     The explicit force overrides NO_COLOR but honors a truecolor
//     advertisement, so real capability is never exceeded.
//   - auto: NO_COLOR set to any value (including empty), TERM=dumb, an empty
//     TERM, or a non-TTY force ProfileAscii; COLORTERM=truecolor|24bit
//     advertises ProfileTrueColor; everything else assumes 256 colors.
func detectProfile(mode string, noColorSet bool, term, colorterm string, isTTY bool) Profile {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case config.ColorModeNever:
		return ProfileAscii
	case config.ColorModeAlways:
		if strings.EqualFold(term, "dumb") || !isTTY {
			return ProfileANSI256
		}
		if strings.Contains(colorterm, "truecolor") || strings.Contains(colorterm, "24bit") {
			return ProfileTrueColor
		}
		return ProfileANSI256
	}
	if noColorSet || strings.EqualFold(term, "dumb") || term == "" || !isTTY {
		return ProfileAscii
	}
	if strings.Contains(colorterm, "truecolor") || strings.Contains(colorterm, "24bit") {
		return ProfileTrueColor
	}
	return ProfileANSI256
}

// DetectDarkBackground asks Lip Gloss whether the output terminal reports a
// dark background. A nil output (no terminal to query) defaults to light,
// matching the historical renderer behavior.
func DetectDarkBackground(output *os.File) bool {
	if output == nil {
		return false
	}
	return lipgloss.HasDarkBackground(os.Stdin, output)
}

// NewStyles resolves a shared Sandbar palette and the terminal color
// capability for the requested color mode.
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
	_, noColor := os.LookupEnv("NO_COLOR")
	isTTY := output != nil && term.IsTerminal(int(output.Fd()))
	profile := detectProfile(mode, noColor, os.Getenv("TERM"), os.Getenv("COLORTERM"), isTTY)

	return &Styles{
		requestedTheme: requested,
		palette:        palette,
		colorMode:      mode,
		colors:         profile != ProfileAscii,
		darkBackground: darkBackground,
		profile:        profile,
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

func (s *Styles) RequestedTheme() string   { return s.requestedTheme }
func (s *Styles) Palette() uxtheme.Palette { return s.palette }
func (s *Styles) ColorMode() string        { return s.colorMode }
func (s *Styles) ColorsEnabled() bool      { return s.colors }
func (s *Styles) DarkBackground() bool     { return s.darkBackground }
func (s *Styles) ColorProfile() Profile    { return s.profile }

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

var hexRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func parseHex(hex string) (r, g, b int, err error) {
	if !hexRE.MatchString(hex) {
		return 0, 0, 0, fmt.Errorf("bad hex %q", hex)
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, err
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), nil
}

// HexToANSI256 maps 8-bit RGB to the nearest xterm-256 entry: the 6x6x6
// color cube (16-231) or the 24-step grayscale ramp (232-255). Grayscale
// candidates are weighted 3x on squared distance so the ramp only wins for
// genuinely achromatic colors.
func HexToANSI256(r, g, b int) int {
	lut := []int{0, 95, 135, 175, 215, 255}
	bestDist, best := 1<<30, 0
	for n := 0; n < 6; n++ {
		for m := 0; m < 6; m++ {
			for k := 0; k < 6; k++ {
				cr, cg, cb := lut[n], lut[m], lut[k]
				d := (cr-r)*(cr-r) + (cg-g)*(cg-g) + (cb-b)*(cb-b)
				if d < bestDist {
					bestDist, best = d, 16+36*n+6*m+k
				}
			}
		}
	}
	for i := 0; i < 24; i++ {
		gv := 8 + 10*i
		d := 3 * ((gv-r)*(gv-r) + (gv-g)*(gv-g) + (gv-b)*(gv-b))
		if d < bestDist {
			bestDist, best = d, 232+i
		}
	}
	return best
}

// downsampleColor returns a glamour-style color string for the active
// profile: hex on truecolor, the nearest xterm-256 index otherwise.
func (s *Styles) downsampleColor(hex string) string {
	if s.profile == ProfileANSI256 && strings.HasPrefix(hex, "#") {
		if r, g, b, err := parseHex(hex); err == nil {
			return strconv.Itoa(HexToANSI256(r, g, b))
		}
	}
	return hex
}

// colorValue resolves a semantic role to a color.Color for the active profile.
// Hex palette values are downsampled to the nearest xterm-256 index on
// 256-color terminals; the ASCII profile renders no color at all.
func (s *Styles) colorValue(role string) color.Color {
	c := s.color(role)
	if c == "" || s.profile == ProfileAscii {
		return nil
	}
	if s.profile == ProfileANSI256 && strings.HasPrefix(c, "#") {
		if r, g, b, err := parseHex(c); err == nil {
			return lipgloss.Color(strconv.Itoa(HexToANSI256(r, g, b)))
		}
	}
	return lipgloss.Color(c)
}

func (s *Styles) newStyle(role string, background bool) Style {
	st := Style{plain: s.profile == ProfileAscii, lg: lipgloss.NewStyle()}
	if c := s.colorValue(role); c != nil {
		if background {
			st.lg = st.lg.Background(c)
		} else {
			st.lg = st.lg.Foreground(c)
		}
	}
	return st
}

// Style returns a foreground style for a semantic role.
func (s *Styles) Style(role string) Style {
	return s.newStyle(role, false)
}

// Background returns a background style for a semantic role.
func (s *Styles) Background(role string) Style {
	return s.newStyle(role, true)
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
