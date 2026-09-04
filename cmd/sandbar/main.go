package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
	"github.com/sashabaranov/go-openai"
	"golang.org/x/term"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/backend"
	"github.com/aetherbird/sandbar/internal/cliui"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/persona"
	"github.com/aetherbird/sandbar/internal/tools"
)

// spinFrames is the braille spinner shown in the status bar while streaming.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

// pendingToolLine holds a composed tool-call header until its result arrives.
type pendingToolLine struct {
	head string
	name string
}

// mergedToolLine joins a held tool-call header with its result preview on a
// single line. A successful command with no preview renders as the bare
// header; failures color the result half red so they stay visible at one line.
// The preview budget derives from the live terminal width so the assembled
// line can't reach the edge and wrap (same arithmetic as the header preview).
func mergedToolLine(head, preview string, width int) string {
	if head == "" {
		return ""
	}
	if preview == "" {
		return head
	}
	budget := width - lipgloss.Width(head) - len("→ ") - 2
	if budget < 4 {
		budget = 4
	}
	p := clip(preview, budget)
	if isFailurePreview(p) {
		return head + " " + sty(cErr).Render("→ "+p)
	}
	return head + " " + sty(cMuted).Render("→ "+p)
}

// isFailurePreview reports whether a result preview text describes a failure:
// a non-zero exit or an error result.
func isFailurePreview(preview string) bool {
	return strings.HasPrefix(preview, "exit ") || strings.HasPrefix(preview, "error:") || strings.HasPrefix(preview, "✗")
}

// pendingInOrder returns held tool lines in emission order. Provider
// tool-call IDs are opaque strings, so lexicographic sorting would print an
// interrupted batch in an arbitrary order; the arrival slice preserves what
// actually ran, and IDs already matched to a result are skipped.
func pendingInOrder(order []string, pending map[string]pendingToolLine) []pendingToolLine {
	lines := make([]pendingToolLine, 0, len(pending))
	for _, id := range order {
		if line, ok := pending[id]; ok {
			lines = append(lines, line)
		}
	}
	return lines
}

// ── Stream channel messages ───────────────────────────────────────────────────
//
// A goroutine consuming Backend.SendMessage writes streamItems into a buffered
// channel; waitForStreamItem is a tea.Cmd that blocks on one read and returns
// it as a tea.Msg, and Update re-issues it after each item. This is the
// canonical BubbleTea real-time streaming pattern — no p.Send/Printf from
// goroutines.

type streamItem struct {
	kind    string // "token" | "label" | "tool" | "result" | "ctx" | "done" | "err"
	content string
	footer  string
	ctxUsed int    // live context size (prompt tokens), for kind "ctx"
	cost    string // cumulative session cost segment, for kind "ctx" ("" = hidden)
	err     error
	gen     int // stream generation that produced this item

	// Compact subagent HUD state. These fields are consumed only when kind is
	// "subagent" and never printed into the transcript as token-by-token noise.
	taskID     string
	taskGoal   string
	taskStatus string

	// toolName names the tool for kind "tool" lines, so Update can drop the
	// separating blank line between consecutive calls of the same tool.
	toolName string

	// repaintAfter forces a clean screen repaint after this line prints
	// (used by compression summary lines to recover inline-renderer drift).
	repaintAfter bool

	// todoSet reports that this item carries the thread's latest todo list in
	// todoRows (a successful todo tool result). Update adopts it into the
	// sticky task panel instead of the list being printed into scrollback.
	todoSet  bool
	todoRows []todoRow

	approvalReq   *tools.ApprovalRequest
	approvalReply chan<- tools.ApprovalDecision

	// Compression event state sync (kind "compression"): the raw event and its
	// type, so Update can maintain compressing/lastCompression without parsing
	// rendered transcript text.
	compType  string
	compEvent *llm.CompressionEvent
}

// waitForStreamItem blocks on one read from ch and tags the delivered item
// with gen — the generation of the stream that owns ch. Every generation gets
// its own channel, so tagging at issue time is exact; Update drops items whose
// generation no longer matches the model's current stream generation.
func waitForStreamItem(ch <-chan streamItem, gen int) tea.Cmd {
	return func() tea.Msg {
		it := <-ch // blocks until next item; safe, runs in BubbleTea's goroutine pool
		it.gen = gen
		return it
	}
}

// bgTaskDoneMsg reports a background sub-agent task reaching a terminal
// status (or a polling failure).
type bgTaskDoneMsg struct {
	taskID string
	status string
	result string
	err    error
}

// pollBackgroundTask polls a detached sub-agent task until it reaches a
// terminal status. Bounded to an hour so a wedged task cannot poll forever.
func pollBackgroundTask(be backend.Backend, taskID string) tea.Cmd {
	return func() tea.Msg {
		watcher, ok := be.(backend.SubagentTaskWatcher)
		if !ok {
			return bgTaskDoneMsg{taskID: taskID, err: fmt.Errorf("backend cannot report subagent status")}
		}
		deadline := time.Now().Add(time.Hour)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			status, result, err := watcher.SubagentTaskStatus(taskID)
			if err != nil {
				return bgTaskDoneMsg{taskID: taskID, err: err}
			}
			if status != "running" {
				return bgTaskDoneMsg{taskID: taskID, status: status, result: result}
			}
		}
		return bgTaskDoneMsg{taskID: taskID, err: fmt.Errorf("timed out waiting for background task")}
	}
}

// ── Other messages ────────────────────────────────────────────────────────────

type tickMsg time.Time

// forceQuitMsg fires a few seconds after Ctrl+D interrupted a stream that
// never reported its terminal item; it force-quits instead of hanging.
type forceQuitMsg struct{}

type statusMsg struct{ used, max int }
type compressDoneMsg struct {
	res agent.CompressionResult
	err error
}

// shellDoneMsg delivers the output of an async "!" shell escape.
type shellDoneMsg struct {
	cmd    string
	output string
	err    error
}

type searchDoneMsg struct {
	query   string
	results []memory.SearchResult
	err     error
}

// ── Session ───────────────────────────────────────────────────────────────────

type session struct {
	cfg        *config.Config
	clientCfg  *config.ClientConfig
	backend    backend.Backend
	local      *localServices
	modelAlias string
	themeName  string
	colorMode  string
	// darkBackground is detected once at startup and reused for every live
	// theme preview; arrowing through the picker must not probe the terminal.
	darkBackground bool
	styles         *styleSet
	threadID       string
	workspace      string
	// effort is the per-turn reasoning effort ("" = provider default),
	// settable at launch with --effort and per session with /effort.
	effort string
	// tropical is the Tropical-mode adaptation of other harnesses' "ultracode"
	// tier: effort is forced to high and the system prompt directs heavy
	// parallel subagent use. Toggled with /tropical or the /effort menu.
	tropical bool
	// planMode makes the next turn read-only with a planning directive,
	// settable at launch with --plan and toggled with /plan.
	planMode bool
}

// localServices contains the concrete services needed only by commands that
// are not part of backend.Backend yet. A nil value leaves those commands
// unavailable; every command that uses one of these services must be
// capability-gated.
type localServices struct {
	store *memory.Store
	ag    *agent.Agent
}

// ── Model ─────────────────────────────────────────────────────────────────────

type appModel struct {
	sess      *session
	styles    *styleSet // immutable; swapped atomically as one unit by /theme
	ta        textarea.Model
	width     int
	height    int           // terminal rows; bounds printLine chunk sizes
	turnStart time.Time     // when the current/last request began streaming
	turnDur   time.Duration // frozen duration of the last completed request
	ctxUsed   int
	ctxMax    int
	costSeg   string // cumulative session cost segment ("" = pricing inactive)
	// Compression UI state. compressing is the in-flight indicator while a
	// compression runs; lastCompression is a session-persistent trace of the
	// most recent outcome — deliberately never cleared, so the status bar
	// keeps showing the last before→after delta.
	compressing     bool
	lastCompression compressionStatus
	streaming       bool
	sized           bool // first WindowSizeMsg seen (skip the launch-time auto-clear)
	spinIdx         int  // animation frame for the streaming spinner
	cancel          context.CancelFunc
	streamCh        <-chan streamItem
	streamGen       int // generation of the stream whose items are currently accepted

	// "!" shell escape in flight: a cancelable context so Ctrl+C stops the
	// running command instead of quitting the app mid-escape. A second Ctrl+C
	// (after the flag clears) quits normally.
	escapeRunning bool
	escapeCancel  context.CancelFunc

	// Turn-draining state: a canceled stream keeps reporting until its terminal
	// item arrives. drainingGen is the generation being drained (0 = none),
	// pendingSends is a FIFO of messages stashed mid-stream to launch right after
	// the drain, and quitAfterStream defers a Ctrl+D quit to the same point.
	drainingGen     int
	pendingSends    []string
	quitAfterStream bool
	history         []string
	histIdx         int
	// draft preserves the current in-progress input when the user starts
	// cycling through history with up/down arrows. Pressing down past the
	// last history entry restores the draft.
	draft string

	// active selection menu (/model, /sessions): pickMode names the menu,
	// pickItems holds the choices, and pickSel is the highlighted row. Rendered
	// as an in-place arrow-navigable popup in View() while pickMode is set.
	pickMode     string
	pickItems    []pickItem
	pickSel      int
	pickProvider string // provider selected in the /model submenu (for title + Esc-back)
	// pickTruncated counts sessions dropped from the full list by the picker's
	// per-group caps, shown as a "… N older hidden" footer row.
	pickTruncated int
	// pickOriginalTheme is restored if a live-preview /theme picker is cancelled.
	pickOriginalTheme string
	// lastPlanText holds the assistant text of the last plan-mode turn so the
	// plan-decision menu's "edit" choice can load it into the input area.
	lastPlanText string

	// slash-command autocomplete: highlighted row in the suggestion popup that
	// appears while the input is a partial "/command" (derived from the input,
	// so no explicit on/off flag is needed). slashDismissed mirrors
	// pathDismissed: set by Esc and cleared on the next text change so the
	// popup can be dismissed without wiping the input.
	slashSel       int
	slashDismissed bool

	// path autocomplete: highlighted row in the filesystem completion popup.
	// Fires when the current word (after the last space) contains a "/".
	// pathDismissed is set by Esc and cleared on the next text change so the
	// popup can be dismissed without modifying the input.
	pathSel       int
	pathDismissed bool

	// @-mention fuzzy picker: highlighted row in the cwd-file popup that
	// appears while the cursor is in a plain "@query" word. mentionDismissed
	// mirrors pathDismissed: set by Esc, cleared on the next text change.
	mentionSel       int
	mentionDismissed bool
	// mentionIdx caches the cwd file index behind the @-mention picker;
	// mentionBuilt guards the one-time walk.
	mentionIdx   []string
	mentionBuilt bool

	// Editor power state (see editor_power.go): a readline-style kill ring,
	// yank-pop bookkeeping, and the undo stack of pre-edit snapshots, all
	// layered over the bubbles textarea.
	killRing  []string
	killIdx   int
	yankState yankSnap
	undoStack []cursorSnap

	// reverse-i-search (Ctrl+R): typing filters history incrementally.
	// searchMode is "" when inactive, "reverse" when active.
	searchMode  string
	searchQuery string
	searchMatch int // index into history of the current match (-1 = none)

	// token accumulator — avoids per-token tea.Printf which forces line
	// breaks. Flushed progressively on the spinner tick (flushTokens) and
	// fully on tool events / turn end.
	tokBuf []byte

	// responseBuf accumulates the full assistant text for the current turn.
	// appModel is copied by value by Bubble Tea, so this must remain a
	// copy-safe slice header rather than strings.Builder (which embeds a
	// self-pointer and must never be copied after its first write).
	responseBuf []byte

	// lastResponseRaw is the raw (unrendered) text of the last completed
	// assistant response, so /noformat can re-print it verbatim. Set on
	// transcript replay of the last assistant message, kept across turns.
	lastResponseRaw string

	// hadToolTurn is true if the current turn contained tool calls; if so,
	// text was printed inline via flushTokens and should not be re-rendered.
	hadToolTurn bool

	// lastToolName is the tool named by the most recent tool line printed as
	// part of an unbroken run. Consecutive lines for the same tool pack
	// together without a separating blank line; anything else that prints
	// (assistant text, diffs, labels, a new turn) resets it.
	lastToolName string

	// liveLabel/liveRendered back the in-frame streaming block: the assistant
	// label and the latest glamour rendering of responseBuf. The block is part
	// of the View frame, so the renderer owns all cursor movement; it is
	// committed to the transcript with one printLine when the turn ends (or
	// the first tool call arrives). The previous design replaced the printed
	// rendering in place with cursor/erase escapes embedded in tea.Printf
	// bodies, which bubbletea v2's cursed renderer executes at its own
	// scroll/insert anchors — desyncing the screen (blank blocks, text
	// stranded below the input box, streamed tokens erasing as they printed).
	liveLabel    bool
	liveRendered string
	liveDirty    bool
	// bgTasks tracks background sub-agent delegations (delegate_task with
	// background: true) by task ID → goal. They survive the turn; a poller
	// delivers each result to the model when the task reaches a terminal
	// status.
	bgTasks map[string]string
	// thinking is true while the model is in a reasoning phase. It drives the
	// animated in-frame indicator (thinkingView) — transient, never printed
	// to the transcript; any non-thinking stream event ends the phase.
	thinking bool

	// Active delegated tasks are rendered as a bounded live HUD above the
	// editor. Terminal tasks are removed and summarized by the surrounding
	// delegate_task result, keeping scrollback compact.
	subagents     map[string]subagentHUDItem
	subagentOrder []string
	approvals     []pendingApproval

	// todos backs the sticky task panel docked above the input: the latest
	// parsed todo list for the thread, updated on every todo tool result
	// instead of re-printing the list into scrollback. Nil when the thread
	// has no tasks (or none known yet).
	todos []todoRow
}

// pickItem is one numbered choice in a /model or /sessions menu.
type pickItem struct {
	id    string // value to act on: a model alias or a thread id
	label string // human label shown in the list
	tag   string // optional muted suffix (e.g. a thread's origin workspace)
}

