package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	"github.com/aetherbird/sandbar/internal/cliui"
	"github.com/aetherbird/sandbar/internal/config"
)

// colorfulStyle returns a style set that keeps fancy glyphs and SGR on, so
// glyph assertions below test palette/roles rather than the ASCII profile.
func colorfulStyle(t *testing.T) *styleSet {
	t.Helper()
	s, err := newStyleSet("system", config.ColorModeAlways, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ── approval-mode chip ───────────────────────────────────────────────────────

func TestStatusLineApprovalChip(t *testing.T) {
	s := colorfulStyle(t)
	mk := func(mode string) appModel {
		return appModel{
			sess:    &session{modelAlias: "m", cfg: &config.Config{Tools: config.ToolsConfig{Approval: config.ToolApprovalConfig{Mode: mode}}}},
			styles:  s,
			width:   120,
			ctxUsed: 100,
			ctxMax:  1000,
		}
	}

	if got := stripANSI(mk("yolo").statusLine()); !strings.Contains(got, "≈") {
		t.Errorf("yolo chip should render the quiet ≈ glyph, got %q", got)
	}
	if got := stripANSI(mk("write").statusLine()); !strings.Contains(got, "│ w │") {
		t.Errorf("write chip should render a warn 'w' segment, got %q", got)
	}
	if got := stripANSI(mk("always-ask").statusLine()); !strings.Contains(got, "⚠ ask") {
		t.Errorf("ask chip should render ⚠ ask, got %q", got)
	}

	// Unconfigured sessions (nil cfg) hide the chip entirely.
	plain := appModel{sess: &session{modelAlias: "m"}, styles: s, width: 120, ctxUsed: 100, ctxMax: 1000}
	if got := stripANSI(plain.statusLine()); strings.Contains(got, "≈") || strings.Contains(got, "ask") {
		t.Errorf("no approval config should hide the chip, got %q", got)
	}
}

// TestStatusLineChipDropsOnNarrowTiers mirrors legacy: the chip only appears
// from the mid tier up; narrow terminals keep the bar to model + activity.
func TestStatusLineChipDropsOnNarrowTiers(t *testing.T) {
	s := colorfulStyle(t)
	m := appModel{
		sess:    &session{modelAlias: "m", cfg: &config.Config{Tools: config.ToolsConfig{Approval: config.ToolApprovalConfig{Mode: "yolo"}}}},
		styles:  s,
		ctxUsed: 100,
		ctxMax:  1000,
	}
	for _, width := range []int{80, 60} {
		m.width = width
		if got := stripANSI(m.statusLine()); !strings.Contains(got, "≈") {
			t.Errorf("width %d: chip should render, got %q", width, got)
		}
	}
	for _, width := range []int{40, 20} {
		m.width = width
		if got := stripANSI(m.statusLine()); strings.Contains(got, "≈") {
			t.Errorf("width %d: chip should be dropped, got %q", width, got)
		}
	}
}

// ── ASCII fallback ───────────────────────────────────────────────────────────

func TestStatusLineAsciiFallback(t *testing.T) {
	s, err := newStyleSet("system", config.ColorModeNever, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ColorProfile() != cliui.ProfileAscii {
		t.Fatalf("precondition: profile = %v, want ASCII", s.ColorProfile())
	}
	m := appModel{
		sess:    &session{modelAlias: "m", cfg: &config.Config{Tools: config.ToolsConfig{Approval: config.ToolApprovalConfig{Mode: "yolo"}}}},
		styles:  s,
		width:   120,
		ctxUsed: 100,
		ctxMax:  1000,
		turnDur: 5e9,
	}
	got := m.statusLine()
	for _, fancy := range []string{"⚓", "█", "░", "│", "≈", "⚠"} {
		if strings.Contains(got, fancy) {
			t.Errorf("ASCII profile still renders %q: %q", fancy, got)
		}
	}
	for _, plain := range []string{"#", "|", "~", "ctx"} {
		if !strings.Contains(got, plain) {
			t.Errorf("ASCII profile should render %q, got %q", plain, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ASCII profile emitted SGR: %q", got)
	}
	if cells := lipgloss.Width(got); cells != 120 {
		t.Errorf("ASCII status width = %d, want 120", cells)
	}
}

// ── exact-width invariant ────────────────────────────────────────────────────

// TestStatusLineNeverWraps pins the width invariant: at every tier boundary
// (and a spread of widths in between), the bar renders exactly width cells,
// on a single line, deterministically — including hostile Unicode model names
// and a long cost segment.
func TestStatusLineNeverWraps(t *testing.T) {
	s := colorfulStyle(t)
	widths := []int{8, 10, 15, 20, 29, 30, 31, 40, 51, 52, 53, 60, 77, 78, 79, 100, 120}
	for _, width := range widths {
		m := appModel{
			sess:    &session{modelAlias: "提供者/海洋-超长模型", cfg: &config.Config{Tools: config.ToolsConfig{Approval: config.ToolApprovalConfig{Mode: "always-ask"}}}},
			styles:  s,
			width:   width,
			ctxUsed: 6842,
			ctxMax:  16384,
			costSeg: "⚑ $12.3456",
		}
		first := m.statusLine()
		if strings.Contains(first, "\n") {
			t.Errorf("width %d: status wrapped onto multiple lines: %q", width, first)
		}
		if cells := lipgloss.Width(first); cells != width {
			t.Errorf("width %d rendered %d cells: %q", width, cells, first)
		}
		if !utf8.ValidString(first) {
			t.Errorf("width %d produced invalid UTF-8", width)
		}
		if second := m.statusLine(); second != first {
			t.Errorf("width %d: rendering is not deterministic:\n%q\n%q", width, first, second)
		}
	}
}
