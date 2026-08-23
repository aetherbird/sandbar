package cliui

import (
	"charm.land/bubbles/v2/textarea"
)

// ApplyTextarea gives all textarea states the same semantic palette, resolved
// against the terminal's color capability. With color disabled, prompt and
// placeholder render without SGR attributes.
func (s *Styles) ApplyTextarea(ta *textarea.Model) {
	if ta == nil {
		return
	}
	base := s.Style(RoleText)
	var cursor textarea.CursorStyle
	if c := s.colorValue(RoleAccent); c != nil {
		cursor.Color = c
	}
	st := textarea.Styles{
		Focused: textarea.StyleState{
			Base:        base.lg,
			Text:        s.Style(RoleText).lg,
			Prompt:      s.Style(RoleAccent).Bold(true).lg,
			Placeholder: s.Style(RoleMuted).Italic(true).lg,
			CursorLine:  base.lg,
			EndOfBuffer: s.Style(RoleMuted).lg,
		},
		Cursor: cursor,
	}
	st.Blurred = st.Focused
	ta.SetStyles(st)
}