func newModel(sess *session) appModel {
	styles := sess.styles
	if styles == nil {
		styles = currentStyles()
	}
	ta := textarea.New()
	ta.Placeholder = "Message sandbar..."
	ta.Focus()
	// Show the "> " prompt only on the first visual row; wrapped/continuation
	// rows get blank padding of the same width so the text stays aligned.
	ta.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "> "
		}
		return "  "
	})
	ta.CharLimit = 150000
	ta.ShowLineNumbers = false
	// Subtract 2 chars for the "> " prompt so text wraps within the terminal.
	initWidth := termWidth() - 2
	if initWidth < 1 {
		initWidth = 1
	}
	ta.SetWidth(initWidth)
	ta.SetHeight(inputMaxHeight)
	ta.MaxHeight = inputMaxHeight
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter"), key.WithDisabled())
	styles.ApplyTextarea(&ta)

	var hist []string
	if b, err := os.ReadFile(historyPath()); err == nil {
		hist = parseHistory(b)
	}

	return appModel{
		sess:      sess,
		styles:    styles,
		ta:        ta,
		width:     termWidth(),
		history:   hist,
		histIdx:   len(hist),
		ctxMax:    contextLengthFor(sess.cfg, sess.modelAlias),
		subagents: make(map[string]subagentHUDItem),
	}
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return textarea.Blink() },
		tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }),
	)
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		prevWidth := m.width
		m.width = msg.Width
		m.height = msg.Height
		// Subtract prompt width (2 chars for "> ") so wrapped text doesn't
		// overflow the terminal's right edge.
		taWidth := msg.Width - 2
		if taWidth < 1 {
			taWidth = 1
		}
		m.ta.SetWidth(taWidth)
		m.syncInputHeight() // re-wrap may change the row count
		// A resize no longer needs a blanket clear — the bottom block just
		// repaints. A width DECREASE still forces a clean repaint: already-
		// printed lines physically re-wrap into more rows than the inline
		// renderer counted, desyncing its line tracking. Also skip the FIRST
		// size event (delivered at launch) — clearing there would wipe the
		// startup banner/hint printed before the program started.
		if m.sized && msg.Width < prevWidth {
			cmds = append(cmds, tea.ClearScreen)
		}
		m.sized = true

	case tickMsg:
		// Tick fast while streaming OR while background sub-agents run, so
		// spinners stay animated — a frozen indicator reads as dead work when
		// delegated agents are actually running. Slow only when truly idle.
		interval := time.Second
		if m.streaming || len(m.bgTasks) > 0 {
			m.spinIdx++
			interval = 120 * time.Millisecond
			if m.streaming && len(m.responseBuf) > 0 {
				if m.hadToolTurn {
					// Tool turn: progressively flush the buffered prose into
					// the transcript (same cadence as the live block below).
					// Waiting for tool events instead dumps everything as one
					// giant unstyled block at the next event.
					if cmd := m.flushTokens(false); cmd != nil {
						cmds = append(cmds, cmd)
					}
				} else {
					// Progressive markdown: re-render the buffer into the
					// in-frame live block. No tea.Printf — the renderer repaints
					// the frame itself, so nothing can drift.
					m.refreshLiveRender()
				}
			}
		}
		cmds = append(cmds, tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) }))

	case statusMsg:
		m.ctxUsed = msg.used
		m.ctxMax = msg.max

	case forceQuitMsg:
		// The interrupted stream never reported its terminal item; force the
		// deferred quit instead of hanging on the way out.
		if m.quitAfterStream {
			return m, tea.Quit
		}

	case compressDoneMsg:
		m.compressing = false
		if msg.err != nil {
			cmds = append(cmds, m.printLine("\n"+sty(cErr).Render("  ⚠ compress failed: "+msg.err.Error())))
		} else {
			m.lastCompression = compressionStatusFromResult(msg.res)
			line, color := renderCompressionLine(compressionEventFromResult(msg.res))
			// Print then clean-repaint so drift cannot hide the input block.
			cmds = append(cmds, tea.Sequence(m.printLine("\n"+sty(color).Render(line)), tea.ClearScreen))
		}
		cmds = append(cmds, m.contextCmd())

	case searchDoneMsg:
		cmds = append(cmds, m.printLine(renderSearchResults(msg)))

	case bgTaskDoneMsg:
		// A background sub-agent finished: hand its result to the model —
		// as a steering message when mid-turn (delivered at the next tool
		// boundary), or as an automatic follow-up turn when idle.
		goal := m.bgTasks[msg.taskID]
		delete(m.bgTasks, msg.taskID)
		m.updateSubagentHUD(streamItem{taskID: msg.taskID, taskStatus: firstNonEmpty(msg.status, "failed")})
		if msg.err != nil {
			cmds = append(cmds, m.printLine("\n"+sty(cWarn).Render("  ⚠ background task "+shortID(msg.taskID)+": "+msg.err.Error())))
			return m, tea.Batch(cmds...)
		}
		note := fmt.Sprintf("[Background sub-agent task %s %s]\nGoal: %s\n\nResult:\n%s",
			msg.taskID, msg.status, firstNonEmpty(goal, "(unknown goal)"), firstNonEmpty(msg.result, "(no output)"))
		switch {
		case m.streaming && m.sess.threadID != "":
			if q, ok := m.sess.backend.(backend.MessageQueuer); ok {
				enqCtx, enqCancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := q.EnqueueUserMessage(enqCtx, m.sess.threadID, note)
				enqCancel()
				if err == nil {
					cmds = append(cmds, m.printLine("\n"+sty(cGreen).Render("  ⇢ background task "+shortID(msg.taskID)+" "+msg.status+" — result queued for the model")))
					return m, tea.Batch(cmds...)
				}
			}
			// Queue endpoint unavailable: stash for after the turn, like any
			// other mid-turn send that cannot steer.
			m.pendingSends = append(m.pendingSends, note)
			cmds = append(cmds, m.printLine("\n"+sty(cGreen).Render("  ⇢ background task "+shortID(msg.taskID)+" "+msg.status+" — result will be sent after this turn")))
		default:
			cmds = append(cmds, m.printLine("\n"+sty(cAccent).Render("  ◈ background task "+shortID(msg.taskID)+" "+msg.status+" — delivering result to the model")))
			cmds = m.startStream(note, cmds)
		}
		return m, tea.Batch(cmds...)

	case shellDoneMsg:
		m.escapeRunning, m.escapeCancel = false, nil
		var b strings.Builder
		b.WriteString("\n" + sty(cWarn).Render("  $ "+msg.cmd) + "\n")
		if msg.output != "" {
			b.WriteString(strings.TrimRight(msg.output, "\n"))
		}
		if msg.err != nil {
			b.WriteString("\n" + sty(cErr).Render("  ✗ "+msg.err.Error()))
		}
		b.WriteString("\n")
		cmds = append(cmds, m.printLine(b.String()))

	// ── stream events coming back from the goroutine via channel ──────────────
	case streamItem:
		// Drop items from a canceled generation: they must neither print into
		// the stream that replaces them nor clear its state. Only the terminal
		// item of the generation being drained still matters — it completes the
		// drain and fires any deferred send/quit.
		if msg.gen != m.streamGen {
			// The interrupted generation is still the authoritative source of
			// the thread identity. Its other items are dropped as stale, but
			// adopting this one keeps the session attached to the thread — an
			// interrupted first turn must not leave "" behind, or the pending
			// follow-up message would silently start a brand-new conversation.
			if msg.kind == "threadID" && msg.content != "" {
				m.sess.threadID = msg.content
			}
			if msg.kind == "approval" {
				denyApprovalItem(msg, "stream is no longer active")
			}
			if (msg.kind == "done" || msg.kind == "err") && msg.gen == m.drainingGen {
				m.drainingGen = 0
				m.streaming = false
				m.turnDur = time.Since(m.turnStart)
				m.cancel = nil
				if msg.kind == "err" {
					cmds = append(cmds, tea.Printf("\n  %s", sty(cWarn).Render("[stopped]")))
				}
				if m.quitAfterStream {
					return m, tea.Quit
				}
				if len(m.pendingSends) > 0 {
					v := m.pendingSends[0]
					m.pendingSends = m.pendingSends[1:]
					cmds = m.startStream(v, cmds)
				}
			} else {
				// Straggler: drop it, but keep draining the old channel.
				cmds = append(cmds, waitForStreamItem(m.streamCh, m.drainingGen))
			}
			return m, tea.Batch(cmds...)
		}
		// Any non-thinking event ends a reasoning phase; the "thinking" case
		// below re-arms it.
		if msg.kind != "thinking" {
			m.thinking = false
		}
		switch msg.kind {
		case "thinking":
			m.thinking = true
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "token":
			if msg.content != "" {
				m.tokBuf = append(m.tokBuf, msg.content...)
				m.responseBuf = append(m.responseBuf, msg.content...)
				m.lastResponseRaw = string(m.responseBuf)
				m.liveDirty = true // next spinner tick re-renders the live block
			}
			// keep accumulating — do NOT tea.Printf per token
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "label":
			// The assistant label heads the live block for a text-only turn;
			// after tool calls have started printing inline, it joins them.
			if m.hadToolTurn {
				if msg.content != "" {
					m.lastToolName = ""
					cmds = append(cmds, m.printLine("\n"+msg.content))
				}
			} else {
				m.liveLabel = true
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "activity":
			// Presentation-only rows do not turn a plain response into a tool
			// response. In particular, coalesced thinking/retry notices must
			// leave progressive Markdown enabled.
			if msg.content != "" {
				m.lastToolName = ""
				cmds = append(cmds, m.printLine("\n"+msg.content))
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "tool", "result", "diff":
			// Prints within one stream item must never interleave: Bubble Tea
			// batches commands concurrently, and a footer or tool line landing
			// between the chunks of a multi-chunk commit splices the frame into
			// the middle of the text. Everything this item prints runs as one
			// ordered Sequence.
			var turnPrints []tea.Cmd
			if !m.hadToolTurn && (m.liveLabel || m.liveRendered != "") {
				// Text streamed before the first tool call: commit the live
				// block to the transcript, and drain tokBuf so the flush
				// below does not print the same text a second time.
				if cmd := m.printResponse(); cmd != nil {
					turnPrints = append(turnPrints, cmd)
				}
				m.tokBuf = m.tokBuf[:0]
			}
			m.hadToolTurn = true
			if msg.todoSet {
				// Adopt the latest todo list into the sticky panel. todoRows is
				// nil for "(no items)", which correctly clears the panel.
				m.todos = msg.todoRows
			}
			flushed := m.flushTokens(true)
			if flushed != nil {
				turnPrints = append(turnPrints, flushed)
				// Assistant text between tool calls breaks the run: the next
				// tool line opens a new block with a blank line above.
				m.lastToolName = ""
			}
			if msg.content != "" {
				// A tool line opens a new block (one blank line above) — except
				// consecutive calls of the SAME tool, which pack together with
				// no separating blank. Everything goes through printLine, which
				// hard-wraps to the terminal width so a printed line can never
				// desync the inline renderer.
				switch msg.kind {
				case "diff":
					m.lastToolName = ""
					turnPrints = append(turnPrints, m.printLine(msg.content))
				case "result":
					m.lastToolName = ""
					turnPrints = append(turnPrints, m.printLine("  "+msg.content))
				default:
					prefix := "\n"
					if msg.toolName != "" && msg.toolName == m.lastToolName {
						prefix = ""
					}
					turnPrints = append(turnPrints, m.printLine(prefix+msg.content))
					m.lastToolName = msg.toolName
				}
			}
			if len(turnPrints) > 0 {
				if msg.repaintAfter {
					// Compression summary lines: force a clean repaint after
					// printing so any inline-renderer drift cannot leave the
					// bottom block (input + status bar) drawn off-position.
					turnPrints = append(turnPrints, tea.ClearScreen)
				}
				cmds = append(cmds, tea.Sequence(turnPrints...))
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "ctx":
			if msg.ctxUsed > 0 {
				m.ctxUsed = msg.ctxUsed
			}
			if msg.cost != "" {
				m.costSeg = msg.cost
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "compression":
			// Compression lifecycle state for the status bar, driven by the raw
			// event (not the rendered line): start sets the in-flight flag,
			// terminal events clear it and record the session-persistent trace.
			switch msg.compType {
			case "compression_start":
				m.compressing = true
			case "compression_end", "compression_error":
				m.compressing = false
				if msg.compEvent != nil {
					m.lastCompression = compressionStatusFromEvent(msg.compEvent)
				}
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "subagent":
			m.updateSubagentHUD(msg)
			// A background delegation outlives the turn: register it and poll
			// the backend so its result is delivered to the model when done.
			if msg.taskStatus == "background" && msg.taskID != "" {
				if m.bgTasks == nil {
					m.bgTasks = make(map[string]string)
				}
				if _, seen := m.bgTasks[msg.taskID]; !seen {
					m.bgTasks[msg.taskID] = msg.taskGoal
					cmds = append(cmds, pollBackgroundTask(m.sess.backend, msg.taskID))
				}
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "approval":
			if msg.approvalReq != nil && msg.approvalReply != nil {
				m.approvals = append(m.approvals, pendingApproval{request: *msg.approvalReq, reply: msg.approvalReply})
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "threadID":
			// New/resolved thread ID from the agent — set it here in the UI
			// goroutine, NOT from the streaming goroutine (avoids a data race
			// on m.sess.threadID).
			if msg.content != "" {
				m.sess.threadID = msg.content
			}
			cmds = append(cmds, waitForStreamItem(m.streamCh, m.streamGen))

		case "done":
			m.streaming = false
			m.compressing = false
			m.turnDur = time.Since(m.turnStart)
			m.cancel = nil
			m.lastToolName = ""
			m.clearSubagentHUD()
			m.clearApprovals("turn completed")
			// Turn end prints as ONE atomic tea.printf: the commit plus footer
			// in a single body. A Sequence of separate printfs leaves chunk
			// boundaries that frame repaints (typing, spinner ticks) splice
			// into — the input box lands mid-answer. One insertAbove cannot
			// be interleaved with anything.
			{
				var body string
				if m.hadToolTurn {
					// Tool calls occurred — text was printed inline; only the
					// buffered tail remains.
					if tail := strings.TrimRight(string(m.tokBuf), "\n"); tail != "" {
						if s := m.marginProse(tail); s != "" {
							body = s + "\n"
						}
					}
					m.tokBuf = m.tokBuf[:0]
				} else {
					// Pure text response — render through glamour markdown.
					body = m.commitResponseBody()
				}
				body += "\n" + msg.footer
				cmds = append(cmds, tea.Printf("%s", wrapForPrint(body, m.printWidth())))
			}
			cmds = append(cmds, m.contextCmd())
			// Messages the user sent mid-turn that could not steer (empty
			// thread ID, queue endpoint unavailable) were stashed in
			// pendingSends. They must not strand here: the turn is over, so
			// the next one fires now — the user's words are never swallowed.
			if len(m.pendingSends) > 0 {
				next := m.pendingSends[0]
				m.pendingSends = m.pendingSends[1:]
				cmds = m.startStream(next, cmds)
			}
			if m.sess.planMode {
				// A plan turn just completed: the server marked the thread
				// pending_approval. Plan mode disengages here — what happens
				// next is driven by the user's decision, not the toggle.
				m.sess.planMode = false
				m.lastPlanText = string(m.responseBuf)
				m.openPlanDecisionPicker()
			}

		case "err":
			m.streaming = false
			m.compressing = false
			m.turnDur = time.Since(m.turnStart)
			m.cancel = nil
			m.lastToolName = ""
			m.clearSubagentHUD()
			m.clearApprovals("turn ended")
			// A provider can fail after yielding useful text. Preserve that partial
			// response before the terminal error instead of silently discarding it.
			// Sequence keeps the response ahead of the error in scrollback.
			var terminalOutput []tea.Cmd
			if m.hadToolTurn {
				if cmd := m.flushTokens(true); cmd != nil {
					terminalOutput = append(terminalOutput, cmd)
				}
			} else if cmd := m.printResponse(); cmd != nil {
				terminalOutput = append(terminalOutput, cmd)
			}
			if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
				terminalOutput = append(terminalOutput, m.printLine("\n  "+sty(cErr).Render("✗ "+msg.err.Error())))
			} else {
				terminalOutput = append(terminalOutput, m.printLine("\n  "+sty(cWarn).Render("[stopped]")))
			}
			cmds = append(cmds, tea.Sequence(terminalOutput...))
		}

	// ── paste ───────────────────────────────────────────────────────────────
	case tea.PasteMsg:
		// v2 delivers bracketed-paste content as PasteMsg (v1 folded it into
		// KeyRunes); forward it to the textarea so pasted text is never
		// silently dropped.
		if m.searchMode != "" {
			m.searchQuery += msg.Content
			m.doReverseSearch()
			return m, nil
		}
		// Oversized pastes collapse to a bounded head + marker (the full
		// text lands in the kill ring — see editor_power.go).
		if m.collapsePaste(msg.Content) {
			m.mentionSel = 0
			m.mentionDismissed = false
			m.draft = ""
			m.syncInputHeight()
			return m, nil
		}
		pre := cursorSnap{value: m.ta.Value(), row: m.ta.Line(), col: m.ta.Column()}
		var taCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)
		cmds = append(cmds, taCmd)
		if m.ta.Value() != pre.value {
			m.pushUndo(pre) // a normal paste is one undoable step
		}
		m.mentionSel = 0
		m.mentionDismissed = false
		m.draft = ""
		m.syncInputHeight()
		return m, nil

	// ── external editor round-trip result ────────────────────────────────────
	case editorDoneMsg:
		return m, m.finishExternalEditor(msg)

	// ── keyboard ─────────────────────────────────────────────────────────────
	case tea.KeyPressMsg:
		if len(m.approvals) > 0 {
			switch msg.String() {
			case "y", "Y":
				m.resolveCurrentApproval(tools.PolicyAllow, "approved in CLI")
				return m, nil
			case "n", "N":
				m.resolveCurrentApproval(tools.PolicyDeny, "denied in CLI")
				return m, nil
			case "esc":
				m.resolveCurrentApproval(tools.PolicyDeny, "denied in CLI")
				return m, nil
			case "ctrl+c":
				m.resolveCurrentApproval(tools.PolicyDeny, "turn cancelled during approval")
				if m.cancel != nil {
					m.cancel()
				}
				return m, nil
			case "ctrl+d":
				saveHistory(m.history)
				m.resolveCurrentApproval(tools.PolicyDeny, "turn cancelled during approval")
				if m.cancel != nil {
					m.cancel()
				}
				m.quitAfterStream = true
				if m.drainingGen == 0 {
					m.drainingGen = m.streamGen
					m.streamGen++
				}
				return m, tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return forceQuitMsg{} })
			}
			return m, nil
		}
		// ── reverse-i-search mode ─────────────────────────────────────────
		// Search keys are intercepted BEFORE the main switch so Enter can
		// submit the matched entry through the normal send path and always
		// clear search mode. Quit/redraw keys fall through so they keep
		// working while searching.
		if m.searchMode == "reverse" {
			switch msg.String() {
			case "esc":
				m.searchMode = "" // exit search mode, keep the matched text
				return m, nil
			case "enter":
				// Exit search mode and fall through so the main Enter handler
				// submits the textarea's value. Completion popups are suppressed
				// for this keypress — a recalled entry that happens to contain a
				// path or an @-word must send, not complete.
				m.searchMode = ""
				m.slashDismissed = true
				m.mentionDismissed = true
				m.pathDismissed = true
			case "backspace":
				if r := []rune(m.searchQuery); len(r) > 0 {
					m.searchQuery = string(r[:len(r)-1])
				}
				m.doReverseSearch()
				return m, nil
			case "ctrl+r":
				m.cycleReverseSearch()
				return m, nil
			case "ctrl+c", "ctrl+d", "ctrl+l":
				// handled by the main switch below
			default:
				// Append printable input (v1 tea.KeyRunes equivalent); swallow
				// everything else in search mode.
				if msg.Text != "" {
					m.searchQuery += msg.Text
					m.doReverseSearch()
				}
				return m, nil
			}
		}
		switch msg.String() {

		case "ctrl+c":
			if m.escapeRunning && m.escapeCancel != nil {
				// First Ctrl+C during a "!" shell escape cancels the command;
				// the escape's own completion (or the cancel) clears the flag,
				// so a second press quits normally.
				m.escapeCancel()
				return m, nil
			}
			if m.streaming && m.cancel != nil {
				if m.drainingGen != 0 {
					// Already draining: a second Ctrl+C forces the quit, with a
					// 2s tick fallback if the stream never reports its terminal item.
					m.quitAfterStream = true
					return m, tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return forceQuitMsg{} })
				}
				m.cancel()
				// goroutine will send "err"/Canceled which clears streaming flag
				return m, nil
			}
			saveHistory(m.history)
			return m, tea.Quit

		case "ctrl+d":
			saveHistory(m.history)
			if m.streaming && m.cancel != nil {
				// Don't quit mid-write: cancel the turn and defer the quit
				// until the old stream's terminal item arrives, with a tick
				// fallback in case it never reports back.
				m.cancel()
				m.quitAfterStream = true
				if m.drainingGen == 0 {
					m.drainingGen = m.streamGen
					m.streamGen++ // items from the canceled stream are now stale
				}
				return m, tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return forceQuitMsg{} })
			}
			return m, tea.Quit

		case "ctrl+l":
			// Force a clean repaint to recover from inline-renderer drift
			// (ghost dividers, leaked prompt, post-resize artifacts). Matches
			// the bash/zsh/vim Ctrl+L convention; also available as /redraw.
			return m, tea.ClearScreen

		case "ctrl+r":
			if m.streaming || m.pickMode != "" {
				return m, nil
			}
			m.searchMode = "reverse"
			m.searchQuery = ""
			m.searchMatch = -1
			return m, nil

		case "esc":
			if m.pickMode == "model" && m.pickProvider != "" {
				return m, m.openProviderPicker() // step back to the provider list
			}
			if m.pickMode != "" {
				return m, m.cancelPick()
			}
			if len(m.slashSuggestions()) > 0 || m.slashDismissed {
				m.slashDismissed = true // suppress until next text change
				m.slashSel = 0
				// Closing the popup shrinks the frame; repaint so the closed
				// popup's rows are not left burned on screen.
				return m, tea.ClearScreen
			}
			if len(m.mentionSuggestions()) > 0 || m.mentionDismissed {
				m.mentionDismissed = true // suppress until next text change
				m.mentionSel = 0
				return m, tea.ClearScreen
			}
			if len(m.pathSuggestions()) > 0 || m.pathDismissed {
				m.pathDismissed = true // suppress until next text change
				return m, tea.ClearScreen
			}
			// Esc interrupts an active turn. Prefer the backend's interrupt
			// endpoint (so the server cancels the turn too); always cancel the
			// local stream so the UI stops immediately.
			if m.streaming && m.drainingGen == 0 && m.cancel != nil {
				if q, ok := m.sess.backend.(backend.MessageQueuer); ok && m.sess.threadID != "" {
					intCtx, intCancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = q.InterruptThread(intCtx, m.sess.threadID)
					intCancel()
				}
				m.cancel()
				// Commit whatever the interrupted turn produced: the live
				// block for a text-only turn, buffered plain text for a tool
				// turn. The stream's terminal item arrives with a stale gen
				// and does not print, so this is the only chance.
				if m.hadToolTurn {
					if cmd := m.flushTokens(true); cmd != nil {
						cmds = append(cmds, cmd)
					}
				} else if cmd := m.printResponse(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.drainingGen = m.streamGen
				m.streamGen++ // items from the interrupted stream are now stale
			}
			return m, tea.Batch(cmds...)

		case "ctrl+k", "ctrl+u", "ctrl+w", "ctrl+y", "alt+y":
			// Readline-style kill-ring editing (see editor_power.go). These
			// keys are intercepted before the textarea, which binds ctrl+k/u/w
			// to plain deletes that never record into a ring.
			if m.pickMode != "" {
				return m, nil // picker overlays consume every key
			}
			m.editKey(msg)
			return m, nil

		case "ctrl+_", "ctrl+/", "ctrl+\x1f":
			// Emacs undo (all three spellings are the same 0x1f byte). Inert
			// while a turn streams or a picker owns the keys.
			if m.streaming || m.pickMode != "" {
				return m, nil
			}
			m.undo()
			return m, nil

		case "ctrl+g":
			// External editor round-trip: hand the terminal to $VISUAL/$EDITOR
			// with the current draft and restore the result on return. Only
			// when idle — a mid-turn suspend would strand the in-flight
			// stream, and an open picker must keep owning the keys.
			if m.streaming || m.pickMode != "" {
				return m, nil
			}
			return m, m.openExternalEditor()

		case "tab":
			// Complete the highlighted slash command (leaves a trailing space for args).
			if sugg := m.slashSuggestions(); len(sugg) > 0 {
				m.ta.SetValue(sugg[m.clampedSlashSel(len(sugg))].name + " ")
				m.ta.CursorEnd()
				m.slashSel = 0
				return m, nil
			}
			// Accept the highlighted @-mention.
			if ms := m.mentionSuggestions(); len(ms) > 0 {
				m.acceptMention(ms)
				m.mentionSel = 0
				m.mentionDismissed = true // prevent immediate re-popup
				return m, nil
			}
			// Complete the highlighted path suggestion (longest common prefix
			// when several candidates match).
			if psugg := m.pathSuggestions(); len(psugg) > 0 {
				m.tabCompletePath(psugg)
				m.pathSel = 0
				return m, nil
			}
			return m, nil

		case "up":
			if m.pickMode != "" {
				m.movePick(-1)
				return m, nil
			}
			if n := len(m.slashSuggestions()); n > 0 {
				if m.slashSel > 0 {
					m.slashSel--
				}
				return m, nil
			}
			if n := len(m.mentionSuggestions()); n > 0 {
				if m.mentionSel > 0 {
					m.mentionSel--
				}
				return m, nil
			}
			if n := len(m.pathSuggestions()); n > 0 {
				if m.pathSel > 0 {
					m.pathSel--
				}
				return m, nil
			}

			// If the cursor is not on the first logical line of a multi-line
			// input, let the textarea handle the key for cursor navigation.
			lines := strings.Split(m.ta.Value(), "\n")
			if len(lines) > 1 && m.ta.Line() > 0 {
				var taCmd tea.Cmd
				m.ta, taCmd = m.ta.Update(msg)
				cmds = append(cmds, taCmd)
				m.syncInputHeight()
				return m, nil
			}

			// Save current input as draft on first entry into history.
			if m.histIdx >= len(m.history) {
				m.draft = m.ta.Value()
			}
			rowsBefore := m.computeVisualRows()
			if m.histIdx > 0 {
				m.histIdx--
				m.ta.SetValue(m.history[m.histIdx])
				m.ta.CursorEnd()
			}
			// Recalling history only fills the input: completion popups must
			// not spawn off the recalled text, and a recalled message that
			// wraps to more (or fewer) rows resizes the frame — repaint so the
			// previous frame's rows are not left burned into the screen.
			m.slashDismissed, m.pathDismissed, m.mentionDismissed = true, true, true
			m.slashSel, m.pathSel, m.mentionSel = 0, 0, 0
			if m.computeVisualRows() != rowsBefore {
				return m, tea.ClearScreen
			}
			return m, nil

		case "down":
			if m.pickMode != "" {
				m.movePick(1)
				return m, nil
			}
			if n := len(m.slashSuggestions()); n > 0 {
				if m.slashSel < n-1 {
					m.slashSel++
				}
				return m, nil
			}
			if n := len(m.mentionSuggestions()); n > 0 {
				if m.mentionSel < n-1 {
					m.mentionSel++
				}
				return m, nil
			}
			if n := len(m.pathSuggestions()); n > 0 {
				if m.pathSel < n-1 {
					m.pathSel++
				}
				return m, nil
			}

			// If the cursor is not on the last logical line of a multi-line
			// input, let the textarea handle the key for cursor navigation.
			lines := strings.Split(m.ta.Value(), "\n")
			if len(lines) > 1 && m.ta.Line() < len(lines)-1 {
				var taCmd tea.Cmd
				m.ta, taCmd = m.ta.Update(msg)
				cmds = append(cmds, taCmd)
				m.syncInputHeight()
				return m, nil
			}

			rowsBefore := m.computeVisualRows()
			if m.histIdx < len(m.history)-1 {
				m.histIdx++
				m.ta.SetValue(m.history[m.histIdx])
				m.ta.CursorEnd()
			} else {
				m.histIdx = len(m.history)
				m.ta.SetValue(m.draft)
				m.ta.CursorEnd()
			}
			// Same contract as up-recall: fill the input only — no popups off
			// recalled text, clean repaint when the wrapped row count changes.
			m.slashDismissed, m.pathDismissed, m.mentionDismissed = true, true, true
			m.slashSel, m.pathSel, m.mentionSel = 0, 0, 0
			if m.computeVisualRows() != rowsBefore {
				return m, tea.ClearScreen
			}
			return m, nil

		case "enter", "alt+enter":
			// A pending /model or /sessions menu selects the highlighted row.
			if m.pickMode != "" {
				return m, m.selectPick()
			}
			// Autocomplete open: run the highlighted command.
			if sugg := m.slashSuggestions(); len(sugg) > 0 {
				cmd := sugg[m.clampedSlashSel(len(sugg))].name
				m.ta.Reset()
				m.slashSel = 0
				m.syncInputHeight()
				return m, m.slashCmd(cmd)
			}
			// @-mention popup open: accept the highlighted match.
			if ms := m.mentionSuggestions(); len(ms) > 0 {
				m.acceptMention(ms)
				m.mentionSel = 0
				m.mentionDismissed = true // prevent immediate re-popup
				return m, nil
			}
			// Path autocomplete open: complete the highlighted entry.
			if psugg := m.pathSuggestions(); len(psugg) > 0 {
				m.completePath(psugg)
				m.pathSel = 0
				m.pathDismissed = true // prevent immediate re-popup
				return m, nil
			}
			// Alt+Enter inserts a newline; a trailing backslash does too (shell
			// continuation convention).
			val := m.ta.Value()
			alt := msg.String() == "alt+enter"
			if alt || strings.HasSuffix(strings.TrimRight(val, " "), "\\") {
				if alt {
					m.ta.InsertString("\n")
				} else {
					trimmed := strings.TrimRight(val, " ")
					trimmed = strings.TrimSuffix(trimmed, "\\")
					spaceCount := len(val) - len(strings.TrimRight(val, " "))
					trimmed += strings.Repeat(" ", spaceCount) + "\n"
					m.ta.SetValue(trimmed)
					m.ta.CursorEnd()
				}
				m.syncInputHeight()
				return m, nil
			}

			v := strings.TrimSpace(m.ta.Value())
			if v == "" {
				return m, nil
			}

			m.ta.Reset()
			m.draft = ""
			// The send consumed the input; re-arm the completion popups that
			// search-mode acceptance suppressed for this keypress.
			m.slashDismissed = false
			m.mentionDismissed = false
			m.pathDismissed = false

			// Shell escape: "!command" runs directly in the user's shell
			// and prints output inline. Does not go through the agent. The
			// raw typed text is kept (echo/history) and never @-expanded.
			if strings.HasPrefix(v, "!") {
				cmdStr := strings.TrimSpace(v[1:])
				if cmdStr == "" {
					return m, nil
				}
				if len(m.history) == 0 || m.history[len(m.history)-1] != v {
					m.history = append(m.history, v)
				}
				m.histIdx = len(m.history)
				cmds = append(cmds, m.printLine("\n"+sty(cMuted).Render("  ⟳ $ "+cmdStr)))
				return m, tea.Batch(append(cmds, m.runShellEscape(cmdStr))...)
			}

			// Slash commands are not added to history — they'd pollute
			// the up/down arrow cycle with session-switching, model
			// selection, and other meta operations.
			if strings.HasPrefix(v, "/") {
				return m, m.slashCmd(v)
			}

			// Keep the RAW typed text for history and the echo: @file
			// expansion is only for the payload sent to the model, so
			// recall and scrollback stay readable when a file is huge.
			if len(m.history) == 0 || m.history[len(m.history)-1] != v {
				m.history = append(m.history, v)
			}
			m.histIdx = len(m.history)

			// Echo user message with a divider above and below; hard-wrapped to
			// the terminal width (the message can be huge after @file expansion).
			div := sty(cBorder).Render(strings.Repeat("─", m.width))
			echo := wrapForPrint(userEchoStyle().Render(v), m.printWidth())
			cmds = append(cmds, tea.Printf("\n%s\n%s\n%s", div, echo, div))

			// Expand @file references — replace each @path with its content —
			// only for what the model receives.
			payload := expandFileRefs(v)

			if m.streaming {
				// Mid-turn message: prefer queueing it for delivery at the next
				// tool boundary over cancelling the turn.
				if m.drainingGen == 0 {
					if q, ok := m.sess.backend.(backend.MessageQueuer); ok && m.sess.threadID != "" {
						enqCtx, enqCancel := context.WithTimeout(context.Background(), 5*time.Second)
						enqErr := q.EnqueueUserMessage(enqCtx, m.sess.threadID, payload)
						enqCancel()
						if enqErr == nil {
							cmds = append(cmds, tea.Printf("\n  %s", sty(cGreen).Render("queued ✓")))
							return m, tea.Batch(cmds...)
						}
						// Non-fatal: fall through to the pending-send path below.
					}
				}
				// Either the turn is already draining, the server predates the
				// queue endpoint, or the thread has no active turn — stash the
				// message to launch after the current stream drains.
				m.pendingSends = append(m.pendingSends, payload)
				return m, tea.Batch(cmds...)
			}

			cmds = m.startStream(payload, cmds)
		}
	}

	// Don't feed keys to the (hidden) input while a picker menu is open.
	if m.pickMode == "" {
		before := m.ta.Value()
		hadPopup := m.anySuggestOpen()
		pre := cursorSnap{value: before, row: m.ta.Line(), col: m.ta.Column()}
		var taCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)
		cmds = append(cmds, taCmd)
		if m.ta.Value() != before {
			// Every direct textarea edit is undoable: snapshot the pre-edit
			// state (this also breaks the yank-pop chain — alt+y only
			// rotates immediately after ctrl+y).
			m.pushUndo(pre)
			m.slashSel = 0
			m.pathSel = 0
			m.pathDismissed = false // re-enable path popup on text change
			m.mentionSel = 0
			m.mentionDismissed = false // re-enable mention popup on text change
			// Clear draft when user types: the saved input is stale.
			m.draft = ""
		}
		// Opening or closing a suggestion popup changes the frame height;
		// the inline renderer leaves the transition's stale rows on screen
		// (burned popup artifacts), so repaint cleanly once per transition.
		if m.anySuggestOpen() != hadPopup {
			cmds = append(cmds, tea.ClearScreen)
		}
		// The textarea uses a fixed viewport height; View() clips it.
	}
	return m, tea.Batch(cmds...)
}

