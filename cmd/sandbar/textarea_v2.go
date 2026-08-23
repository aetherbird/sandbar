package main

import (
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	"github.com/aetherbird/sandbar/internal/cliui"
)

// applyTextareaV2Style ports cliui.Styles.ApplyTextarea onto the v2 textarea.
//
// SPIKE NOTE: internal/cliui still renders with lipgloss v1 (its Styles type
// owns a v1 *lipgloss.Renderer, which has no v2 equivalent), so its
// ApplyTextarea takes a *v1 textarea.Model. This shim builds the equivalent
// v2 textarea.Styles from the same palette tokens. It is the one place the
// v1/v2 styling worlds meet until cliui itself moves to lipgloss v2 — a
// deliberate semantic decision to defer.
func applyTextareaV2Style(ta *textarea.Model, s *cliui.Styles) {
	if ta == nil {
		return
	}
	style := func(role string) lipgloss.Style {
		base := lipgloss.NewStyle()
		if color := s.Color(role); color != "" {
			base = base.Foreground(lipgloss.Color(color))
		}
		return base
	}
	base := style(cliui.RoleText)
	var cursor textarea.CursorStyle
	if color := s.Color(cliui.RoleAccent); color != "" {
		cursor.Color = lipgloss.Color(color)
	}
	st := textarea.Styles{
		Focused: textarea.StyleState{
			Base:        base,
			Text:        style(cliui.RoleText),
			Prompt:      style(cliui.RoleAccent).Bold(true),
			Placeholder: style(cliui.RoleMuted).Italic(true),
			CursorLine:  base,
			EndOfBuffer: style(cliui.RoleMuted),
		},
		Cursor: cursor,
	}
	st.Blurred = st.Focused
	ta.SetStyles(st)
}
