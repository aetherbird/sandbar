// Editor power: a readline-style kill ring, yank-pop, undo (ctrl+_), a
// large-paste collapse, and an external-editor round-trip (ctrl+g) layered
// over the bubbles v2 textarea — the legacy sandbar editor deltas.
//
// Every mutation works at the textarea's logical (line, rune-column) cursor
// and rewrites via SetValue + a reposition, rather than fighting the widget's
// internal edit model. The kill ring holds the last 20 kills; alt+y rotates
// through it after a ctrl+y yank. Undo snapshots value+cursor before each
// mutation (100 deep); oversized pastes collapse to a bounded head while the
// full text stays recoverable from the kill ring.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// killRingMax bounds the kill ring; undoDepth bounds the undo stack.
const (
	killRingMax = 20
	undoDepth   = 100
)

// cursorSnap is a snapshot of the editor's value and logical cursor.
type cursorSnap struct {
	value    string
	row, col int
}

// yankSnap tracks a ctrl+y insertion so alt+y can swap it for the next ring
// entry without re-computing where the inserted text landed.
type yankSnap struct {
	pre    cursorSnap
	active bool
}

// editKey dispatches the readline-style editing keys (see the Update switch).
func (m *appModel) editKey(k tea.KeyPressMsg) {
	switch k.String() {
	case "ctrl+k":
		m.killLine()
	case "ctrl+u":
		m.killToStart()
	case "ctrl+w":
		m.killWordBackward()
	case "ctrl+y":
		m.yank()
	case "alt+y":
		m.yankPop()
	}
}

// --- kill ring -------------------------------------------------------------

// addKill appends to the kill ring and points the yank cursor at it.
func (m *appModel) addKill(text string) {
	if text == "" {
		return
	}
	m.killRing = append(m.killRing, text)
	if len(m.killRing) > killRingMax {
		m.killRing = m.killRing[len(m.killRing)-killRingMax:]
	}
	m.killIdx = len(m.killRing) - 1
}

// killLine kills from the cursor to the end of the current line; at end of
// line it kills the newline, joining the next line up.
func (m *appModel) killLine() {
	value := m.ta.Value()
	lines := strings.Split(value, "\n")
	row, col := m.ta.Line(), m.ta.Column()
	if row >= len(lines) {
		return
	}
	r := []rune(lines[row])
	if col >= len(r) && row+1 >= len(lines) {
		return // nothing to kill at the end of the last line
	}
	m.pushUndoState()
	if col >= len(r) {
		m.addKill("\n")
		lines[row] += lines[row+1]
		lines = append(lines[:row+1], lines[row+2:]...)
		m.commitLines(lines, row, col)
		return
	}
	m.addKill(string(r[col:]))
	lines[row] = string(r[:col])
	m.commitLines(lines, row, col)
}

// killToStart kills from the start of the line to the cursor.
func (m *appModel) killToStart() {
	value := m.ta.Value()
	lines := strings.Split(value, "\n")
	row, col := m.ta.Line(), m.ta.Column()
	if row >= len(lines) || col == 0 {
		return
	}
	m.pushUndoState()
	r := []rune(lines[row])
	m.addKill(string(r[:col]))
	lines[row] = string(r[col:])
	m.commitLines(lines, row, 0)
}

// killWordBackward kills the whitespace-delimited word ending just before
// the cursor (readline's unix-word-rubout).
func (m *appModel) killWordBackward() {
	value := m.ta.Value()
	lines := strings.Split(value, "\n")
	row, col := m.ta.Line(), m.ta.Column()
	if row >= len(lines) {
		return
	}
	r := []rune(lines[row])
	i := col
	for i > 0 && isSpaceRune(r[i-1]) {
		i--
	}
	for i > 0 && !isSpaceRune(r[i-1]) {
		i--
	}
	if i == col {
		return
	}
	m.pushUndoState()
	m.addKill(string(r[i:col]))
	lines[row] = string(r[:i]) + string(r[col:])
	m.commitLines(lines, row, i)
}

// yank inserts the current kill-ring entry at the cursor and arms alt+y.
func (m *appModel) yank() {
	if len(m.killRing) == 0 {
		return
	}
	m.pushUndoState()
	m.yankState = yankSnap{pre: cursorSnap{m.ta.Value(), m.ta.Line(), m.ta.Column()}, active: true}
	m.insertAtCursor(m.killRing[m.killIdx])
}

