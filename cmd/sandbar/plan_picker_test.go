package main

import (
	"context"
	"testing"

	"github.com/aetherbird/sandbar/internal/backend"
)

type planCLIBackend struct {
	*fakeCLIBackend
	decideCalls []string
	decideErr   error
}

func (b *planCLIBackend) DecidePlan(_ context.Context, threadID, action string) error {
	b.decideCalls = append(b.decideCalls, threadID+":"+action)
	return b.decideErr
}

func TestPlanTurnCompletionOpensDecisionPicker(t *testing.T) {
	be := &planCLIBackend{fakeCLIBackend: &fakeCLIBackend{}}
	m := newModel(&session{backend: be, threadID: "t1", planMode: true})
	m.streamGen = 3
	m.streamCh = make(chan streamItem)
	m.streaming = true
	m.responseBuf = []byte("1. do the thing")

	updated, _ := m.Update(streamItem{gen: m.streamGen, kind: "done", footer: "1s"})
	m = updated.(appModel)

	if m.sess.planMode {
		t.Fatal("plan mode stayed on after the plan turn completed")
	}
	if m.pickMode != "plan" {
		t.Fatalf("decision picker did not open: pickMode = %q", m.pickMode)
	}
	if len(m.pickItems) != 3 || m.pickItems[0].id != "approve" || m.pickItems[1].id != "edit" || m.pickItems[2].id != "cancel" {
		t.Fatalf("unexpected picker items: %+v", m.pickItems)
	}
	if m.lastPlanText != "1. do the thing" {
		t.Fatalf("plan text not captured: %q", m.lastPlanText)
	}

	// A normal turn never opens the picker.
	m2 := newModel(&session{backend: be, threadID: "t1"})
	m2.streamGen = 4
	m2.streamCh = make(chan streamItem)
	m2.streaming = true
	updated, _ = m2.Update(streamItem{gen: m2.streamGen, kind: "done", footer: "1s"})
	m2 = updated.(appModel)
	if m2.pickMode != "" {
		t.Fatalf("picker opened after a normal turn: %q", m2.pickMode)
	}
}

func TestDecidePlanActions(t *testing.T) {
	be := &planCLIBackend{fakeCLIBackend: &fakeCLIBackend{}}

	// Approve records the decision on the backend.
	m := newModel(&session{backend: be, threadID: "t1"})
	m.decidePlan("approve")
	if len(be.decideCalls) != 1 || be.decideCalls[0] != "t1:approve" {
		t.Fatalf("approve calls: %v", be.decideCalls)
	}

	// Edit loads the plan into the input and records nothing backend-side —
	// the amended message is a normal turn that clears the pending state.
	m.lastPlanText = "the plan text"
	m.decidePlan("edit")
	if got := m.ta.Value(); got != "the plan text" {
		t.Fatalf("input after edit: %q", got)
	}
	if len(be.decideCalls) != 1 {
		t.Fatalf("edit should not call the backend: %v", be.decideCalls)
	}

	// Cancel rejects best-effort.
	m.decidePlan("cancel")
	if got := be.decideCalls[len(be.decideCalls)-1]; got != "t1:reject" {
		t.Fatalf("cancel calls: %v", be.decideCalls)
	}

	// selectPick routes the plan menu through decidePlan.
	m.openPlanDecisionPicker()
	m.pickSel = 0
	m.selectPick()
	if got := be.decideCalls[len(be.decideCalls)-1]; got != "t1:approve" {
		t.Fatalf("selectPick approve: %v", be.decideCalls)
	}
	if m.pickMode != "" {
		t.Fatalf("picker stayed open: %q", m.pickMode)
	}
}

func TestResumeSessionRestoresPlanState(t *testing.T) {
	be := &planCLIBackend{fakeCLIBackend: &fakeCLIBackend{
		details: map[string]*backend.ThreadDetail{
			"pending": {
				ThreadSummary: backend.ThreadSummary{ID: "pending", PlanMode: "pending_approval"},
				Messages: []backend.Message{
					{Role: "user", Content: "plan it"},
					{Role: "assistant", Content: "the plan"},
				},
			},
			"planning": {ThreadSummary: backend.ThreadSummary{ID: "planning", PlanMode: "planning"}},
			"plain":    {ThreadSummary: backend.ThreadSummary{ID: "plain"}},
		},
	}}

	// A pending decision re-opens the menu over the resumed transcript.
	m := newModel(&session{backend: be})
	m.width = 80
	m.resumeSession("pending")
	if m.pickMode != "plan" {
		t.Fatalf("pending plan did not reopen the picker: %q", m.pickMode)
	}
	if m.lastPlanText != "the plan" {
		t.Fatalf("plan text not recovered from transcript: %q", m.lastPlanText)
	}
	if m.sess.planMode {
		t.Fatal("pending approval must not re-arm the plan-mode toggle")
	}

	// An interrupted plan turn re-arms plan mode.
	m2 := newModel(&session{backend: be})
	m2.width = 80
	m2.resumeSession("planning")
	if !m2.sess.planMode {
		t.Fatal("'planning' state did not restore the plan-mode toggle")
	}
	if m2.pickMode != "" {
		t.Fatalf("picker opened for 'planning' state: %q", m2.pickMode)
	}

	// A plain thread clears any stale toggle.
	m3 := newModel(&session{backend: be})
	m3.width = 80
	m3.sess.planMode = true
	m3.resumeSession("plain")
	if m3.sess.planMode {
		t.Fatal("plain thread kept a stale plan-mode toggle")
	}
	if m3.pickMode != "" {
		t.Fatalf("picker opened for a plain thread: %q", m3.pickMode)
	}
}