func (m appModel) View() tea.View {
	div := sty(cBorder).Render(strings.Repeat("─", m.width))
	stat := m.statusLine()
	mid := ""
	if len(m.approvals) > 0 {
		mid = m.approvalPromptView()
	} else if m.pickMode != "" {
		mid = m.pickerView() // arrow-navigable menu replaces the input box
	} else {
		// Every branch that keeps the textarea visible must clip it: bubbles
		// pads its View to the full fixed viewport height, so an unclipped
		// branch would lurch the layout when a popup appears. Build the block
		// from scratch (never start from m.ta.View() unclipped).
		var popup string
		if m.searchMode == "reverse" {
			// No match: readline shows "(failed reverse-i-search)`query'" so
			// a stale match is never presented as current.
			prompt := sty(cWarn).Render("(reverse-i-search)`" + m.searchQuery + "': ")
			if m.searchMatch < 0 && m.searchQuery != "" {
				prompt = sty(cErr).Render("(failed reverse-i-search)`" + m.searchQuery + "': ")
			}
			popup = prompt + sty(cMuted).Render(m.ta.Value()) + "\n"
		} else if sugg := m.slashSuggestions(); len(sugg) > 0 {
			popup = m.slashSuggestView(sugg) + "\n" // autocomplete above input
		} else if ms := m.mentionSuggestions(); len(ms) > 0 {
			popup = m.mentionSuggestView(ms) + "\n" // @-mention picker above input
		} else if psugg := m.pathSuggestions(); len(psugg) > 0 {
			popup = m.pathSuggestView(psugg) + "\n" // path popup above input
		}
		mid = m.overflowIndicator() + popup + m.clipTextarea(m.ta.View())
	}
	// Docked panels (subagent HUD above the task list) sit above the input,
	// separated from it by a dashed rule — lighter than the solid outer
	// dividers so the input row reads as its own region.
	if dv := m.dockedPanelsView(); dv != "" {
		mid = dv + "\n" + mid
	}
	// Live area at the top of the frame — animated thinking indicator over
	// the progressive markdown block — so both repaint as plain frame content,
	// with no out-of-band cursor surgery.
	var live []string
	if tv := m.thinkingView(); tv != "" {
		live = append(live, tv)
	}
	if lb := m.liveBlockView(); lb != "" {
		live = append(live, lb)
	}
	if len(live) > 0 {
		mid = strings.Join(live, "\n") + "\n" + mid
	}
	return tea.NewView(fmt.Sprintf("%s\n%s\n%s\n%s", div, mid, div, stat))
}

// dockedPanelsView renders the panels docked above the input — subagent HUD
// over the task list — followed by a dashed separator rule. It returns ""
// when no panel is showing.
func (m appModel) dockedPanelsView() string {
	var docked []string
	if hud := m.subagentHUDView(); hud != "" {
		docked = append(docked, hud)
	}
	if tp := m.todoPanelView(); tp != "" {
		docked = append(docked, tp)
	}
	if len(docked) == 0 {
		return ""
	}
	return strings.Join(docked, "\n") + "\n" + sty(cBorder).Render(strings.Repeat("╌", m.width))
}

// inputMaxHeight caps the visual rows the input area may display.
const inputMaxHeight = 15

// countVisualRows is computeVisualRows without the inputMaxHeight cap, so the
// overflow indicator can tell how many rows sit above the clipped viewport.
func (m *appModel) countVisualRows() int {
	w := m.ta.Width()
	if w < 1 {
		w = 1
	}
	rows := 0
	for _, ln := range strings.Split(m.ta.Value(), "\n") {
		rows += wrapRowCount([]rune(ln), w)
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// wrapRowCount returns the number of visual rows the bubbles v2 textarea
// renders for one logical line at the given wrap width. It mirrors the
// textarea's private word-wrap exactly, including its trailing-space
// accounting: a line that lands exactly on the wrap width spills an extra
// (often space-only) row, and a line whose last word would sit flush against
// the width is pushed onto a new row. A plain ceil(width/w) estimate misses
// both cases, so clipTextarea used to trim a row the textarea actually drew —
// the reported "typed characters disappear at the end of the 3rd/4th visual
// line" bug. Keep this in lockstep with
// charm.land/bubbles/v2/textarea.textarea.wrap.
func wrapRowCount(line []rune, width int) int {
	var (
		lines  = [][]rune{{}}
		word   = []rune{}
		row    int
		spaces int
	)
	for _, r := range line {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}

		if spaces > 0 {
			if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces > width {
				row++
				lines = append(lines, []rune{})
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], repeatSpaces(spaces)...)
				spaces = 0
				word = nil
			} else {
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], repeatSpaces(spaces)...)
				spaces = 0
				word = nil
			}
		} else {
			// If the last character is a double-width rune, the word may not
			// fit on this line even when it does not exceed the width.
			lastCharLen := runewidth.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastCharLen > width {
				// If the current line has content, the word fills it and
				// moves to the next line.
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}

	if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces >= width {
		lines = append(lines, []rune{})
		lines[row+1] = append(lines[row+1], word...)
		spaces++
		lines[row+1] = append(lines[row+1], repeatSpaces(spaces)...)
	} else {
		lines[row] = append(lines[row], word...)
		spaces++
		lines[row] = append(lines[row], repeatSpaces(spaces)...)
	}

	return len(lines)
}

// repeatSpaces mirrors the textarea's trailing-space padding (kept in sync
// with wrapRowCount).
func repeatSpaces(n int) []rune {
	return []rune(strings.Repeat(" ", n))
}

// overflowIndicator renders a one-line muted hint when the input exceeds the
// displayed cap and the textarea has silently scrolled, so the hidden content
// above is at least discoverable.
func (m *appModel) overflowIndicator() string {
	total := m.countVisualRows()
	if total <= inputMaxHeight {
		return ""
	}
	return sty(cMuted).Render(fmt.Sprintf("↑ %d lines above", total-inputMaxHeight)) + "\n"
}

// computeVisualRows returns the number of visual rows the textarea content
// would occupy at the current wrap width. Used to clip the rendered viewport
// so only the occupied portion is displayed.
func (m *appModel) computeVisualRows() int {
	rows := m.countVisualRows()
	if rows > inputMaxHeight {
		rows = inputMaxHeight
	}
	return rows
}

// clipTextarea trims the textarea's rendered view (which always uses the full
// viewport height of inputMaxHeight) to only the visually occupied rows, so
// empty rows below the content are not rendered as blank space. Empty input
// still shows one row for the placeholder.
func (m *appModel) clipTextarea(rendered string) string {
	lines := strings.Split(rendered, "\n")
	needed := m.computeVisualRows()
	if needed <= 0 {
		needed = 1
	}
	if needed >= len(lines) {
		return rendered
	}
	return strings.Join(lines[:needed], "\n")
}

// syncInputHeight is a no-op kept for backward compat with call sites.
// Height management is now handled by the fixed viewport + clipTextarea.
func (m *appModel) syncInputHeight() {}

	// tokFlushThreshold is how many bytes tokBuf may accumulate before a spinner
// tick flushes it mid-stream (progressive assistant text) instead of waiting
// for the next tool event or "done". Tool turns flush plain progressive prose
// the same way: the alternative is tokens accumulating silently and dumping
// as one giant unstyled block at the next event.
const tokFlushThreshold = 512

// flushTokens prints buffered assistant text. The final flush (tool events,
// "done", mid-stream send) drains everything. A mid-stream flush (final=false,
// from the spinner tick) only fires once the buffer has grown past
// tokFlushThreshold, and cuts at the last line/word boundary so a word is
// never split across two flushes; the tail stays buffered for the next tick.
func (m *appModel) flushTokens(final bool) tea.Cmd {
	if len(m.tokBuf) == 0 {
		return nil
	}
	s := string(m.tokBuf)
	if !final {
		if len(s) < tokFlushThreshold {
			return nil
		}
		i := strings.LastIndexAny(s, " \n")
		if i < 0 {
			return nil // one unbroken word so far — can't split it mid-word
		}
		s, m.tokBuf = s[:i+1], m.tokBuf[i+1:]
		return tea.Printf("%s", m.marginProse(s))
	}
	m.tokBuf = m.tokBuf[:0]
	// Render through glamour (markdown styling, unwrapped) with the content
	// margin gutter. Glamour renders without wrapping — its reflow wrapper
	// breaks words at hyphens — so WrapPrint owns all line breaking at
	// proseWidth (printWidth minus the margin) and only breaks at whitespace.
	rendered := renderMarkdown(s)
	if rendered == "" {
		return tea.Printf("%s\n", m.marginProse(s))
	}
	return tea.Printf("%s\n", indentGlamourOutput(wrapForPrint(rendered, m.proseWidth())))
}

// ── Stream launch ─────────────────────────────────────────────────────────────
//
// startStream launches a fresh stream generation for input v and arms the read
// pump. Every launch bumps streamGen so late items from a canceled generation
// are recognized as stale and dropped in Update.

