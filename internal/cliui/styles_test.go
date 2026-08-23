package cliui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"github.com/aetherbird/sandbar/internal/config"
)

// TestDetectProfileMatrix pins the full environment matrix: NO_COLOR (set to
// any value, including empty), TERM=dumb, an empty TERM, and non-TTYs force
// ASCII in auto mode; COLORTERM advertises truecolor; always forces at least
// 256 even off a TTY and overrides NO_COLOR; never forces ASCII.
func TestDetectProfileMatrix(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		noColorSet bool
		term       string
		colorterm  string
		isTTY      bool
		want       Profile
	}{
		{name: "auto tty truecolor", mode: config.ColorModeAuto, term: "xterm-256color", colorterm: "truecolor", isTTY: true, want: ProfileTrueColor},
		{name: "auto tty 24bit", mode: config.ColorModeAuto, term: "xterm-256color", colorterm: "24bit", isTTY: true, want: ProfileTrueColor},
		{name: "auto tty default 256", mode: config.ColorModeAuto, term: "xterm-256color", isTTY: true, want: ProfileANSI256},
		{name: "auto NO_COLOR set", mode: config.ColorModeAuto, noColorSet: true, term: "xterm-256color", colorterm: "truecolor", isTTY: true, want: ProfileAscii},
		{name: "auto NO_COLOR empty value", mode: config.ColorModeAuto, noColorSet: true, term: "xterm-256color", isTTY: true, want: ProfileAscii},
		{name: "auto TERM dumb", mode: config.ColorModeAuto, term: "dumb", isTTY: true, want: ProfileAscii},
		{name: "auto empty TERM", mode: config.ColorModeAuto, term: "", isTTY: true, want: ProfileAscii},
		{name: "auto non tty", mode: config.ColorModeAuto, term: "xterm-256color", colorterm: "truecolor", isTTY: false, want: ProfileAscii},
		{name: "never on tty", mode: config.ColorModeNever, term: "xterm-256color", colorterm: "truecolor", isTTY: true, want: ProfileAscii},
		{name: "never on pipe", mode: config.ColorModeNever, isTTY: false, want: ProfileAscii},
		{name: "always on pipe", mode: config.ColorModeAlways, term: "xterm-256color", isTTY: false, want: ProfileANSI256},
		{name: "always on dumb tty", mode: config.ColorModeAlways, term: "dumb", isTTY: true, want: ProfileANSI256},
		{name: "always overrides NO_COLOR", mode: config.ColorModeAlways, noColorSet: true, term: "xterm-256color", isTTY: true, want: ProfileANSI256},
		{name: "always tty truecolor", mode: config.ColorModeAlways, term: "xterm-256color", colorterm: "truecolor", isTTY: true, want: ProfileTrueColor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectProfile(tt.mode, tt.noColorSet, tt.term, tt.colorterm, tt.isTTY); got != tt.want {
				t.Fatalf("detectProfile(%q, noColor=%v, TERM=%q, COLORTERM=%q, tty=%v) = %v, want %v",
					tt.mode, tt.noColorSet, tt.term, tt.colorterm, tt.isTTY, got, tt.want)
			}
		})
	}
}

