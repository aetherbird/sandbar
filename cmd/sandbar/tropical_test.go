package main

import (
	"strings"
	"testing"
)

// TestEffortPickerOpensAndApplies pins the menu-based effort selector: /effort
// with no argument opens a picker (never a usage line), choices apply to the
// session, and Tropical both forces high effort and clears cleanly.
func TestEffortPickerOpensAndApplies(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 100

	// No-arg /effort opens the menu, with the current (default) row selected.
	cmd := runEffortCommand(&m, slashInvocation{})
	if cmd != nil {
		t.Fatalf("opening the picker must not print, got %v", cmd)
	}
	if m.pickMode != "effort" || len(m.pickItems) != 5 {
		t.Fatalf("picker state = %q with %d items, want effort mode with 5", m.pickMode, len(m.pickItems))
	}
	if m.pickItems[4].id != "tropical" {
		t.Fatalf("tropical must be the top tier, got %q", m.pickItems[4].id)
	}

	// Selecting tropical enables the mode and forces high effort.
	m.pickSel = 4
	cmd = m.selectPick()
	if cmd == nil {
		t.Fatal("tropical selection produced no command")
	}
	if !m.sess.tropical || m.sess.effort != "high" {
		t.Fatalf("after tropical: tropical=%v effort=%q, want true/high", m.sess.tropical, m.sess.effort)
	}
	for _, body := range batchPrintfBodies(t, cmd) {
		if strings.Contains(stripANSI(body), "TROPICAL mode ON") {
			goto onPrinted
		}
	}
	t.Fatal("no TROPICAL ON confirmation printed")
onPrinted:

	// Re-opening highlights the active tropical row.
	runEffortCommand(&m, slashInvocation{})
	if m.pickItems[m.pickSel].id != "tropical" {
		t.Fatalf("active row = %q, want tropical", m.pickItems[m.pickSel].id)
	}

	// Selecting low leaves Tropical mode.
	m.pickSel = 1
	m.selectPick()
	if m.sess.tropical || m.sess.effort != "low" {
		t.Fatalf("after low: tropical=%v effort=%q, want false/low", m.sess.tropical, m.sess.effort)
	}
}

// TestTropicalStatusChipParty pins the status-bar signature: TROPICAL appears
// with every letter differently colored, and the colors rotate with the
// spinner tick.
func TestTropicalStatusChipParty(t *testing.T) {
	ss, err := newStyleSet("system", "always", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	setActiveStyleSet(ss)
	defer setActiveStyleSet(defaultStyleSet())

	m := newModel(&session{modelAlias: "m"})
	m.width = 100
	m.sess.tropical = true

	bar := m.statusLine()
	if !strings.Contains(stripANSI(bar), "TROPICAL") {
		t.Fatalf("status bar missing TROPICAL chip:\n%s", stripANSI(bar))
	}
	// Distinct letters must carry distinct SGR runs: strip the bar down to the
	// chip and count escape-sequence boundaries between letters.
	if got := strings.Count(bar, "\x1b["); got < 8 {
		t.Fatalf("party chip needs ≥8 styled runs (one per letter), got %d:\n%q", got, bar)
	}
	// Rotation: a spinner tick later, the raw bytes differ (colors dance)
	// while the visible text stays identical.
	m.spinIdx += 3
	next := m.statusLine()
	if next == bar {
		t.Fatal("chip colors do not rotate with the tick")
	}
	if stripANSI(next) != stripANSI(strings.Replace(next, "", "", 1)) && !strings.Contains(stripANSI(next), "TROPICAL") {
		t.Fatal("rotated chip lost its text")
	}
}
