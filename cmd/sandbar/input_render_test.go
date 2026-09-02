package main

// Regression tests for reported TUI input/streaming rendering bugs:
//
//   - Bug B: characters typed near the end of a visual line of the input box
//     vanished until more typing happened. The app clips the textarea's
//     rendered view to a row count derived from a naive ceil(width/w) estimate,
//     but the bubbles v2 textarea's real word wrap spills an extra row when a
//     line lands exactly on the wrap width (or its last word would sit flush
//     against it). The clip then dropped the row holding the last typed
//     characters (and the cursor). TestTypedCharsVisibleAtWrapBoundaries pins
//     the fix: the clip must follow the textarea's own wrap.
//
//   - Bug A: while assistant text streamed, the display desynced — streamed
//     tokens erased as they printed, blank blocks opened, and the input box
//     could vanish. The progressive markdown re-render replaced its previous
//     printing in place with cursor-up/erase escapes embedded in tea.Printf
//     bodies; bubbletea v2's cursed renderer wraps printf bodies in its own
//     scroll/insert program and tracks a screen model those escapes corrupt,
//     so erased rows were never repainted. The fix renders the streaming
//     markdown inside the View frame (live block) and commits it with one
//     plain printLine at turn end. TestTypedTextSurvivesStreamingFlush pins
//     the contract: ticks emit no printf surgery (no cursor-movement escapes
//     in any printf body), the live block carries the streamed text in the
//     View, and the commit prints the full response exactly once.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// typeTestKey feeds a single printable key through the real Update path.
func typeTestKey(t *testing.T, m *appModel, ch string) {
	t.Helper()
	r := []rune(ch)[0]
	upd, _ := m.Update(tea.KeyPressMsg{Code: r, Text: ch})
	*m = upd.(appModel)
}

// isPrintfMsg reports whether msg is the message Bubble Tea delivers for a
// tea.Printf command (an unexported one-field struct holding the body).
func isPrintfMsg(msg tea.Msg) (string, bool) {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Struct || v.NumField() != 1 {
		return "", false
	}
	f := v.Field(0)
	if f.Kind() != reflect.String {
		return "", false
	}
	return f.String(), true
}

// batchPrintfBodies evaluates a command (walking Batch/Sequence containers)
// and returns the bodies of every tea.Printf it contains — the bytes the
// inline renderer would write to the terminal. Commands that block (e.g.
// waitForStreamItem parking on the stream channel) are skipped via a short
// execution timeout; their goroutine leaks for the remainder of the test
// binary's life, which is harmless here.
func batchPrintfBodies(t *testing.T, cmd tea.Cmd) []string {
	t.Helper()
	var out []string
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg, ok := cmdMsgWithTimeout(c, time.Second)
		if !ok {
			return // blocking cmd — never a printf
		}
		if body, ok := isPrintfMsg(msg); ok {
			out = append(out, body)
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, nested := range batch {
				walk(nested)
			}
			return
		}
		if v := reflect.ValueOf(msg); v.IsValid() && v.Kind() == reflect.Slice {
			for i := 0; i < v.Len(); i++ {
				if nested, ok := v.Index(i).Interface().(tea.Cmd); ok {
					walk(nested)
				}
			}
		}
	}
	walk(cmd)
	return out
}

// cmdMsgWithTimeout executes cmd, returning its message; ok=false when the
// command does not finish within d (treated as a blocking command).
func cmdMsgWithTimeout(cmd tea.Cmd, d time.Duration) (tea.Msg, bool) {
	type result struct{ msg tea.Msg }
	ch := make(chan result, 1)
	go func() { ch <- result{cmd()} }()
	select {
	case r := <-ch:
		return r.msg, true
	case <-time.After(d):
		return nil, false
	}
}

// cursorEscapes are the sequences that corrupt the cursed renderer's screen
// model when embedded in printf bodies: they move/erase the terminal cursor
// out-of-band, so rows they blank are never repainted (drift, vanishing
// streamed text). No printf body may contain them.
var cursorEscapes = []string{"\x1b[A", "\x1b[2K", "\x1b[1B", "\x1b[J"}

func assertNoCursorEscapes(t *testing.T, step string, bodies []string) {
	t.Helper()
	for _, body := range bodies {
		for _, esc := range cursorEscapes {
			if strings.Contains(body, esc) {
				t.Fatalf("after %s: printf body contains cursor escape %q (renderer desync):\n%q", step, esc, body)
			}
		}
	}
}

