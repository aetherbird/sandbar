package main

import (
	"strings"
	"testing"
)

func TestMergedToolLine(t *testing.T) {
	cases := []struct {
		name         string
		head, prev   string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "success joins on one line",
			head:         "⚙ shell_exec: hostname",
			prev:         "the-example-host",
			wantContains: []string{"⚙ shell_exec: hostname", "→ the-example-host"},
		},
		{
			name:         "failure keeps its exit text",
			head:         "⚙ shell_exec: ssh probe",
			prev:         "exit 255 — connection refused",
			wantContains: []string{"⚙ shell_exec: ssh probe", "→ exit 255 — connection refused"},
		},
		{
			name:       "silent success is bare header",
			head:       "⚙ file_write: notes.md",
			prev:       "",
			wantAbsent: []string{"→"},
		},
		{
			name:         "error results survive",
			head:         "⚙ file_read: gone.txt",
			prev:         "error: read gone.txt: no such file",
			wantContains: []string{"→ error: read gone.txt: no such file"},
		},
	}
	for _, c := range cases {
		got := mergedToolLine(c.head, c.prev, 120)
		if got == "" {
			t.Errorf("%s: empty line", c.name)
			continue
		}
		for _, want := range c.wantContains {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in %q", c.name, want, got)
			}
		}
		for _, absent := range c.wantAbsent {
			if strings.Contains(got, absent) {
				t.Errorf("%s: unexpected %q in %q", c.name, absent, got)
			}
		}
	}
	// Long previews are clipped, not wrapped into a second content line.
	long := strings.Repeat("x", 500)
	got := mergedToolLine("⚙ shell_exec: cat big.log", long, 120)
	if strings.Contains(got, strings.Repeat("x", 100)) {
		t.Error("long preview must be clipped")
	}
}

func TestIsFailurePreview(t *testing.T) {
	for _, yes := range []string{"exit 255 — boom", "error: denied", "✗ timed out"} {
		if !isFailurePreview(yes) {
			t.Errorf("%q should classify as failure", yes)
		}
	}
	for _, no := range []string{"the-example-host", "3 files matched", ""} {
		if isFailurePreview(no) {
			t.Errorf("%q should not classify as failure", no)
		}
	}
}

// TestPendingInOrderPreservesEmissionOrder pins the interrupted-turn flush:
// held tool-call headers must surface in the order the calls arrived, not
// lexicographic ID order — IDs are opaque provider strings, so sorting them
// scrambles which step actually came first.
func TestPendingInOrderPreservesEmissionOrder(t *testing.T) {
	order := []string{"call_zzz", "call_aaa", "call_mmm"}
	pending := map[string]pendingToolLine{
		"call_zzz": {head: "⚙ shell_exec: first"},
		"call_aaa": {head: "⚙ file_read: second"},
		"call_mmm": {head: "⚙ file_write: third"},
	}
	got := pendingInOrder(order, pending)
	if len(got) != 3 {
		t.Fatalf("want 3 held lines, got %d", len(got))
	}
	want := []string{"⚙ shell_exec: first", "⚙ file_read: second", "⚙ file_write: third"}
	for i, w := range want {
		if got[i].head != w {
			t.Errorf("position %d: want %q, got %q", i, w, got[i].head)
		}
	}
}

// TestPendingInOrderSkipsMatchedIDs proves a result that already arrived
// removes its header from the flush set, so a completed call is never printed
// twice (once merged, once as a bare header).
func TestPendingInOrderSkipsMatchedIDs(t *testing.T) {
	order := []string{"call_a", "call_b", "call_c"}
	pending := map[string]pendingToolLine{
		"call_b": {head: "⚙ file_read: held"},
	}
	got := pendingInOrder(order, pending)
	if len(got) != 1 {
		t.Fatalf("want 1 held line after matching, got %d", len(got))
	}
	if got[0].head != "⚙ file_read: held" {
		t.Errorf("want the unmatched header, got %q", got[0].head)
	}
}
