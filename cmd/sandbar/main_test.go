package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/backend"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/memory"
)

func TestIsPipedFalse(t *testing.T) {
	// When running under go test, stdin is typically a pipe or pseudo-tty.
	// We can only verify the function doesn't panic here.
	_ = isPiped()
}

func TestStoreOpenError_BadMigrationsPath(t *testing.T) {
	// Verify that opening a store with a nonexistent migrations directory
	// returns an error, not a nil store silently.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	badMigrations := filepath.Join(dir, "nonexistent_migrations")

	store, err := memory.OpenWithMigrations(dbPath, badMigrations)
	if err == nil {
		if store != nil {
			store.Close()
		}
		t.Fatal("expected error for nonexistent migrations path, got nil error")
	}
}

func TestNilStoreGuard_BadDBPath(t *testing.T) {
	// Verify that opening a store with an invalid database path produces
	// an error. The CLI's runOneShot checks for both err and nil store
	// before proceeding. This test validates the error path is reachable.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "subdir", "missing.db") // subdir doesn't exist
	badMigrations := filepath.Join(dir, "migrations")
	if err := os.MkdirAll(badMigrations, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := memory.OpenWithMigrations(dbPath, badMigrations)
	if err == nil && store == nil {
		t.Fatal("expected error or non-nil store for bad db path, got nil error and nil store")
	}
	if store != nil {
		store.Close()
	}
}

// TestStatusLineTimerFreezesWhenIdle verifies the status-bar timer reflects the
// current/last request duration rather than session uptime: it shows "--"
// before any request, the frozen turn duration when idle, and does not climb
// between renders while idle. Regression: the timer was wired to session start
// (time.Since(m.start)) and ticked upward forever even after a response landed.
func TestStatusLineTimerFreezesWhenIdle(t *testing.T) {
	// Non-zero context so the ctx field renders numerically; any "--" then comes
	// only from the timer.
	m := appModel{sess: &session{modelAlias: "test-model"}, width: 120, ctxUsed: 100, ctxMax: 1000}

	// Before any request: idle, no completed turn → "--".
	if got := stripANSI(m.statusLine()); !strings.Contains(got, "--") {
		t.Errorf("fresh status line should show --, got: %q", got)
	}

	// After a completed request: frozen at the turn's duration, stable across renders.
	m.streaming = false
	m.turnDur = 5 * time.Second
	first := stripANSI(m.statusLine())
	if !strings.Contains(first, "5s") {
		t.Errorf("idle status line should show frozen 5s, got: %q", first)
	}
	time.Sleep(20 * time.Millisecond)
	if second := stripANSI(m.statusLine()); second != first {
		t.Errorf("idle timer must not climb between renders:\n first:  %q\n second: %q", first, second)
	}
}

// TestStatusLineTimerLiveWhileStreaming verifies the timer shows elapsed turn
// time (not "--") while a request is in flight.
func TestStatusLineTimerLiveWhileStreaming(t *testing.T) {
	m := appModel{
		sess:      &session{modelAlias: "test-model"},
		width:     120,
		ctxUsed:   100,
		ctxMax:    1000,
		streaming: true,
		turnStart: time.Now().Add(-3 * time.Second),
	}
	got := stripANSI(m.statusLine())
	if strings.Contains(got, "--") {
		t.Errorf("streaming status line should show a live timer, not --, got: %q", got)
	}
}

func TestPresentationRowsKeepProgressiveMarkdownEnabled(t *testing.T) {
	m := newModel(&session{modelAlias: "test-model"})
	m.streamGen = 7
	m.streamCh = make(chan streamItem)

	step := func(kind string) {
		t.Helper()
		updated, _ := m.Update(streamItem{gen: m.streamGen, kind: kind, content: kind})
		m = updated.(appModel)
	}

	step("label")
	if m.hadToolTurn {
		t.Fatal("assistant label disabled progressive Markdown")
	}
	step("activity")
	if m.hadToolTurn {
		t.Fatal("thinking/retry activity disabled progressive Markdown")
	}
	step("tool")
	if !m.hadToolTurn {
		t.Fatal("an actual tool call must switch to inline tool-turn rendering")
	}
}

// TestResponseAccumulatorSurvivesValueCopiesAndGC guards the Bubble Tea model
// contract: Update copies appModel by value. A strings.Builder stored in the
// model leaves its escape-analysis self-pointer aimed at an obsolete receiver
// after the first copy and can make the GC report a fatal bad pointer. A plain
// byte slice remains valid across the same copy/collection pattern.
func TestResponseAccumulatorSurvivesValueCopiesAndGC(t *testing.T) {
	m := newModel(&session{modelAlias: "test-model"})
	m.streamGen = 11
	m.streamCh = make(chan streamItem)

	const chunks = 512
	for i := 0; i < chunks; i++ {
		updated, _ := m.Update(streamItem{gen: m.streamGen, kind: "token", content: "x"})
		m = updated.(appModel)
		if i%8 == 0 {
			runtime.GC()
		}
	}
	if got := string(m.responseBuf); got != strings.Repeat("x", chunks) {
		t.Fatalf("response accumulator length/content mismatch: got %d bytes", len(got))
	}
	runtime.KeepAlive(m)
}

// TestLiveHelpers covers the small formatting helpers that ship in the live
// cmd/sandbar TUI status bar / footer.
func TestLiveHelpers(t *testing.T) {
	if got := shortModel("deepseek/deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Errorf("shortModel = %q", got)
	}
	if got := shortModel("plain-model"); got != "plain-model" {
		t.Errorf("shortModel(plain) = %q", got)
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1K", 131000: "131K"} {
		if got := fmtTok(n); got != want {
			t.Errorf("fmtTok(%d) = %q, want %q", n, got, want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{300 * time.Millisecond, "0.3s"}, {7 * time.Second, "7s"}, {62 * time.Second, "1m2s"}} {
		if got := fmtDur(c.d); got != c.want {
			t.Errorf("fmtDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	if stripANSI("\033[1;38;5;38m⚓\033[0m x") != "⚓ x" {
		t.Errorf("stripANSI did not strip escapes: %q", stripANSI("\033[1mx\033[0m"))
	}
}

// TestViewRendersStatusBar smoke-tests the live appModel View(): it must render
// the model name and status-bar anchor without panicking. Built via newModel so
// the embedded textarea is initialized like the real program.
func TestViewRendersStatusBar(t *testing.T) {
	m := newModel(&session{modelAlias: "openrouter/some-model"})
	m.width = 100
	m.ctxUsed, m.ctxMax = 50, 1000
	out := stripANSI(m.View())
	if !strings.Contains(out, "some-model") {
		t.Errorf("View should show the model name, got: %q", out)
	}
	if !strings.Contains(out, "⚓") {
		t.Errorf("View should render the status bar anchor, got: %q", out)
	}
}

// TestViewPlainBranchClipsTextarea guards against the empty-input giant box:
// the plain (no popup) branch must render exactly one textarea row, not the
// unclipped 15-row padded view stacked under a clipped one.
func TestViewPlainBranchClipsTextarea(t *testing.T) {
	m := newModel(&session{modelAlias: "openrouter/some-model"})
	m.width = 100
	m.ctxUsed, m.ctxMax = 50, 1000
	out := stripANSI(m.View())
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Errorf("idle View with empty input: got %d lines, want 4 (div, input, div, status):\n%s", len(lines), out)
	}
	if !strings.Contains(lines[1], "Message sandbar...") {
		t.Errorf("input row missing placeholder:\n%s", out)
	}
}

func TestOnelineAndClip(t *testing.T) {
	if got := oneline("a\n  b\t c\n"); got != "a b c" {
		t.Errorf("oneline = %q, want %q", got, "a b c")
	}
	if got := clip("hello", 10); got != "hello" {
		t.Errorf("clip short = %q", got)
	}
	if got := clip("hello world", 5); got != "hell…" {
		t.Errorf("clip = %q, want %q", got, "hell…")
	}
}

func TestToolPreview(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"shell_exec", `{"command":"ls -la /quests"}`, "ls -la /quests"},
		{"file_read", `{"path":"docs/SPEC.md"}`, "docs/SPEC.md"},
		{"search_files", `{"pattern":"func main","path":"."}`, "func main"},
		{"git", `{"action":"status"}`, "status"},
		{"image_generate", `{"prompt":"a red fox in snow"}`, "a red fox in snow"},
		{"vision_analyze", `{"image_path":"/tmp/cat.png"}`, "/tmp/cat.png"},
		{"shell_exec", `{"command":"echo one\ntwo"}`, "echo one two"}, // newlines collapsed
		{"unknown_tool", `{"query":"fallback works"}`, "fallback works"},
		{"shell_exec", ``, ""},            // no args
		{"shell_exec", `{"other":1}`, ""}, // primary key absent
	}
	for _, c := range cases {
		if got := toolPreview(c.tool, json.RawMessage(c.args)); got != c.want {
			t.Errorf("toolPreview(%s, %s) = %q, want %q", c.tool, c.args, got, c.want)
		}
	}
}

func TestToolResultPreview(t *testing.T) {
	cases := []struct {
		name string
		tool string
		raw  string
		want string
	}{
		{"shell success with output", "shell_exec", "Exit code: 0\nStdout:\nhello world\n", "hello world"},
		{"shell success no output", "shell_exec", "Exit code: 0\nStdout:\n", ""},
		{"shell failure with stderr", "shell_exec", "Exit code: 1\nStdout:\n\nStderr:\nno such file", "exit 1 — no such file"},
		{"shell failure no streams", "shell_exec", "Exit code: 2\nStdout:\n", "exit 2"},
		{"non-shell result", "file_read", "# Title\nbody line", "# Title body line"},
		{"empty", "shell_exec", "", ""},
	}
	for _, c := range cases {
		if got := toolResultPreview(c.tool, c.raw); got != c.want {
			t.Errorf("%s: toolResultPreview = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsDiffOutput(t *testing.T) {
	diffs := []string{
		"── patched src/main.go (+1 -1) ──\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1,1 +1,1 @@\n-a\n+b",
		"--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n-a\n+b",
	}
	for _, d := range diffs {
		if !isDiffOutput(d) {
			t.Errorf("isDiffOutput should be true for:\n%s", d)
		}
	}
	notDiffs := []string{
		"",
		"File written successfully.",
		"# Title\nbody line",
		"Exit code: 0\nStdout:\nhello",
	}
	for _, d := range notDiffs {
		if isDiffOutput(d) {
			t.Errorf("isDiffOutput should be false for: %q", d)
		}
	}
}

func TestRenderDiff(t *testing.T) {
	raw := "── wrote f.txt (+2 -0) ──\n--- a/f.txt\n+++ b/f.txt\n@@ -1,1 +1,3 @@\n a\n+b\n+c"
	out := renderDiff(raw, 120)
	// Should preserve the diff structure as separate (ANSI-wrapped) lines.
	plain := stripANSI(out)
	for _, want := range []string{"── wrote f.txt (+2 -0) ──", "--- a/f.txt", "+++ b/f.txt", "@@ -1,1 +1,3 @@", " a", "+b", "+c"} {
		if !strings.Contains(plain, want) {
			t.Errorf("renderDiff missing %q in:\n%s", want, plain)
		}
	}
	// Each rendered line should be 4-space indented.
	for _, line := range strings.Split(plain, "\n") {
		if line != "" && !strings.HasPrefix(line, "    ") {
			t.Errorf("renderDiff line not 4-space indented: %q", line)
		}
	}
	// Non-diff content renders empty.
	if out := renderDiff("", 120); out != "" {
		t.Errorf("renderDiff(empty) = %q, want empty", out)
	}
}

func TestRenderDiffTruncates(t *testing.T) {
	// Build a diff with many more lines than the cap.
	var sb strings.Builder
	sb.WriteString("── wrote big.txt (+N -0) ──\n")
	sb.WriteString("--- a/big.txt\n+++ b/big.txt\n@@ -1,1 +1,1 @@\n")
	for i := 0; i < maxDiffPreviewLines+30; i++ {
		sb.WriteString("+line\n")
	}
	out := stripANSI(renderDiff(sb.String(), 200))
	if !strings.Contains(out, "more lines)") {
		t.Errorf("renderDiff should truncate large diffs with a note, got tail:\n%s", out[len(out)-200:])
	}
}

// TestComputeVisualRows verifies the visual row count for multi-line and
// soft-wrapped content, capped at inputMaxHeight. The textarea now uses a
// fixed viewport height; net rows is used to clip the display in clipTextarea.
func TestComputeVisualRows(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetWidth(40)

	// Single short line → 1 visual row.
	m.ta.SetValue("hello")
	if h := m.computeVisualRows(); h != 1 {
		t.Errorf("short line visual rows = %d, want 1", h)
	}

	// Three hard lines → 3 visual rows.
	m.ta.SetValue("a\nb\nc")
	if h := m.computeVisualRows(); h != 3 {
		t.Errorf("three-line visual rows = %d, want 3", h)
	}

	// One long line that soft-wraps past the wrap width → more than 1 row.
	wrapW := m.ta.Width()
	m.ta.SetValue(strings.Repeat("x", wrapW*3+1))
	if h := m.computeVisualRows(); h < 4 {
		t.Errorf("wrapped line visual rows = %d, want >= 4 (wrapW=%d)", h, wrapW)
	}

	// Absurdly long content is capped at inputMaxHeight.
	m.ta.SetValue(strings.Repeat("y", wrapW*100))
	if h := m.computeVisualRows(); h != inputMaxHeight {
		t.Errorf("capped visual rows = %d, want %d", h, inputMaxHeight)
	}

	// Cleared → 1 visual row (placeholder).
	m.ta.SetValue("")
	if h := m.computeVisualRows(); h != 1 {
		t.Errorf("cleared visual rows = %d, want 1", h)
	}
}

func TestClipTextarea(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetWidth(40)

	// 15 lines rendered, 1 line of content → clip to 1 line.
	fullView := strings.Repeat("line\n", 15)
	m.ta.SetValue("hello")
	if clipped := m.clipTextarea(fullView); strings.Count(clipped, "\n") != 0 {
		t.Errorf("single-line clip: got %d lines, want 1: %q", strings.Count(clipped, "\n")+1, clipped)
	}

	// 15 lines rendered, 3 lines of content → clip to 3 lines.
	m.ta.SetValue("a\nb\nc")
	if clipped := m.clipTextarea(fullView); strings.Count(clipped, "\n") != 2 {
		t.Errorf("three-line clip: got %d lines, want 3: %q", strings.Count(clipped, "\n")+1, clipped)
	}
}

// TestPromptOnlyOnFirstRow verifies the "> " prompt is shown only on the first
// visual row; wrapped continuation rows get blank-aligned padding instead of a
// repeated prompt.
func TestPromptOnlyOnFirstRow(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.ta.SetWidth(24)
	m.ta.SetValue(strings.Repeat("x", 50)) // wraps to multiple rows
	m.syncInputHeight()
	view := stripANSI(m.ta.View())
	if n := strings.Count(view, ">"); n != 1 {
		t.Errorf("prompt '>' should appear once across wrapped rows, got %d in:\n%s", n, view)
	}
	if m.ta.Height() < 2 {
		t.Errorf("expected the box to have grown past one row, height=%d", m.ta.Height())
	}
}

// twoProviderSession builds a session whose config has two providers so the
// /model provider→model submenu flow can be exercised.
func twoProviderSession(current string) *session {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "prov-a", Models: map[string]config.ModelConfig{"a1": {}, "a2": {}, "a3": {}}},
			{Name: "prov-b", Models: map[string]config.ModelConfig{"b1": {}}},
		},
	}
	be := &fakeCLIBackend{models: []string{"prov-a/a1", "prov-a/a2", "prov-a/a3", "prov-b/b1"}}
	return &session{cfg: cfg, backend: be, local: &localServices{}, modelAlias: current}
}

func TestModelPickerSelection(t *testing.T) {
	m := newModel(twoProviderSession("a1"))
	m.openProviderPicker()
	if m.pickMode != "provider" || len(m.pickItems) != 2 {
		t.Fatalf("provider picker not armed: mode=%q items=%d", m.pickMode, len(m.pickItems))
	}
	m.selectPick() // pick prov-a (cursor starts on the current model's provider)
	if m.pickMode != "model" || m.pickProvider != "prov-a" {
		t.Fatalf("did not drill into prov-a models: mode=%q provider=%q", m.pickMode, m.pickProvider)
	}
	// models are sorted (a1,a2,a3); cursor starts on current "a1" → move to "a2".
	m.movePick(1)
	m.selectPick()
	if m.sess.modelAlias != "prov-a/a2" {
		t.Errorf("model after select = %q, want prov-a/a2 (provider-qualified)", m.sess.modelAlias)
	}
	if m.pickMode != "" {
		t.Errorf("pick mode should clear after selecting a model, got %q", m.pickMode)
	}
}

func TestModelPickerEscGoesBackToProviders(t *testing.T) {
	m := newModel(twoProviderSession("a1"))
	m.openProviderPicker()
	m.selectPick() // into prov-a models
	if m.pickMode != "model" {
		t.Fatalf("expected model submenu, got %q", m.pickMode)
	}
	// Esc from the model submenu steps back to the provider list.
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := tm.(appModel).pickMode; got != "provider" {
		t.Errorf("Esc from model submenu should return to provider list, got %q", got)
	}
}

func TestPickerCursorClampAndStart(t *testing.T) {
	m := newModel(twoProviderSession("a3"))
	m.openProviderPicker()
	m.selectPick() // prov-a models (a1,a2,a3); current a3 → cursor on index 2
	if m.pickSel != 2 {
		t.Errorf("cursor should start on current model a3 (index 2), got %d", m.pickSel)
	}
	m.movePick(5)
	if m.pickSel != 2 {
		t.Errorf("movePick should clamp to last index, got %d", m.pickSel)
	}
	m.movePick(-9)
	if m.pickSel != 0 {
		t.Errorf("movePick should clamp to 0, got %d", m.pickSel)
	}
}

func TestPickerCancel(t *testing.T) {
	m := newModel(twoProviderSession("a1"))
	m.openProviderPicker()
	m.movePick(1)
	m.cancelPick()
	if m.sess.modelAlias != "a1" {
		t.Errorf("cancel must not change model, got %q", m.sess.modelAlias)
	}
	if m.pickMode != "" {
		t.Error("pick mode should clear after cancel")
	}
}

// bigProviderSession builds a session with one provider holding n models, for
// the windowing/indent render tests.
func bigProviderSession(n int, current string) *session {
	models := map[string]config.ModelConfig{}
	for i := 0; i < n; i++ {
		models["model-"+string(rune('a'+i))] = config.ModelConfig{}
	}
	cfg := &config.Config{Providers: []config.ProviderConfig{{Name: "big", Models: models}}}
	available := make([]string, 0, len(models))
	for model := range models {
		available = append(available, "big/"+model)
	}
	return &session{cfg: cfg, backend: &fakeCLIBackend{models: available}, local: &localServices{}, modelAlias: current}
}

func TestPickerViewRendersWithWindowing(t *testing.T) {
	m := newModel(bigProviderSession(25, "model-u"))
	m.width = 100
	m.openProviderPicker()
	m.selectPick() // into big's 25 models
	out := stripANSI(m.pickerView())
	if !strings.Contains(out, "▸") {
		t.Errorf("picker should show a cursor marker:\n%s", out)
	}
	if !strings.Contains(out, "more") {
		t.Errorf("a 25-item list should show a scroll hint:\n%s", out)
	}
	if !strings.Contains(out, "model-u") {
		t.Errorf("the selected model should be visible in the window:\n%s", out)
	}
}

func TestProviderPickerListsProviders(t *testing.T) {
	m := newModel(twoProviderSession("a1"))
	m.openProviderPicker()
	if m.pickItems[0].id != "prov-a" || m.pickItems[1].id != "prov-b" {
		t.Errorf("provider items = %q, %q; want prov-a, prov-b", m.pickItems[0].id, m.pickItems[1].id)
	}
	m.selectPick() // prov-a
	// model submenu shows bare aliases (provider is in the title), sorted.
	want := []string{"a1", "a2", "a3"}
	for i, w := range want {
		if m.pickItems[i].id != w || m.pickItems[i].label != w {
			t.Errorf("model[%d] = %+v, want id/label %q", i, m.pickItems[i], w)
		}
	}
}

func TestPickerRowsNotOverIndented(t *testing.T) {
	m := newModel(bigProviderSession(25, "model-u"))
	m.width = 100
	m.openProviderPicker()
	m.selectPick()
	for _, ln := range strings.Split(stripANSI(m.pickerView()), "\n") {
		if strings.Contains(ln, "more") || strings.TrimSpace(ln) == "" {
			continue
		}
		lead := len(ln) - len(strings.TrimLeft(ln, " "))
		if lead > 4 {
			t.Errorf("picker row over-indented (lipgloss padding leak): %q (lead=%d)", ln, lead)
		}
	}
}

func TestSlashSuggestions(t *testing.T) {
	m := newModel(&session{modelAlias: "m", local: &localServices{store: new(memory.Store), ag: new(agent.Agent)}})
	cases := []struct {
		in        string
		wantN     int
		wantFirst string
	}{
		{"/", len(slashCommands), "/model"},
		{"/m", 1, "/model"},
		{"/s", 2, "/sessions"}, // /sessions, /search
		{"/c", 2, "/compress"}, // /compress, /clear
		{"/zzz", 0, ""},
		{"/model ", 0, ""}, // trailing space → args mode, no popup
		{"sup", 0, ""},     // not a slash command
		{"", 0, ""},
	}
	for _, c := range cases {
		m.ta.SetValue(c.in)
		got := m.slashSuggestions()
		if len(got) != c.wantN {
			t.Errorf("%q: got %d suggestions, want %d", c.in, len(got), c.wantN)
			continue
		}
		if c.wantN > 0 && got[0].name != c.wantFirst {
			t.Errorf("%q: first suggestion = %q, want %q", c.in, got[0].name, c.wantFirst)
		}
	}
}

func TestSlashSuggestPopupInView(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	m.width = 100
	m.ta.SetValue("/")
	out := stripANSI(m.View())
	if !strings.Contains(out, "/model") || !strings.Contains(out, "Tab complete") {
		t.Errorf("typing / should surface the autocomplete popup:\n%s", out)
	}
}

func TestSearchMessagesReturnsTypedResultMessage(t *testing.T) {
	store, err := memory.Open(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	title := "Search thread"
	thread, err := store.CreateThread(&title, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	content := "a distinctive search needle"
	if _, err := store.AppendMessage(thread.ID, "user", &content, nil); err != nil {
		t.Fatalf("append message: %v", err)
	}

	m := newModel(&session{local: &localServices{store: store}, modelAlias: "m"})
	cmd := m.searchMessages("needle")
	if cmd == nil {
		t.Fatal("searchMessages returned a nil command")
	}
	rawMsg := cmd()
	msg, ok := rawMsg.(searchDoneMsg)
	if !ok {
		t.Fatalf("search command returned %T, want searchDoneMsg", rawMsg)
	}
	if msg.err != nil {
		t.Fatalf("search command failed: %v", msg.err)
	}
	if len(msg.results) != 1 {
		t.Fatalf("search returned %d results, want 1", len(msg.results))
	}

	output := stripANSI(renderSearchResults(msg))
	for _, want := range []string{"1 result(s) for \"needle\"", "Search thread", "distinctive search needle"} {
		if !strings.Contains(output, want) {
			t.Errorf("rendered search output missing %q:\n%s", want, output)
		}
	}

	_, renderCmd := m.Update(msg)
	if renderCmd == nil {
		t.Fatal("Update did not return a command to print search results")
	}
	if nested, ok := renderCmd().(tea.Cmd); ok {
		t.Fatalf("search rendering returned a nested tea.Cmd (%T)", nested)
	}
}

func TestRenderSearchResultsTruncatesUnicodeSafely(t *testing.T) {
	snippet := strings.Repeat("界", 121)
	output := renderSearchResults(searchDoneMsg{
		query: "unicode",
		results: []memory.SearchResult{{
			ThreadID: "thread-id",
			Snippet:  snippet,
		}},
	})
	if !utf8.ValidString(output) {
		t.Fatalf("rendered search output is not valid UTF-8: %q", output)
	}
	if got := strings.Count(output, "界"); got != 120 {
		t.Fatalf("rendered snippet contains %d runes, want 120", got)
	}
	if !strings.Contains(output, "界…") {
		t.Fatalf("rendered snippet has no truncation marker: %q", output)
	}
}

// TestFirstWindowSizeSkipsClear documents that the launch-time WindowSizeMsg
// does not trigger an auto-clear (which would wipe the startup banner); only
// later width-DECREASE resizes do. Verified via the `sized` flag the guard reads.
func TestFirstWindowSizeSkipsClear(t *testing.T) {
	m := newModel(&session{modelAlias: "m"})
	if m.sized {
		t.Fatal("model should start un-sized")
	}
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !tm.(appModel).sized {
		t.Error("first WindowSizeMsg should set sized=true so the next resize clears, not this one")
	}
}

func TestRenderLastExchange(t *testing.T) {
	m := newModel(&session{modelAlias: "x"})
	m.width = 80
	msgs := []backend.Message{
		{Role: "user", Content: "first q"},
		{Role: "assistant", Content: "first a"},
		{Role: "user", Content: "the last question"},
		{Role: "assistant", Content: "the last answer"},
	}
	out := stripANSI(m.renderLastExchange(msgs))
	if !strings.Contains(out, "the last question") {
		t.Errorf("should show last user message:\n%s", out)
	}
	if !strings.Contains(out, "◈ sandbar") || !strings.Contains(out, "the last answer") {
		t.Errorf("should show last assistant response:\n%s", out)
	}
	if strings.Contains(out, "first q") {
		t.Errorf("should show only the LAST exchange, not earlier turns:\n%s", out)
	}
	if m.renderLastExchange(nil) != "" {
		t.Error("no messages should render empty")
	}
}

// TestRenderLastExchangeStylesAssistantMarkdown pins the resume-styling fix:
// stored assistant Markdown must go through the themed Glamour renderer — the
// same one the live stream uses — not plain word-wrap. Before the fix a
// resumed session showed raw "# Heading" text with no theme colors.
func TestRenderLastExchangeStylesAssistantMarkdown(t *testing.T) {
	m := newModel(&session{modelAlias: "x"})
	m.width = 80
	msgs := []backend.Message{
		{Role: "user", Content: "show me"},
		{Role: "assistant", Content: "# Title\n\nsome **bold** and `code`"},
	}
	out := m.renderLastExchange(msgs)

	if !strings.Contains(stripANSI(out), "Title") {
		t.Errorf("heading text missing after render:\n%s", out)
	}
	if !strings.Contains(stripANSI(out), "bold") {
		t.Errorf("bold text missing after render:\n%s", out)
	}

	// Under a color profile, Glamour consumes the Markdown markers and emits
	// themed ANSI; under NoTTY (piped/CI) it renders literal markers. Assert
	// per profile so the test is honest in both environments.
	if currentStyles().ColorsEnabled() {
		if strings.Contains(out, "**bold**") {
			t.Errorf("raw bold markers survived — Markdown not rendered:\n%s", out)
		}
		if !strings.Contains(out, "\x1b[") {
			t.Errorf("expected ANSI styling in rendered assistant block:\n%q", out)
		}
	}
}

// TestRenderStoredAssistantFallback proves resume never drops content when
// Glamour yields nothing: the plain-wrap fallback still returns the full text.
func TestRenderStoredAssistantFallback(t *testing.T) {
	text := "plain answer with no markdown at all, just words that wrap"
	got := renderStoredAssistant(text, 40)
	if !strings.Contains(stripANSI(got), "plain answer") {
		t.Errorf("fallback dropped content:\n%s", got)
	}
}

// TestWrapForPrint checks the ANSI-aware wrap applied to every printed
// (tea.Printf) payload. Lines break only at whitespace — hyphenated words,
// flags, and paths move to the next line whole; a token wider than the width
// (long URL, unbroken path) is hard-broken mid-token so no printed line can
// overflow and desync the inline renderer.
func TestWrapForPrint(t *testing.T) {
	green := "\033[38;5;114m"
	reset := "\033[0m"
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"empty string", "", 10, ""},
		{"shorter than width", "hello", 10, "hello"},
		{"exactly width", "abcdefghij", 10, "abcdefghij"},
		{"breaks at space", "hello bigworld", 10, "hello\nbigworld"},
		{"space at break is consumed", "hello world", 5, "hello\nworld"},
		{"no breakpoint hard-breaks", "abcdefghijklmno", 10, "abcdefghij\nklmno"},
		{"wide runes hard-break", "界界界界界", 4, "界界\n界界\n界"},
		{"long unbroken word hard-broken", strings.Repeat("a", 25), 10, "aaaaaaaaaa\naaaaaaaaaa\naaaaa"},
		{"hyphenated word stays whole when it fits", "hi there-buddy", 13, "hi\nthere-buddy"},
		{"oversized hyphenated token hard-broken", "hello-bigworld", 10, "hello-bigw\norld"},
		{"flags and paths never split at hyphens", "deploy with --name=example and self-hostable", 24, "deploy with\n--name=example and\nself-hostable"},
		{"existing newlines preserved", "abc\nde", 5, "abc\nde"},
		{"short line fits within width", "abc\nde", 5, "abc\nde"},
		{"ansi sequences preserved and not counted", green + "abcde fghij" + reset, 10, green + "abcde\nfghij" + reset},
		{"width < 1 is a no-op", "hello", 0, "hello"},
	}
	for _, c := range cases {
		got := wrapForPrint(c.in, c.width)
		if got != c.want {
			t.Errorf("%s: wrapForPrint(%q, %d) = %q, want %q", c.name, c.in, c.width, got, c.want)
		}
	}
}

// TestStatusBarWidthStable verifies the status bar always renders exactly
// m.width display cells wide: the background band must reach the right edge
// and stay put as the spinner animates, the ctx percent climbs, and the timer
// changes. Regression: padding was computed with len() on 3-byte/1-cell
// glyphs (⚓ │ █ ░ + braille), leaving the band ~28 cells short and jittering
// on every 120ms tick.
func TestStatusBarWidthStable(t *testing.T) {
	const width = 120

	// Streaming states: vary spinner frame, ctx percent, and elapsed time
	// (including a duration longer than the fixed-width segment, which the
	// trailing pad must absorb).
	for _, ctxUsed := range []int{0, 50, 620, 950, 1000, 1300} {
		for _, spin := range []int{0, 3, 7} {
			for _, elapsed := range []time.Duration{100 * time.Millisecond, 3 * time.Second, 75 * time.Second, 700 * time.Second} {
				m := appModel{
					sess:      &session{modelAlias: "prov/test-model"},
					width:     width,
					ctxUsed:   ctxUsed,
					ctxMax:    1000,
					streaming: true,
					spinIdx:   spin,
					turnStart: time.Now().Add(-elapsed),
				}
				if got := lipgloss.Width(m.statusLine()); got != width {
					t.Errorf("streaming ctx=%d spin=%d elapsed=%v: statusLine width = %d, want %d",
						ctxUsed, spin, elapsed, got, width)
				}
			}
		}
	}

	// Idle states: fresh ("--"), frozen short/long durations, no ctx gauge.
	for _, m := range []appModel{
		{sess: &session{modelAlias: "test-model"}, width: width},
		{sess: &session{modelAlias: "test-model"}, width: width, ctxUsed: 100, ctxMax: 1000, turnDur: 5 * time.Second},
		{sess: &session{modelAlias: "test-model"}, width: width, ctxUsed: 100, ctxMax: 1000, turnDur: 62 * time.Second},
	} {
		if got := lipgloss.Width(m.statusLine()); got != width {
			t.Errorf("idle %+v: statusLine width = %d, want %d", m.turnDur, got, width)
		}
	}
}

// TestFlushTokensProgressive covers the mid-stream partial flush: below the
// threshold nothing prints; past it, the buffer is cut at the last word/line
// boundary (never mid-word) and the tail stays buffered; a final flush drains
// everything regardless of size.
func TestFlushTokensProgressive(t *testing.T) {
	m := appModel{width: 80}

	// Below threshold: no-op.
	m.tokBuf = []byte("short")
	if cmd := m.flushTokens(false); cmd != nil {
		t.Error("below-threshold partial flush should be a no-op")
	}
	if string(m.tokBuf) != "short" {
		t.Errorf("buffer changed on no-op flush: %q", m.tokBuf)
	}

	// Past threshold: flush up to the last word boundary, keep the tail.
	head := strings.Repeat("x", 400)
	tail := strings.Repeat("y", 300)
	m.tokBuf = []byte(head + " " + tail)
	cmd := m.flushTokens(false)
	if cmd == nil {
		t.Fatal("past-threshold partial flush should print")
	}
	if got := string(m.tokBuf); got != tail {
		t.Errorf("buffer tail = %d bytes, want the %d y's after the last space", len(got), len(tail))
	}
	printed := fmt.Sprint(cmd()) // tea.Printf cmd yields a string-kind message
	if !strings.Contains(printed, "xxxx") || strings.Contains(printed, "yyyy") {
		t.Error("flushed chunk should contain the head only, not the buffered tail")
	}

	// Unbroken word: no boundary to cut at — the partial flush must wait.
	m.tokBuf = []byte(strings.Repeat("z", tokFlushThreshold+100))
	if cmd := m.flushTokens(false); cmd != nil {
		t.Error("no word boundary → partial flush must not split mid-word")
	}
	if len(m.tokBuf) != tokFlushThreshold+100 {
		t.Errorf("buffer changed on boundary-less flush: %d bytes", len(m.tokBuf))
	}

	// Final flush drains everything, even below the threshold.
	m.tokBuf = []byte("remaining text")
	if cmd := m.flushTokens(true); cmd == nil {
		t.Error("final flush must print the remainder")
	}
	if len(m.tokBuf) != 0 {
		t.Errorf("final flush must drain the buffer, left %q", m.tokBuf)
	}
}

func TestWorkspaceMismatchWarning(t *testing.T) {
	cases := []struct {
		threadWS  string
		currentWS string
		want      string
	}{
		{"", "/work/a", ""}, // unknown thread workspace — nothing to compare
		{"/work/a", "/work/a", ""},
		{"/work/a", "/work/b", "thread workspace: /work/a — tools will run in /work/b"},
	}
	for _, c := range cases {
		if got := workspaceMismatchWarning(c.threadWS, c.currentWS); got != c.want {
			t.Errorf("workspaceMismatchWarning(%q, %q) = %q, want %q", c.threadWS, c.currentWS, got, c.want)
		}
	}
}

func TestSessionPickerGroupsByWorkspace(t *testing.T) {
	store, err := memory.OpenWithMigrations(filepath.Join(t.TempDir(), "test.db"), "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ws := "/work/current"
	cur, err := store.CreateThreadWithWorkspace(nil, nil, ws)
	if err != nil {
		t.Fatalf("create current thread: %v", err)
	}
	other, err := store.CreateThreadWithWorkspace(nil, nil, "/work/other")
	if err != nil {
		t.Fatalf("create other thread: %v", err)
	}
	legacy, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create legacy thread: %v", err)
	}

	be := backend.NewLocalBackend(&config.Config{Workspace: ws}, store, nil, nil)
	m := newModel(&session{backend: be, local: &localServices{store: store}, workspace: ws, threadID: cur.ID})
	if cmd := m.openSessionPicker(); cmd != nil {
		t.Fatalf("openSessionPicker returned a command: %v", cmd)
	}
	if m.pickMode != "session" {
		t.Fatalf("pick mode: got %q, want session", m.pickMode)
	}
	if len(m.pickItems) != 3 {
		t.Fatalf("pick items: got %d, want 3", len(m.pickItems))
	}
	// Current-workspace thread first, then strays tagged with their origin.
	if m.pickItems[0].id != cur.ID {
		t.Errorf("first item = %q, want current-workspace thread %q", m.pickItems[0].id, cur.ID)
	}
	if m.pickItems[0].tag != "" {
		t.Errorf("first item tag = %q, want none", m.pickItems[0].tag)
	}
	if m.pickItems[1].id != other.ID || m.pickItems[1].tag != "/work/other" {
		t.Errorf("second item = %+v, want other thread tagged %q", m.pickItems[1], "/work/other")
	}
	if m.pickItems[2].id != legacy.ID || m.pickItems[2].tag != "?" {
		t.Errorf("third item = %+v, want legacy thread tagged %q", m.pickItems[2], "?")
	}
	if m.pickSel != 0 {
		t.Errorf("cursor = %d, want 0 (the current thread)", m.pickSel)
	}
}
