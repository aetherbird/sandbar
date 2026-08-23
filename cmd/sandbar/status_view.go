package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/aetherbird/sandbar/internal/cliui"
)

func (m appModel) turnActivity() (spinner, duration string) {
	spinner, duration = " ", "--"
	switch {
	case m.streaming:
		spinner = spinFrames[m.spinIdx%len(spinFrames)]
		duration = fmtDur(time.Since(m.turnStart))
	case m.turnDur > 0:
		duration = fmtDur(m.turnDur)
	}
	return spinner, duration
}

// sym picks the plain-ASCII glyph when the style set renders for the ASCII
// profile (color disabled), so the bar stays 7-bit clean there.
func sym(s *styleSet, fancy, plain string) string {
	if s != nil && s.ColorProfile() == cliui.ProfileAscii {
		return plain
	}
	return fancy
}

// approvalChip renders the approval-mode segment from the active tool
// config, or "" when the mode is unknown/unconfigured. Yolo is quiet (a
// single dim glyph) — the bar should whisper when nothing needs you and
// speak up when approvals will interrupt (legacy statusbar behavior).
func (m appModel) approvalChip(s *styleSet) string {
	mode := ""
	if m.sess != nil && m.sess.cfg != nil {
		mode = strings.ToLower(strings.TrimSpace(m.sess.cfg.Tools.Approval.Mode))
	}
	switch mode {
	case "always-ask", "ask":
		return s.Style(cWarn).Render(sym(s, "⚠", "!") + " ask")
	case "write":
		return s.Style(cWarn).Render("w")
	case "yolo":
		return s.Style(cMuted).Render(sym(s, "≈", "~"))
	case "":
		return ""
	default:
		return s.Style(cMuted).Render(mode)
	}
}

func (m appModel) contextStatus(s *styleSet, compact bool) (string, string) {
	if m.ctxMax <= 0 {
		return "ctx --", cMuted
	}
	pct := float64(m.ctxUsed) / float64(m.ctxMax)
	pi := int(pct * 100)
	// Thermometer thresholds: warn at 80% context, critical at 90%.
	role := cGreen
	if pi >= 90 {
		role = cErr
	} else if pi >= 80 {
		role = cWarn
	}
	if compact {
		return fmt.Sprintf("ctx %d%%", pi), role
	}
	// The ASCII profile drops the block gauge: █/░ are box-drawing glyphs
	// (legacy renders a plain "ctx NN" there instead).
	if s != nil && s.ColorProfile() == cliui.ProfileAscii {
		return fmt.Sprintf("ctx %s/%s %d%%", fmtTok(m.ctxUsed), fmtTok(m.ctxMax), pi), role
	}
	filled := int(8 * pct)
	if filled < 0 {
		filled = 0
	}
	if filled > 8 {
		filled = 8
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 8-filled)
	return fmt.Sprintf("%s/%s [%s] %d%%", fmtTok(m.ctxUsed), fmtTok(m.ctxMax), bar, pi), role
}

// statusLine uses width tiers rather than allowing a fixed full status to wrap.
// Narrow terminals progressively drop the gauge and timer while retaining the
// active model and streaming state. The rendered line is always exactly the
// terminal width: ansi.Truncate clips deterministically from the end and the
// remainder pads, so it can never physically wrap.
func (m appModel) statusLine() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	s := m.styles
	if s == nil {
		s = currentStyles()
	}
	spinner, duration := m.turnActivity()
	model := shortModel(m.sess.modelAlias)
	if model == "" {
		model = "no model"
	}

	icon := s.Style(cAccent).Bold(true).Render(sym(s, "⚓", "#"))
	modelText := s.Style(cPurple).Bold(true).Render(model)
	separator := s.Style(cBorder).Render(" " + sym(s, "│", "|") + " ")
	chip := m.approvalChip(s)
	if chip != "" {
		chip = separator + chip // its own segment, like legacy's
	}
	activity := s.Style(cMuted).Render(spinner + " " + duration)
	// Cost segment appears only when the active model has catalog pricing;
	// unknown and free models hide it instead of showing a meaningless "$0".
	cost := ""
	if seg := m.costSeg; seg != "" {
		cost = separator + s.Style(cMuted).Render(seg)
	}
	var inner string
	switch {
	case width >= 78:
		ctx, role := m.contextStatus(s, false)
		inner = " " + icon + " " + modelText + chip + separator + s.Style(role).Render(ctx) + separator + activity + cost
	case width >= 52:
		ctx, role := m.contextStatus(s, true)
		inner = " " + icon + " " + modelText + chip + separator + s.Style(role).Render(ctx) + separator + activity + cost
	case width >= 30:
		inner = " " + icon + " " + modelText + separator + activity + cost
	default:
		inner = " " + icon + " " + modelText + cost
	}
	inner = ansi.Truncate(inner, width, "")
	if pad := width - lipgloss.Width(inner); pad > 0 {
		inner += strings.Repeat(" ", pad)
	}
	return s.Background(cSurface).Render(inner)
}
