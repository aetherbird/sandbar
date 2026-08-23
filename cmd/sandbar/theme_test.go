package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/aetherbird/sandbar/internal/cliui"
	"github.com/aetherbird/sandbar/internal/config"
	uxtheme "github.com/aetherbird/sandbar/internal/ui/theme"
)

func withStyles(t *testing.T, next *styleSet) {
	t.Helper()
	previous := currentStyles()
	setActiveStyleSet(next)
	t.Cleanup(func() { setActiveStyleSet(previous) })
}

func TestPreferredThemePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		env        string
		configured string
		want       string
	}{
		{name: "flag wins", flag: " dracula ", env: "nord", configured: "light", want: "dracula"},
		{name: "environment wins", env: " tokyo-night ", configured: "light", want: "tokyo-night"},
		{name: "configuration wins", configured: " catppuccin-mocha ", want: "catppuccin-mocha"},
		{name: "system fallback", want: uxtheme.System},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := preferredTheme(tt.flag, tt.env, tt.configured); got != tt.want {
				t.Fatalf("preferredTheme(%q, %q, %q) = %q, want %q", tt.flag, tt.env, tt.configured, got, tt.want)
			}
		})
	}
}

func TestEveryThemeBuildsSemanticStyles(t *testing.T) {
	roles := []string{cAccent, cPurple, cMuted, cBright, cWarn, cErr, cLavender, cThink, cGreen, cBorder, cSurface}
	for _, id := range uxtheme.IDs() {
		t.Run(id, func(t *testing.T) {
			s, err := newStyleSet(id, config.ColorModeAlways, true, nil)
			if err != nil {
				t.Fatalf("newStyleSet: %v", err)
			}
			if s.Palette().ID != id {
				t.Fatalf("resolved palette = %q, want %q", s.Palette().ID, id)
			}
			if s.ColorProfile() != cliui.ProfileANSI256 {
				t.Fatalf("profile = %v, want forced non-TTY ANSI256 fallback", s.ColorProfile())
			}
			for _, role := range roles {
				if color := s.Color(role); color == "" {
					t.Errorf("semantic role %q has no color", role)
				}
				if rendered := s.Style(role).Render("x"); rendered == "" {
					t.Errorf("semantic role %q rendered empty output", role)
				}
			}
		})
	}
}