// TestNewStylesColorModeProfiles pins the NewStyles wiring: never and auto
// (off a TTY) resolve to ASCII; always forces ANSI256 off a TTY.
func TestNewStylesColorModeProfiles(t *testing.T) {
	never, err := NewStyles("dark", config.ColorModeNever, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if never.ColorProfile() != ProfileAscii || never.ColorsEnabled() {
		t.Fatalf("never: profile=%v colors=%v, want ASCII and disabled", never.ColorProfile(), never.ColorsEnabled())
	}

	always, err := NewStyles("dark", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if always.ColorProfile() != ProfileANSI256 || !always.ColorsEnabled() {
		t.Fatalf("always: profile=%v colors=%v, want ANSI256 and enabled", always.ColorProfile(), always.ColorsEnabled())
	}

	t.Setenv("NO_COLOR", "")
	auto, err := NewStyles("dark", config.ColorModeAuto, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auto.ColorProfile() != ProfileAscii || auto.ColorsEnabled() {
		t.Fatalf("auto with NO_COLOR: profile=%v colors=%v, want ASCII and disabled", auto.ColorProfile(), auto.ColorsEnabled())
	}
}

// TestColorNeverEmitsZeroSGR pins the --color never contract: no SGR
// anywhere, including attributes chained by callers (bold, italic) and
// multi-string block rendering.
func TestColorNeverEmitsZeroSGR(t *testing.T) {
	s, err := NewStyles("dark", config.ColorModeNever, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{
		s.Style(RoleAccent).Render("accent"),
		s.Style(RoleAccent).Bold(true).Render("bold accent"),
		s.Style(RoleMuted).Italic(true).Render("muted italic"),
		s.Style(RoleText).Render("a", "b"),
		s.Background(RoleSurface).Render("surface"),
	}
	for _, out := range outputs {
		if strings.Contains(out, "\x1b[") {
			t.Fatalf("--color never output contains SGR: %q", out)
		}
	}
	if got := s.Style(RoleText).Render("a", "b"); got != "a b" {
		t.Fatalf("plain block render = %q, want lipgloss-equivalent space join", got)
	}
}

// TestColorAlwaysOnPipeEmitsSGR pins the --color always contract: forcing
// color on a non-TTY output still emits SGR, downsampled to the ANSI256
// fallback. The dark accent (#60a5fa) downsamples to xterm 75.
func TestColorAlwaysOnPipeEmitsSGR(t *testing.T) {
	s, err := NewStyles("dark", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ColorProfile() != ProfileANSI256 {
		t.Fatalf("profile = %v, want forced non-TTY ANSI256", s.ColorProfile())
	}
	out := s.Style(RoleAccent).Render("accent")
	if !strings.Contains(out, "\x1b[38;5;75m") {
		t.Fatalf("--color always on a pipe must emit downsampled SGR, got %q", out)
	}
	bold := s.Style(RoleAccent).Bold(true).Render("accent")
	if !strings.Contains(bold, "38;5;75") {
		t.Fatalf("bold forced output lost its color: %q", bold)
	}
	if !strings.Contains(bold, "\x1b[") {
		t.Fatalf("--color always on a pipe must emit SGR, got %q", bold)
	}
}

// TestStyleProfiles pins per-profile rendering: truecolor carries the exact
// hex escape, ANSI256 the nearest xterm index, ASCII plain text.
func TestStyleProfiles(t *testing.T) {
	accent := "accent"
	for _, tt := range []struct {
		name    string
		profile Profile
		want    string
		absent  string
	}{
		{name: "truecolor", profile: ProfileTrueColor, want: "\x1b[38;2;96;165;250m", absent: "38;5;75"},
		{name: "ansi256", profile: ProfileANSI256, want: "\x1b[38;5;75m", absent: "38;2;96;165;250"},
		{name: "ascii", profile: ProfileAscii, want: "accent"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := stylesFor(t, "dark", true, tt.profile)
			out := s.Style(accent).Render("accent")
			if !strings.Contains(out, tt.want) {
				t.Fatalf("style render = %q, want %q", out, tt.want)
			}
			if tt.absent != "" && strings.Contains(out, tt.absent) {
				t.Fatalf("style render = %q, must not contain %q", out, tt.absent)
			}
		})
	}
}

// TestHexToANSI256 ports the legacy nearest-color cases.
func TestHexToANSI256(t *testing.T) {
	cases := []struct {
		hex  string
		want int
	}{
		{"#000000", 16},  // cube origin, exact
		{"#ffffff", 231}, // cube far corner, exact
		{"#808080", 244}, // mid gray lands on the ramp
		{"#0b1e26", 234}, // dark teal: channels cluster -> grayscale ramp wins
		{"#60a5fa", 75},  // sandbar dark accent
		{"#10b981", 36},  // sandbar default success
	}
	for _, tc := range cases {
		r, g, b, err := parseHex(tc.hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := HexToANSI256(r, g, b); got != tc.want {
			t.Errorf("HexToANSI256(%s) = %d, want %d", tc.hex, got, tc.want)
		}
	}
}

// TestApplyTextareaRespectsColorMode pins the textarea bridge: color carries
// the downsampled palette, and --color never leaves prompt and placeholder
// without color or attribute SGR. The widget's own static cursor renders a
// reverse-video block internally, which cliui does not control.
func TestApplyTextareaRespectsColorMode(t *testing.T) {
	newTA := func() *textarea.Model {
		ta := textarea.New()
		ta.Focus()
		ta.SetWidth(40)
		ta.SetValue("hello")
		return &ta
	}
	// colorAndAttrSGR matches color (38;/48;) and attribute (bold/italic/
	// underline) sequences, excluding the widget-internal reverse cursor.
	colorAndAttrSGR := func(s string) bool {
		for _, seq := range []string{"\x1b[38", "\x1b[48", "\x1b[1m", "\x1b[3m", "\x1b[4m", "\x1b[5m"} {
			if strings.Contains(s, seq) {
				return true
			}
		}
		return false
	}
	t.Run("never", func(t *testing.T) {
		s, err := NewStyles("dark", config.ColorModeNever, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		ta := newTA()
		s.ApplyTextarea(ta)
		if view := ta.View(); colorAndAttrSGR(view) {
			t.Fatalf("textarea view carries color/attribute SGR under --color never: %q", view)
		}
	})
	t.Run("always", func(t *testing.T) {
		s, err := NewStyles("dark", config.ColorModeAlways, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		ta := newTA()
		s.ApplyTextarea(ta)
		// The accent cursor color is downsampled to xterm 75 on 256 profiles.
		if view := ta.View(); !strings.Contains(view, "38;5;75") {
			t.Fatalf("textarea view lost the accent palette under --color always: %q", view)
		}
	})
}
