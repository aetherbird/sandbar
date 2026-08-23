package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestSubagentHUDTracksAndRemovesActiveTasks(t *testing.T) {
	m := appModel{width: 80, styles: defaultStyleSet(), subagents: map[string]subagentHUDItem{}}
	m.updateSubagentHUD(streamItem{kind: "subagent", taskID: "task-123456789", taskGoal: "inspect the backend", taskStatus: "running", content: "reading files"})

	view := m.subagentHUDView()
	for _, want := range []string{"agents 1", "task-123", "inspect the backend", "reading files"} {
		if !strings.Contains(stripANSI(view), want) {
			t.Fatalf("HUD %q does not contain %q", stripANSI(view), want)
		}
	}

	m.updateSubagentHUD(streamItem{kind: "subagent", taskID: "task-123456789", taskStatus: "completed"})
	if got := m.subagentHUDView(); got != "" {
		t.Fatalf("terminal task remained in HUD: %q", got)
	}
}

func TestSubagentHUDIsBounded(t *testing.T) {
	m := appModel{width: 36, styles: defaultStyleSet(), subagents: map[string]subagentHUDItem{}}
	for i := 0; i < maxSubagentHUDRows+2; i++ {
		id := string(rune('a' + i))
		m.updateSubagentHUD(streamItem{kind: "subagent", taskID: id, taskGoal: strings.Repeat("wide界", 20), taskStatus: "running"})
	}
	plain := stripANSI(m.subagentHUDView())
	if lines := strings.Count(plain, "\n") + 1; lines != maxSubagentHUDRows+2 {
		t.Fatalf("HUD line count = %d, want %d (header + rows + overflow)", lines, maxSubagentHUDRows+2)
	}
	if !strings.Contains(plain, "+2 more") {
		t.Fatalf("bounded HUD missing overflow count: %q", plain)
	}
	for i, line := range strings.Split(m.subagentHUDView(), "\n") {
		if got := lipgloss.Width(line); got > m.width {
			t.Fatalf("HUD row %d width = %d, exceeds terminal width %d: %q", i, got, m.width, stripANSI(line))
		}
	}
}

func TestSubagentHUDEveryRowFitsNarrowTerminal(t *testing.T) {
	m := appModel{width: 8, styles: defaultStyleSet(), subagents: map[string]subagentHUDItem{}}
	for i := 0; i < maxSubagentHUDRows+1; i++ {
		m.updateSubagentHUD(streamItem{
			kind: "subagent", taskID: strings.Repeat(string(rune('a'+i)), 24),
			taskGoal: strings.Repeat("界", 30), taskStatus: "running", content: strings.Repeat("activity", 20),
		})
	}
	for i, line := range strings.Split(m.subagentHUDView(), "\n") {
		if got := lipgloss.Width(line); got > m.width {
			t.Fatalf("narrow HUD row %d width = %d, want <= %d: %q", i, got, m.width, stripANSI(line))
		}
	}
}