// yankPop swaps the just-yanked text for the previous ring entry.
func (m *appModel) yankPop() {
	if !m.yankState.active || len(m.killRing) == 0 {
		return
	}
	m.ta.SetValue(m.yankState.pre.value)
	m.reposition(m.yankState.pre.row, m.yankState.pre.col)
	m.killIdx--
	if m.killIdx < 0 {
		m.killIdx = len(m.killRing) - 1
	}
	m.insertAtCursor(m.killRing[m.killIdx])
}

// --- low-level editor helpers ---------------------------------------------

// reposition moves the text cursor to the logical (row, col) after a
// SetValue. The textarea exposes no logical-row setter (CursorDown/CursorUp
// move across soft-wrapped visual rows), so movement happens under an
// effectively no-wrap width — where the vertical moves ARE logical — and the
// real width is restored after.
func (m *appModel) reposition(row, col int) {
	lines := strings.Split(m.ta.Value(), "\n")
	if len(lines) == 0 {
		m.ta.Reset()
		return
	}
	if row >= len(lines) {
		row = len(lines) - 1
	}
	width := m.ta.Width()
	m.ta.SetWidth(1 << 20) // no soft wrapping: CursorUp/CursorDown walk logical rows
	m.ta.MoveToEnd()
	for m.ta.Line() > row {
		m.ta.CursorUp()
	}
	m.ta.SetCursorColumn(col)
	m.ta.SetWidth(width)
}

// commitLines writes lines back and repositions the cursor.
func (m *appModel) commitLines(lines []string, row, col int) {
	m.ta.SetValue(strings.Join(lines, "\n"))
	m.reposition(row, col)
}

