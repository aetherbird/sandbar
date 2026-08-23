package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/backend"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/memory"
)

// ── Item 1: reverse-i-search ────────────────────────────────────────────────

// TestReverseSearchEnterSendsAndExits pins the search-stuck fix: Enter while in
// search mode must submit the matched entry AND clear searchMode, so later
// keystrokes type normally instead of extending the invisible query.
func TestReverseSearchEnterSendsAndExits(t *testing.T) {
	m := newModel(&session{modelAlias: "m", backend: &fakeCLIBackend{}})
	m.streamGen = 1
	m.streamCh = make(chan streamItem)
	m.history = []string{"alpha", "beta gamma"}
	m.histIdx = 2

	// Enter search mode and type "beta".
	upd, _ := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = upd.(appModel)
	upd, _ = m.Update(tea.KeyPressMsg{Code: 'b', Text: "beta"})
	m = upd.(appModel)
	if m.searchMode != "reverse" || m.searchMatch != 1 {
		t.Fatalf("search state: mode=%q match=%d, want reverse/1", m.searchMode, m.searchMatch)
	}

	// Enter submits through the normal path and exits search mode.
	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(appModel)
	if m.searchMode != "" {
		t.Fatalf("searchMode after Enter = %q, want cleared", m.searchMode)
	}
	if m.streaming != true {
		t.Fatal("Enter in search mode should send the matched entry")
	}
}

// TestReverseSearchFailingState verifies a query with no match clears the
// stale match (so the prompt renders the failing state) and restores it when
// the query matches again.
func TestReverseSearchFailingState(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.history = []string{"alpha"}
	m.searchMode = "reverse"
	m.searchQuery = "alpha"
	m.doReverseSearch()
	if m.searchMatch != 0 {
		t.Fatalf("match = %d, want 0", m.searchMatch)
	}
	m.searchQuery = "zzz"
	m.doReverseSearch()
	if m.searchMatch != -1 {
		t.Fatalf("no-match searchMatch = %d, want -1 (failed state)", m.searchMatch)
	}
	if v := m.ta.Value(); v != "alpha" {
		t.Logf("note: textarea keeps previous row on fail (readline prints it too): %q", v)
	}
	m.searchQuery = "alp"
	m.doReverseSearch()
	if m.searchMatch != 0 {
		t.Fatalf("re-match searchMatch = %d, want 0", m.searchMatch)
	}
}

// TestReverseSearchCycleRollback pins the Ctrl+R cycle fix: cycling when no
// older match exists must leave the current match and displayed row alone.
func TestReverseSearchCycleRollback(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.history = []string{"one", "two one", "three one"}
	m.searchMode = "reverse"
	m.searchQuery = "one"
	m.doReverseSearch() // match = index 2 (most recent)
	if m.searchMatch != 2 {
		t.Fatalf("initial match = %d, want 2", m.searchMatch)
	}
	m.cycleReverseSearch() // → index 1 ("two one")
	if m.searchMatch != 1 {
		t.Fatalf("after cycle match = %d, want 1", m.searchMatch)
	}
	if v := m.ta.Value(); v != "two one" {
		t.Fatalf("displayed row = %q, want %q", v, "two one")
	}
	m.cycleReverseSearch() // → index 0 ("one")
	if m.searchMatch != 0 {
		t.Fatalf("after cycle match = %d, want 0", m.searchMatch)
	}
	if v := m.ta.Value(); v != "one" {
		t.Fatalf("displayed row = %q, want %q", v, "one")
	}
	m.cycleReverseSearch() // no older match: state must not move
	if m.searchMatch != 0 {
		t.Fatalf("cycle with no older match moved match to %d, want 0", m.searchMatch)
	}
	if v := m.ta.Value(); v != "one" {
		t.Fatalf("displayed row after no-op cycle = %q, want %q", v, "one")
	}
}

// ── Item 2: layout lurch ────────────────────────────────────────────────────

// TestSuggestBranchesClipTextarea pins the layout-lurch fix: the slash-suggest,
// path-suggest, and search branches must render the same clipped textarea
// height as the plain branch, so the block doesn't jump from 1 to 15 rows.
func TestSuggestBranchesClipTextarea(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 100
	plain := m.View()

	m.ta.SetValue("/")
	m.width = 100
	withPopup := m.View()
	plainLines := strings.Count(plain.Content, "\n")
	popupLines := strings.Count(withPopup.Content, "\n")
	// The popup adds its own rows; the input block below must contribute the
	// same single row as the plain view, not the full 15-row viewport.
	if popupLines-plainLines > 6 { // 17 suggestions + hint on a 100-col terminal
		t.Errorf("slash popup view lurch: plain=%d lines, popup=%d lines", plainLines, popupLines)
	}
	if popupLines > plainLines+len(slashCommands)+3 {
		t.Errorf("slash popup view renders unclipped textarea: plain=%d popup=%d", plainLines, popupLines)
	}
}

