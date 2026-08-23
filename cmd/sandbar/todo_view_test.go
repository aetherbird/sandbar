package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseTodoList(t *testing.T) {
	content := "Task list:\n" +
		"  [ ] 1 Assess the codebase\n" +
		"  [>] 2 Write the comparison\n" +
		"  [✓] 3 Read the docs\n" +
		"  [-] 4 Dropped idea\n"
	rows := parseTodoList(content)
	if len(rows) != 4 {
		t.Fatalf("rows: got %d, want 4", len(rows))
	}
	want := []todoRow{
		{status: "pending", id: "1", content: "Assess the codebase"},
		{status: "in_progress", id: "2", content: "Write the comparison"},
		{status: "completed", id: "3", content: "Read the docs"},
		{status: "cancelled", id: "4", content: "Dropped idea"},
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("row %d: got %+v, want %+v", i, rows[i], w)
		}
	}
}

func TestParseTodoListRejectsNonList(t *testing.T) {
	for _, content := range []string{
		"",
		"(no items)",
		`todo create item 1 requires a non-empty "content" string`,
		"Task list:\n  bogus line",
		"Task list:\n  [?] 1 unknown icon\n",
		"Task list:\n  [ ] \n",
		"Task list:\n  [ ] 1\n",
		"Task list:\n",
	} {
		if rows := parseTodoList(content); rows != nil {
			t.Errorf("parseTodoList(%q) = %+v, want nil fallback", content, rows)
		}
	}
}

func TestTodoPanelView(t *testing.T) {
	m := appModel{width: 80, todos: []todoRow{
		{status: "pending", id: "1", content: "first"},
		{status: "in_progress", id: "2", content: "second"},
		{status: "completed", id: "3", content: "third"},
	}}
	panel := m.todoPanelView()
	for _, want := range []string{"tasks 2 open · 3 total", "☐ first", "◐ second", "☑ third"} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel missing %q\ngot:\n%s", want, panel)
		}
	}
}

func TestTodoPanelViewEmpty(t *testing.T) {
	if panel := (appModel{width: 80}).todoPanelView(); panel != "" {
		t.Errorf("empty todos: got %q, want \"\"", panel)
	}
}

func TestDockedPanelsViewSeparatesInput(t *testing.T) {
	m := appModel{width: 80, todos: []todoRow{
		{status: "pending", id: "1", content: "open thing"},
	}}
	dv := m.dockedPanelsView()
	if dv == "" {
		t.Fatal("docked view with todos: got \"\", want panels + separator")
	}
	if !strings.HasSuffix(stripANSI(dv), strings.Repeat("╌", 80)) {
		t.Errorf("docked view should end with a dashed separator rule\ngot:\n%s", dv)
	}
	if !strings.Contains(dv, "tasks 1 open · 1 total") {
		t.Errorf("docked view missing task header\ngot:\n%s", dv)
	}
}

func TestDockedPanelsViewEmpty(t *testing.T) {
	if dv := (appModel{width: 80}).dockedPanelsView(); dv != "" {
		t.Errorf("no panels: got %q, want \"\"", dv)
	}
}

func TestTodoPanelViewHidesSettledListWhenIdle(t *testing.T) {
	done := []todoRow{
		{status: "completed", id: "1", content: "first"},
		{status: "cancelled", id: "2", content: "second"},
	}
	if panel := (appModel{width: 80, todos: done}).todoPanelView(); panel != "" {
		t.Errorf("settled list while idle: got panel, want \"\"\n%s", panel)
	}
	// During a turn the panel stays up so the user watches the final tick.
	if panel := (appModel{width: 80, streaming: true, todos: done}).todoPanelView(); panel == "" {
		t.Error("settled list while streaming: got \"\", want panel")
	}
	// An open item keeps the panel up even when idle.
	openList := append(done, todoRow{status: "pending", id: "3", content: "third"})
	if panel := (appModel{width: 80, todos: openList}).todoPanelView(); panel == "" {
		t.Error("open list while idle: got \"\", want panel")
	}
}

