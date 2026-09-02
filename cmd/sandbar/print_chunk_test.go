package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestPrintLineChunksTallPayloads pins the insertAbove safety bound: a payload
// taller than the terminal must be printed as sequential chunks, each no
// taller than the frame-safe budget — one giant printf desyncs the cursed
// renderer (frame floats above the payload tail with blank gaps).
func TestPrintLineChunksTallPayloads(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 80
	m.height = 24

	payload := make([]string, 120)
	for i := range payload {
		payload[i] = fmt.Sprintf("line %d", i)
	}
	bodies := batchPrintfBodies(t, m.printLine(strings.Join(payload, "\n")))

	if len(bodies) < 2 {
		t.Fatalf("120-row payload printed as %d printf(s), want chunked", len(bodies))
	}
	// Budget for height 24 is 16 rows; every chunk must respect it.
	for i, body := range bodies {
		if n := strings.Count(body, "\n") + 1; n > 16 {
			t.Fatalf("chunk %d is %d rows, want ≤16", i, n)
		}
	}
	// Nothing lost: concatenating chunks (minus per-chunk trailing newlines)
	// reproduces the full payload.
	var joined strings.Builder
	for _, body := range bodies {
		joined.WriteString(strings.TrimSuffix(body, "\n"))
		joined.WriteString("\n")
	}
	got := strings.TrimSuffix(joined.String(), "\n")
	if got != strings.Join(payload, "\n") {
		t.Fatalf("chunked print lost content:\n%q", got[:min(len(got), 200)])
	}

	// Short payloads stay single printf chunks.
	one := batchPrintfBodies(t, m.printLine("short line"))
	if len(one) != 1 {
		t.Fatalf("short payload printed as %d chunks, want 1", len(one))
	}
}

// TestPrintLineChunkBudgetFollowsTerminalHeight pins that the budget tracks
// the terminal: a taller terminal yields proportionally larger chunks.
func TestPrintLineChunkBudgetFollowsTerminalHeight(t *testing.T) {
	line := strings.Repeat("x", 200) // wraps to 3 rows at width 78

	small := newModel(&session{modelAlias: "m"})
	small.width, small.height = 80, 12
	tall := newModel(&session{modelAlias: "m"})
	tall.width, tall.height = 80, 100

	// 60 wrapped rows: small terminal must chunk, tall terminal must not.
	payload := strings.Repeat(line+"\n", 20)
	payload = strings.TrimSuffix(payload, "\n")
	if got := len(batchPrintfBodies(t, small.printLine(payload))); got < 2 {
		t.Fatalf("height 12 failed to chunk a 60-row payload")
	}
	if got := len(batchPrintfBodies(t, tall.printLine(payload))); got != 1 {
		t.Fatalf("height 100 chunked a 60-row payload into %d printfs, want 1", got)
	}
}