func TestSystemThemeResolvesForTerminalBackground(t *testing.T) {
	dark, err := newStyleSet(uxtheme.System, config.ColorModeNever, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	light, err := newStyleSet(uxtheme.System, config.ColorModeNever, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dark.Palette().ID != "dark" || light.Palette().ID != "light" {
		t.Fatalf("system resolved to dark=%q light=%q", dark.Palette().ID, light.Palette().ID)
	}
	if dark.RequestedTheme() != uxtheme.System || light.RequestedTheme() != uxtheme.System {
		t.Fatalf("system preference was not retained: dark=%q light=%q", dark.RequestedTheme(), light.RequestedTheme())
	}
}

func TestNoColorDisablesSGRAcrossRenderedOutput(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	s, err := newStyleSet("nord", config.ColorModeAuto, true, os.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	withStyles(t, s)
	if s.ColorsEnabled() {
		t.Fatal("NO_COLOR with auto mode should disable colors")
	}
	if profile := s.ColorProfile(); profile != cliui.ProfileAscii {
		t.Fatalf("profile = %v, want ASCII", profile)
	}

	m := appModel{
		sess:    &session{modelAlias: "模型/海洋", styles: s},
		styles:  s,
		width:   80,
		ctxUsed: 50,
		ctxMax:  100,
	}
	output := strings.Join([]string{
		s.Style(cAccent).Bold(true).Render("accent"),
		m.statusLine(),
		renderMarkdown("# Heading\n\n**body**"),
		renderDiff("--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new", 72),
	}, "\n")
	if strings.Contains(output, "\x1b[") {
		t.Fatalf("NO_COLOR output contains an ANSI control sequence: %q", output)
	}
}

func TestMarkdownRendererInvalidatesForThemeAndProfile(t *testing.T) {
	light, err := newStyleSet("light", config.ColorModeAlways, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	withStyles(t, light)
	lightOutput := renderMarkdown("# Sandbar\n\n`code`")
	if light.Palette().ID != "light" || light.ColorProfile() != cliui.ProfileANSI256 {
		t.Fatalf("light presentation = theme %q profile %v", light.Palette().ID, light.ColorProfile())
	}

	dark, err := newStyleSet("dark", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	setActiveStyleSet(dark)
	darkOutput := renderMarkdown("# Sandbar\n\n`code`")
	if dark.Palette().ID != "dark" || dark.ColorProfile() != cliui.ProfileANSI256 {
		t.Fatalf("dark presentation = theme %q profile %v", dark.Palette().ID, dark.ColorProfile())
	}
	if darkOutput == lightOutput {
		t.Fatal("light and dark Markdown output should use different palette escapes")
	}

	plain, err := newStyleSet("dark", config.ColorModeNever, true, os.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	setActiveStyleSet(plain)
	plainOutput := renderMarkdown("# Sandbar\n\n`code`")
	if plain.Palette().ID != "dark" || plain.ColorProfile() != cliui.ProfileAscii {
		t.Fatalf("plain presentation = theme %q profile %v", plain.Palette().ID, plain.ColorProfile())
	}
	if strings.Contains(plainOutput, "\x1b[") {
		t.Fatalf("ASCII Markdown contains SGR output: %q", plainOutput)
	}
}

func TestResponsiveStatusWidthsWithUnicode(t *testing.T) {
	// ColorModeAlways keeps the fancy glyphs: this test pins width stability,
	// not the ASCII-profile swap (covered in status_view_test.go).
	s, err := newStyleSet("tokyo-night", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := appModel{
		sess:    &session{modelAlias: "提供者/海洋-超长模型", styles: s},
		styles:  s,
		ctxUsed: 6842,
		ctxMax:  16384,
	}
	for _, width := range []int{20, 40, 60, 80, 120} {
		m.width = width
		got := m.statusLine()
		if !utf8.ValidString(got) {
			t.Errorf("width %d produced invalid UTF-8: %q", width, got)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("width %d status wrapped onto multiple lines: %q", width, got)
		}
		if cells := lipgloss.Width(got); cells != width {
			t.Errorf("width %d rendered %d cells: %q", width, cells, got)
		}
		if !strings.Contains(got, "⚓") {
			t.Errorf("width %d dropped the status anchor: %q", width, got)
		}
	}
}

func TestThemePickerPreviewCancelAndSelectPersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &config.ClientConfig{Theme: "nord", ColorMode: config.ColorModeNever, FontSize: 15}
	if err := client.Save(); err != nil {
		t.Fatalf("save initial client preference: %v", err)
	}
	initial, err := newStyleSet("nord", config.ColorModeNever, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	withStyles(t, initial)
	m := newModel(&session{
		clientCfg:      client,
		themeName:      "nord",
		colorMode:      config.ColorModeNever,
		darkBackground: true,
		styles:         initial,
	})

	selectID := func(id string) {
		t.Helper()
		for i, item := range m.pickItems {
			if item.id == id {
				m.pickSel = i
				return
			}
		}
		t.Fatalf("theme %q not present in picker", id)
	}

	m.openThemePicker()
	selectID("dracula")
	m.previewSelectedTheme()
	if m.styles.Palette().ID != "dracula" || m.sess.themeName != "dracula" {
		t.Fatalf("preview did not install Dracula: palette=%q preference=%q", m.styles.Palette().ID, m.sess.themeName)
	}
	if client.Theme != "nord" {
		t.Fatalf("preview persisted early: client theme = %q", client.Theme)
	}
	m.cancelPick()
	if m.styles.Palette().ID != "nord" || m.sess.themeName != "nord" {
		t.Fatalf("cancel did not restore Nord: palette=%q preference=%q", m.styles.Palette().ID, m.sess.themeName)
	}
	loaded, err := config.LoadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Theme != "nord" {
		t.Fatalf("cancel changed persisted theme to %q", loaded.Theme)
	}

	m.openThemePicker()
	selectID("dracula")
	m.previewSelectedTheme()
	selectedCmd := m.selectPick()
	if selectedCmd == nil {
		t.Fatal("selecting a theme should emit a confirmation notice")
	}
	if reflect.TypeOf(selectedCmd()) == reflect.TypeOf(tea.ClearScreen()) {
		t.Fatal("selecting a theme must not clear native transcript scrollback")
	}
	if m.pickMode != "" || m.styles.Palette().ID != "dracula" || m.sess.themeName != "dracula" {
		t.Fatalf("selection state: mode=%q palette=%q preference=%q", m.pickMode, m.styles.Palette().ID, m.sess.themeName)
	}
	loaded, err = config.LoadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Theme != "dracula" {
		t.Fatalf("selected theme was not persisted: %q", loaded.Theme)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "sandbar", "client.yaml")); err != nil {
		t.Fatalf("client preference file: %v", err)
	}
}

func TestThemeChangesReuseCachedBackgroundDetection(t *testing.T) {
	initial, err := newStyleSet(uxtheme.System, config.ColorModeNever, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	withStyles(t, initial)
	m := newModel(&session{
		themeName:      uxtheme.System,
		colorMode:      config.ColorModeNever,
		darkBackground: false,
		styles:         initial,
	})
	if err := m.installTheme(uxtheme.System); err != nil {
		t.Fatal(err)
	}
	if got := m.styles.Palette().ID; got != "light" {
		t.Fatalf("cached light background resolved system theme to %q", got)
	}

	m.sess.darkBackground = true
	if err := m.installTheme(uxtheme.System); err != nil {
		t.Fatal(err)
	}
	if got := m.styles.Palette().ID; got != "dark" {
		t.Fatalf("cached dark background resolved system theme to %q", got)
	}
}

func TestPickerViewTruncatesANSIByCellWidth(t *testing.T) {
	s, err := newStyleSet("tokyo-night", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	withStyles(t, s)
	m := newModel(&session{styles: s, colorMode: config.ColorModeAlways, darkBackground: true})
	m.width = 20
	m.pickMode = "theme"
	m.pickItems = []pickItem{{id: "wide", label: "海洋界面主题名字非常长", tag: "长分组"}}

	got := m.pickerView()
	if !strings.Contains(got, "\x1b[") {
		t.Fatal("test setup did not produce ANSI-styled picker output")
	}
	for i, line := range strings.Split(got, "\n") {
		if !utf8.ValidString(line) {
			t.Errorf("line %d is invalid UTF-8: %q", i, line)
		}
		if cells := lipgloss.Width(line); cells > m.width {
			t.Errorf("line %d is %d cells at width %d: %q", i, cells, m.width, line)
		}
	}
}

func TestFormatThemeListIncludesSystemAndCatalog(t *testing.T) {
	got := formatThemeList()
	for _, id := range append([]string{uxtheme.System}, uxtheme.IDs()...) {
		if !strings.Contains(got, id+"\t") {
			t.Errorf("theme list is missing %q", id)
		}
	}
}

func TestGlamourBlankNormalizationIndentsTranscript(t *testing.T) {
	got := indentGlamourOutput("\nfirst paragraph\n\n\nsecond paragraph\n")
	if got != "  first paragraph\n\n  second paragraph" {
		t.Fatalf("normalized transcript = %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if line != "" && !strings.HasPrefix(line, strings.Repeat(" ", contentMargin)) {
			t.Fatalf("content line missing margin: %q", line)
		}
	}
}
