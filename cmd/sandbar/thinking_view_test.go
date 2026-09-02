package main

import (
	"strings"
	"testing"
	"time"
)

// newThinkingModel returns a model mid-stream in a reasoning phase, ready for
// indicator tests.
func newThinkingModel(t *testing.T) appModel {
	t.Helper()
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	m.streamGen = 1
	m.streamCh = make(chan streamItem)
	m.streaming = true
	upd, _ := m.Update(streamItem{gen: 1, kind: "thinking"})
	m = upd.(appModel)
	if !m.thinking {
		t.Fatal("thinking event did not arm the indicator")
	}
	return m
}

// TestThinkingIndicatorAnimated pins the animation: the indicator rides in the
// View frame, steps visibly on every spinner tick even with colors disabled
// (the dots alone animate), and rotates the themed gradient when colors are on.
func TestThinkingIndicatorAnimated(t *testing.T) {
	m := newThinkingModel(t)

	if view := stripANSI(m.View().Content); !strings.Contains(view, "Thinking") {
		t.Fatalf("indicator missing from View:\n%s", view)
	}

	// No-color profile: consecutive ticks must still change the frame.
	frames := make([]string, 3)
	for i := range frames {
		m.spinIdx++
		frames[i] = stripANSI(m.View().Content)
	}
	for i := range frames {
		if frames[i] == frames[(i+1)%len(frames)] {
			t.Fatalf("indicator static across ticks (no-color profile):\n%s", frames[i])
		}
	}

	// Color profile: spinIdx advanced by a full dot cycle leaves only the
	// gradient rotation to differ.
	ss, err := newStyleSet("system", "always", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	setActiveStyleSet(ss)
	defer setActiveStyleSet(defaultStyleSet())
	a := m.View().Content
	m.spinIdx += 3
	if b := m.View().Content; a == b {
		t.Fatalf("gradient did not rotate across a full dot cycle:\n%q", a)
	}
}

// TestThinkingIndicatorTransient pins the lifecycle: any non-thinking event
// clears the row from the frame, thinking re-arms it, and it is never printed
// to the transcript (no printf carries it).
func TestThinkingIndicatorTransient(t *testing.T) {
	m := newThinkingModel(t)

	// The tick refreshes the frame; the indicator must stay in-frame only.
	upd, cmd := m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	for _, body := range batchPrintfBodies(t, cmd) {
		if strings.Contains(stripANSI(body), "Thinking") {
			t.Fatalf("indicator leaked into a printf body:\n%q", body)
		}
	}

	// A token ends the phase; the row leaves the frame.
	upd, _ = m.Update(streamItem{gen: 1, kind: "token", content: "hello"})
	m = upd.(appModel)
	if m.thinking {
		t.Fatal("token did not end the thinking phase")
	}
	if view := stripANSI(m.View().Content); strings.Contains(view, "Thinking") {
		t.Fatalf("indicator survived a token event:\n%s", view)
	}

	// A later reasoning chunk re-arms it.
	upd, _ = m.Update(streamItem{gen: 1, kind: "thinking"})
	m = upd.(appModel)
	if !m.thinking || !strings.Contains(stripANSI(m.View().Content), "Thinking") {
		t.Fatal("indicator did not re-arm on a later thinking event")
	}

	// Turn end clears it via the same non-thinking path.
	upd, _ = m.Update(streamItem{gen: 1, kind: "activity", content: "↻ retrying"})
	m = upd.(appModel)
	if m.thinking {
		t.Fatal("activity event did not end the thinking phase")
	}
}