// ── Item 3: @file expansion ─────────────────────────────────────────────────

// TestExpandFileRefsWhitelistAndCap covers the extension whitelist additions,
// bare basenames, .env exclusion, and the 100KB truncation cap.
func TestExpandFileRefsWhitelistAndCap(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// New extensions inline.
	for _, name := range []string{"a.mk", "b.proto", "c.xml", "d.log", "e.yml", "f.toml", "g.ini", "h.conf", "i.mod", "j.sum", "k.make"} {
		p := write(name, "content")
		got := expandFileRefs("see @" + p)
		if !strings.Contains(got, "content") {
			t.Errorf("@%s did not expand", name)
		}
	}
	// Bare basenames inline.
	for _, name := range []string{"Makefile", "Dockerfile", "go.mod", "go.sum", "Rakefile", "Justfile"} {
		p := write(name, "content")
		if got := expandFileRefs("see @" + p); !strings.Contains(got, "content") {
			t.Errorf("@%s did not expand", name)
		}
	}
	// .env is deliberately NOT inlined.
	envPath := write(".env", "SECRET=1")
	if got := expandFileRefs("see @" + envPath); strings.Contains(got, "SECRET") {
		t.Error("@.env must not be inlined (secrets footgun)")
	}
	// Relative .env is also excluded.
	if got := expandFileRefs("see @.env"); got != "see @.env" {
		t.Errorf("relative @.env changed: %q", got)
	}
	// Unknown extension stays as-is.
	if got := expandFileRefs("see @data.bin"); got != "see @data.bin" {
		t.Errorf("unknown extension expanded: %q", got)
	}
	// Size cap with marker.
	big := write("big.log", strings.Repeat("x", maxFileRefBytes+50))
	got := expandFileRefs("@" + big)
	if !strings.Contains(got, "[truncated]") {
		t.Error("oversized file missing [truncated] marker")
	}
	if strings.Count(got, "x") > maxFileRefBytes+10 {
		t.Errorf("oversized file not capped: %d x's", strings.Count(got, "x"))
	}
	// Small file has no marker.
	if strings.Contains(expandFileRefs("@"+write("small.log", "tiny")), "[truncated]") {
		t.Error("small file wrongly truncated")
	}
}

// TestSendKeepsRawHistoryAndEcho verifies the raw @file text (not the expanded
// content) lands in history while the model receives the expansion.
func TestSendKeepsRawHistoryAndEcho(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // keep the real user history out
	dir := t.TempDir()
	p := filepath.Join(dir, "note.md")
	os.WriteFile(p, []byte("FILECONTENT"), 0644)

	be := &fakeCLIBackend{}
	m := newModel(&session{modelAlias: "m", backend: be})
	m.streamGen = 1
	m.streamCh = make(chan streamItem)

	upd, _ := m.Update(tea.KeyPressMsg{Text: "read @" + p})
	m = upd.(appModel)
	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(appModel)

	if len(m.history) != 1 || m.history[0] != "read @"+p {
		t.Fatalf("history = %#v, want the raw typed text", m.history)
	}
	// The stream goroutine writes asynchronously; wait for the payload.
	// Read under the fake's mutex — SendMessage writes the same field
	// from the stream goroutine.
	got := ""
	for i := 0; i < 50 && got == ""; i++ {
		be.mu.Lock()
		got = be.lastMessage
		be.mu.Unlock()
		if got == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got == "read @"+p || !strings.Contains(got, "FILECONTENT") {
		t.Fatalf("model payload = %q, want @file expansion", got)
	}
}

// ── Item 4: history JSONL ───────────────────────────────────────────────────

// TestHistoryJSONLRoundTrip pins multi-line history surviving a save→load
// cycle and legacy raw lines still loading.
func TestHistoryJSONLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	hist := []string{"single line", "line one\nline two\nline three", "!echo hi"}
	saveHistory(hist)

	loaded := parseHistory(mustReadHistory(t, dir))
	if len(loaded) != 3 {
		t.Fatalf("loaded %d entries, want 3: %#v", len(loaded), loaded)
	}
	if loaded[1] != hist[1] {
		t.Errorf("multi-line entry = %q, want %q", loaded[1], hist[1])
	}

	// Legacy raw lines load unchanged.
	legacy := []byte("plain entry\nanother one\n\n")
	got := parseHistory(legacy)
	if len(got) != 2 || got[0] != "plain entry" || got[1] != "another one" {
		t.Fatalf("legacy parse = %#v", got)
	}

	// Slash commands are skipped on load.
	got = parseHistory([]byte("{\"text\":\"/model\"}\nkeep me\n"))
	if len(got) != 1 || got[0] != "keep me" {
		t.Fatalf("slash filter parse = %#v", got)
	}

	// Cap: saving more than maxHistoryEntries drops the oldest.
	var many []string
	for i := 0; i < maxHistoryEntries+150; i++ {
		many = append(many, "entry")
	}
	saveHistory(many)
	loaded = parseHistory(mustReadHistory(t, dir))
	if len(loaded) != maxHistoryEntries {
		t.Fatalf("capped history = %d entries, want %d", len(loaded), maxHistoryEntries)
	}
}