// runTemplateCommand expands an unknown "/name args" line as a prompt
// template when one matches (persona/templates.go): the body is expanded
// with the args and submitted as the user message, with a one-line header
// echoing the expansion. It returns nil when no template claims the name,
// letting slashCmd fall through to the unknown-command notice.
func (m *appModel) runTemplateCommand(name string, in slashInvocation) tea.Cmd {
	if m.sess == nil {
		return nil // hand-built test models carry no session; nothing to expand
	}
	templates := persona.DiscoverTemplates(m.sess.workspace)
	t, ok := persona.FindTemplate(templates, strings.TrimPrefix(name, "/"))
	if !ok {
		return nil
	}
	payload := persona.ExpandTemplate(t, in.tail)
	// The typed line is a prompt, not a meta command: keep it in history for
	// ↑ recall like a regular send.
	if len(m.history) == 0 || m.history[len(m.history)-1] != in.raw {
		m.history = append(m.history, in.raw)
	}
	m.histIdx = len(m.history)
	cmds := []tea.Cmd{m.printLine("\n" + sty(cMuted).Render("  » template: "+t.Name) + "\n")}
	if m.streaming {
		m.pendingSends = append(m.pendingSends, payload)
		return tea.Batch(cmds...)
	}
	return tea.Batch(m.startStream(payload, cmds)...)
}

func (m *appModel) startStream(v string, cmds []tea.Cmd) []tea.Cmd {
	m.clearSubagentHUD()
	m.clearApprovals("new turn started")
	m.streamGen++
	ch := make(chan streamItem, 256)
	m.streamCh = ch
	m.streaming = true
	m.turnStart = time.Now()
	m.tokBuf = m.tokBuf[:0]
	m.responseBuf = m.responseBuf[:0]
	m.lastResponseRaw = ""
	m.hadToolTurn = false
	m.liveLabel = false
	m.liveRendered = ""
	m.liveDirty = false
	m.thinking = false
	m.launchStreamGoroutine(v, ch)                          // starts goroutine, returns immediately
	return append(cmds, waitForStreamItem(ch, m.streamGen)) // start the read pump
}

// launchStreamGoroutine starts Backend.SendMessage in a background goroutine that writes
// streamItems into ch. Returns immediately — caller must issue waitForStreamItem(ch).
func (m *appModel) launchStreamGoroutine(input string, ch chan<- streamItem) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	// Tag CLI-originated turns so buildMessages can tailor the system prompt
	// (plain-text formatting directive). Explicit Request.Source wins over this.
	ctx = agent.WithRequestSource(ctx, "cli")
	ctx = tools.WithApprovalHandler(ctx, tools.ApprovalHandlerFunc(func(approvalCtx context.Context, request tools.ApprovalRequest) (tools.ApprovalDecision, error) {
		reply := make(chan tools.ApprovalDecision, 1)
		requestCopy := request
		select {
		case ch <- streamItem{kind: "approval", approvalReq: &requestCopy, approvalReply: reply}:
		case <-approvalCtx.Done():
			return tools.ApprovalDecision{}, approvalCtx.Err()
		}
		select {
		case decision := <-reply:
			return decision, nil
		case <-approvalCtx.Done():
			return tools.ApprovalDecision{}, approvalCtx.Err()
		}
	}))

	// Capture the width now (race-free) so preview lines can be kept narrower
	// than the terminal. Lines that reach the terminal edge wrap and leave the
	// live status bar's tail uncleared on that row — the artifact we avoid here.
	width := m.width
	if width <= 0 {
		width = 80
	}
	// Snapshot mutable session state before the goroutine starts. Model/thread
	// switches happen on the Bubble Tea goroutine and must not race with request
	// construction or swap the backend beneath an in-flight turn.
	streamBackend := m.sess.backend
	threadID := m.sess.threadID
	modelAlias := m.sess.modelAlias
	// Cost rollups price this turn's usage events against the catalog; nil
	// (disabled, unknown, or free model) leaves every segment hidden.
	var costs *costTracker
	if m.sess.clientCfg != nil && m.sess.clientCfg.ShowCost {
		costs = newCostTracker(m.sess.cfg, modelAlias)
	}

	go func() {
		defer cancel()

		sendAt := time.Now()
		var firstTok time.Time
		var pTok, cTok int
		var firstLabel bool
		var thinkingShown bool
		subagentDraftShown := make(map[string]bool)

		if streamBackend == nil {
			ch <- streamItem{kind: "err", err: errors.New("CLI backend is not configured")}
			return
		}
		// Tropical mode travels as the effort string; the agent maps it to
		// high effort plus the heavy-subagent system directive.
		effort := m.sess.effort
		if m.sess.tropical {
			effort = "tropical"
		}
		events, err := streamBackend.SendMessage(ctx, threadID, modelAlias, input, effort, m.sess.planMode)
		if err != nil {
			ch <- streamItem{kind: "err", err: err}
			return
		}
		if events == nil {
			ch <- streamItem{kind: "err", err: unexpectedStreamEndError()}
			return
		}

		var tid string
		var streamErr error
		terminal := false
		// Held tool-call headers, flushed when their result arrives (or at
		// stream end, so an interrupted batch still shows what was running).
		// callOrder records arrival order; IDs are opaque strings, so the
		// end-of-stream flush cannot recover it from the map alone.
		pendingToolHeads := make(map[string]pendingToolLine)
		var callOrder []string
	streamLoop:
		for ev := range events {
			if ev.Type != "thinking" {
				thinkingShown = false
			}
			switch ev.Type {
			case "token":
				if firstTok.IsZero() {
					firstTok = time.Now()
				}
				if !firstLabel {
					ch <- streamItem{kind: "label", content: sty(cPurple).Bold(true).Render("◈ sandbar")}
					firstLabel = true
				}
				ch <- streamItem{kind: "token", content: ev.Content}

			case "thinking":
				// Reasoning arrives as many small chunks. Surface one state
				// change instead of one per chunk: the animated in-frame
				// indicator (thinkingView) takes over from there and any
				// non-thinking event ends the phase.
				if !thinkingShown {
					ch <- streamItem{kind: "thinking"}
					thinkingShown = true
				}

			case "intermediate":
				// "Processing..." is immediately followed by concrete tool rows.
				// Retry notices are useful, so retain only those informational events.
				if strings.HasPrefix(ev.Content, "retrying ") {
					ch <- streamItem{kind: "activity", content: sty(cWarn).Render("↻ " + ev.Content)}
				} else if strings.HasPrefix(ev.Content, "compression: ") {
					// Non-fatal compression plumbing notice (e.g. a saved summary
					// that could not be loaded): surfaced once per session.
					ch <- streamItem{kind: "activity", content: sty(cWarn).Render("⚠ " + strings.TrimPrefix(ev.Content, "compression: "))}
				}

			case "user_message":
				// A queued mid-turn message was injected. The full text was echoed
				// at send time; confirm delivery with one compact line.
				ch <- streamItem{kind: "activity", content: sty(cMuted).Render("↳ " + clip(oneline(ev.Content), width-4))}

			case "tool_call":
				// Compose the header (tool name plus a one-line preview of its
				// primary argument) but hold it until the result arrives: a
				// tool call renders as ONE line — header joined with its
				// result preview — instead of a header line plus a result
				// line. Held headers flush at stream end if a result never
				// arrives (interrupted turn).
				head := sty(cAccent).Render("⚙") + " " + sty(cLavender).Bold(true).Render(ev.ToolName)
				if p := toolPreview(ev.ToolName, ev.Arguments); p != "" {
					// Budget = width − 4-space indent − "⚙ " − name − ": " − margin,
					// so the assembled line can't reach the terminal edge and wrap.
					budget := width - 2 - 2 - len(ev.ToolName) - 2 - 2
					head += sty(cMuted).Render(": " + clip(p, budget))
				}
				pendingToolHeads[ev.ToolCallID] = pendingToolLine{head: head, name: ev.ToolName}
				callOrder = append(callOrder, ev.ToolCallID)

			case "tool_result":
				// Join the held header with its result preview on one line.
				// Failures (non-zero exits, error results) color the result
				// half red so they stay impossible to miss at one line.
				// File-edit diffs keep their multi-line block: the header
				// prints as its own line, the diff below it, unchanged.
				pending := pendingToolHeads[ev.ToolCallID]
				delete(pendingToolHeads, ev.ToolCallID)
				if isDiffOutput(ev.Content) {
					if pending.head != "" {
						ch <- streamItem{kind: "tool", content: pending.head, toolName: pending.name}
					}
					if d := renderDiff(ev.Content, width); d != "" {
						ch <- streamItem{kind: "diff", content: d}
					}
				} else if pending.name == "todo" {
					// Successful todo mutations feed the sticky task panel above
					// the input instead of appending the whole list to
					// scrollback: print only a compact header line and carry the
					// parsed rows on the item. Errors and unrecognized output
					// keep the default one-line rendering below.
					if rows := parseTodoList(ev.Content); rows != nil {
						summary := fmt.Sprintf("%d tasks", len(rows))
						line := mergedToolLine(pending.head, summary, width)
						if line == "" {
							line = pending.head
						}
						ch <- streamItem{kind: "tool", content: line, toolName: pending.name, todoSet: true, todoRows: rows}
					} else if strings.TrimSpace(ev.Content) == "(no items)" {
						line := mergedToolLine(pending.head, "(no items)", width)
						if line == "" {
							line = pending.head
						}
						ch <- streamItem{kind: "tool", content: line, toolName: pending.name, todoSet: true}
					} else if line := mergedToolLine(pending.head, toolResultPreview(ev.ToolName, ev.Content), width); line != "" {
						ch <- streamItem{kind: "tool", content: line, toolName: pending.name}
					}
				} else if pending.head != "" {
					if line := mergedToolLine(pending.head, toolResultPreview(ev.ToolName, ev.Content), width); line != "" {
						ch <- streamItem{kind: "tool", content: line, toolName: pending.name}
					}
				} else if p := toolResultPreview(ev.ToolName, ev.Content); p != "" {
					// Orphan result (no held header): render as before.
					ch <- streamItem{kind: "result", content: sty(cMuted).Render(clip(p, width-4-2))}
				}

			case "subagent_start":
				ch <- streamItem{kind: "subagent", taskID: firstNonEmpty(ev.TaskID, ev.ToolCallID), taskGoal: ev.TaskGoal, taskStatus: firstNonEmpty(ev.TaskStatus, "running"), content: "starting"}

			case "subagent_tool_call":
				activity := ev.ToolName
				if p := toolPreview(ev.ToolName, ev.Arguments); p != "" {
					activity += ": " + p
				}
				ch <- streamItem{kind: "subagent", taskID: firstNonEmpty(ev.TaskID, ev.ToolCallID), taskGoal: ev.TaskGoal, taskStatus: "running", content: activity}

			case "subagent_tool_result":
				ch <- streamItem{kind: "subagent", taskID: firstNonEmpty(ev.TaskID, ev.ToolCallID), taskGoal: ev.TaskGoal, taskStatus: "running", content: ev.ToolName + " complete"}

			case "subagent_token":
				// Token deltas can number in the hundreds. The final delegate result
				// follows in provider order, so show one compact drafting state.
				if !subagentDraftShown[ev.ToolCallID] {
					subagentDraftShown[ev.ToolCallID] = true
					ch <- streamItem{kind: "subagent", taskID: firstNonEmpty(ev.TaskID, ev.ToolCallID), taskGoal: ev.TaskGoal, taskStatus: "running", content: "drafting response…"}
				}

			case "subagent_error":
				delete(subagentDraftShown, ev.ToolCallID)
				ch <- streamItem{kind: "subagent", taskID: firstNonEmpty(ev.TaskID, ev.ToolCallID), taskGoal: ev.TaskGoal, taskStatus: firstNonEmpty(ev.TaskStatus, "failed"), content: oneline(ev.Content)}
				message := fmt.Sprintf("⚠ %s: %s", subagentTag(ev.ToolCallID), oneline(ev.Content))
				ch <- streamItem{kind: "tool", content: sty(cErr).Render(clip(message, width-4))}

			case "subagent_done":
				delete(subagentDraftShown, ev.ToolCallID)
				ch <- streamItem{kind: "subagent", taskID: firstNonEmpty(ev.TaskID, ev.ToolCallID), taskGoal: ev.TaskGoal, taskStatus: "completed"}
				// The enclosing delegate_task result follows in provider order.
				// Its progress has already been surfaced above, so avoid printing
				// the same final response a second time here.

			case "usage":
				pTok, cTok = ev.PromptTokens, ev.CompletionTokens
				// Live context-gauge update: prompt tokens = current context size.
				costs.add(ev)
				ch <- streamItem{kind: "ctx", ctxUsed: ev.PromptTokens, cost: costs.segment()}

			case "compression_start":
				if ev.Compression != nil {
					c := ev.Compression
					ch <- streamItem{kind: "compression", compType: "compression_start", compEvent: c}
					var sb strings.Builder
					sb.WriteString("⟳ compressing context")
					if c.ModelAlias != "" {
						sb.WriteString(" with " + c.ModelAlias)
					}
					if c.BeforeTokens > 0 && c.BudgetTokens > 0 && c.BudgetTokens < c.BeforeTokens {
						pct := 100 * c.BeforeTokens / c.BudgetTokens
						sb.WriteString(fmt.Sprintf(" (%s tokens, %d%% of %s budget)", fmtTok(c.BeforeTokens), pct, fmtTok(c.BudgetTokens)))
					} else if c.BeforeTokens > 0 {
						sb.WriteString(fmt.Sprintf(" (%s tokens)", fmtTok(c.BeforeTokens)))
					}
					sb.WriteString("…")
					ch <- streamItem{kind: "tool", content: sty(cWarn).Render(clip(sb.String(), width-4))}
				}

			case "compression_end":
				if ev.Compression != nil {
					ch <- streamItem{kind: "compression", compType: "compression_end", compEvent: ev.Compression}
					line, color := renderCompressionLine(ev.Compression)
					ch <- streamItem{kind: "tool", content: sty(color).Render(line), repaintAfter: true}
				}

			case "compression_error":
				if ev.Compression != nil {
					ch <- streamItem{kind: "compression", compType: "compression_error", compEvent: ev.Compression}
					reason := ev.Compression.FallbackReason
					if reason == "" && ev.Compression.Error != "" {
						reason = ev.Compression.Error
					}
					line := "⚠ compression failed: " + reason
					if ev.Compression.ElapsedMS > 0 {
						line += fmt.Sprintf(" (in %s)", fmtDurMS(ev.Compression.ElapsedMS))
					}
					ch <- streamItem{kind: "tool", content: sty(cErr).Render(clip(line, width-2-2)), repaintAfter: true}
				}

			case "auxiliary_usage":
				if ev.UsagePurpose == "compression" {
					msg := fmt.Sprintf("summarizer: %d in · %d out", ev.PromptTokens, ev.CompletionTokens)
					if ev.TotalTokens > 0 {
						msg += fmt.Sprintf(" (%d total", ev.TotalTokens)
						if ev.UsageCallCount > 1 {
							msg += fmt.Sprintf(", %d calls", ev.UsageCallCount)
						}
						msg += ")"
					}
					ch <- streamItem{kind: "result", content: sty(cMuted).Render(msg)}
				}

			case "thread":
				// Thread identity announced at turn start — capture it so the
				// end-of-stream "threadID" item fires even when the turn is
				// interrupted before the terminal "done" event, AND forward it
				// immediately: mid-turn steering (EnqueueUserMessage) needs the
				// thread ID during the FIRST turn, or queued messages silently
				// fall into pendingSends and are never delivered.
				if ev.ThreadID != "" {
					tid = ev.ThreadID
					ch <- streamItem{kind: "threadID", content: ev.ThreadID}
				}

			case "done":
				if ev.ThreadID != "" {
					tid = ev.ThreadID
				}
				terminal = true
				break streamLoop

			case "error":
				streamErr = streamEventError(ev.Content)
				terminal = true
				break streamLoop
			}
		}
		if !terminal {
			if ctx.Err() != nil {
				streamErr = ctx.Err()
			} else {
				streamErr = unexpectedStreamEndError()
			}
		}

		// Flush headers whose result never arrived (interrupted turn) so the
		// transcript still records what was in flight, as bare header lines.
		for _, pending := range pendingInOrder(callOrder, pendingToolHeads) {
			ch <- streamItem{kind: "tool", content: pending.head, toolName: pending.name}
		}

		if tid != "" {
			ch <- streamItem{kind: "threadID", content: tid}
		}

		if streamErr != nil {
			ch <- streamItem{kind: "err", err: streamErr}
		} else {
			foot := buildFooter(sendAt, firstTok, time.Now(), pTok, cTok)
			ch <- streamItem{kind: "done", footer: foot}
		}
	}()
}

// ── Context fetch ─────────────────────────────────────────────────────────────

func (m appModel) contextCmd() tea.Cmd {
	tid := m.sess.threadID
	if tid == "" || m.sess.backend == nil {
		return nil
	}
	be := m.sess.backend
	cfg, model := m.sess.cfg, m.sess.modelAlias
	isLocal := m.sess.local != nil
	return func() tea.Msg {
		u, mx, _ := be.GetContextInfo(tid)
		// Prefer the active session model's window for the max so the gauge is
		// correct right after a /model switch (GetContextInfo resolves the
		// thread's stored model, which may differ from the switched-to model).
		if isLocal {
			if cl := contextLengthFor(cfg, model); cl > 0 {
				mx = cl
			}
		}
		return statusMsg{used: u, max: mx}
	}
}

// contextLengthFor returns the context-window size of a model alias, or 0 if it
// can't be resolved.
func contextLengthFor(cfg *config.Config, alias string) int {
	if cfg == nil || alias == "" {
		return 0
	}
	if r, err := llm.NewRegistry(cfg).ResolveModel(alias); err == nil {
		return r.ContextLength
	}
	return 0
}

// runShellEscape executes a command in the user's shell and reports the output
// via shellDoneMsg. It runs inside a tea.Cmd (BubbleTea's goroutine pool) with
// a 30s timeout — a synchronous CombinedOutput in Update would freeze the
// whole UI for the duration of the command. Output prints on receipt, wrapped
// to the terminal width like everything else. The context is held on the model
// so Ctrl+C cancels the in-flight command instead of quitting the app.
func (m *appModel) runShellEscape(cmdStr string) tea.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m.escapeRunning = true
	m.escapeCancel = cancel
	return func() tea.Msg {
		defer cancel()
		output, err := exec.CommandContext(ctx, shell, "-c", cmdStr).CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after 30s")
		} else if ctx.Err() == context.Canceled {
			err = fmt.Errorf("cancelled")
		}
		return shellDoneMsg{cmd: cmdStr, output: string(output), err: err}
	}
}

// slashSuggestions returns the commands matching the current partial input. It
// is active only while the input is a "/command" prefix with no space yet (i.e.
// still typing the command name, not its arguments). Aliases match too — the
// suggestion popup lists the canonical command for each matching alias.
func (m appModel) slashSuggestions() []slashCommand {
	if m.slashDismissed {
		return nil
	}
	v := m.ta.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		return nil
	}
	var out []slashCommand
	for _, command := range slashCommands {
		if !command.available(&m) || !command.matchesPrefix(v) {
			continue
		}
		out = append(out, command)
	}
	return out
}

// clampedSlashSel keeps the highlighted suggestion in range as the list filters.
func (m appModel) clampedSlashSel(n int) int {
	s := m.slashSel
	if s >= n {
		s = n - 1
	}
	if s < 0 {
		s = 0
	}
	return s
}

// maxSlashPopupRows caps the visible suggestion rows; the full list windows
// around the selection and the hint reports the hidden remainder, so opening
// the popup never lurches the layout taller than the input block's budget.
const maxSlashPopupRows = 5

