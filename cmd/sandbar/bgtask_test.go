package main

import (
	"testing"
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