func mustReadHistory(t *testing.T, xdg string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(xdg, "sandbar", "history"))
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	return b
}

// ── Item 5: shell escape ────────────────────────────────────────────────────

// TestShellEscapePrintsHintAndHistory verifies the "!" escape records the raw
// command in history and prints an immediate hint.
func TestShellEscapePrintsHintAndHistory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // keep the real user history out
	m := newModel(&session{modelAlias: "m", backend: &fakeCLIBackend{}})
	m.streamGen = 1
	m.streamCh = make(chan streamItem)

	upd, _ := m.Update(tea.KeyPressMsg{Text: "!echo hi"})
	m = upd.(appModel)
	upd, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(appModel)

	if len(m.history) != 1 || m.history[0] != "!echo hi" {
		t.Fatalf("history = %#v, want raw ! command", m.history)
	}
	if !m.escapeRunning || m.escapeCancel == nil {
		t.Fatal("escape-running state not armed")
	}
	if cmd == nil {
		t.Fatal("no command returned for shell escape")
	}
	// The completion clears the state.
	upd, _ = m.Update(shellDoneMsg{cmd: "echo hi", output: "hi\n"})
	if upd.(appModel).escapeRunning {
		t.Fatal("escapeRunning not cleared on completion")
	}
}

// ── Item 6: aliases ─────────────────────────────────────────────────────────

// TestAliasSuggestionsMatch ensures slashSuggestions matches aliases.
func TestAliasSuggestionsMatch(t *testing.T) {
	m := newModel(&session{modelAlias: "m", local: &localServices{store: new(memory.Store), ag: new(agent.Agent)}})
	m.ta.SetValue("/q")
	got := m.slashSuggestions()
	if len(got) != 1 || got[0].name != "/quit" {
		t.Fatalf("/q suggestions = %#v, want /quit", got)
	}
	m.ta.SetValue("/ex")
	got = m.slashSuggestions()
	if len(got) != 1 || got[0].name != "/quit" {
		t.Fatalf("/ex suggestions = %#v, want /quit via /exit alias", got)
	}
	m.ta.SetValue("/branch")
	if got := m.slashSuggestions(); len(got) != 1 || got[0].name != "/fork" {
		t.Fatalf("/branch suggestions = %#v, want /fork", got)
	}
	m.ta.SetValue("/compact")
	if got := m.slashSuggestions(); len(got) != 1 || got[0].name != "/compress" {
		t.Fatalf("/compact suggestions = %#v, want /compress", got)
	}
	// Help lists aliases next to their commands.
	help := m.helpText()
	for _, want := range []string{"/quit, /q, /exit", "/fork, /branch", "/compress, /compact", "keys", "Ctrl+R", "reverse-search history"} {
		if !strings.Contains(help, want) {
			t.Errorf("helpText missing %q", want)
		}
	}
}

// ── Item 7: overflow indicator ──────────────────────────────────────────────

func TestOverflowIndicator(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetWidth(40)
	if got := m.overflowIndicator(); got != "" {
		t.Errorf("short input shows indicator: %q", got)
	}
	m.ta.SetValue(strings.Repeat("line\n", inputMaxHeight+6) + "line") // 22 rows, no trailing newline
	if got := m.overflowIndicator(); !strings.Contains(stripANSI(got), "7 lines above") {
		t.Errorf("overflow indicator = %q, want '↑ 7 lines above'", got)
	}
}

// ── Item 8: Esc dismisses slash popup without wiping input ─────────────────

