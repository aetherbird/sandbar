package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/cliui"
	"github.com/aetherbird/sandbar/internal/llm"
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

// compressionStatus is the session-persistent trace of the most recent
// compression outcome shown in the status bar. It is deliberately never
// cleared: the owner wants a running record of the last before→after delta.
type compressionStatus struct {
	beforeTokens int
	afterTokens  int
	modelAlias   string
	elapsedMS    int64
	failed       bool // last compression fell back or errored
	reason       string
}

// compressionStatusFromEvent adapts a streamed compression terminal event.
func compressionStatusFromEvent(c *llm.CompressionEvent) compressionStatus {
	failed := c.Outcome == string(agent.CompressionOutcomeFallback) ||
		c.Outcome == string(agent.CompressionOutcomeError) || c.FallbackUsed
	return compressionStatus{
		beforeTokens: c.BeforeTokens,
		afterTokens:  c.AfterTokens,
		modelAlias:   c.ModelAlias,
		elapsedMS:    c.ElapsedMS,
		failed:       failed,
		reason:       c.FallbackReason,
	}
}

// compressionStatusFromResult adapts a /compress result.
func compressionStatusFromResult(res agent.CompressionResult) compressionStatus {
	failed := res.Outcome == agent.CompressionOutcomeFallback ||
		res.Outcome == agent.CompressionOutcomeError || res.FallbackUsed
	return compressionStatus{
		beforeTokens: res.BeforeTokens,
		afterTokens:  res.AfterTokens,
		modelAlias:   res.SummaryModelAlias,
		elapsedMS:    res.ElapsedMS,
		failed:       failed,
		reason:       res.FallbackReason,
	}
}

// compressionSegment renders the right-block compression indicator: an
// in-flight spinner while a compression runs, otherwise the persistent
// before→after trace. The ASCII profile swaps in 7-bit glyphs.
func (m appModel) compressionSegment(s *styleSet) string {
	if m.compressing {
		return s.Style(cWarn).Render(sym(s, "⟳ compressing…", "~ compressing..."))
	}
	lc := m.lastCompression
	if lc.beforeTokens <= 0 || lc.afterTokens <= 0 {
		return ""
	}
	role := cMuted
	if lc.failed {
		role = cWarn
	}
	seg := sym(s,
		"⇣"+fmtTok(lc.beforeTokens)+"→"+fmtTok(lc.afterTokens),
		"c:"+fmtTok(lc.beforeTokens)+">"+fmtTok(lc.afterTokens))
	return s.Style(role).Render(seg)
}

// statusLine renders the full-width status bar as two fixed blocks: the LEFT
// block (model, approval chip, context gauge — truncatable, tiered by width)
// and the RIGHT block (spinner/duration, cost, compression — fixed and
// right-aligned). The middle is padded with spaces so the bar always spans
// exactly the terminal width on a single line, deterministically.
//
// When space is tight the right block wins: the left block shrinks via
// ansi.Truncate-from-end down to the model-only tier. Only a terminal
// narrower than the right block itself truncates the right block, from the
// end.
func (m appModel) statusLine() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	s := m.styles
	if s == nil {
		s = currentStyles()
	}

	// LEFT block (truncatable).
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
	var left string
	switch {
	case width >= 78:
		ctx, role := m.contextStatus(s, false)
		left = " " + icon + " " + modelText + chip + separator + s.Style(role).Render(ctx)
	case width >= 52:
		ctx, role := m.contextStatus(s, true)
		left = " " + icon + " " + modelText + chip + separator + s.Style(role).Render(ctx)
	default:
		left = " " + icon + " " + modelText
	}

	// RIGHT block (fixed): spinner/duration, cost, compression state.
	spinner, duration := m.turnActivity()
	right := s.Style(cMuted).Render(spinner + " " + duration)
	// Cost segment appears only when the active model has catalog pricing;
	// unknown and free models hide it instead of showing a meaningless "$0".
	if seg := m.costSeg; seg != "" {
		right += separator + s.Style(cMuted).Render(seg)
	}
	if comp := m.compressionSegment(s); comp != "" {
		right += separator + comp
	}
	right = " " + right

	rightW := lipgloss.Width(right)
	if rightW > width {
		right = ansi.Truncate(right, width, "")
		rightW = width
	}
	avail := width - rightW
	left = ansi.Truncate(left, avail, "")
	if pad := width - lipgloss.Width(left) - rightW; pad > 0 {
		left += strings.Repeat(" ", pad)
	}
	return s.Background(cSurface).Render(left + right)
}