// slashSuggestView renders the autocomplete popup shown above the input.
func (m appModel) slashSuggestView(sugg []slashCommand) string {
	sel := m.clampedSlashSel(len(sugg))
	start := 0
	if sel > maxSlashPopupRows-1 {
		start = sel - maxSlashPopupRows + 1
	}
	end := start + maxSlashPopupRows
	if end > len(sugg) {
		end = len(sugg)
	}
	var b strings.Builder
	// Fixed-height body (see pathSuggestView): the frame must not change
	// height as the filter narrows, or the inline renderer burns stale popup
	// rows into scrollback.
	shown := 0
	for i := start; i < end; i++ {
		c := sugg[i]
		name := fmt.Sprintf("%-12s", c.name)
		if i == sel {
			b.WriteString(sty(cAccent).Render("  ▸ "+name) + "  " + sty(cBright).Render(c.desc) + "\n")
		} else {
			b.WriteString("    " + sty(cLavender).Render(name) + "  " + sty(cMuted).Render(c.desc) + "\n")
		}
		shown++
	}
	for ; shown < maxSlashPopupRows; shown++ {
		b.WriteString("\n")
	}
	hint := "    ↑↓ move · Tab complete · Enter run · Esc dismiss"
	if hidden := len(sugg) - (end - start); hidden > 0 {
		hint += fmt.Sprintf(" · %d more", hidden)
	}
	b.WriteString(sty(cMuted).Render(hint))
	return b.String()
}

// ── Path autocomplete ─────────────────────────────────────────────────────────

// pathSuggestions returns filesystem entries matching the current word if it
// contains a "/". Slash command suggestions take priority (they fire when the
// entire input starts with "/" with no spaces). Path completion fires on any
// word after a space that contains a "/" — e.g. "read /home/sc" or
// "edit ~/que".
func (m appModel) pathSuggestions() []string {
	if m.pathDismissed {
		return nil
	}
	v := m.ta.Value()
	// Don't interfere with slash command suggestions.
	if strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \n") {
		return nil
	}
	// Extract the word at the cursor (everything after the last whitespace).
	lastSpace := strings.LastIndexAny(v, " \n")
	word := v[lastSpace+1:]

	slashIdx := strings.LastIndex(word, "/")
	if slashIdx < 0 {
		return nil
	}

	dir := word[:slashIdx]
	prefix := word[slashIdx+1:]

	if dir == "" {
		dir = "/"
	}
	if dir == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			dir = home
		}
	} else if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			dir = home + dir[1:]
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Skip hidden files unless the user typed a leading dot.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// clampedPathSel keeps the highlighted row in range as the list filters.
func (m appModel) clampedPathSel(n int) int {
	s := m.pathSel
	if s >= n {
		s = n - 1
	}
	if s < 0 {
		s = 0
	}
	return s
}

// pathSuggestView renders the filesystem completion popup shown above the input.
func (m appModel) pathSuggestView(sugg []string) string {
	sel := m.clampedPathSel(len(sugg))
	const maxShow = 10
	start := 0
	if sel > maxShow-1 {
		start = sel - maxShow + 1
	}
	end := start + maxShow
	if end > len(sugg) {
		end = len(sugg)
	}
	var b strings.Builder
	// The popup body is padded to a fixed row count: a variable-height popup
	// changes the frame height on every keystroke filter, and the inline
	// renderer leaves stale popup rows burned into scrollback at each change.
	shown := 0
	for i := start; i < end; i++ {
		name := sugg[i]
		if i == sel {
			b.WriteString(sty(cAccent).Render("  ▸ "+name) + "\n")
		} else {
			b.WriteString(sty(cMuted).Render("    "+name) + "\n")
		}
		shown++
	}
	for ; shown < maxShow; shown++ {
		b.WriteString("\n")
	}
	hint := "    ↑↓ move · Tab/Enter complete · Esc dismiss"
	if hidden := len(sugg) - (end - start); hidden > 0 {
		hint += fmt.Sprintf(" · %d more…", hidden)
	}
	b.WriteString(sty(cMuted).Render(hint))
	return b.String()
}

// completePath replaces the current word's filename portion with the
// selected suggestion (Enter). The directory portion (everything up to and
// including the last "/") is preserved.
func (m *appModel) completePath(sugg []string) {
	sel := m.clampedPathSel(len(sugg))
	if sel < 0 || sel >= len(sugg) {
		return
	}
	m.fillPathWord(sugg[sel])
}

// anySuggestOpen reports whether any inline suggestion popup (slash command,
// @-mention, path completion) is currently showing.
func (m appModel) anySuggestOpen() bool {
	return len(m.slashSuggestions()) > 0 || len(m.mentionSuggestions()) > 0 || len(m.pathSuggestions()) > 0
}

// tabCompletePath completes the current path word like bash (Tab): a single
// candidate replaces the word outright; several candidates fill their
// longest common prefix, and a second Tab then narrows or selects from the
// refreshed list. Enter remains the way to pick a specific highlighted row.
func (m *appModel) tabCompletePath(sugg []string) {
	chosen := ""
	if len(sugg) == 1 {
		chosen = sugg[0]
	} else if len(sugg) > 1 {
		chosen = longestCommonPrefix(sugg)
	}
	if chosen == "" {
		return
	}
	m.fillPathWord(chosen)
}

// fillPathWord replaces the filename portion of the word after the last
// whitespace, preserving the directory portion (everything up to and
// including the last "/").
func (m *appModel) fillPathWord(replacement string) {
	v := m.ta.Value()
	lastSpace := strings.LastIndexAny(v, " \n")
	wordStart := lastSpace + 1
	word := v[wordStart:]
	slashIdx := strings.LastIndex(word, "/")
	if slashIdx < 0 {
		return
	}
	dirPart := word[:slashIdx+1] // includes trailing "/"
	m.ta.SetValue(v[:wordStart] + dirPart + replacement)
	m.ta.CursorEnd()
	m.syncInputHeight()
}

// longestCommonPrefix finds the shared prefix of all candidates.
func longestCommonPrefix(in []string) string {
	if len(in) == 0 {
		return ""
	}
	p := in[0]
	for _, s := range in[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

// ── Reverse-i-search ──────────────────────────────────────────────────────────

// doReverseSearch finds the most recent history entry containing the current
// search query and loads it into the textarea.
func (m *appModel) doReverseSearch() {
	if m.searchQuery == "" {
		m.searchMatch = -1
		m.ta.Reset()
		m.syncInputHeight()
		return
	}
	for i := len(m.history) - 1; i >= 0; i-- {
		if strings.Contains(m.history[i], m.searchQuery) {
			m.searchMatch = i
			m.ta.SetValue(m.history[i])
			m.ta.CursorEnd()
			m.syncInputHeight()
			return
		}
	}
	// Nothing matches: clear the stale match so the prompt renders the
	// failing state instead of showing the previous hit as if current.
	m.searchMatch = -1
}

// cycleReverseSearch moves to the next OLDER match on Ctrl+R. When no older
// entry matches, the current match and its row stay put (readline behavior).
func (m *appModel) cycleReverseSearch() {
	for i := m.searchMatch - 1; i >= 0; i-- {
		if strings.Contains(m.history[i], m.searchQuery) {
			m.searchMatch = i
			m.ta.SetValue(m.history[i])
			m.ta.CursorEnd()
			m.syncInputHeight()
			return
		}
	}
}

// ── @-file inclusion ──────────────────────────────────────────────────────────

// fileRefExts lists the relative-path extensions eligible for @file inclusion.
// Note .env is deliberately excluded: auto-inlining secrets into a prompt is a
// footgun.
var fileRefExts = []string{
	"go", "py", "js", "ts", "md", "txt", "yaml", "yml", "json", "sql", "sh",
	"html", "css", "toml", "rs", "c", "cpp", "h", "rb", "java", "kt", "swift",
	"php", "vue", "jsx", "tsx",
	"mk", "make", "proto", "xml", "log",
	"ini", "conf", "mod", "sum",
}

// fileRefBasenames lists extension-less basenames eligible for inclusion —
// common project files whose identity is the whole name.
var fileRefBasenames = map[string]bool{
	"Makefile":   true,
	"Dockerfile": true,
	"go.mod":     true,
	"go.sum":     true,
	"Rakefile":   true,
	"Justfile":   true,
}

// maxFileRefBytes caps a single expanded file's content at ~100KB with a
// [truncated] marker, so an @-reference can't blow the context window or the
// prompt echo.
const maxFileRefBytes = 100 * 1024

// fileRefPattern matches @path tokens in user input. Captures absolute paths,
// home-relative paths, and relative paths that look like an includable file
// (a whitelisted extension or a known extension-less basename). Stops at
// whitespace.
var fileRefPattern = regexp.MustCompile(`@(/[\w./-]+|~/[\w./-]+|[\w][\w./-]*)`)

// isFileRefPath reports whether a relative path is eligible for inclusion.
func isFileRefPath(path string) bool {
	if fileRefBasenames[filepath.Base(path)] {
		return true
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	for _, e := range fileRefExts {
		if ext == e {
			return true
		}
	}
	return false
}

// expandFileRefs replaces @path references with the file's content wrapped in
// code fences. Files that don't exist, can't be read, or aren't on the
// includable list (relative paths only) are left as-is.
func expandFileRefs(input string) string {
	return fileRefPattern.ReplaceAllStringFunc(input, func(match string) string {
		path := match[1:] // strip the leading @
		if filepath.Base(path) == ".env" {
			return match
		}
		if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "~/") && !isFileRefPath(path) {
			return match // relative path not on the includable list
		}
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				path = home + path[1:]
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return match // leave the @path as-is if unreadable
		}
		content := strings.TrimRight(string(data), "\n")
		lang := strings.TrimPrefix(filepath.Ext(path), ".")
		if len(content) > maxFileRefBytes {
			// Back up to a rune boundary so the cut can't split a UTF-8
			// sequence and poison the prompt with invalid bytes.
			cut := maxFileRefBytes
			for cut > 0 && !utf8.RuneStart(content[cut]) {
				cut--
			}
			content = content[:cut] + "\n[truncated]"
		}
		return fmt.Sprintf("\n```%s title=%s\n%s\n```", lang, filepath.Base(path), content)
	})
}

// openModelPicker arms an arrow-navigable model menu (rendered by pickerView).
// openProviderPicker arms the first level of the /model menu: pick a provider.
func (m *appModel) openProviderPicker() tea.Cmd {
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  no backend configured"))
	}
	available, err := backendModels(context.Background(), m.sess.backend)
	if err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ list models: "+err.Error()))
	}
	if len(available) == 0 {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  no models available"))
	}

	// Backends without a local provider config (e.g. hand-built test models)
	// expose a flat model list and intentionally do not require the local
	// provider registry. Present that list directly.
	if m.sess.local == nil || m.sess.cfg == nil || len(m.sess.cfg.Providers) == 0 {
		sort.Strings(available)
		m.pickMode = "model"
		m.pickProvider = ""
		m.pickItems = m.pickItems[:0]
		m.pickSel = 0
		for i, alias := range available {
			m.pickItems = append(m.pickItems, pickItem{id: alias, label: alias})
			if alias == m.sess.modelAlias {
				m.pickSel = i
			}
		}
		return nil
	}
	curProvider := ""
	if r, err := llm.NewRegistry(m.sess.cfg).ResolveModel(m.sess.modelAlias); err == nil {
		curProvider = r.ProviderName
	}
	m.pickMode = "provider"
	m.pickProvider = ""
	m.pickItems = m.pickItems[:0]
	m.pickSel = 0
	for _, p := range m.sess.cfg.Providers {
		aliases := modelsForProvider(available, p.Name)
		if len(aliases) == 0 {
			continue
		}
		m.pickItems = append(m.pickItems, pickItem{id: p.Name, label: fmt.Sprintf("%-22s (%d models)", p.Name, len(aliases))})
		if p.Name == curProvider {
			m.pickSel = len(m.pickItems) - 1 // start on the current model's provider
		}
	}
	if len(m.pickItems) == 0 {
		m.pickMode = ""
		return tea.Printf("\n%s\n", sty(cWarn).Render("  no configured providers match backend models"))
	}
	return nil
}

// openModelsForProvider arms the second level: pick a model within a provider.
func (m *appModel) openModelsForProvider(provider string) tea.Cmd {
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  no backend configured"))
	}
	available, err := backendModels(context.Background(), m.sess.backend)
	if err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ list models: "+err.Error()))
	}
	aliases := modelsForProvider(available, provider)
	if len(aliases) == 0 {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  no models for "+provider))
	}
	sort.Strings(aliases)
	m.pickMode = "model"
	m.pickProvider = provider
	m.pickItems = m.pickItems[:0]
	m.pickSel = 0
	for i, a := range aliases {
		m.pickItems = append(m.pickItems, pickItem{id: a, label: a})
		if a == m.sess.modelAlias {
			m.pickSel = i // start on the current model if it's in this provider
		}
	}
	return nil
}

func modelsForProvider(models []string, provider string) []string {
	prefix := provider + "/"
	aliases := make([]string, 0, len(models))
	for _, model := range models {
		if strings.HasPrefix(model, prefix) {
			aliases = append(aliases, strings.TrimPrefix(model, prefix))
		}
	}
	return aliases
}

// openSessionPicker arms an arrow-navigable list of recent threads, grouped by
// workspace: sessions created in the current directory come first, and sessions
// from other workspaces follow tagged with their origin — so a conversation
// never masquerades as belonging to this directory. Legacy threads with an
// unknown workspace are tagged "?".
func (m *appModel) openSessionPicker() tea.Cmd {
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ no backend configured"))
	}
	threads, err := m.sess.backend.ListThreads()
	if err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}
	if len(threads) == 0 {
		return tea.Printf("\n%s\n", sty(cMuted).Render("  no past sessions"))
	}
	m.pickMode = "session"
	m.pickItems = m.pickItems[:0]
	m.pickSel = 0
	// Cap each group so sessions from other workspaces always stay visible
	// even when the current directory has many.
	const hereLimit, elsewhereLimit = 20, 10
	var here, elsewhere []pickItem
	for _, t := range threads {
		title := "untitled"
		if strings.TrimSpace(t.Title) != "" {
			title = t.Title
		}
		updated := time.Unix(t.UpdatedAt, 0).Format("Jan 2 15:04")
		item := pickItem{
			id:    t.ID,
			label: fmt.Sprintf("%-8s  %-40s  %s", shortID(t.ID), clip(title, 40), updated),
		}
		// Empty workspace metadata means "unknown", not "the same place".
		// This matters for legacy threads and older Core servers where both sides
		// may be empty: presenting those threads as local would be misleading.
		if t.Workspace != "" && t.Workspace == m.sess.workspace {
			here = append(here, item)
		} else {
			tag := t.Workspace
			if tag == "" {
				tag = "?"
			}
			item.tag = clip(tag, 24)
			elsewhere = append(elsewhere, item)
		}
	}
	dropped := 0
	if len(here) > hereLimit {
		dropped += len(here) - hereLimit
		here = here[:hereLimit]
	}
	if len(elsewhere) > elsewhereLimit {
		dropped += len(elsewhere) - elsewhereLimit
		elsewhere = elsewhere[:elsewhereLimit]
	}
	m.pickTruncated = dropped
	m.pickItems = append(m.pickItems, here...)
	m.pickItems = append(m.pickItems, elsewhere...)
	for i, it := range m.pickItems {
		if it.id == m.sess.threadID {
			m.pickSel = i
			break
		}
	}
	return nil
}

// workspaceMismatchWarning returns a warning when a thread's recorded
// workspace differs from the session's current one, or "" when they match (or
// the thread's workspace is unknown). Resuming across directories keeps the
// message history but silently rebinds the tools — the caller must surface it.
func workspaceMismatchWarning(threadWS, currentWS string) string {
	if threadWS == "" || threadWS == currentWS {
		return ""
	}
	return fmt.Sprintf("thread workspace: %s — tools will run in %s", threadWS, currentWS)
}

// resolveThreadID returns the full thread id for an id or an unambiguous
// case-insensitive prefix of a known thread. An empty string means no match;
// ambiguous returns the candidates so the caller can list them.
func (m *appModel) resolveThreadID(idOrPrefix string) (string, []string, error) {
	if m.sess.backend == nil {
		return "", nil, fmt.Errorf("no backend configured")
	}
	if idOrPrefix == "" {
		return "", nil, nil
	}
	threads, err := m.sess.backend.ListThreads()
	if err != nil {
		return "", nil, err
	}
	var matches []string
	for _, t := range threads {
		if strings.EqualFold(t.ID, idOrPrefix) {
			return t.ID, nil, nil // exact (case-insensitive) hit wins
		}
		if len(idOrPrefix) < len(t.ID) && strings.EqualFold(t.ID[:len(idOrPrefix)], idOrPrefix) {
			matches = append(matches, t.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil, nil
	}
	return "", matches, nil
}

// resumeSession switches to threadID, redraws the screen, and reprints the last
// exchange of that conversation so resuming shows context instead of nothing.
// A short unique prefix of a known thread id is accepted; an ambiguous prefix
// lists its candidates.
func (m *appModel) resumeSession(threadID string) tea.Cmd {
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ no backend configured"))
	}
	if len(threadID) < 32 {
		full, ambiguous, err := m.resolveThreadID(threadID)
		if err != nil && len(ambiguous) == 0 {
			return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
		}
		if len(ambiguous) > 1 {
			var b strings.Builder
			b.WriteString(fmt.Sprintf("  ⚠ %q matches %d threads:\n", threadID, len(ambiguous)))
			for i, id := range ambiguous {
				if i >= 8 {
					b.WriteString(fmt.Sprintf("    … and %d more\n", len(ambiguous)-i))
					break
				}
				b.WriteString("    " + id + "\n")
			}
			return m.printLine("\n" + sty(cWarn).Render(strings.TrimRight(b.String(), "\n")))
		}
		if full != "" {
			threadID = full
		}
	}
	detail, err := m.sess.backend.GetThread(threadID)
	if err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}

	// Commit the session switch only after the backend confirms the target.
	// A typo or stale ID must leave the active conversation
	// and its draft/context state intact.
	m.sess.threadID = threadID
	m.ctxUsed, m.ctxMax = 0, 0
	m.costSeg = ""
	m.draft = ""
	// Hydrate the sticky task panel from the durable todo list so a resumed
	// thread shows its tasks immediately. A fetch failure simply starts with
	// no panel until the next todo call.
	m.todos = nil
	if tl, ok := m.sess.backend.(backend.TodoLister); ok {
		if items, err := tl.ListTodos(context.Background(), threadID); err == nil {
			m.todos = todoRowsFromMemory(items)
		}
	}
	// Restore the persisted plan-mode lifecycle so /plan state survives
	// restarts: 'planning' re-arms the toggle, 'pending_approval' re-opens
	// the decision menu over the resumed transcript.
	m.sess.planMode = detail.PlanMode == "planning"
	pendingPlan := detail.PlanMode == "pending_approval"

	title := strings.TrimSpace(detail.Title)
	cmds := []tea.Cmd{tea.ClearScreen} // clean redraw before showing the resumed session
	if w := workspaceMismatchWarning(detail.Workspace, m.sess.workspace); w != "" {
		// The message history travels, but this session's tools run in the
		// current workspace — say so instead of silently rebinding.
		cmds = append(cmds, m.printLine("\n"+sty(cWarn).Render("  ⚠ "+w)))
	}
	msgs := detail.Messages
	header := "  ◈ resumed " + shortID(threadID)
	if title != "" {
		header += "  " + title
	}
	cmds = append(cmds, m.printLine("\n"+sty(cAccent).Render(header)))
	if ex := m.renderLastExchange(msgs); ex != "" {
		// ex is printed as-is: its message blocks are pre-wrapped inside
		// renderLastExchange and its divider lines are exactly m.width cells,
		// so it must NOT go through printLine (which would wrap the dividers).
		cmds = append(cmds, tea.Printf("%s", ex))
	}
	if pendingPlan {
		m.lastPlanText = lastAssistantText(msgs)
		cmds = append(cmds, m.printLine("\n"+sty(cWarn).Render("  ⚠ this session has a plan awaiting your decision")))
		m.openPlanDecisionPicker()
	}
	cmds = append(cmds, m.contextCmd())
	return tea.Sequence(cmds...) // ordered: clear → header → exchange → ctx
}