func TestEscDismissesSlashPopupKeepsText(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetValue("/mod")
	if len(m.slashSuggestions()) == 0 {
		t.Fatal("precondition: suggestions visible")
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = upd.(appModel)
	if v := m.ta.Value(); v != "/mod" {
		t.Fatalf("Esc wiped input: %q", v)
	}
	if len(m.slashSuggestions()) != 0 {
		t.Fatal("Esc should suppress the popup")
	}
	if !m.slashDismissed {
		t.Fatal("slashDismissed flag not set")
	}
}

// ── Item 9: mergedToolLine width budget ─────────────────────────────────────

func TestMergedToolLineWidthBudget(t *testing.T) {
	long := strings.Repeat("x", 400)
	narrow := mergedToolLine("⚙ shell_exec: cat big.log", long, 40)
	wide := mergedToolLine("⚙ shell_exec: cat big.log", long, 200)
	if lipglossWidth(narrow) > 40 || lipglossWidth(wide) > 200 {
		t.Errorf("budget exceeded: narrow=%d wide=%d", lipglossWidth(narrow), lipglossWidth(wide))
	}
	if lipglossWidth(narrow) >= lipglossWidth(wide) {
		t.Error("wider terminal should allow a longer preview")
	}
	// Degenerate narrow width still renders something sane.
	if mergedToolLine("⚙ t: c", long, 10) == "" {
		t.Error("narrow width dropped the line")
	}
}

// ── Item 10: gauge thresholds ───────────────────────────────────────────────

func TestContextGaugeThresholds(t *testing.T) {
	m := appModel{sess: &session{}, width: 100}
	cases := []struct {
		used, max int
		want      string
	}{
		{0, 1000, cGreen},
		{50, 100, cGreen},   // 50% — no dead warn tier anymore
		{79, 100, cGreen},   // 79%
		{80, 100, cWarn},    // 80%
		{89, 100, cWarn},    // 89%
		{90, 100, cErr},     // 90%
		{100, 100, cErr},    // 100%
		{999, 1000, cErr},   // 99%
		{800, 1000, cWarn},  // 80%
		{500, 1000, cGreen}, // 50% stays green
	}
	for _, c := range cases {
		m.ctxUsed, m.ctxMax = c.used, c.max
		if _, role := m.contextStatus(true); role != c.want {
			t.Errorf("ctx %d/%d role = %q, want %q", c.used, c.max, role, c.want)
		}
	}
}

// ── Item 17: sessions hidden footer ─────────────────────────────────────────

// TestSessionPickerHiddenFooter verifies dropped sessions surface as a footer.
func TestSessionPickerHiddenFooter(t *testing.T) {
	store, err := memory.OpenWithMigrations(filepath.Join(t.TempDir(), "t.db"), "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	const n = 25 // over the hereLimit of 20
	for i := 0; i < n; i++ {
		if _, err := store.CreateThreadWithWorkspace(nil, nil, "/w"); err != nil {
			t.Fatal(err)
		}
	}
	be := backend.NewLocalBackend(&config.Config{Workspace: "/w"}, store, nil, nil)
	m := newModel(&session{backend: be, local: &localServices{store: store}, workspace: "/w"})
	m.width = 100
	if cmd := m.openSessionPicker(); cmd != nil {
		t.Fatalf("openSessionPicker returned %v", cmd)
	}
	if m.pickMode != "session" || m.pickTruncated != n-20 {
		t.Fatalf("mode=%q truncated=%d, want session/%d", m.pickMode, m.pickTruncated, n-20)
	}
	out := stripANSI(m.pickerView())
	if !strings.Contains(out, "5 older hidden") {
		t.Errorf("picker missing hidden footer:\n%s", out)
	}
}

// ── Item 18: /resume prefix ─────────────────────────────────────────────────

func TestResumePrefixMatch(t *testing.T) {
	be := &fakeCLIBackend{threads: []backend.ThreadSummary{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{ID: "abcdddddddddddddddddddddddddddddd"},
	}}
	m := newModel(&session{modelAlias: "m", backend: be})

	// Unique prefix resolves.
	full, amb, err := m.resolveThreadID("AAA")
	if err != nil || len(amb) != 0 || full != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("resolve(AAA) = %q %v %v", full, amb, err)
	}
	// Ambiguous prefix lists candidates.
	full, amb, err = m.resolveThreadID("a")
	if err != nil || full != "" || len(amb) != 2 {
		t.Fatalf("resolve(a) = %q %v %v, want 2 ambiguous", full, amb, err)
	}
	// Full id resolves to itself.
	full, _, err = m.resolveThreadID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || full != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("resolve(full b) = %q %v", full, err)
	}
	// Unknown prefix resolves to nothing.
	full, _, _ = m.resolveThreadID("zzz")
	if full != "" {
		t.Fatalf("resolve(zzz) = %q, want empty", full)
	}
}