// newLiveStreamModel returns a model mid-stream in a tool-less turn, with the
// label seen and the given tokens accumulated, ready for tick/commit tests.
func newLiveStreamModel(t *testing.T, tokens string) appModel {
	t.Helper()
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	m.streamGen = 1
	m.streamCh = make(chan streamItem)
	m.streaming = true
	m.hadToolTurn = false

	upd, _ := m.Update(streamItem{gen: 1, kind: "label", content: "label"})
	m = upd.(appModel)
	if !m.liveLabel {
		t.Fatal("label item did not arm the live block")
	}
	upd, _ = m.Update(streamItem{gen: 1, kind: "token", content: tokens})
	m = upd.(appModel)
	return m
}

// TestTypedTextSurvivesStreamingFlush interleaves stream tokens, spinner
// ticks, and keystrokes exactly as the live session does. The progressive
// rendering must live in the View frame (never a printf), and no printf body
// may carry cursor-movement escapes — the class of payload that desynced the
// renderer and erased streamed tokens as they printed.
func TestTypedTextSurvivesStreamingFlush(t *testing.T) {
	m := newLiveStreamModel(t, strings.Repeat("hello ", 20))

	// The user types while the stream is live.
	typeTestKey(t, &m, "Z")

	// View before any tick: label armed, text not rendered yet — but typed
	// input must be visible regardless.
	if !strings.Contains(stripANSI(m.View().Content), "Z") {
		t.Fatalf("typed text missing from View:\n%s", stripANSI(m.View().Content))
	}

	// First tick refreshes the live block in-frame. It must not print.
	upd, cmd := m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	if cmd == nil {
		t.Fatal("streaming tick produced no command")
	}
	assertNoCursorEscapes(t, "first tick", batchPrintfBodies(t, cmd))
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "◈ sandbar") {
		t.Fatalf("live block label missing from View:\n%s", view)
	}
	if !strings.Contains(view, "hello") {
		t.Fatalf("streamed text missing from live block:\n%s", view)
	}
	if !strings.Contains(view, "Z") {
		t.Fatalf("typed text missing from View after tick:\n%s", view)
	}

	// More tokens arrive; the block grows in-frame, still without printing.
	upd, _ = m.Update(streamItem{gen: 1, kind: "token", content: strings.Repeat("world ", 60)})
	m = upd.(appModel)
	upd, cmd = m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	assertNoCursorEscapes(t, "growing tick", batchPrintfBodies(t, cmd))
	if view = stripANSI(m.View().Content); !strings.Contains(view, "world") {
		t.Fatalf("grown stream missing from live block:\n%s", view)
	}
	if !strings.Contains(view, "Z") {
		t.Fatalf("typed text missing from View after growing tick:\n%s", view)
	}
	if !m.streaming {
		t.Fatal("tick lost streaming state")
	}

	// Commit (turn end): one plain printLine of label + full text, and the
	// live block leaves the frame.
	commit := m.printResponse()
	if commit == nil {
		t.Fatal("commit produced no print")
	}
	bodies := batchPrintfBodies(t, commit)
	if len(bodies) != 1 {
		t.Fatalf("commit produced %d printfs, want exactly 1: %q", len(bodies), bodies)
	}
	assertNoCursorEscapes(t, "commit", bodies)
	joined := stripANSI(strings.Join(bodies, ""))
	for _, want := range []string{"◈ sandbar", "hello", "world"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commit missing %q:\n%s", want, joined)
		}
	}
	if view = stripANSI(m.View().Content); strings.Contains(view, "hello") {
		t.Fatalf("live block still showing after commit:\n%s", view)
	}
	if m.liveLabel || m.liveRendered != "" {
		t.Fatal("live state not cleared by commit")
	}
}

// TestToolArrivalCommitsLiveBlock pins the text-then-tools transition: the
// first tool item commits the live block to the transcript exactly once and
// drains tokBuf, so the inline flush cannot print the same text twice.
func TestToolArrivalCommitsLiveBlock(t *testing.T) {
	m := newLiveStreamModel(t, "answer preamble before tools ")

	upd, cmd := m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	_ = cmd
	if !strings.Contains(stripANSI(m.View().Content), "preamble") {
		t.Fatal("live block not showing before tool arrival")
	}

	upd, cmd = m.Update(streamItem{gen: 1, kind: "tool", toolName: "file_read", content: "⚙ file_read: x.go"})
	m = upd.(appModel)
	bodies := batchPrintfBodies(t, cmd)
	assertNoCursorEscapes(t, "tool arrival", bodies)
	count := strings.Count(stripANSI(strings.Join(bodies, "")), "preamble")
	if count != 1 {
		t.Fatalf("committed text printed %d times, want exactly 1: %q", count, bodies)
	}
	if m.liveLabel || m.liveRendered != "" {
		t.Fatal("live state survived tool arrival")
	}
	if len(m.tokBuf) != 0 {
		t.Fatalf("tokBuf not drained on commit: %q", m.tokBuf)
	}
	if !m.hadToolTurn {
		t.Fatal("tool arrival did not mark the turn")
	}
	if view := stripANSI(m.View().Content); strings.Contains(view, "preamble") {
		t.Fatalf("live block still in frame after tool arrival:\n%s", view)
	}
}

