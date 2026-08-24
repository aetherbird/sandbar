package main

import (
	"strings"
	"testing"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/llm"
)

// TestInputSurvivesMidTurnCompression replays the exact stream-item sequence
// a mid-turn auto-compression produces and asserts the input box is still
// rendered (and no picker/approval state got stuck replacing it).
func TestInputSurvivesMidTurnCompression(t *testing.T) {
	m := newModel(&session{modelAlias: "m", backend: &fakeCLIBackend{}})
	m.streaming = true
	m.streamGen = 1
	m.width = 100
	m.ta.SetWidth(98)

	seq := []streamItem{
		{gen: 1, kind: "ctx", ctxUsed: 1000, cost: ""},
		{gen: 1, kind: "activity", content: "⚠ summary could not be loaded (continuing with full history)"},
		{gen: 1, kind: "tool", content: "read foo.go"},
		{gen: 1, kind: "compression", compType: "compression_start"},
		{gen: 1, kind: "compression", compType: "compression_end", compEvent: &llm.CompressionEvent{
			Outcome: "compressed", BeforeTokens: 128000, AfterTokens: 42000,
			ModelAlias: "deepseek-direct/deepseek-v4-flash", ElapsedMS: 3200,
		}},
		{gen: 1, kind: "token", content: "resuming after compaction"},
		{gen: 1, kind: "done", footer: "1s"},
	}
	for _, si := range seq {
		up, _ := m.Update(si)
		m = up.(appModel)
	}

	view := m.View().Content
	if !strings.Contains(view, "sandbar...") {
		t.Fatalf("input box missing from View after compression sequence:\n%s", view)
	}
	if m.pickMode != "" {
		t.Fatalf("pickMode stuck after compression: %q", m.pickMode)
	}
	if len(m.approvals) > 0 {
		t.Fatalf("approval state stuck after compression: %d pending", len(m.approvals))
	}
}

// TestInputSurvivesManualCompress exercises the /compress path
// (compressDoneMsg) with docked panels present, the turn streaming.
func TestInputSurvivesManualCompressWithPanels(t *testing.T) {
	m := newModel(&session{modelAlias: "m", backend: &fakeCLIBackend{}})
	m.streaming = true
	m.streamGen = 1
	m.width = 100
	m.ta.SetWidth(98)
	m.updateSubagentHUD(streamItem{kind: "subagent", taskID: "task-123456789", taskGoal: "inspect", taskStatus: "running"})

	m.compressing = true
	up, _ := m.Update(compressDoneMsg{res: agent.CompressionResult{}})
	m = up.(appModel)
	up, _ = m.Update(streamItem{gen: 1, kind: "done", footer: "2s"})
	m = up.(appModel)

	view := m.View().Content
	if !strings.Contains(view, "sandbar...") {
		t.Fatalf("input box missing from View after manual /compress:\n%s", view)
	}
}
