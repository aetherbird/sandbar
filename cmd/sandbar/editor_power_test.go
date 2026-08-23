package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// setCursor places the textarea's logical cursor at (row, col).
func setCursor(t *testing.T, m *appModel, row, col int) {
	t.Helper()
	m.reposition(row, col)
}

func pressCtrl(t *testing.T, m *appModel, code rune) {
	t.Helper()
	upd, _ := m.Update(tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
	*m = upd.(appModel)
}

func pressAlt(t *testing.T, m *appModel, code rune) {
	t.Helper()
	upd, _ := m.Update(tea.KeyPressMsg{Code: code, Mod: tea.ModAlt})
	*m = upd.(appModel)
}

// ── kill ring ────────────────────────────────────────────────────────────────

func TestKillLineToEnd(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("hello world")
	setCursor(t, &m, 0, 5)
	pressCtrl(t, &m, 'k')
	if got := m.ta.Value(); got != "hello" {
		t.Fatalf("value after ctrl+k = %q, want %q", got, "hello")
	}
	if len(m.killRing) != 1 || m.killRing[0] != " world" {
		t.Fatalf("kill ring = %q, want [%q]", m.killRing, " world")
	}
	if m.ta.Line() != 0 || m.ta.Column() != 5 {
		t.Fatalf("cursor = (%d,%d), want (0,5)", m.ta.Line(), m.ta.Column())
	}
}

func TestKillLineAtEndOfLineJoinsNext(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("one\ntwo")
	setCursor(t, &m, 0, 3)
	pressCtrl(t, &m, 'k')
	if got := m.ta.Value(); got != "onetwo" {
		t.Fatalf("value after EOL ctrl+k = %q, want %q", got, "onetwo")
	}
	if len(m.killRing) != 1 || m.killRing[0] != "\n" {
		t.Fatalf("kill ring = %q, want [newline]", m.killRing)
	}
}

func TestKillLineNoopAtLastLineEnd(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("one")
	m.ta.CursorEnd()
	pressCtrl(t, &m, 'k')
	if m.ta.Value() != "one" || len(m.killRing) != 0 {
		t.Fatalf("ctrl+k at EOF should no-op: value=%q ring=%q", m.ta.Value(), m.killRing)
	}
}

func TestKillToStart(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("hello world")
	setCursor(t, &m, 0, 5)
	pressCtrl(t, &m, 'u')
	if got := m.ta.Value(); got != " world" {
		t.Fatalf("value after ctrl+u = %q, want %q", got, " world")
	}
	if len(m.killRing) != 1 || m.killRing[0] != "hello" {
		t.Fatalf("kill ring = %q, want [%q]", m.killRing, "hello")
	}
	if m.ta.Column() != 0 {
		t.Fatalf("cursor col = %d, want 0", m.ta.Column())
	}
	// At column 0 there is nothing to kill.
	pressCtrl(t, &m, 'u')
	if len(m.killRing) != 1 {
		t.Fatalf("ctrl+u at col 0 should not grow the ring: %q", m.killRing)
	}
}

func TestKillWordBackwardUnixRubout(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	// Cursor right after the word: the word alone dies, whitespace stays.
	m.ta.SetValue("foo bar baz")
	setCursor(t, &m, 0, 7)
	pressCtrl(t, &m, 'w')
	if got := m.ta.Value(); got != "foo  baz" {
		t.Fatalf("value after ctrl+w = %q, want %q", got, "foo  baz")
	}
	if len(m.killRing) != 1 || m.killRing[0] != "bar" {
		t.Fatalf("kill ring = %q, want [%q]", m.killRing, "bar")
	}
	// Cursor after trailing whitespace: the word plus its leading space dies.
	m.ta.SetValue("foo bar baz")
	setCursor(t, &m, 0, 8)
	pressCtrl(t, &m, 'w')
	if got := m.ta.Value(); got != "foo baz" {
		t.Fatalf("value after ctrl+w past space = %q, want %q", got, "foo baz")
	}
	if len(m.killRing) != 2 || m.killRing[1] != "bar " {
		t.Fatalf("kill ring = %q, want second entry %q", m.killRing, "bar ")
	}
}

func TestYankInsertsCurrentRingEntry(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.addKill("first")
	m.addKill("second")
	m.ta.SetValue("ab")
	setCursor(t, &m, 0, 1)
	pressCtrl(t, &m, 'y')
	if got := m.ta.Value(); got != "asecondb" {
		t.Fatalf("value after ctrl+y = %q, want %q", got, "asecondb")
	}
	if m.ta.Line() != 0 || m.ta.Column() != 8 {
		t.Fatalf("cursor = (%d,%d), want (0,8)", m.ta.Line(), m.ta.Column())
	}
	if !m.yankState.active {
		t.Fatal("yank should arm alt+y")
	}
}

func TestYankMultiLineEntry(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.addKill("x\ny")
	m.ta.SetValue("ab")
	setCursor(t, &m, 0, 1)
	pressCtrl(t, &m, 'y')
	if got := m.ta.Value(); got != "ax\nyb" {
		t.Fatalf("multi-line yank = %q, want %q", got, "ax\nyb")
	}
	if m.ta.Line() != 1 || m.ta.Column() != 2 {
		t.Fatalf("cursor = (%d,%d), want (1,2)", m.ta.Line(), m.ta.Column())
	}
}

func TestYankPopRotatesThroughRing(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.addKill("one")
	m.addKill("two")
	m.ta.SetValue("")
	pressCtrl(t, &m, 'y')
	if m.ta.Value() != "two" {
		t.Fatalf("yank = %q, want %q (latest kill)", m.ta.Value(), "two")
	}
	pressAlt(t, &m, 'y')
	if m.ta.Value() != "one" {
		t.Fatalf("alt+y = %q, want %q (previous kill)", m.ta.Value(), "one")
	}
	pressAlt(t, &m, 'y')
	if m.ta.Value() != "two" {
		t.Fatalf("second alt+y = %q, want %q (wraps)", m.ta.Value(), "two")
	}
}

func TestYankPopOnlyImmediatelyAfterYank(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.addKill("one")
	m.addKill("two")
	m.ta.SetValue("")
	pressCtrl(t, &m, 'y')
	if m.ta.Value() != "two" {
		t.Fatalf("yank = %q, want %q", m.ta.Value(), "two")
	}
	// Typing breaks the yank-pop chain.
	upd, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = upd.(appModel)
	pressAlt(t, &m, 'y')
	if m.ta.Value() != "twox" {
		t.Fatalf("alt+y after typing should no-op, value = %q", m.ta.Value())
	}
}

func TestKillBreaksYankPopChain(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.addKill("one")
	m.addKill("two")
	m.ta.SetValue("")
	pressCtrl(t, &m, 'y')
	if m.ta.Value() != "two" {
		t.Fatalf("yank = %q, want %q", m.ta.Value(), "two")
	}
	// A kill between yank and alt+y invalidates the pre-yank snapshot.
	m.ta.SetValue("hello world")
	setCursor(t, &m, 0, 5)
	pressCtrl(t, &m, 'k')
	pressAlt(t, &m, 'y')
	if m.ta.Value() != "hello" {
		t.Fatalf("alt+y after kill should no-op, value = %q", m.ta.Value())
	}
}

func TestKillRingCap(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	for i := 0; i < killRingMax+5; i++ {
		m.addKill(string(rune('a'+i%26)) + strings.Repeat("x", i%3))
	}
	if len(m.killRing) != killRingMax {
		t.Fatalf("ring len = %d, want %d", len(m.killRing), killRingMax)
	}
	if m.killIdx != killRingMax-1 {
		t.Fatalf("killIdx = %d, want %d", m.killIdx, killRingMax-1)
	}
}

func TestKillKeysInertWhilePickerOpen(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("hello world")
	setCursor(t, &m, 0, 5)
	m.pickMode = "model"
	pressCtrl(t, &m, 'k')
	if m.ta.Value() != "hello world" || len(m.killRing) != 0 {
		t.Fatalf("kill keys must not fire while a picker is open: value=%q ring=%q", m.ta.Value(), m.killRing)
	}
}

// ── external editor ──────────────────────────────────────────────────────────

// installStubEditor puts a stub executable first on PATH and points $EDITOR at
// it (the pattern used by internal/tools tests).
func installStubEditor(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-editor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("EDITOR", "stub-editor")
	t.Setenv("VISUAL", "")
}

// editorRoundTripModel runs one external-editor round trip inside a real tea
// program: the Program is what dispatches tea.ExecProcess (release the
// terminal, run $EDITOR, deliver the editorDoneMsg back). It quits on
// receipt so the test can inspect the outcome.
type editorRoundTripModel struct {
	m   *appModel
	got *editorDoneMsg
}

func (e *editorRoundTripModel) Init() tea.Cmd { return e.m.openExternalEditor() }

func (e *editorRoundTripModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, ok := msg.(editorDoneMsg); ok {
		e.got = &done
		return e, tea.Quit
	}
	return e, nil
}

func (e *editorRoundTripModel) View() tea.View { return tea.NewView("") }

func runExternalEditor(t *testing.T, m *appModel) editorDoneMsg {
	t.Helper()
	w := &editorRoundTripModel{m: m}
	p := tea.NewProgram(w, tea.WithInput(nil), tea.WithOutput(io.Discard))
	if _, err := p.Run(); err != nil {
		t.Fatalf("program run: %v", err)
	}
	if w.got == nil {
		t.Fatal("editor round trip never delivered a message")
	}
	return *w.got
}

func TestExternalEditorRoundTrip(t *testing.T) {
	installStubEditor(t, `printf 'edited from stub' > "$1"`)
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("draft")
	done := runExternalEditor(t, &m)
	if done.err != nil {
		t.Fatalf("editor exec failed: %v", done.err)
	}
	upd, _ := m.Update(done)
	m = upd.(appModel)
	if got := m.ta.Value(); got != "edited from stub" {
		t.Fatalf("value after round-trip = %q, want %q", got, "edited from stub")
	}
	if _, err := os.Stat(done.path); !os.IsNotExist(err) {
		t.Fatalf("temp file %q should be removed", done.path)
	}
}

func TestExternalEditorSeedsEmptyDraft(t *testing.T) {
	installStubEditor(t, "exit 0") // opens and saves without changes
	m := newModel(&session{modelAlias: "m"})
	done := runExternalEditor(t, &m)
	if done.err != nil {
		t.Fatalf("editor exec failed: %v", done.err)
	}
	// The seeded file carries the comment line; after read-back it is
	// stripped and the (unchanged) empty draft survives.
	data, err := os.ReadFile(done.path)
	if err != nil {
		t.Fatalf("reading temp file: %v", err)
	}
	if !strings.Contains(string(data), extEditSeed) {
		t.Fatalf("empty draft should seed the temp file with the comment line, got %q", data)
	}
	upd, _ := m.Update(done)
	m = upd.(appModel)
	if m.ta.Value() != "" {
		t.Fatalf("value after no-op edit = %q, want empty", m.ta.Value())
	}
}

func TestExternalEditorKeepsOriginalOnFailure(t *testing.T) {
	installStubEditor(t, "exit 3")
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("draft")
	setCursor(t, &m, 0, 2)
	done := runExternalEditor(t, &m)
	if done.err == nil {
		t.Fatal("expected non-zero editor exit to surface as err")
	}
	upd, cmd := m.Update(done)
	m = upd.(appModel)
	if cmd == nil {
		t.Fatal("failure should print an error line")
	}
	if m.ta.Value() != "draft" {
		t.Fatalf("failed editor must keep the original draft, got %q", m.ta.Value())
	}
}

func TestExternalEditorKeepsOriginalWhenUnchanged(t *testing.T) {
	installStubEditor(t, "exit 0") // saves without touching the file
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("draft")
	setCursor(t, &m, 0, 2)
	done := runExternalEditor(t, &m)
	if done.err != nil {
		t.Fatalf("editor exec failed: %v", done.err)
	}
	upd, cmd := m.Update(done)
	m = upd.(appModel)
	if cmd != nil {
		t.Fatal("unchanged file should not print anything")
	}
	if m.ta.Value() != "draft" {
		t.Fatalf("unchanged file must keep the original draft, got %q", m.ta.Value())
	}
	if m.ta.Column() != 2 {
		t.Fatalf("unchanged file must keep the original cursor, col = %d", m.ta.Column())
	}
}

func TestExternalEditorBlockedMidStreamAndInPicker(t *testing.T) {
	installStubEditor(t, `printf 'edited' > "$1"`)
	for name, setup := range map[string]func(m *appModel){
		"streaming": func(m *appModel) { m.streaming = true },
		"picker":    func(m *appModel) { m.pickMode = "model" },
	} {
		t.Run(name, func(t *testing.T) {
			m := newModel(&session{modelAlias: "m"})
			m.ta.SetValue("draft")
			setup(&m)
			upd, cmd := m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
			m = upd.(appModel)
			if cmd != nil {
				t.Fatal("ctrl+g must be ignored while a turn/picker owns the keys")
			}
			if m.ta.Value() != "draft" {
				t.Fatalf("draft changed: %q", m.ta.Value())
			}
		})
	}
}

func TestEditorCommandSelection(t *testing.T) {
	t.Setenv("VISUAL", "stub-editor -w")
	t.Setenv("EDITOR", "ignored-editor")
	cmd := editorCommand("/tmp/f.md")
	if cmd.Args[0] != "stub-editor" || len(cmd.Args) != 3 || cmd.Args[1] != "-w" || cmd.Args[2] != "/tmp/f.md" {
		t.Fatalf("VISUAL with args = %v", cmd.Args)
	}
	t.Setenv("VISUAL", "")
	cmd = editorCommand("/tmp/f.md")
	if cmd.Args[0] != "ignored-editor" || len(cmd.Args) != 2 {
		t.Fatalf("EDITOR fallback = %v", cmd.Args)
	}
	t.Setenv("EDITOR", "")
	cmd = editorCommand("/tmp/f.md")
	if cmd.Args[0] != "vi" {
		t.Fatalf("default editor = %v, want vi", cmd.Args)
	}
}

// ── undo (ctrl+_) ───────────────────────────────────────────────────────────

func pressCtrlUnderscore(t *testing.T, m *appModel) {
	t.Helper()
	upd, _ := m.Update(tea.KeyPressMsg{Code: 0x1f, Mod: tea.ModCtrl})
	*m = upd.(appModel)
}

func TestUndoRestoresValueAndCursorAfterKill(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("hello world")
	setCursor(t, &m, 0, 5)
	pressCtrl(t, &m, 'k') // kills " world"
	if m.ta.Value() != "hello" {
		t.Fatalf("precondition: value = %q, want %q", m.ta.Value(), "hello")
	}
	pressCtrlUnderscore(t, &m)
	if got := m.ta.Value(); got != "hello world" {
		t.Fatalf("undo value = %q, want %q", got, "hello world")
	}
	if m.ta.Line() != 0 || m.ta.Column() != 5 {
		t.Fatalf("undo cursor = (%d,%d), want (0,5)", m.ta.Line(), m.ta.Column())
	}
}

func TestUndoRestoresAfterYank(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.addKill("two")
	m.ta.SetValue("ab")
	setCursor(t, &m, 0, 1)
	pressCtrl(t, &m, 'y') // "atwob"
	if m.ta.Value() != "atwob" {
		t.Fatalf("precondition: value = %q", m.ta.Value())
	}
	pressCtrlUnderscore(t, &m)
	if got := m.ta.Value(); got != "ab" {
		t.Fatalf("undo value = %q, want %q", got, "ab")
	}
	if m.ta.Line() != 0 || m.ta.Column() != 1 {
		t.Fatalf("undo cursor = (%d,%d), want (0,1)", m.ta.Line(), m.ta.Column())
	}
}

func TestUndoRestoresAfterExternalEditor(t *testing.T) {
	installStubEditor(t, `printf 'edited from stub' > "$1"`)
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("draft")
	setCursor(t, &m, 0, 2)
	done := runExternalEditor(t, &m)
	if done.err != nil {
		t.Fatalf("editor exec failed: %v", done.err)
	}
	upd, _ := m.Update(done)
	m = upd.(appModel)
	if m.ta.Value() != "edited from stub" {
		t.Fatalf("precondition: value = %q", m.ta.Value())
	}
	pressCtrlUnderscore(t, &m)
	if got := m.ta.Value(); got != "draft" {
		t.Fatalf("undo value = %q, want %q", got, "draft")
	}
	if m.ta.Line() != 0 || m.ta.Column() != 2 {
		t.Fatalf("undo cursor = (%d,%d), want (0,2)", m.ta.Line(), m.ta.Column())
	}
}

func TestUndoRestoresAfterTyping(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("ab")
	m.ta.CursorEnd()
	upd, _ := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = upd.(appModel)
	if m.ta.Value() != "abc" {
		t.Fatalf("precondition: value = %q", m.ta.Value())
	}
	pressCtrlUnderscore(t, &m)
	if got := m.ta.Value(); got != "ab" {
		t.Fatalf("undo value = %q, want %q", got, "ab")
	}
}

func TestUndoReplaysStepsInReverse(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("")
	m.ta.CursorEnd()
	for _, step := range []string{"a", "b", "c"} {
		upd, _ := m.Update(tea.KeyPressMsg{Code: rune(step[0]), Text: step})
		m = upd.(appModel)
	}
	if m.ta.Value() != "abc" {
		t.Fatalf("precondition: value = %q", m.ta.Value())
	}
	for _, want := range []string{"ab", "a", ""} {
		pressCtrlUnderscore(t, &m)
		if got := m.ta.Value(); got != want {
			t.Fatalf("undo = %q, want %q", got, want)
		}
	}
	pressCtrlUnderscore(t, &m) // empty stack: no-op
	if m.ta.Value() != "" {
		t.Fatalf("undo on empty stack must no-op, value = %q", m.ta.Value())
	}
}

func TestUndoRestoresAfterMentionInsertion(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("fix @go")
	m.ta.CursorEnd()
	m.replaceMention("@go.mod")
	if m.ta.Value() != "fix @go.mod" {
		t.Fatalf("precondition: value = %q", m.ta.Value())
	}
	pressCtrlUnderscore(t, &m)
	if got := m.ta.Value(); got != "fix @go" {
		t.Fatalf("undo value = %q, want %q", got, "fix @go")
	}
}

func TestUndoRestoresAfterNormalPaste(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("ab")
	m.ta.CursorEnd()
	upd, _ := m.Update(tea.PasteMsg{Content: "cd"})
	m = upd.(appModel)
	if m.ta.Value() != "abcd" {
		t.Fatalf("precondition: value = %q", m.ta.Value())
	}
	pressCtrlUnderscore(t, &m)
	if got := m.ta.Value(); got != "ab" {
		t.Fatalf("undo value = %q, want %q", got, "ab")
	}
}

func TestUndoStackEvictsOldestBeyond100(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	for i := 0; i < undoDepth+5; i++ {
		m.pushUndo(cursorSnap{value: strings.Repeat("x", i+1), row: 0, col: i})
	}
	if len(m.undoStack) != undoDepth {
		t.Fatalf("undo stack len = %d, want capped at %d", len(m.undoStack), undoDepth)
	}
	// The five oldest snapshots are evicted; the oldest survivor is the 6th.
	if got := m.undoStack[0].value; got != strings.Repeat("x", 6) {
		t.Fatalf("oldest survivor = %q, want %q", got, strings.Repeat("x", 6))
	}
}

func TestUndoInertMidStreamAndInPicker(t *testing.T) {
	for name, setup := range map[string]func(m *appModel){
		"streaming": func(m *appModel) { m.streaming = true },
		"picker":    func(m *appModel) { m.pickMode = "model" },
	} {
		t.Run(name, func(t *testing.T) {
			m := newModel(&session{modelAlias: "m"})
			m.ta.SetValue("ab")
			m.ta.CursorEnd()
			m.pushUndoState()
			m.ta.SetValue("abc")
			setup(&m)
			pressCtrlUnderscore(t, &m)
			if m.ta.Value() != "abc" {
				t.Fatalf("undo must be inert, value = %q", m.ta.Value())
			}
		})
	}
}

func TestUndoNoopKillCreatesNoStep(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("ab")
	m.ta.CursorEnd()
	m.pushUndoState() // snapshot "ab"
	m.ta.SetValue("abc")
	m.ta.CursorEnd()
	pressCtrl(t, &m, 'k') // kill at end of last line: no-op, no new snapshot
	pressCtrlUnderscore(t, &m)
	if m.ta.Value() != "ab" {
		t.Fatalf("no-op kill must not consume the undo history, value = %q", m.ta.Value())
	}
}

// ── large-paste collapse ─────────────────────────────────────────────────────

func TestPasteCollapseOverLineBound(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	full := b.String()
	m.ta.SetValue("")
	m.ta.CursorEnd()
	upd, _ := m.Update(tea.PasteMsg{Content: full})
	m = upd.(appModel)

	v := m.ta.Value()
	lines := strings.Split(v, "\n")
	if len(lines) != pasteHeadLines+1 {
		t.Fatalf("collapsed value has %d lines, want %d head lines + marker", len(lines), pasteHeadLines+1)
	}
	if !strings.Contains(v, "(sandbar paste limit)") {
		t.Fatalf("marker missing: %q", v)
	}
	if !strings.Contains(v, "more lines collapsed") {
		t.Fatalf("line collapse should use the line marker: %q", v)
	}
	for i := 0; i < pasteHeadLines; i++ {
		if lines[i] != fmt.Sprintf("line %04d", i) {
			t.Fatalf("head line %d = %q", i, lines[i])
		}
	}
	// The FULL original paste lands in the kill ring.
	if len(m.killRing) != 1 || m.killRing[0] != full {
		t.Fatalf("kill ring must hold the full original paste")
	}
	// Cursor at the end of the inserted text (the marker line).
	if m.ta.Line() != pasteHeadLines || m.ta.Column() != len([]rune(lines[pasteHeadLines])) {
		t.Fatalf("cursor = (%d,%d), want end of marker line", m.ta.Line(), m.ta.Column())
	}
	// The collapse is one undoable step.
	pressCtrlUnderscore(t, &m)
	if m.ta.Value() != "" {
		t.Fatalf("undo after collapse = %q, want empty", m.ta.Value())
	}
}

func TestPasteCollapseOverCharBound(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	full := "line1\n" + strings.Repeat("a", 30000) + "\nline3" // 3 lines, >20k chars
	m.ta.SetValue("xSUFFIX")
	setCursor(t, &m, 0, 1) // paste mid-buffer, before the suffix
	upd, _ := m.Update(tea.PasteMsg{Content: full})
	m = upd.(appModel)

	v := m.ta.Value()
	if len(v) > pasteHeadChars+len("xSUFFIX")+64 {
		t.Fatalf("collapsed value (%d chars) exceeds head bound + marker", len(v))
	}
	if !strings.Contains(v, "chars collapsed (sandbar paste limit)") {
		t.Fatalf("char collapse should use the char marker: %q", v)
	}
	if !strings.HasPrefix(v, "x") || !strings.HasSuffix(v, "SUFFIX") {
		t.Fatalf("paste must insert at the cursor, got %q", v)
	}
	if len(m.killRing) != 1 || m.killRing[0] != full {
		t.Fatalf("kill ring must hold the full original paste")
	}
	// Cursor at the end of the inserted text, before the suffix: the marker
	// line is line 2, at the marker's rune length.
	marker := fmt.Sprintf("… %d chars collapsed (sandbar paste limit)", len(full)-pasteHeadChars)
	if m.ta.Line() != 2 || m.ta.Column() != len([]rune(marker)) {
		t.Fatalf("cursor = (%d,%d), want (2,%d)", m.ta.Line(), m.ta.Column(), len([]rune(marker)))
	}
}

func TestPasteUnderBoundsUnaffected(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	content := strings.Repeat("hello world\n", 9) + "hello world"
	m.ta.SetValue("")
	m.ta.CursorEnd()
	upd, _ := m.Update(tea.PasteMsg{Content: content})
	m = upd.(appModel)
	if strings.Contains(m.ta.Value(), "sandbar paste limit") {
		t.Fatalf("small paste must not collapse: %q", m.ta.Value())
	}
	if len(m.killRing) != 0 {
		t.Fatalf("small paste must not enter the kill ring: %q", m.killRing)
	}
	if m.ta.Value() != content {
		t.Fatalf("value = %q, want passthrough", m.ta.Value())
	}
}

func TestPasteExactlyAtBoundsNotCollapsed(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	// Exactly 200 lines: still under the trigger.
	content := strings.Repeat("x\n", 199) + "x"
	if got := strings.Count(content, "\n") + 1; got != 200 {
		t.Fatalf("precondition: %d lines", got)
	}
	m.ta.SetValue("")
	m.ta.CursorEnd()
	upd, _ := m.Update(tea.PasteMsg{Content: content})
	m = upd.(appModel)
	if strings.Contains(m.ta.Value(), "sandbar paste limit") {
		t.Fatal("exactly 200 lines must not collapse")
	}
}
