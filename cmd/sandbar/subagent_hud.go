package main

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const maxSubagentHUDRows = 6

type subagentHUDItem struct {
	id       string
	goal     string
	activity string
	status   string
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (m *appModel) updateSubagentHUD(item streamItem) {
	id := item.taskID
	if id == "" {
		return
	}
	if item.taskStatus == "completed" || item.taskStatus == "failed" || item.taskStatus == "interrupted" {
		delete(m.subagents, id)
		for i, existing := range m.subagentOrder {
			if existing == id {
				m.subagentOrder = append(m.subagentOrder[:i], m.subagentOrder[i+1:]...)
				break
			}
		}
		return
	}
	if m.subagents == nil {
		m.subagents = make(map[string]subagentHUDItem)
	}
	state, exists := m.subagents[id]
	if !exists {
		state.id = id
		m.subagentOrder = append(m.subagentOrder, id)
	}
	if item.taskGoal != "" {
		state.goal = oneline(item.taskGoal)
	}
	if item.content != "" {
		state.activity = oneline(item.content)
	}
	state.status = firstNonEmpty(item.taskStatus, "running")
	m.subagents[id] = state
}

func (m *appModel) clearSubagentHUD() {
	clear(m.subagents)
	m.subagentOrder = m.subagentOrder[:0]
}

func (m appModel) subagentHUDView() string {
	if len(m.subagents) == 0 {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	styles := m.styles
	if styles == nil {
		styles = currentStyles()
	}

	active := make([]subagentHUDItem, 0, len(m.subagents))
	for _, id := range m.subagentOrder {
		if state, ok := m.subagents[id]; ok {
			active = append(active, state)
		}
	}
	if len(active) == 0 {
		return ""
	}

	visible := len(active)
	if visible > maxSubagentHUDRows {
		visible = maxSubagentHUDRows
	}
	lines := make([]string, 0, visible+2)
	header := fmt.Sprintf(" agents %d ", len(active))
	lines = append(lines, ansi.Truncate(styles.Style(cAccent).Bold(true).Render(header), width, ""))
	for _, state := range active[:visible] {
		goal := state.goal
		if goal == "" {
			goal = "delegated task"
		}
		activity := state.activity
		if activity == "" {
			activity = "working…"
		}
		prefix := spinFrames[m.spinIdx%len(spinFrames)] + " " + shortID(state.id) + "  "
		line := styles.Style(cAccent).Render(prefix) + styles.Style(cBright).Render(goal) + styles.Style(cMuted).Render("  ·  "+activity)
		lines = append(lines, ansi.Truncate(line, width, ""))
	}
	if hidden := len(active) - visible; hidden > 0 {
		lines = append(lines, ansi.Truncate(styles.Style(cMuted).Render(fmt.Sprintf("  +%d more", hidden)), width, ""))
	}

	maxWidth := 0
	for _, line := range lines {
		if w := lipgloss.Width(line); w > maxWidth {
			maxWidth = w
		}
	}
	if maxWidth < width {
		for i, line := range lines {
			if pad := maxWidth - lipgloss.Width(line); pad > 0 {
				lines[i] = line + strings.Repeat(" ", pad)
			}
		}
	}
	return strings.Join(lines, "\n")
}
