package main

import (
	"strings"
	"testing"
	"time"
)

// TestBackgroundTaskRegistration pins the registration path: a "subagent"
// streamItem carrying status "background" registers the task and schedules
// the completion poller.
func TestBackgroundTaskRegistration(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	m.streamGen = 1
	m.streamCh = make(chan streamItem)

	upd, _ := m.Update(streamItem{gen: 1, kind: "subagent", taskID: "task-1", taskStatus: "background", taskGoal: "survey"})
	m2 := upd.(appModel)
	if m2.bgTasks["task-1"] != "survey" {
		t.Fatalf("background task not registered: %+v", m2.bgTasks)
	}
}

// TestBackgroundTaskDeliveryIdle pins the idle delivery: a completed poll
// hands the result to the model as an automatic follow-up turn.
func TestBackgroundTaskDeliveryIdle(t *testing.T) {
	m := newModel(&session{modelAlias: "m", backend: &fakeCLIBackend{}})
	m.width = 80
	m.bgTasks = map[string]string{"task-1": "survey"}

	upd, _ := m.Update(bgTaskDoneMsg{taskID: "task-1", status: "completed", result: "mapped 12 packages"})
	m2 := upd.(appModel)
	if _, still := m2.bgTasks["task-1"]; still {
		t.Fatal("task not removed after delivery")
	}
	if !m2.streaming {
		t.Fatal("completed background task did not start a follow-up turn while idle")
	}
}

// TestBackgroundTasksKeepIndicatorsAlive pins the "still working" contract:
// while background sub-agents run between turns, the status bar animates and
// reports the agent count, the tick stays fast, and the HUD rows survive the
// turn ending.
func TestBackgroundTasksKeepIndicatorsAlive(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 100

	// Two background agents registered; the turn is over.
	m.bgTasks = map[string]string{"t1": "survey", "t2": "verify"}
	m.subagents = map[string]subagentHUDItem{
		"t1": {id: "t1", goal: "survey", status: "background"},
		"t2": {id: "t2", goal: "verify", status: "background"},
	}
	m.subagentOrder = []string{"t1", "t2"}
	m.turnDur = 53 * time.Second

	bar := m.statusLine()
	if !strings.Contains(bar, "2 agents") {
		t.Fatalf("status bar must show the live agent count:\n%s", bar)
	}
	// Spinner animates across ticks while agents run.
	spinA, _ := m.turnActivity()
	m.spinIdx += 3
	spinB, _ := m.turnActivity()
	if spinA == spinB {
		t.Fatal("spinner does not animate while background tasks run")
	}

	// The turn ends: HUD rows for background tasks survive.
	m.clearSubagentHUD()
	if len(m.subagents) != 2 || len(m.subagentOrder) != 2 {
		t.Fatalf("background HUD rows cleared at turn end: %+v", m.subagents)
	}

	// The tick stays on the fast cadence while agents run.
	upd, _ := m.Update(tickMsg(time.Now()))
	m2 := upd.(appModel)
	if m2.spinIdx == m.spinIdx {
		t.Fatal("tick did not advance the spinner with background tasks running")
	}

	// Completion removes the row and drops the count (bgTaskDoneMsg deletes
	// from bgTasks, then the HUD row).
	delete(m.bgTasks, "t1")
	m.updateSubagentHUD(streamItem{taskID: "t1", taskStatus: "completed"})
	if _, still := m.subagents["t1"]; still {
		t.Fatal("completed background task kept its HUD row")
	}
	if !strings.Contains(m.statusLine(), "1 agent") {
		t.Fatalf("status bar must show 1 agent after one completes:\n%s", m.statusLine())
	}
}