// lastAssistantText returns the content of the most recent assistant message,
// or "" when the thread has none.
func lastAssistantText(msgs []backend.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// renderLastExchange renders the thread's last user→assistant exchange the way
// the live chat does (margin + divider + ◈ label), or "" if there is none.
// Assistant text goes through the same themed Markdown renderer the live path
// uses, so a resumed conversation looks identical to the original stream.
// User text uses the same bold userEchoStyle as the live echo. The returned
// block is hard-wrapped to the terminal width so a long line from an old
// session can't overflow and desync the inline renderer.
func (m *appModel) renderLastExchange(msgs []backend.Message) string {
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return ""
	}
	w := m.width
	if w <= 0 {
		w = 80
	}
	div := sty(cBorder).Render(strings.Repeat("─", w))
	var b strings.Builder
	if c := msgs[lastUser].Content; strings.TrimSpace(c) != "" {
		b.WriteString("\n" + wrapForPrint(userEchoStyle().Render(c), m.printWidth()) + "\n" + div)
	}
	for i := lastUser + 1; i < len(msgs); i++ {
		if msgs[i].Role == "assistant" {
			if c := msgs[i].Content; strings.TrimSpace(c) != "" {
				b.WriteString("\n\n" + sty(cPurple).Bold(true).Render("◈ sandbar") + "\n")
				b.WriteString(renderStoredAssistant(c, m.printWidth()))
				// Track the replayed raw text so /noformat works right after
				// a resume, before any new response arrives.
				m.lastResponseRaw = c
			}
		}
	}
	return b.String()
}

// renderStoredAssistant renders stored assistant Markdown the way the final
// live flush does: through the themed Glamour renderer (unwrapped), wrapped
// at width-contentMargin with whitespace-only breaking, with glamour's
// blank-separator lines collapsed and every content line indented by the
// content margin. Falls back to ANSI-aware plain wrapping (same wrap rules)
// when Glamour yields nothing (empty render or renderer construction
// failure), so resume never drops content.
func renderStoredAssistant(text string, width int) string {
	w := width - contentMargin
	if w < 1 {
		w = 1
	}
	if rendered := renderMarkdown(text); rendered != "" {
		return indentGlamourOutput(wrapForPrint(rendered, w))
	}
	return prefixContentMargin(wrapForPrint(text, w))
}

// movePick shifts the picker cursor by delta, clamped to the list.
func (m *appModel) movePick(delta int) {
	m.pickSel += delta
	if m.pickSel < 0 {
		m.pickSel = 0
	}
	if m.pickSel > len(m.pickItems)-1 {
		m.pickSel = len(m.pickItems) - 1
	}
	if m.pickMode == "theme" {
		m.previewSelectedTheme()
	}
}

// cancelPick closes the menu without acting.
func (m *appModel) cancelPick() tea.Cmd {
	if m.pickMode == "theme" && m.pickOriginalTheme != "" {
		_ = m.installTheme(m.pickOriginalTheme)
	}
	m.pickMode, m.pickItems, m.pickSel = "", nil, 0
	m.pickOriginalTheme = ""
	return tea.Printf("\n%s\n", sty(cMuted).Render("  (cancelled)"))
}

// openPlanDecisionPicker shows the approve/edit/cancel menu after a plan-mode
// turn completes (or when a resumed thread has a plan awaiting a decision).
func (m *appModel) openPlanDecisionPicker() {
	m.pickMode = "plan"
	m.pickItems = []pickItem{
		{id: "approve", label: "approve — the plan executes on your next message"},
		{id: "edit", label: "edit — load the plan into the input and amend it"},
		{id: "cancel", label: "cancel — discard the plan"},
	}
	m.pickSel = 0
}

// decidePlan applies the user's pick from the plan-decision menu.
func (m *appModel) decidePlan(action string) tea.Cmd {
	decide := func(verb string) error {
		decider, ok := m.sess.backend.(backend.PlanDecider)
		if !ok {
			return fmt.Errorf("backend cannot record plan decisions")
		}
		return decider.DecidePlan(context.Background(), m.sess.threadID, verb)
	}
	switch action {
	case "approve":
		if err := decide("approve"); err != nil {
			return m.printLine("\n" + sty(cErr).Render("  ⚠ could not record approval: "+err.Error()))
		}
		return m.printLine("\n" + sty(cAccent).Render("  ◈ plan approved — it executes on your next message") + "\n")
	case "edit":
		// No backend call: the amended message is a normal turn, which clears
		// the pending plan on its own.
		m.ta.SetValue(m.lastPlanText)
		return m.printLine("\n" + sty(cAccent).Render("  ◈ plan loaded into the input — amend it, then send to execute") + "\n")
	default: // cancel
		_ = decide("reject") // best-effort; a discarded plan needs no error surface
		return m.printLine("\n" + sty(cMuted).Render("  (plan discarded)") + "\n")
	}
}

// openEffortPicker replaces typed effort values with a menu, like the model
// and theme pickers. Tropical rides at the bottom as the top tier.
func (m *appModel) openEffortPicker() tea.Cmd {
	m.pickMode = "effort"
	m.pickItems = m.pickItems[:0]
	m.pickSel = 0
	m.pickItems = append(m.pickItems,
		pickItem{id: "default", label: "Default", tag: "provider decides"},
		pickItem{id: "low", label: "Low"},
		pickItem{id: "medium", label: "Medium"},
		pickItem{id: "high", label: "High"},
		pickItem{id: "tropical", label: "TROPICAL", tag: "max effort + heavy subagents"},
	)
	current := "default"
	if m.sess.tropical {
		current = "tropical"
	} else if m.sess.effort != "" {
		current = m.sess.effort
	}
	for i, item := range m.pickItems {
		if item.id == current {
			m.pickSel = i
			break
		}
	}
	return nil
}

// applyEffortChoice acts on a selection from the effort picker. Every choice
// but tropical leaves Tropical mode; tropical forces high effort.
func (m *appModel) applyEffortChoice(id string) tea.Cmd {
	if id == "tropical" {
		return m.setTropical(!m.sess.tropical)
	}
	m.sess.tropical = false
	switch id {
	case "low", "medium", "high":
		m.sess.effort = id
		return m.printLine("\n" + sty(cAccent).Render("  ◈ effort set to "+id+" (applies from the next message)") + "\n")
	default:
		m.sess.effort = ""
		return m.printLine("\n" + sty(cAccent).Render("  ◈ effort reset to provider default") + "\n")
	}
}

// setTropical toggles Tropical mode. Effort is forced to high while on; the
// previous explicit effort choice is forgotten — turning Tropical off resets
// to the provider default, mirroring the effort picker's semantics.
func (m *appModel) setTropical(on bool) tea.Cmd {
	m.sess.tropical = on
	if on {
		m.sess.effort = "high"
		return m.printLine("\n" + tropicalPartyText("  ◈ TROPICAL mode ON — max effort + heavy subagent parallelism (/tropical to exit)") + "\n")
	}
	m.sess.effort = ""
	return m.printLine("\n" + sty(cAccent).Render("  ◈ TROPICAL mode OFF — effort back to provider default") + "\n")
}

// tropicalPartyText renders every letter of s in a different themed color —
// the party look. Rotation is fixed (offset 0); the status-bar chip is the
// animated variant.
func tropicalPartyText(s string) string {
	party := []string{cErr, cWarn, cGreen, cAccent, cPurple, cLavender, cThink, cBright}
	var b strings.Builder
	i := 0
	for _, r := range s {
		if r == ' ' || r == '—' || r == '-' || r == '/' || r == '(' || r == ')' || r == '+' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(sty(party[i%len(party)]).Bold(true).Render(string(r)))
		i++
	}
	return b.String()
}

// selectPick acts on the highlighted item and closes the menu.
func (m *appModel) selectPick() tea.Cmd {
	if m.pickSel < 0 || m.pickSel >= len(m.pickItems) {
		return m.cancelPick()
	}
	mode, chosen := m.pickMode, m.pickItems[m.pickSel]
	m.pickMode, m.pickItems, m.pickSel = "", nil, 0
	switch mode {
	case "provider":
		return m.openModelsForProvider(chosen.id) // drill into the provider's models
	case "model":
		// Store provider-qualified alias so ResolveModel targets the
		// specific host the user picked, not just the first provider
		// that happens to define this alias.
		qualifiedAlias := chosen.id
		if m.pickProvider != "" {
			qualifiedAlias = m.pickProvider + "/" + chosen.id
		}
		m.sess.modelAlias = qualifiedAlias
		m.costSeg = "" // accumulated cost belonged to the previous model
		if cl := contextLengthFor(m.sess.cfg, qualifiedAlias); cl > 0 {
			m.ctxMax = cl // reflect the new window immediately
		}
		// Show the provider-qualified alias: with the same model name served
		// by several hosts, the bare name hides which one is running.
		return tea.Printf("\n%s\n", sty(cAccent).Render("  ◈ model → "+qualifiedAlias))
	case "effort":
		return m.applyEffortChoice(chosen.id)
	case "session":
		return m.resumeSession(chosen.id)
	case "plan":
		return m.decidePlan(chosen.id)
	case "theme":
		m.pickOriginalTheme = ""
		return m.setTheme(chosen.id, true)
	}
	return nil
}

// pickerView renders the active menu as an in-place list with a moving cursor,
// windowed so long lists (e.g. 25 models) scroll around the selection.
func (m appModel) pickerView() string {
	title, escHint := "select a model", "Esc cancel"
	switch m.pickMode {
	case "session":
		// Show the current workspace so the grouping (current-workspace
		// sessions first, others tagged) is self-explanatory.
		title = "resume a session — " + clip(m.sess.workspace, 36)
	case "provider":
		title = "select a provider"
	case "model":
		if m.pickProvider != "" {
			title = "select a model — " + m.pickProvider
			escHint = "Esc back" // back to the provider list
		}
	case "effort":
		title = "select effort"
	case "theme":
		title = "select a theme — live preview"
	case "plan":
		title = "plan ready — what next?"
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	writeLine := func(line string) {
		// Picker content may contain nested ANSI styles and wide Unicode. Clip by
		// display cells so neither the heading nor a long model/session row wraps
		// and desynchronizes Bubble Tea's inline renderer on narrow terminals.
		b.WriteString(ansi.Truncate(line, width, ""))
		b.WriteByte('\n')
	}
	writeLine(sty(cAccent).Render("  ◈ "+title) +
		sty(cMuted).Render("    ↑↓ move · Enter select · "+escHint))

	const window = 10
	start := 0
	if len(m.pickItems) > window {
		start = m.pickSel - window/2
		if start < 0 {
			start = 0
		}
		if start > len(m.pickItems)-window {
			start = len(m.pickItems) - window
		}
	}
	end := start + window
	if end > len(m.pickItems) {
		end = len(m.pickItems)
	}
	if start > 0 {
		// Newline OUTSIDE Render: a trailing \n inside lipgloss pads the empty
		// line to block width, shifting the next row to the right.
		writeLine(sty(cMuted).Render(fmt.Sprintf("    ↑ %d more", start)))
	}
	for i := start; i < end; i++ {
		label := m.pickItems[i].label
		if m.pickItems[i].tag != "" {
			label += "  " + sty(cMuted).Render("· "+m.pickItems[i].tag)
		}
		if i == m.pickSel {
			writeLine(sty(cAccent).Render("  ▸ ") + sty(cBright).Render(label))
		} else {
			writeLine("    " + sty(cMuted).Render(label))
		}
	}
	if end < len(m.pickItems) {
		writeLine(sty(cMuted).Render(fmt.Sprintf("    ↓ %d more", len(m.pickItems)-end)))
	}
	// Sessions dropped by the picker's per-group caps, on top of the window
	// scroll count above: "↓ N more" only counts the unwindowed pickItems.
	if m.pickMode == "session" && m.pickTruncated > 0 {
		writeLine(sty(cMuted).Render(fmt.Sprintf("    … %d older hidden", m.pickTruncated)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *appModel) setTitle(name string) tea.Cmd {
	if m.sess.threadID == "" {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  ⚠ no active session — send a message first"))
	}
	if name == "" {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  ⚠ usage: /title <name>"))
	}
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ no backend configured"))
	}
	if err := m.sess.backend.RenameThread(m.sess.threadID, name); err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}
	return m.printLine("\n" + sty(cAccent).Render("  ◈ title set: "+name) + "\n")
}

func (m *appModel) undoLast() tea.Cmd {
	if m.sess.threadID == "" {
		return tea.Printf("\n%s\n", sty(cMuted).Render("  nothing to undo"))
	}
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ no backend configured"))
	}
	detail, err := m.sess.backend.GetThread(m.sess.threadID)
	if err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}
	lastUserSeq := -1
	for _, mm := range detail.Messages {
		if mm.Role == "user" {
			lastUserSeq = mm.Seq
		}
	}
	if lastUserSeq < 0 {
		return tea.Printf("\n%s\n", sty(cMuted).Render("  nothing to undo"))
	}
	if err := m.sess.backend.UndoThread(m.sess.threadID, lastUserSeq); err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}
	return tea.Batch(
		tea.Printf("\n%s\n", sty(cAccent).Render("  ◈ removed last exchange")),
		m.contextCmd(),
	)
}

func (m *appModel) forkSession() tea.Cmd {
	if m.sess.threadID == "" {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  ⚠ no active session to fork"))
	}
	if m.sess.local == nil || m.sess.local.store == nil {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  ⚠ /fork requires the local session services"))
	}
	newID, err := m.sess.local.store.ForkThread(m.sess.threadID)
	if err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}
	m.sess.threadID = newID
	return tea.Printf("\n%s\n", sty(cAccent).Render("  ◈ forked → "+shortID(newID)+"  (now on the branch)"))
}

// compressNow forces a context compression. Summarization is an LLM call, so it
// runs in a background cmd and reports back via compressDoneMsg.
func (m *appModel) compressNow() tea.Cmd {
	if m.sess.threadID == "" {
		return tea.Printf("\n%s\n", sty(cMuted).Render("  nothing to compress yet"))
	}
	if m.sess.local == nil || m.sess.local.ag == nil {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  ⚠ /compress requires the local session services"))
	}
	tid, model, ag := m.sess.threadID, m.sess.modelAlias, m.sess.local.ag
	// Show which model will be used — compression.model overrides the
	// conversation model when configured.
	compressModel := model
	if m.sess.cfg.Compression.Model != "" {
		compressModel = m.sess.cfg.Compression.Model
	}
	m.compressing = true
	return tea.Batch(
		tea.Printf("\n%s", sty(cWarn).Render("  ⟳ compressing context with "+compressModel+"…")),
		func() tea.Msg {
			res, err := ag.CompressNow(context.Background(), tid, model)
			return compressDoneMsg{res: res, err: err}
		},
	)
}

// renderCompressionLine renders a compression outcome with per-outcome styling.
// It is the shared renderer for both the /compress result and streamed
// compression_end events so the two surfaces can never drift apart.
func renderCompressionLine(c *llm.CompressionEvent) (string, string) {
	// reductionPct is the integer percentage the token count dropped.
	reductionPct := func() int {
		if c.BeforeTokens <= 0 {
			return 0
		}
		return 100 * (c.BeforeTokens - c.AfterTokens) / c.BeforeTokens
	}
	// elapsed suffixes a duration when the operation was timed.
	elapsed := func() string {
		if c.ElapsedMS <= 0 {
			return ""
		}
		return fmt.Sprintf(" · in %s", fmtDurMS(c.ElapsedMS))
	}
	switch c.Outcome {
	case string(agent.CompressionOutcomeCompressed):
		line := fmt.Sprintf("  ◈ LLM summarized: %s → %s", fmtTokF(c.BeforeTokens), fmtTokF(c.AfterTokens))
		if pct := reductionPct(); pct > 0 {
			line += fmt.Sprintf(" (−%d%%)", pct)
		}
		var details []string
		if c.ModelAlias != "" {
			details = append(details, "model "+c.ModelAlias)
		}
		if c.ElapsedMS > 0 {
			details = append(details, "in "+fmtDurMS(c.ElapsedMS))
		}
		if c.CompressedMessageCount > 0 {
			details = append(details, fmt.Sprintf("%d messages", c.CompressedMessageCount))
		}
		if c.PrunedToolOutputs > 0 {
			details = append(details, fmt.Sprintf("%d tool outputs trimmed", c.PrunedToolOutputs))
		}
		if len(details) > 0 {
			line += " (" + strings.Join(details, ", ") + ")"
		}
		if c.TargetTokens > 0 {
			line += fmt.Sprintf(" · target ≤ %d", c.TargetTokens)
			if c.RecentTailTokens > 0 {
				line += fmt.Sprintf(" · raw recent tail %d", c.RecentTailTokens)
				if c.RecentTailTargetTokens > 0 {
					line += fmt.Sprintf(" (floor %d)", c.RecentTailTargetTokens)
				}
			}
		}
		if c.SummaryTotalTokens > 0 {
			line += fmt.Sprintf(" · summarizer %d in · %d out", c.SummaryPromptTokens, c.SummaryCompletionTokens)
		}
		// A non-fatal persistence failure still compresses this turn; show it as
		// a warning suffix rather than silently dropping the record.
		if c.Error != "" {
			line += " — " + c.Error
			return line, cWarn
		}
		return line, cAccent

	case string(agent.CompressionOutcomePruned):
		line := fmt.Sprintf("  ◈ pruned tool outputs: %s → %s", fmtTokF(c.BeforeTokens), fmtTokF(c.AfterTokens))
		if pct := reductionPct(); pct > 0 {
			line += fmt.Sprintf(" (−%d%%)", pct)
		}
		if c.PrunedToolOutputs > 0 {
			line += fmt.Sprintf(" (%d outputs)", c.PrunedToolOutputs)
		}
		line += elapsed()
		return line, cMuted

	case string(agent.CompressionOutcomeFallback):
		if c.AfterTokens >= c.BeforeTokens {
			line := fmt.Sprintf("  ◈ nothing further to compress (%s → %s tokens)", fmtTokF(c.BeforeTokens), fmtTokF(c.AfterTokens))
			if c.FallbackReason != "" {
				line += " — " + c.FallbackReason
			}
			line += elapsed()
			return line, cWarn
		}
		line := fmt.Sprintf("  ◈ truncated to budget: %s → %s", fmtTokF(c.BeforeTokens), fmtTokF(c.AfterTokens))
		if pct := reductionPct(); pct > 0 {
			line += fmt.Sprintf(" (−%d%%)", pct)
		}
		if c.FallbackReason != "" {
			line += " — " + c.FallbackReason
		}
		line += elapsed()
		return line, cWarn

	case string(agent.CompressionOutcomeError):
		line := "  ⚠ compression error"
		if c.FallbackReason != "" {
			line += ": " + c.FallbackReason
		}
		line += elapsed()
		return line, cErr

	case string(agent.CompressionOutcomeNone):
		return "  ◈ nothing to compress (conversation too short)", cMuted

	default:
		status := fmt.Sprintf("  ⟳ message-context estimate compressed: %s → %s", fmtTokF(c.BeforeTokens), fmtTokF(c.AfterTokens))
		if c.TargetTokens > 0 {
			status += fmt.Sprintf(" (target ≤ %d", c.TargetTokens)
			if c.RecentTailTokens > 0 {
				status += fmt.Sprintf("; raw recent tail %d", c.RecentTailTokens)
				if c.RecentTailTargetTokens > 0 {
					status += fmt.Sprintf(" (floor %d)", c.RecentTailTargetTokens)
				}
			}
			status += ")"
		}
		status += elapsed()
		return status, cMuted
	}
}