// TestEscInterruptCommitsPartialResponse pins the interrupt path: interrupting
// a text-only turn commits the partial response, because the stream's terminal
// item arrives stale and never prints.
func TestEscInterruptCommitsPartialResponse(t *testing.T) {
	m := newLiveStreamModel(t, "partial wisdom ")
	upd, cmd := m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	_ = cmd

	// The interrupt guard requires an active cancel handle (normally armed by
	// launchStreamGoroutine).
	m.cancel = func() {}

	upd, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = upd.(appModel)
	if cmd == nil {
		t.Fatal("interrupt produced no command")
	}
	bodies := batchPrintfBodies(t, cmd)
	assertNoCursorEscapes(t, "interrupt", bodies)
	if !strings.Contains(stripANSI(strings.Join(bodies, "")), "partial wisdom") {
		t.Fatalf("interrupt dropped the partial response: %q", bodies)
	}
	if m.liveLabel || m.liveRendered != "" {
		t.Fatal("live state survived interrupt")
	}
}

// TestTypedCharsVisibleAtWrapBoundaries reproduces Bug B: with the old clip
// arithmetic, the last typed word (and the cursor) disappeared from the View
// whenever the textarea's wrap put them on a row the estimate did not count.
func TestTypedCharsVisibleAtWrapBoundaries(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	m.ta.SetWidth(78) // newModel sets termWidth()-2; text wrap width = 76
	wrapW := m.ta.Width()
	if wrapW < 1 {
		t.Fatal("zero wrap width")
	}

	check := func(step int, last string) {
		t.Helper()
		view := stripANSI(m.View().Content)
		if !strings.Contains(view, last) {
			t.Fatalf("step %d: last typed char %q invisible in View (value=%q):\n%s",
				step, last, m.ta.Value(), view)
		}
		// Single logical line (no newlines typed): LineInfo().Height is the
		// textarea's own count of rendered rows. The clip must keep them all.
		if got, want := m.computeVisualRows(), m.ta.LineInfo().Height; got != want {
			t.Fatalf("step %d: clipped rows %d != textarea rows %d (value=%q)",
				step, got, want, m.ta.Value())
		}
	}

	// Scenario 1: a word that lands flush against the wrap width is pushed
	// onto its own row — 72 a's + " ZXY" is exactly the wrap width, so "ZXY"
	// lands on a row the old ceil() estimate did not count. The user report:
	// typing near the end of a visual line, characters vanish until more
	// typing happens.
	probe := strings.Repeat("a", wrapW-4) + " ZXY"
	for i := 0; i < len(probe); i++ {
		typeTestKey(t, &m, probe[i:i+1])
		check(i, probe[i:i+1])
	}

	// Scenario 2: continuous runs through every multiple of the wrap width —
	// the wrap emits a phantom trailing row at exact multiples, and it must
	// survive clipping too (distinct uppercase letters make a dropped tail
	// detectable).
	m2 := newModel(&session{modelAlias: "m"})
	m2.width = 80
	m2.ta.SetWidth(78)
	for i := 0; i < 3*wrapW+5; i++ {
		ch := string(rune('A' + i%26))
		typeTestKey(t, &m2, ch)
		if !strings.Contains(stripANSI(m2.View().Content), ch) {
			t.Fatalf("char %d %q invisible at wrap boundary (value len=%d)", i, ch, len(m2.ta.Value()))
		}
	}

	// Scenario 3: the same boundary word on the 4th visual line (the exact
	// "3rd or 4th visual line" report, on an 80-column terminal).
	m3 := newModel(&session{modelAlias: "m"})
	m3.width = 80
	m3.ta.SetWidth(78)
	prefix := strings.Repeat("a", 2*wrapW) + strings.Repeat("b", wrapW-5) + " "
	for _, ch := range prefix + "WXYZ" {
		typeTestKey(t, &m3, string(ch))
	}
	view := stripANSI(m3.View().Content)
	for _, want := range []string{"b", "WXYZ"} {
		if !strings.Contains(view, want) {
			t.Fatalf("boundary word %q missing from View (value=%q):\n%s", want, m3.ta.Value(), view)
		}
	}
}
