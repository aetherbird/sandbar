package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	uxtheme "sandbar/internal/ui/theme"
)

func (m *appModel) openThemePicker() tea.Cmd {
	m.pickMode = "theme"
	m.pickItems = m.pickItems[:0]
	m.pickSel = 0
	m.pickOriginalTheme = m.sess.themeName
	if m.pickOriginalTheme == "" {
		m.pickOriginalTheme = uxtheme.System
	}
	m.pickItems = append(m.pickItems, pickItem{id: uxtheme.System, label: "System (Auto)", tag: "System"})
	for _, p := range uxtheme.List() {
		m.pickItems = append(m.pickItems, pickItem{id: p.ID, label: p.Label, tag: p.Group})
	}
	for i, item := range m.pickItems {
		if item.id == m.sess.themeName {
			m.pickSel = i
			break
		}
	}
	return nil
}

func (m *appModel) installTheme(name string) error {
	next, err := newStyleSet(name, m.sess.colorMode, m.sess.darkBackground, os.Stdout)
	if err != nil {
		return err
	}
	m.styles = next
	m.sess.styles = next
	m.sess.themeName = next.RequestedTheme()
	setActiveStyleSet(next)
	next.ApplyTextarea(&m.ta)
	return nil
}

func (m *appModel) previewSelectedTheme() {
	if m.pickMode != "theme" || m.pickSel < 0 || m.pickSel >= len(m.pickItems) {
		return
	}
	_ = m.installTheme(m.pickItems[m.pickSel].id)
}

func (m *appModel) setTheme(name string, persist bool) tea.Cmd {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return m.openThemePicker()
	}
	if err := m.installTheme(name); err != nil {
		return m.printLine("\n" + sty(cErr).Render("  ⚠ "+err.Error()) + "\n")
	}
	if persist && m.sess.clientCfg != nil {
		m.sess.clientCfg.Theme = m.sess.themeName
		if err := m.sess.clientCfg.Save(); err != nil {
			return m.printLine("\n" + sty(cWarn).Render("  ⚠ theme applied but preference was not saved: "+err.Error()) + "\n")
		}
	}
	label := m.styles.Palette().Label
	if m.sess.themeName == uxtheme.System {
		label = "System (" + label + ")"
	}
	// Native scrollback is intentionally retained. Already-printed transcript
	// blocks keep their original colors; the live composer and future output use
	// the selected theme immediately.
	return m.printLine("\n" + sty(cAccent).Render(fmt.Sprintf("  ◈ theme → %s", label)) + "\n")
}