// compressionEventFromResult adapts the /compress result to the shared event
// shape used by renderCompressionLine.
func compressionEventFromResult(res agent.CompressionResult) *llm.CompressionEvent {
	return &llm.CompressionEvent{
		Outcome:                 string(res.Outcome),
		ModelAlias:              res.SummaryModelAlias,
		ModelID:                 res.SummaryModelID,
		BeforeTokens:            res.BeforeTokens,
		AfterTokens:             res.AfterTokens,
		BudgetTokens:            res.BudgetTokens,
		TargetTokens:            res.TargetTokens,
		RecentTailTargetTokens:  res.RecentTailTargetTokens,
		RecentTailTokens:        res.RecentTailTokens,
		CompressedMessageCount:  res.CompressedCount,
		PrunedToolOutputs:       res.PrunedToolOutputs,
		SummaryPromptTokens:     res.SummaryPromptTokens,
		SummaryCompletionTokens: res.SummaryCompletionTokens,
		SummaryTotalTokens:      res.SummaryTotalTokens,
		SummaryCallCount:        res.SummaryCallCount,
		FallbackUsed:            res.FallbackUsed,
		FallbackReason:          res.FallbackReason,
		Error:                   res.SaveError,
		ElapsedMS:               res.ElapsedMS,
	}
}

// searchMessages runs an FTS5 query against the message store and prints
// matching results with thread titles and snippets.
func (m *appModel) searchMessages(query string) tea.Cmd {
	if query == "" {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  usage: /search <query>"))
	}
	if m.sess.local == nil || m.sess.local.store == nil {
		return tea.Printf("\n%s\n", sty(cWarn).Render("  ⚠ /search requires the local session services"))
	}
	store := m.sess.local.store
	q := query
	return func() tea.Msg {
		results, err := store.SearchMessages(q, 10)
		return searchDoneMsg{query: q, results: results, err: err}
	}
}

func renderSearchResults(msg searchDoneMsg) string {
	if msg.err != nil {
		return "\n" + sty(cErr).Render("  ⚠ search error: "+msg.err.Error()) + "\n"
	}
	if len(msg.results) == 0 {
		return "\n" + sty(cMuted).Render("  no results for \""+msg.query+"\"") + "\n"
	}

	var b strings.Builder
	b.WriteString("\n" + sty(cAccent).Render(fmt.Sprintf("  ◈ %d result(s) for \"%s\"", len(msg.results), msg.query)) + "\n")
	for i, r := range msg.results {
		title := "Untitled"
		if r.ThreadTitle != nil && *r.ThreadTitle != "" {
			title = *r.ThreadTitle
		}
		snippet := r.Snippet
		snippet = truncateRunes(snippet, 120)
		// Clean up FTS5 snippet markers for terminal display.
		snippet = strings.ReplaceAll(snippet, "<b>", "")
		snippet = strings.ReplaceAll(snippet, "</b>", "")
		b.WriteString(fmt.Sprintf("  %s  %s\n    %s\n",
			sty(cLavender).Render(shortID(r.ThreadID)),
			sty(cBright).Render(title),
			sty(cMuted).Render(snippet)))
		if i < len(msg.results)-1 {
			b.WriteString("\n")
		}
	}
	return b.String() + "\n"
}

