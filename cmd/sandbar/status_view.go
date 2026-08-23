package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

func (m appModel) contextStatus(compact bool) (string, string) {
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
// active model and streaming state.
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

	icon := s.Style(cAccent).Bold(true).Render("⚓")
	modelText := s.Style(cPurple).Bold(true).Render(model)
	separator := s.Style(cBorder).Render(" │ ")
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
		ctx, role := m.contextStatus(false)
		inner = " " + icon + " " + modelText + separator + s.Style(role).Render(ctx) + separator + activity + cost
	case width >= 52:
		ctx, role := m.contextStatus(true)
		inner = " " + icon + " " + modelText + separator + s.Style(role).Render(ctx) + separator + activity + cost
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