// ── Item 19: /delete ────────────────────────────────────────────────────────

// TestDeleteCommandTwoStep covers the confirm flow and the post-delete state.
func TestDeleteCommandTwoStep(t *testing.T) {
	be := &fakeCLIBackend{}
	m := newModel(&session{modelAlias: "m", backend: be, threadID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})

	// No active thread: clean message, no crash.
	m2 := newModel(&session{modelAlias: "m", backend: be})
	if cmd := runDeleteCommand(&m2, slashInvocation{}); cmd == nil {
		t.Fatal("delete with no thread returned nil")
	}

	// First /delete explains the confirm step and does NOT delete.
	cmd := runDeleteCommand(&m, slashInvocation{args: []string{}})
	if cmd == nil {
		t.Fatal("first /delete returned nil")
	}
	if m.sess.threadID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatal("first /delete must not delete the thread")
	}

	// /delete confirm deletes and resets to a clean state + picker.
	cmd = runDeleteCommand(&m, slashInvocation{args: []string{"confirm"}})
	if cmd == nil {
		t.Fatal("/delete confirm returned nil")
	}
	if m.sess.threadID != "" || m.ctxUsed != 0 || m.todos != nil {
		t.Fatalf("post-delete state: thread=%q ctx=%d todos=%v", m.sess.threadID, m.ctxUsed, m.todos)
	}
}

// ── Item 15/16 helpers ──────────────────────────────────────────────────────

func TestHistoryPathHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgtest")
	if got := historyPath(); got != "/tmp/xdgtest/sandbar/history" {
		t.Fatalf("historyPath = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := historyPath(); !strings.HasSuffix(got, filepath.Join(".config", "sandbar", "history")) {
		t.Fatalf("historyPath fallback = %q", got)
	}
}

func TestClipCountsDisplayCells(t *testing.T) {
	// Wide glyphs: 6 界 = 12 cells; clip(…, 7) must fit in 7 cells.
	got := clip(strings.Repeat("界", 6), 7)
	if w := lipglossWidth(got); w > 7 {
		t.Errorf("clip wide glyphs width = %d, want <= 7 (%q)", w, got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("clipped string missing ellipsis: %q", got)
	}
	// Short string unchanged.
	if got := clip("hello", 10); got != "hello" {
		t.Errorf("clip short = %q", got)
	}
}

func lipglossWidth(s string) int { return lipgloss.Width(s) }

// TestExpandFileRefsTruncatesOnRuneBoundary pins the UTF-8-safe cap: the cut
// point backs up to a rune boundary so the expansion stays valid UTF-8.
func TestExpandFileRefsTruncatesOnRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.log")
	// Multi-byte runes straddling the cap boundary.
	if err := os.WriteFile(p, []byte(strings.Repeat("界", maxFileRefBytes/3+50)), 0644); err != nil {
		t.Fatal(err)
	}
	got := expandFileRefs("@" + p)
	if !utf8.ValidString(got) {
		t.Fatal("truncated expansion is not valid UTF-8")
	}
	if !strings.Contains(got, "[truncated]") {
		t.Error("missing [truncated] marker")
	}
}

// TestReverseSearchEnterSendsWithPathMatch verifies search acceptance is not
// hijacked by the path-completion popup when the recalled entry contains a
// path (Enter must SEND, not complete).
func TestReverseSearchEnterSendsWithPathMatch(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "notes.md"), []byte("x"), 0644)

	be := &fakeCLIBackend{}
	m := newModel(&session{modelAlias: "m", backend: be})
	m.streamGen = 1
	m.streamCh = make(chan streamItem)
	wd, _ := os.Getwd()
	_ = wd
	m.history = []string{"read " + filepath.Join(dir, "sub") + "/notes.md"}
	m.histIdx = 1

	upd, _ := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = upd.(appModel)
	upd, _ = m.Update(tea.KeyPressMsg{Text: "notes"})
	m = upd.(appModel)
	if m.searchMatch != 0 {
		t.Fatalf("match = %d, want 0", m.searchMatch)
	}
	// The path popup would fire on this value outside search mode.
	if len(m.pathSuggestions()) == 0 {
		t.Fatal("precondition: path popup should be triggerable for this value")
	}

	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(appModel)
	if m.searchMode != "" {
		t.Fatal("searchMode not cleared")
	}
	if !m.streaming {
		t.Fatal("Enter should send the recalled entry, not complete the path")
	}
	if m.slashDismissed || m.pathDismissed {
		t.Fatal("dismiss flags must be re-armed after send")
	}
}