func TestTodoPanelViewCapsLongLists(t *testing.T) {
	var todos []todoRow
	for i := 0; i < maxTodoPanelRows+3; i++ {
		todos = append(todos, todoRow{status: "pending", id: "1", content: "task"})
	}
	panel := (appModel{width: 80, todos: todos}).todoPanelView()
	if !strings.Contains(panel, "+3 more") {
		t.Errorf("panel should collapse overflow\ngot:\n%s", panel)
	}
	if got := strings.Count(panel, "☐ task"); got != maxTodoPanelRows {
		t.Errorf("visible rows: got %d, want %d", got, maxTodoPanelRows)
	}
}

func TestTodoDisplayOrderRanksLiveWorkFirst(t *testing.T) {
	rows := []todoRow{
		{status: "completed", id: "1", content: "done old"},
		{status: "pending", id: "2", content: "open old"},
		{status: "completed", id: "3", content: "done new"},
		{status: "in_progress", id: "4", content: "live"},
		{status: "cancelled", id: "5", content: "dropped"},
		{status: "pending", id: "6", content: "open new"},
	}
	got := todoDisplayOrder(rows)
	want := []string{"live", "open old", "open new", "done old", "done new", "dropped"}
	for i, w := range want {
		if got[i].content != w {
			t.Fatalf("row %d: got %q, want %q (full order: %+v)", i, got[i].content, w, got)
		}
	}
	// The canonical slice must be untouched.
	if rows[0].content != "done old" || rows[1].content != "open old" {
		t.Fatalf("todoDisplayOrder mutated the original slice: %+v", rows)
	}
}

func TestTodoPanelViewShowsOpenWorkBeforeSettledHistory(t *testing.T) {
	// The production shape: a long multi-milestone thread where early
	// positions are all completed and the newest items are the open work.
	// The capped panel must show the open items, never a wall of ☑ with the
	// live work hidden behind "+N more".
	var todos []todoRow
	for i := 0; i < 16; i++ {
		todos = append(todos, todoRow{status: "completed", id: fmt.Sprint(i + 1), content: "milestone done"})
	}
	for i, content := range []string{"M2 backend", "M2 API", "M2 tests"} {
		todos = append(todos, todoRow{status: "pending", id: fmt.Sprint(17 + i), content: content})
	}
	panel := (appModel{width: 80, todos: todos}).todoPanelView()
	for _, want := range []string{"☐ M2 backend", "☐ M2 API", "☐ M2 tests", "+11 more"} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel missing %q\ngot:\n%s", want, panel)
		}
	}
	// Settled history may fill leftover slots, but only after every open
	// item: the first ☑ must come after the last ☐.
	lines := strings.Split(panel, "\n")
	lastOpen, firstSettled := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "☐ ") {
			lastOpen = i
		}
		if strings.Contains(line, "☑ ") && firstSettled < 0 {
			firstSettled = i
		}
	}
	if lastOpen < 0 {
		t.Fatalf("no open items visible\ngot:\n%s", panel)
	}
	if firstSettled >= 0 && firstSettled < lastOpen {
		t.Errorf("settled history ranked above open work (first ☑ at %d, last ☐ at %d)\ngot:\n%s", firstSettled, lastOpen, panel)
	}

	// in_progress outranks pending when both overflow the cap.
	todos = append(todos, todoRow{status: "in_progress", id: "20", content: "live edit"})
	overflow := make([]todoRow, 0, maxTodoPanelRows+2)
	for i := 0; i < maxTodoPanelRows; i++ {
		overflow = append(overflow, todoRow{status: "pending", id: fmt.Sprint(100 + i), content: "queued"})
	}
	overflow = append(overflow, todos...)
	panel = (appModel{width: 80, todos: overflow}).todoPanelView()
	if !strings.Contains(panel, "◐ live edit") {
		t.Errorf("panel hid the in_progress item behind pending overflow\ngot:\n%s", panel)
	}
}