// insertAtCursor inserts text at the current cursor (which may contain
// newlines), leaving the cursor at the end of the inserted text.
func (m *appModel) insertAtCursor(text string) {
	value := m.ta.Value()
	lines := strings.Split(value, "\n")
	row, col := m.ta.Line(), m.ta.Column()
	if row >= len(lines) {
		return
	}
	r := []rune(lines[row])
	inserted := string(r[:col]) + text + string(r[col:])
	seg := strings.Split(inserted, "\n")
	joined := make([]string, 0, len(lines)+len(seg)-1)
	joined = append(joined, lines[:row]...)
	joined = append(joined, seg...)
	joined = append(joined, lines[row+1:]...)
	m.ta.SetValue(strings.Join(joined, "\n"))
	m.reposition(row+len(seg)-1, len([]rune(seg[len(seg)-1])))
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' }

// --- undo ------------------------------------------------------------------

// pushUndo records the pre-edit snapshot that undo will restore. It also
// deactivates the yank-pop chain: alt+y only rotates immediately after
// ctrl+y, and any other mutation invalidates the pre-yank snapshot.
func (m *appModel) pushUndo(snap cursorSnap) {
	m.yankState.active = false
	m.undoStack = append(m.undoStack, snap)
	if len(m.undoStack) > undoDepth {
		m.undoStack = m.undoStack[len(m.undoStack)-undoDepth:]
	}
}

// pushUndoState snapshots the current value and cursor.
func (m *appModel) pushUndoState() {
	m.pushUndo(cursorSnap{value: m.ta.Value(), row: m.ta.Line(), col: m.ta.Column()})
}

// undo restores the most recent pre-mutation snapshot.
func (m *appModel) undo() {
	if len(m.undoStack) == 0 {
		return
	}
	snap := m.undoStack[len(m.undoStack)-1]
	m.undoStack = m.undoStack[:len(m.undoStack)-1]
	m.yankState.active = false
	m.ta.SetValue(snap.value)
	m.reposition(snap.row, snap.col)
	m.syncInputHeight()
}

// --- large-paste collapse ---------------------------------------------------

// pasteTriggerLines/pasteTriggerChars bound what a single paste drops into
// the editor unchanged; anything over collapses to pasteHeadLines lines /
// pasteHeadChars chars plus a marker line, so a megabyte dump can't wedge
// the renderer.
const (
	pasteTriggerLines = 200
	pasteTriggerChars = 20000
	pasteHeadLines    = 40
	pasteHeadChars    = 8000
)

// collapsePaste consumes an oversized paste, returning true when it did.
// The buffer gets the bounded head plus a marker line; the kill ring gets
// the FULL original text, so nothing pasted is ever unrecoverable (collapse
// is a display/buffer concern only).
func (m *appModel) collapsePaste(text string) bool {
	lines := strings.Count(text, "\n") + 1
	if lines <= pasteTriggerLines && len(text) <= pasteTriggerChars {
		return false
	}
	m.pushUndoState()
	m.addKill(text)
	head := text
	lineCut := false
	if lines > pasteHeadLines {
		lineCut = true
		idx := 0
		for i := 0; i < pasteHeadLines; i++ {
			j := strings.IndexByte(head[idx:], '\n')
			if j < 0 {
				idx = len(head)
				break
			}
			idx += j + 1
		}
		head = head[:idx]
	}
	charCut := false
	if len(head) > pasteHeadChars {
		charCut = true
		cut := pasteHeadChars
		// Back up to a rune boundary so the cut can't split a UTF-8
		// sequence and poison the buffer with invalid bytes.
		for cut > 0 && !utf8.RuneStart(head[cut]) {
			cut--
		}
		head = head[:cut]
	}
	marker := ""
	switch {
	case lineCut:
		marker = fmt.Sprintf("… %d more lines collapsed (sandbar paste limit)", lines-(strings.Count(head, "\n")+1))
	case charCut:
		marker = fmt.Sprintf("… %d chars collapsed (sandbar paste limit)", len(text)-len(head))
	}
	inserted := strings.TrimRight(head, "\n") + "\n" + marker
	start := m.cursorOffset()
	m.insertAtCursor(inserted)
	m.repositionToOffset(start + len(inserted)) // cursor at the end of the inserted text
	return true
}

// --- external editor round-trip ---------------------------------------------

// extEditSeed is written into the temp file when the draft is empty, so the
// user has a comment line to edit over; it is stripped on read-back.
const extEditSeed = "<!-- sandbar: your message below this line -->"

// editorDoneMsg carries the result of an external-editor round-trip.
type editorDoneMsg struct {
	path string
	err  error
}

// openExternalEditor suspends the REPL and opens $VISUAL/$EDITOR (default vi)
// on a temp file seeded with the current draft, restoring it on return.
func (m *appModel) openExternalEditor() tea.Cmd {
	f, err := os.CreateTemp("", "sandbar-edit-*.md")
	if err != nil {
		return m.printLine("\n" + sty(cErr).Render("  ✗ editor: "+err.Error()))
	}
	path := f.Name()
	seed := m.ta.Value()
	if strings.TrimSpace(seed) == "" {
		seed = extEditSeed + "\n"
	}
	if _, err := f.WriteString(seed); err != nil {
		f.Close()
		os.Remove(path)
		return m.printLine("\n" + sty(cErr).Render("  ✗ editor: "+err.Error()))
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return m.printLine("\n" + sty(cErr).Render("  ✗ editor: "+err.Error()))
	}
	return tea.ExecProcess(editorCommand(path), func(err error) tea.Msg {
		return editorDoneMsg{path: path, err: err}
	})
}

// editorCommand builds the exec.Cmd from $VISUAL/$EDITOR (default vi).
func editorCommand(path string) *exec.Cmd {
	name := os.Getenv("VISUAL")
	if name == "" {
		name = os.Getenv("EDITOR")
	}
	if name == "" {
		name = "vi"
	}
	fields := strings.Fields(name)
	return exec.Command(fields[0], append(fields[1:], path)...)
}

// finishExternalEditor reads the edited temp file back into the editor. A
// failed editor (non-zero exit) or an unchanged file keeps the original
// draft; a seeded empty draft strips the seed comment again.
func (m *appModel) finishExternalEditor(msg editorDoneMsg) tea.Cmd {
	defer os.Remove(msg.path)
	if msg.err != nil {
		return m.printLine("\n" + sty(cErr).Render("  ✗ editor: "+msg.err.Error()))
	}
	data, err := os.ReadFile(msg.path)
	if err != nil {
		return m.printLine("\n" + sty(cErr).Render("  ✗ editor: read back: "+err.Error()))
	}
	content := strings.TrimRight(string(data), "\n")
	if strings.HasPrefix(content, extEditSeed) {
		content = strings.TrimSpace(strings.TrimPrefix(content, extEditSeed))
	}
	if content == m.ta.Value() {
		return nil // editor made no change: keep the original draft and cursor
	}
	m.pushUndoState() // the write-back is one undoable step
	m.ta.SetValue(content)
	m.ta.CursorEnd()
	m.syncInputHeight()
	return nil
}
