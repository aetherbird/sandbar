package main

import (
	"os"
	"sync/atomic"

	"github.com/charmbracelet/lipgloss"

	"sandbar/internal/cliui"
)

// Keep short role names at existing call sites while the presentation package
// owns their palette mapping.
const (
	cAccent   = cliui.RoleAccent
	cPurple   = cliui.RoleAccentStrong
	cMuted    = cliui.RoleMuted
	cBright   = cliui.RoleText
	cWarn     = cliui.RoleWarning
	cErr      = cliui.RoleDanger
	cLavender = cliui.RoleSecondary
	cThink    = cliui.RoleThinking
	cGreen    = cliui.RoleSuccess
	cBorder   = cliui.RoleBorder
	cSurface  = cliui.RoleSurface
)

type styleSet = cliui.Styles

var activeStyleSet atomic.Pointer[styleSet]

func terminalSupportsColor(mode string, output *os.File) bool {
	return cliui.TerminalSupportsColor(mode, output)
}

func detectDarkBackground() bool { return cliui.DetectDarkBackground(os.Stdout) }

func preferredTheme(flagValue, envValue, configured string) string {
	return cliui.PreferredTheme(flagValue, envValue, configured)
}

func newStyleSet(name, colorMode string, darkBackground bool, output *os.File) (*styleSet, error) {
	return cliui.NewStyles(name, colorMode, darkBackground, output)
}

func defaultStyleSet() *styleSet { return cliui.DefaultStyles(os.Stdout) }

func setActiveStyleSet(s *styleSet) {
	if s == nil {
		s = defaultStyleSet()
	}
	activeStyleSet.Store(s)
	resetMarkdownRenderer()
}

func currentStyles() *styleSet {
	if s := activeStyleSet.Load(); s != nil {
		return s
	}
	s := defaultStyleSet()
	if activeStyleSet.CompareAndSwap(nil, s) {
		return s
	}
	return activeStyleSet.Load()
}

// sty remains a narrow migration bridge for render helpers that do not own an
// appModel. New stateful views read their immutable style pointer from the model.
func sty(role string) lipgloss.Style { return currentStyles().Style(role) }

// userEchoStyle renders the user's own sent messages: bold in the success role,
// so prompts read distinctly from assistant output. Both the live echo and the
// resumed-transcript replay use it, keeping the two visually identical.
func userEchoStyle() lipgloss.Style { return sty(cGreen).Bold(true) }

func formatThemeList() string { return cliui.FormatThemeList() }