func truncateRunes(s string, max int) string {
	if max < 0 {
		max = 0
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// shortID trims a thread id to its first 8 characters for display.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func subagentTag(toolCallID string) string {
	if toolCallID == "" {
		return "subagent"
	}
	return "subagent " + shortID(toolCallID)
}

// ── Runner ────────────────────────────────────────────────────────────────────

func runBubbleTea(sess *session) {
	w := termWidth()
	// ansi.Truncate counts display cells, so a long workspace with multi-byte
	// runes can't split a UTF-8 sequence.
	ws := ansi.Truncate(sess.workspace, 38, "…")

	fmt.Println()
	fmt.Println(
		sty(cAccent).Bold(true).Render("◈ sandbar") + sty(cBorder).Render("  │  ") +
			sty(cMuted).Render("model: ") + sty(cPurple).Bold(true).Render(shortModel(sess.modelAlias)) +
			sty(cBorder).Render("  │  ") + sty(cMuted).Render("ws: ") + sty(cMuted).Render(ws),
	)
	fmt.Println(sty(cBorder).Render(strings.Repeat("─", w)))
	fmt.Println(sty(cMuted).Render("  /help  ·  Ctrl+D quit  ·  Ctrl+C stop  ·  Esc interrupt  ·  Ctrl+R search  ·  Ctrl+L redraw  ·  ↑↓ history  ·  Alt+Enter newline  ·  \\\\ then Enter for line break"))
	fmt.Println()

	p := tea.NewProgram(newModel(sess))

	// SIGINT/SIGTERM: quit through the normal Ctrl+D path (cancel the active
	// stream, drain it, save history) instead of dying mid-write.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	progDone := make(chan struct{})
	go func() {
		select {
		case <-sigCtx.Done():
			// v2 idiom: a synthetic Ctrl+D key press reaches the same drain
			// path the real keystroke would take.
			p.Send(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
		case <-progDone:
		}
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	close(progDone)
}

// ── Entry point ───────────────────────────────────────────────────────────────

const compressionJSONContract = "sandbar-compression-json/1"

type compressionCLIRequest struct {
	Contract            string                         `json:"contract"`
	Messages            []openai.ChatCompletionMessage `json:"messages"`
	MaxOutputTokens     *int                           `json:"max_output_tokens"`
	MinimumUsefulTokens *int                           `json:"minimum_useful_tokens"`
	RetryShort          *bool                          `json:"retry_short"`
	TimeoutSeconds      *int                           `json:"timeout_seconds"`
}

type compressionSummaryResultEvent struct {
	Type                    string `json:"type"`
	Contract                string `json:"contract"`
	Content                 string `json:"content"`
	UsagePurpose            string `json:"usage_purpose"`
	ModelAlias              string `json:"model_alias"`
	ModelID                 string `json:"model_id"`
	PromptTokens            int    `json:"prompt_tokens"`
	CompletionTokens        int    `json:"completion_tokens"`
	TotalTokens             int    `json:"total_tokens"`
	LocalSummaryTokens      int    `json:"local_summary_tokens"`
	SummaryCallCount        int    `json:"summary_call_count"`
	SummaryUsageCallCount   int    `json:"summary_usage_call_count"`
	Retried                 bool   `json:"retried"`
	ElapsedMS               int64  `json:"elapsed_ms"`
	PrunedToolOutputs       int    `json:"pruned_tool_outputs"`
	MinimumUsefulTokensUsed int    `json:"minimum_useful_tokens_used"`
}

type compressionErrorEvent struct {
	Type     string `json:"type"`
	Contract string `json:"contract"`
	Content  string `json:"content"`
}

type compressionDoneEvent struct {
	Type     string `json:"type"`
	Contract string `json:"contract"`
}

func main() {
	if handled, exitCode := runAdminCommand(context.Background(), os.Args[1:], os.Stdout, os.Stderr); handled {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}

	var (
		modelFlag            = flag.String("model", "", "Model alias")
		workspaceFlag        = flag.String("workspace", "", "Workspace dir")
		configFlag           = flag.String("config", "", "Config path")
		threadFlag           = flag.String("thread", "", "Resume thread")
		resumeFlag           = flag.String("resume", "", "Resume thread (alias)")
		jsonFlag             = flag.Bool("json", false, "Emit newline-delimited JSON events (for scripting/benchmarking)")
		summarizeContextFlag = flag.Bool("summarize-context", false, "Summarize a JSON message batch from stdin without running an agent turn (requires --json and --model)")
		disableSubagentsFlag = flag.Bool("disable-subagents", false, "Disable delegate_task and resume_task for this local agent run")
		toolsFlag            = flag.String("tools", "", "Restrict this local run to these tools, comma-separated (e.g. file_read,shell_exec); the rest are not advertised. Empty = all tools")
		effortFlag           = flag.String("effort", "", "Reasoning effort for turns in this run: low, medium, or high (empty = provider default)")
		planFlag             = flag.Bool("plan", false, "Plan mode: read-only investigation for this turn; present a plan instead of changing anything")
		themeFlag            = flag.String("theme", "", "CLI theme (overrides SANDBAR_THEME and client config; use --list-themes for IDs)")
		colorFlag            = flag.String("color", "", "Color output: auto, always, or never")
		listThemesFlag       = flag.Bool("list-themes", false, "List CLI theme IDs and exit")
		versionFlag          = flag.Bool("version", false, "Print version and exit")
	)
	flag.CommandLine.Usage = func() { writeRootUsage(flag.CommandLine.Output(), flag.CommandLine) }
	flag.Parse()
	if *versionFlag {
		fmt.Println("sandbar", resolvedVersion())
		return
	}
	if *listThemesFlag {
		fmt.Println(formatThemeList())
		return
	}

	// Standalone summarization deliberately branches before DB-path resolution,
	// client defaults, workspace setup, tool construction, and Agent.Chat. The
	// candidate is always the explicit -model value, never compression.model.
	if *summarizeContextFlag {
		if !*jsonFlag {
			fmt.Fprintln(os.Stderr, "--summarize-context requires --json")
			os.Exit(2)
		}
		cfg, err := loadCoreConfig(*configFlag)
		if err != nil {
			die("%v", err)
		}
		if err := runSummarizeContext(cfg, *modelFlag, flag.Args(), os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}

	clientCfg, err := config.LoadClientConfig()
	if err != nil {
		die("loading client config: %v", err)
	}
	// Appearance precedence is explicit CLI flag, environment override,
	// persisted client preference, then automatic system light/dark selection.
	themeName := preferredTheme(*themeFlag, os.Getenv("SANDBAR_THEME"), clientCfg.Theme)
	colorMode := clientCfg.ColorMode
	if *colorFlag != "" {
		switch strings.ToLower(strings.TrimSpace(*colorFlag)) {
		case config.ColorModeAuto, config.ColorModeAlways, config.ColorModeNever:
			colorMode = strings.ToLower(strings.TrimSpace(*colorFlag))
		default:
			die("invalid --color %q (want auto, always, or never)", *colorFlag)
		}
	}
	darkBackground := detectDarkBackground()
	cliStyles, err := newStyleSet(themeName, colorMode, darkBackground, os.Stdout)
	if err != nil {
		die("loading CLI theme: %v", err)
	}
	setActiveStyleSet(cliStyles)

	toolAllowlist, err := parseToolAllowlist(*toolsFlag)
	if err != nil {
		die("%v", err)
	}
	effortAllow := strings.TrimSpace(*effortFlag)
	switch effortAllow {
	case "", "low", "medium", "high", "tropical":
	default:
		die("--effort must be low, medium, high, or tropical (got %q)", effortAllow)
	}
	modelAlias := *modelFlag
	if modelAlias == "" && clientCfg.DefaultModel != "" {
		modelAlias = clientCfg.DefaultModel
	}

	resumeID := *threadFlag
	if resumeID == "" {
		resumeID = *resumeFlag
	}

	message := strings.Join(flag.Args(), " ")
	piped := isPiped()

	cfg, err := loadCoreConfig(*configFlag)
	if err != nil {
		die("%v", err)
	}
	dbPath := config.DBPath(cfg.Database)
	workspace := cfg.Workspace
	if wd, getwdErr := os.Getwd(); getwdErr == nil && *workspaceFlag == "" {
		workspace = wd
	}
	if *workspaceFlag != "" {
		workspace = *workspaceFlag
	}
	runtime, err := openLocalRuntime(cfg, dbPath, workspace, localRuntimeOptions{
		DisableSubagents: *disableSubagentsFlag,
		AllowedTools:     toolAllowlist,
	})
	if err != nil {
		die("%v", err)
	}
	defer runtime.close()
	modelAlias, err = chooseInitialModel(context.Background(), runtime.backend, modelAlias)
	if err != nil {
		die("loading models: %v", err)
	}
	if resumeID != "" {
		if err := printWorkspaceWarning(runtime.backend, resumeID, workspace, os.Stderr); err != nil {
			die("loading thread: %v", err)
		}
	}
	if message != "" || piped {
		if err := runOneShot(runtime.backend, runtime.cfg, modelAlias, resumeID, message, effortAllow, *planFlag, *jsonFlag, clientCfg.ShowCost, os.Stdin, piped, os.Stdout, os.Stderr); err != nil {
			die("turn: %v", err)
		}
		return
	}

	runtime.agent.WarmupCompressionModel()
	runtime.agent.StartKeepalive()
	runBubbleTea(&session{
		cfg:            runtime.cfg,
		clientCfg:      clientCfg,
		backend:        runtime.backend,
		local:          &localServices{store: runtime.store, ag: runtime.agent},
		modelAlias:     modelAlias,
		themeName:      cliStyles.RequestedTheme(),
		colorMode:      cliStyles.ColorMode(),
		darkBackground: cliStyles.DarkBackground(),
		styles:         cliStyles,
		workspace:      workspace,
		threadID:       resumeID,
		effort:         strings.TrimPrefix(effortAllow, "tropical"),
		tropical:       effortAllow == "tropical",
		planMode:       *planFlag,
	})
}

func runSummarizeContext(cfg *config.Config, modelAlias string, positional []string, input io.Reader, output io.Writer) error {
	enc := json.NewEncoder(output)
	enc.SetEscapeHTML(false)
	emitError := func(err error) error {
		if encodeErr := enc.Encode(compressionErrorEvent{
			Type:     "error",
			Contract: compressionJSONContract,
			Content:  err.Error(),
		}); encodeErr != nil {
			return fmt.Errorf("%v; encode compression error event: %w", err, encodeErr)
		}
		return err
	}

	if cfg == nil {
		return emitError(errors.New("configuration is required"))
	}
	if strings.TrimSpace(modelAlias) == "" {
		return emitError(errors.New("--model is required for --summarize-context"))
	}
	if len(positional) != 0 {
		return emitError(errors.New("--summarize-context accepts its request only on stdin"))
	}

	dec := json.NewDecoder(input)
	dec.DisallowUnknownFields()
	var request compressionCLIRequest
	if err := dec.Decode(&request); err != nil {
		return emitError(fmt.Errorf("decode compression request: %w", err))
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values are not allowed")
		}
		return emitError(fmt.Errorf("compression request must contain exactly one JSON object: %w", err))
	}
	if request.Contract != compressionJSONContract {
		return emitError(fmt.Errorf("unsupported compression contract %q", request.Contract))
	}
	if len(request.Messages) == 0 {
		return emitError(errors.New("messages must contain at least one message"))
	}
	for i, message := range request.Messages {
		switch message.Role {
		case openai.ChatMessageRoleSystem, openai.ChatMessageRoleUser, openai.ChatMessageRoleAssistant, openai.ChatMessageRoleTool:
		default:
			return emitError(fmt.Errorf("messages[%d].role %q is not supported", i, message.Role))
		}
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens <= 0 {
		return emitError(errors.New("max_output_tokens is required and must be positive"))
	}
	if request.MinimumUsefulTokens == nil || *request.MinimumUsefulTokens < 0 {
		return emitError(errors.New("minimum_useful_tokens is required and must not be negative"))
	}
	if request.RetryShort == nil {
		return emitError(errors.New("retry_short is required"))
	}
	if request.TimeoutSeconds == nil || *request.TimeoutSeconds <= 0 {
		return emitError(errors.New("timeout_seconds is required and must be positive"))
	}

	resolved, err := llm.NewRegistry(cfg).ResolveModel(modelAlias)
	if err != nil {
		return emitError(fmt.Errorf("resolve summary model: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*request.TimeoutSeconds)*time.Second)
	defer cancel()
	started := time.Now()
	result, err := agent.SummarizeContext(ctx, modelAlias, resolved, agent.ContextSummaryRequest{
		Messages:            request.Messages,
		MaxOutputTokens:     *request.MaxOutputTokens,
		MinimumUsefulTokens: *request.MinimumUsefulTokens,
		RetryShort:          *request.RetryShort,
	})
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		return emitError(err)
	}

	if err := enc.Encode(compressionSummaryResultEvent{
		Type:                    "summary_result",
		Contract:                compressionJSONContract,
		Content:                 result.Summary,
		UsagePurpose:            "compression",
		ModelAlias:              result.ModelAlias,
		ModelID:                 result.ModelID,
		PromptTokens:            result.PromptTokens,
		CompletionTokens:        result.CompletionTokens,
		TotalTokens:             result.TotalTokens,
		LocalSummaryTokens:      result.LocalSummaryTokens,
		SummaryCallCount:        result.CallCount,
		SummaryUsageCallCount:   result.UsageCallCount,
		Retried:                 result.Retried,
		ElapsedMS:               elapsed,
		PrunedToolOutputs:       result.PrunedToolOutputs,
		MinimumUsefulTokensUsed: result.MinimumUsefulTokensUsed,
	}); err != nil {
		return fmt.Errorf("encode summary result: %w", err)
	}
	if err := enc.Encode(compressionDoneEvent{Type: "done", Contract: compressionJSONContract}); err != nil {
		return fmt.Errorf("encode summary done event: %w", err)
	}
	return nil
}

// ── One-shot mode ─────────────────────────────────────────────────────────────

func runOneShot(be backend.Backend, cfg *config.Config, modelAlias, resumeID, message, effort string, plan bool, jsonOut bool, showCost bool, input io.Reader, piped bool, output, errorOutput io.Writer) error {
	if be == nil {
		return errors.New("CLI backend is not configured")
	}
	if piped {
		data, err := io.ReadAll(input)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		if len(data) > 0 {
			message += "\n\n```\n" + string(data) + "\n```"
		}
	}
	if message == "" {
		return errors.New("message is required")
	}
	enc := json.NewEncoder(output)
	enc.SetEscapeHTML(false) // keep <, >, & literal in tool args/output
	subagentDraftShown := make(map[string]bool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = agent.WithRequestSource(ctx, "cli")
	events, err := be.SendMessage(ctx, resumeID, modelAlias, message, effort, plan)
	if err != nil {
		return err
	}
	if events == nil {
		return unexpectedStreamEndError()
	}
	var streamErr error
	terminal := false
	// Cost rollup: price this run's usage events against the embedded catalog;
	// disabled, unknown, or free models keep the footer hidden.
	var costs *costTracker
	if showCost {
		costs = newCostTracker(cfg, modelAlias)
	}
streamLoop:
	for ev := range events {
		if jsonOut {
			// Canonical JSONL: one StreamEvent per line (tokens, tool calls,
			// tool results, usage, compression, done). This is the structured
			// surface scripting consumers read.
			if err := enc.Encode(ev); err != nil {
				return fmt.Errorf("encode stream event: %w", err)
			}
			switch ev.Type {
			case "done":
				terminal = true
				break streamLoop
			case "error":
				streamErr = streamEventError(ev.Content)
				terminal = true
				break streamLoop
			}
			continue
		}
		switch ev.Type {
		case "token":
			fmt.Fprint(output, ev.Content)
		case "subagent_tool_call":
			preview := toolPreview(ev.ToolName, ev.Arguments)
			if preview != "" {
				preview = ": " + preview
			}
			fmt.Fprintf(errorOutput, "[%s tool: %s%s]\n", subagentTag(ev.ToolCallID), ev.ToolName, preview)
		case "subagent_tool_result":
			if preview := toolResultPreview(ev.ToolName, ev.Content); preview != "" {
				fmt.Fprintf(errorOutput, "[%s result: %s]\n", subagentTag(ev.ToolCallID), preview)
			}
		case "subagent_token":
			if !subagentDraftShown[ev.ToolCallID] {
				subagentDraftShown[ev.ToolCallID] = true
				fmt.Fprintf(errorOutput, "[%s: drafting response…]\n", subagentTag(ev.ToolCallID))
			}
		case "subagent_done":
			delete(subagentDraftShown, ev.ToolCallID)
		case "subagent_error":
			delete(subagentDraftShown, ev.ToolCallID)
			fmt.Fprintf(errorOutput, "[%s error: %s]\n", subagentTag(ev.ToolCallID), oneline(ev.Content))
		case "compression_start":
			fmt.Fprintln(errorOutput, "[compression: starting context compression]")
		case "compression_end":
			if ev.Compression != nil {
				fmt.Fprintf(errorOutput, "[compression: estimated message tokens %d → %d", ev.Compression.BeforeTokens, ev.Compression.AfterTokens)
				if ev.Compression.TargetTokens > 0 {
					fmt.Fprintf(errorOutput, "; target ≤ %d; raw recent tail %d (floor %d)", ev.Compression.TargetTokens, ev.Compression.RecentTailTokens, ev.Compression.RecentTailTargetTokens)
				}
				fmt.Fprintln(errorOutput, "]")
			}
		case "compression_error":
			if ev.Compression != nil {
				reason := ev.Compression.FallbackReason
				if ev.Compression.Error != "" {
					reason = ev.Compression.Error
				}
				fmt.Fprintf(errorOutput, "[compression error: %s]\n", reason)
			}
		case "auxiliary_usage":
			if ev.UsagePurpose == "compression" {
				fmt.Fprintf(errorOutput, "[summarizer usage: %d in · %d out]\n", ev.PromptTokens, ev.CompletionTokens)
			}
		case "usage":
			costs.add(ev)
		case "error":
			streamErr = streamEventError(ev.Content)
			terminal = true
			break streamLoop
		case "done":
			terminal = true
			break streamLoop
		}
	}
	if !terminal {
		streamErr = unexpectedStreamEndError()
	}
	if !jsonOut {
		fmt.Fprintln(output)
	}
	if seg := costs.segment(); seg != "" {
		fmt.Fprintln(errorOutput, "cost "+strings.TrimPrefix(seg, "⚑ "))
	}
	return streamErr
}

func streamEventError(content string) error {
	if content = strings.TrimSpace(content); content != "" {
		return errors.New(content)
	}
	return errors.New("stream reported an error")
}

func unexpectedStreamEndError() error {
	return fmt.Errorf("stream ended before canonical done event: %w", io.ErrUnexpectedEOF)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func buildFooter(sent, firstTok, done time.Time, pTok, cTok int) string {
	parts := []string{fmtTime(sent), fmtDur(done.Sub(sent))}
	if !firstTok.IsZero() {
		parts = append(parts, "ttft "+fmtDur(firstTok.Sub(sent)))
	}
	if cTok > 0 {
		tps := ""
		if s := done.Sub(sent).Seconds(); s > 0 {
			tps = fmt.Sprintf("  %.0f tk/s", float64(cTok)/s)
		}
		parts = append(parts, fmt.Sprintf("%d out · %d in%s", cTok, pTok, tps))
	}
	return sty(cMuted).Italic(true).Render(strings.Join(parts, "  ·  "))
}

func shortModel(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func fmtTok(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.0fK", float64(n)/1000)
}

// fmtTokF is fmtTok with one decimal place at thousands scale, used by the
// compression summaries where a precise before→after delta reads better
// (e.g. "128.0K → 42.3K").
func fmtTokF(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fK", float64(n)/1000)
}

// fmtDurMS renders a millisecond duration for compression outcomes: sub-second
// stays in milliseconds, under a minute keeps one decimal second ("3.2s"),
// longer durations fall back to m/s like fmtDur.
func fmtDurMS(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%ds", ms/60000, (ms%60000)/1000)
}

func fmtTime(t time.Time) string {
	h, m, a := t.Hour(), t.Minute(), "am"
	if h >= 12 {
		a = "pm"
	}
	h = h % 12
	if h == 0 {
		h = 12
	}
	return fmt.Sprintf("%d:%02d%s", h, m, a)
}

func fmtDur(d time.Duration) string {
	if d.Milliseconds() < 1000 {
		return fmt.Sprintf("%.1fs", float64(d.Milliseconds())/1000)
	}
	if d.Seconds() < 60 {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Seconds())/60, int(d.Seconds())%60)
}

// oneline collapses all runs of whitespace (including newlines) to single
// spaces, so multi-line tool input/output renders as one tidy line.
func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// wrapForPrint wraps every line of s to at most width display cells, breaking
// lines only at whitespace (see cliui.WrapPrint). Words — hyphenated
// compounds, file paths, URLs, CLI flags — move to the next line whole; a
// single token wider than width is hard-broken mid-token so it can never
// overflow. ANSI escape sequences pass through intact and don't count toward
// the width.
//
// Every tea.Printf payload must go through this: BubbleTea's inline renderer
// truncates View lines to the terminal width but NOT queued print lines, so a
// printed line wider than the terminal wraps physically while the renderer's
// line count desyncs — the source of ghost dividers and leaked prompt rows.
func wrapForPrint(s string, width int) string {
	return cliui.WrapPrint(s, width)
}

// printWidth is the maximum cell width for a printed (tea.Printf) line: two
// cells short of the terminal so a line can never touch the right edge and
// physically wrap. Falls back to 80 when no WindowSizeMsg has been seen.
func (m appModel) printWidth() int {
	w := m.width
	if w <= 0 {
		w = 80
	}
	w -= 2
	if w < 1 {
		w = 1
	}
	return w
}

// printLine queues s for printing, hard-wrapped to printWidth. Payloads taller
// than the frame-safe row budget are split into sequential chunks: Bubble Tea's
// insertAbove misplaces the frame when a single print exceeds the visible
// screen (observed as the frame floating above the payload tail with blank
// gaps), so no one print may be taller than the terminal.
func (m appModel) printLine(s string) tea.Cmd {
	wrapped := wrapForPrint(s, m.printWidth())
	const minChunkRows = 10
	budget := m.height - 8
	if budget < minChunkRows {
		budget = minChunkRows
	}
	lines := strings.Split(wrapped, "\n")
	if len(lines) <= budget {
		return tea.Printf("%s", wrapped)
	}
	var chunks []tea.Cmd
	for start := 0; start < len(lines); start += budget {
		end := min(start+budget, len(lines))
		chunks = append(chunks, tea.Printf("%s", strings.Join(lines[start:end], "\n")))
	}
	return tea.Sequence(chunks...)
}

// thinkingView renders the animated in-frame reasoning indicator: "Thinking"
// with a themed color gradient that rotates every spinner tick (120ms), plus
// stepping trailing dots, so the row visibly moves in every color profile —
// including no-color terminals, where the dots alone animate. Transient by
// design: nothing is printed to the transcript, and the row disappears the
// moment a non-thinking event (token, tool, completion) arrives.
func (m appModel) thinkingView() string {
	if !m.streaming || !m.thinking {
		return ""
	}
	cycle := []string{cThink, cLavender, cAccent, cPurple}
	var b strings.Builder
	b.WriteString(sty(cThink).Italic(true).Render("⟳") + " ")
	for i, r := range "Thinking" {
		b.WriteString(sty(cycle[(i+m.spinIdx)%len(cycle)]).Render(string(r)))
	}
	for i := m.spinIdx%3 + 1; i > 0; i-- {
		b.WriteString(sty(cThink).Render("."))
	}
	return b.String()
}

// liveBlockView renders the in-frame streaming block: the assistant label
// atop the latest glamour rendering of the accumulating response. It returns
// "" when no live block is showing (not streaming, a tool turn, or no text
// yet). Living inside the frame is what keeps progressive rendering correct:
// the cursed renderer repaints frame content by cell diff, with no reliance
// on where a previous print left the terminal cursor.
func (m appModel) liveBlockView() string {
	if !m.streaming || m.hadToolTurn {
		return ""
	}
	if !m.liveLabel && m.liveRendered == "" {
		return ""
	}
	var b strings.Builder
	if m.liveLabel {
		b.WriteString(sty(cPurple).Bold(true).Render("◈ sandbar"))
	}
	if m.liveRendered != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.liveRendered)
	}
	return b.String()
}

// refreshLiveRender re-renders responseBuf into the live-block cache. Called
// from the spinner tick, the same cadence (and the same one glamour render
// per tick) the old in-place flush paid.
func (m *appModel) refreshLiveRender() {
	if !m.liveDirty && m.liveRendered != "" {
		return
	}
	m.liveRendered = renderStoredAssistant(string(m.responseBuf), m.printWidth())
	m.liveDirty = false
}

// commitResponseBody builds the transcript block for a completed pure-text
// turn: the label plus the final glamour rendering, exactly as
// renderLastExchange shapes a resumed exchange. It clears the live-block
// state; the caller owns the actual print.
func (m *appModel) commitResponseBody() string {
	text := string(m.responseBuf)
	label := m.liveLabel
	m.liveLabel = false
	m.liveRendered = ""
	m.liveDirty = false
	if strings.TrimSpace(text) == "" {
		return ""
	}
	var b strings.Builder
	if label {
		b.WriteString("\n\n" + sty(cPurple).Bold(true).Render("◈ sandbar") + "\n")
	}
	b.WriteString(renderStoredAssistant(text, m.printWidth()))
	return b.String()
}

// printResponse commits the streamed assistant text to the transcript: one
// printLine of the label plus the final glamour rendering, in the same shape
// renderLastExchange uses when replaying a stored thread. The progressive
// rendering lived inside the frame, so committing clears the live block and
// the frame simply shrinks — nothing printed ever needs replacing. Returns
// nil when the turn produced no text.
func (m *appModel) printResponse() tea.Cmd {
	text := string(m.responseBuf)
	label := m.liveLabel
	m.liveLabel = false
	m.liveRendered = ""
	m.liveDirty = false
	if text == "" {
		return nil
	}
	var b strings.Builder
	if label {
		b.WriteString("\n\n" + sty(cPurple).Bold(true).Render("◈ sandbar") + "\n")
	}
	b.WriteString(renderStoredAssistant(text, m.printWidth()))
	return m.printLine(b.String())
}

// clip truncates s to at most n display cells, appending an ellipsis when
// shortened. ANSI-aware (ansi.Truncate), so wide glyphs can't overflow a line
// budget the way a rune count can.
func clip(s string, n int) string {
	if n < 1 {
		return "…"
	}
	return ansi.Truncate(s, n, "…")
}

// primaryArg maps a tool name to the argument most worth previewing.
var primaryArg = map[string]string{
	"shell_exec":     "command",
	"file_read":      "path",
	"file_write":     "path",
	"file_append":    "path",
	"file_patch":     "path",
	"search_files":   "pattern",
	"web_search":     "query",
	"web_fetch":      "url",
	"git":            "action",
	"job":            "action",
	"delegate_task":  "goal",
	"image_generate": "prompt",
	"vision_analyze": "image_path",
}

// toolPreview returns a one-line preview of a tool call's primary argument
// (e.g. the command for shell_exec, the path for file_read), parsed from the
// raw JSON arguments. Returns "" when no sensible preview is available.
func toolPreview(toolName string, rawArgs json.RawMessage) string {
	if len(rawArgs) == 0 {
		return ""
	}
	var args map[string]interface{}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ""
	}
	key := primaryArg[toolName]
	if key == "" {
		for _, k := range []string{"command", "path", "pattern", "query", "url", "goal", "action", "prompt", "name"} {
			if _, ok := args[k]; ok {
				key = k
				break
			}
		}
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	return clip(oneline(fmt.Sprintf("%v", v)), 80)
}

// toolResultPreview turns a raw tool result into a compact one-line preview for
// the TUI. Shell results are formatted "Exit code: N\nStdout:\n…" for the model;
// here we surface the real stdout (or a failure note) instead of the exit-code
// wrapper. Returns "" to render nothing (e.g. a successful command with no
// output). Diff output from file edits is handled separately by renderDiff.
func toolResultPreview(toolName, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if preview, ok := supervisedJobResultPreview(toolName, raw); ok {
		return clip(preview, 160)
	}
	if strings.HasPrefix(raw, "Exit code:") {
		return shellResultPreview(raw)
	}
	return clip(oneline(raw), 160)
}

// isDiffOutput reports whether a tool result looks like a unified diff emitted
// by file_write/file_append/file_patch (a "── action path … ──" summary header
// or standard ---/+++/@@ markers).
func isDiffOutput(raw string) bool {
	s := strings.TrimRight(raw, "\n")
	if strings.HasPrefix(s, "── ") {
		return true
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "+++ ") {
			return true
		}
	}
	return false
}

// maxDiffPreviewLines caps how many diff lines the TUI renders inline so a huge
// change doesn't drown the transcript.
const maxDiffPreviewLines = 60

// renderDiff renders a unified-diff tool result as a multi-line, colorized,
// 4-space-indented block for the TUI. Returns "" if there is nothing to show.
func renderDiff(raw string, width int) string {
	raw = strings.TrimRight(raw, "\n")
	if raw == "" {
		return ""
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxDiffPreviewLines {
		note := fmt.Sprintf("… (%d more lines)", len(lines)-maxDiffPreviewLines+1)
		kept := make([]string, 0, maxDiffPreviewLines)
		kept = append(kept, lines[:maxDiffPreviewLines-1]...)
		kept = append(kept, note)
		lines = kept
	}
	// Budget for content on each line: 4-space indent + a small right margin.
	budget := width - 6
	if budget < 20 {
		budget = 20
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("    ")
		b.WriteString(styleDiffLine(line, budget))
	}
	return b.String()
}

// styleDiffLine applies per-line coloring for a unified-diff line, clipped to
// the available width. Prefix conventions match standard `git diff`.
func styleDiffLine(line string, budget int) string {
	switch {
	case strings.HasPrefix(line, "── "):
		return sty(cAccent).Bold(true).Render(clip(line, budget))
	case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
		return sty(cMuted).Render(clip(line, budget))
	case strings.HasPrefix(line, "@@"):
		return sty(cLavender).Render(clip(line, budget))
	case strings.HasPrefix(line, "+"):
		return sty(cGreen).Render(clip(line, budget))
	case strings.HasPrefix(line, "-"):
		return sty(cErr).Render(clip(line, budget))
	default:
		return sty(cMuted).Render(clip(line, budget))
	}
}

func shellResultPreview(raw string) string {
	var code int
	first := raw
	if i := strings.IndexByte(raw, '\n'); i >= 0 {
		first = raw[:i]
	}
	fmt.Sscanf(first, "Exit code: %d", &code)

	stdout := oneline(sectionBody(raw, "Stdout:"))
	stderr := oneline(sectionBody(raw, "Stderr:"))

	if code != 0 {
		msg := stderr
		if msg == "" {
			msg = stdout
		}
		if msg == "" {
			return fmt.Sprintf("exit %d", code)
		}
		return clip(fmt.Sprintf("exit %d — %s", code, msg), 160)
	}
	// Success: show output if any; stay silent when the command printed nothing.
	return clip(stdout, 160)
}

// sectionBody returns the text following header up to the next "\nStderr:"
// section (or end of string), trimmed.
func sectionBody(raw, header string) string {
	i := strings.Index(raw, header)
	if i < 0 {
		return ""
	}
	body := raw[i+len(header):]
	if j := strings.Index(body, "\nStderr:"); j >= 0 {
		body = body[:j]
	}
	return strings.TrimSpace(body)
}

func stripANSI(s string) string {
	var o strings.Builder
	esc := false
	for _, r := range s {
		if r == '\033' {
			esc = true
			continue
		}
		if esc {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				esc = false
			}
			continue
		}
		o.WriteRune(r)
	}
	return o.String()
}

// loadCoreConfig resolves and loads the core config, falling back to a
// zero-config synthesized from $OPENAI_API_KEY when nothing is found. The
// env fallback prints a one-line notice and seeds the commented template for
// the next boot.
func loadCoreConfig(explicit string) (*config.Config, error) {
	configPath, err := config.Resolve(explicit)
	if err == nil {
		return config.Load(configPath)
	}
	if cfg, ok := config.DefaultFromEnv(); ok {
		fmt.Fprintln(os.Stderr, "sandbar: no config found — running zero-config from environment variables; create ~/.config/sandbar/config.yaml (see config.yaml.example) for full setup")
		config.WriteDefaultConfigTemplate()
		return cfg, nil
	}
	return nil, fmt.Errorf("%v\nhint: set $OPENAI_API_KEY for a zero-config boot with OpenAI defaults", err)
}

// historyPath returns the CLI input history location, honoring
// $XDG_CONFIG_HOME the same way config/resolve.go's search paths do.
func historyPath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "sandbar", "history")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "." // no home: relative path, like config/client.go's fallback
	}
	return filepath.Join(home, ".config", "sandbar", "history")
}

// historyEntry is the JSONL record persisted per history item. Multi-line
// inputs round-trip because the newlines live inside the JSON string, not as
// record separators.
type historyEntry struct {
	Text string `json:"text"`
}

// maxHistoryEntries caps the stored history; the oldest entries are dropped.
const maxHistoryEntries = 2000

// parseHistory decodes the history file: JSONL entries first, falling back to
// legacy raw lines (pre-JSONL files stored one entry per physical line, which
// split multi-line inputs on reload). Slash commands are skipped.
func parseHistory(b []byte) []string {
	var hist []string
	for _, l := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		var e historyEntry
		if err := json.Unmarshal([]byte(trimmed), &e); err == nil && e.Text != "" {
			trimmed = e.Text
		}
		if !strings.HasPrefix(trimmed, "/") {
			hist = append(hist, trimmed)
		}
	}
	return hist
}

func saveHistory(hist []string) {
	if len(hist) > maxHistoryEntries {
		hist = hist[len(hist)-maxHistoryEntries:] // drop oldest
	}
	histPath := historyPath()
	os.MkdirAll(filepath.Dir(histPath), 0700)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, h := range hist {
		if err := enc.Encode(historyEntry{Text: h}); err != nil {
			return // a broken entry must not corrupt the rest
		}
	}
	os.WriteFile(histPath, b.Bytes(), 0644)
}

func die(f string, a ...interface{}) { fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...); os.Exit(1) }
func isPiped() bool                  { fi, _ := os.Stdin.Stat(); return (fi.Mode() & os.ModeCharDevice) == 0 }
