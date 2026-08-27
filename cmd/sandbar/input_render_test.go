package main

// Regression tests for the two reported TUI input rendering bugs:
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
//   - Bug A: while assistant text streamed, text typed into the input box
//     visibly disappeared. The tick-driven in-place markdown re-render used
//     erase-to-end-of-screen (CSI J), which wiped the input box and status bar
//     below the block; Bubble Tea's diff renderer repaints only rows whose
//     content changed, so the typed text stayed invisible until the next
//     keystroke. TestTypedTextSurvivesStreamingFlush pins the fix: the flush
//     payload must erase exactly the previous rendering's rows and never touch
//     anything below the block.

import (
	"fmt"
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

// batchFirstPrintfBody evaluates a command (walking Batch/Sequence
// containers) and returns the body of the first tea.Printf it finds — the
// bytes the inline renderer writes to the terminal.
func batchFirstPrintfBody(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	msg := cmd()
	if body, ok := isPrintfMsg(msg); ok {
		return body
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, nested := range batch {
			if body := batchFirstPrintfBody(t, nested); body != "" {
				return body
			}
		}
		return ""
	}
	if v := reflect.ValueOf(msg); v.IsValid() && v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			if nested, ok := v.Index(i).Interface().(tea.Cmd); ok {
				if body := batchFirstPrintfBody(t, nested); body != "" {
					return body
				}
			}
		}
	}
	return ""
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

// TestTypedTextSurvivesStreamingFlush reproduces Bug A: interleave stream
// tokens, spinner ticks, and keystrokes exactly as the live session does, and
// assert the typed text stays visible in the View after every step while the
// flush payload never erases anything below the markdown block.
func TestTypedTextSurvivesStreamingFlush(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	m.streamGen = 1
	m.streamCh = make(chan streamItem)
	m.streaming = true
	m.hadToolTurn = false

	assertTypedVisible := func(step string) {
		t.Helper()
		if !strings.Contains(stripANSI(m.View().Content), "Z") {
			t.Fatalf("after %s: typed text missing from View:\n%s", step, stripANSI(m.View().Content))
		}
	}

	// Token accumulation (the streaming goroutine's output).
	upd, _ := m.Update(streamItem{gen: 1, kind: "token", content: strings.Repeat("hello ", 20)})
	m = upd.(appModel)

	// The user types while the stream is live.
	typeTestKey(t, &m, "Z")
	assertTypedVisible("typing during stream")

	// First tick flush: no previous rendering yet, so no repositioning at all.
	upd, cmd := m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	if cmd == nil {
		t.Fatal("streaming tick produced no command")
	}
	if body := batchFirstPrintfBody(t, cmd); body == "" || strings.Contains(body, "\033[J") {
		t.Fatalf("first flush payload must be plain block text with no erase-below, got %q", body)
	}
	assertTypedVisible("first flush")

	// More tokens arrive; the block grows.
	upd, _ = m.Update(streamItem{gen: 1, kind: "token", content: strings.Repeat("world ", 60)})
	m = upd.(appModel)

	// Second tick flush: replaces the previous rendering in place. It must
	// erase exactly the previous block's rows (plus its separating blank row)
	// and never emit erase-below (CSI J), which would wipe the input box and
	// status bar that live under the block.
	oldLines := m.lastRenderLines
	upd, cmd = m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	if cmd == nil {
		t.Fatal("streaming tick produced no command")
	}
	body := batchFirstPrintfBody(t, cmd)
	if body == "" {
		t.Fatal("growing flush produced no payload")
	}
	if strings.Contains(body, "\033[J") {
		t.Fatalf("flush payload contains erase-below (erases the input box): %q", body)
	}
	wantPrefix := fmt.Sprintf("\033[%dA", oldLines+1) + strings.Repeat("\033[2K\033[1B", oldLines+1)
	if !strings.HasPrefix(body, wantPrefix) {
		t.Fatalf("flush prefix = %q, want scoped erase of exactly the old block %q", body[:min(len(body), 48)], wantPrefix)
	}
	// The payload is the block plus one trailing newline: the inline renderer
	// inserts exactly that many rows above the frame and the final cursor
	// lands on the frame's first row, where the renderer anchors the repaint.
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("flush payload must end with a newline (frame anchor), got %q", body[len(body)-8:])
	}
	if got := strings.Count(strings.TrimSuffix(body, "\n"), "\n") + 1; got != m.lastRenderLines {
		t.Fatalf("flush payload block lines = %d, want %d", got, m.lastRenderLines)
	}
	assertTypedVisible("growing flush")

	// A shrinking flush (shorter reflowed block) must also stay scoped.
	m.responseBuf = []byte("tiny")
	oldLines = m.lastRenderLines
	upd, cmd = m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	body = batchFirstPrintfBody(t, cmd)
	if strings.Contains(body, "\033[J") {
		t.Fatalf("shrinking flush payload contains erase-below: %q", body)
	}
	wantPrefix = fmt.Sprintf("\033[%dA", oldLines+1) + strings.Repeat("\033[2K\033[1B", oldLines+1)
	if !strings.HasPrefix(body, wantPrefix) {
		t.Fatalf("shrinking flush prefix = %q, want %q", body[:min(len(body), 48)], wantPrefix)
	}
	assertTypedVisible("shrinking flush")

	// Ticks keep firing while the user types: the streaming tick re-arms
	// itself, so the flush path above is what runs between keystrokes.
	upd, cmd = m.Update(tickMsg(time.Now()))
	m = upd.(appModel)
	if cmd == nil || !m.streaming {
		t.Fatal("streaming tick did not re-arm")
	}
	assertTypedVisible("tick while typing")
}
