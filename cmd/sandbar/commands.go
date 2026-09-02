package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// slashRequirement identifies the optional service a command needs beyond the
// common backend.Backend contract. Keeping this metadata beside the handler
// lets dispatch, help, and autocomplete agree about availability.
type slashRequirement uint8

const (
	slashBackend slashRequirement = iota
	slashLocalStore
	slashLocalAgent
)

type slashInvocation struct {
	raw  string
	args []string
	tail string
}

type slashHandler func(*appModel, slashInvocation) tea.Cmd

// slashCommand is the single command definition used for dispatch, aliases,
// help, autocomplete, and backend capability checks.
type slashCommand struct {
	name        string
	aliases     []string
	desc        string
	requirement slashRequirement
	run         slashHandler
}

var slashCommands []slashCommand

func init() {
	slashCommands = []slashCommand{
		{name: "/model", desc: "switch model (menu)", run: func(m *appModel, _ slashInvocation) tea.Cmd { return m.openProviderPicker() }},
		{name: "/effort", desc: "set reasoning effort (menu: default | low | medium | high | tropical)", run: runEffortCommand},
		{name: "/tropical", desc: "toggle TROPICAL mode — max effort + heavy subagent parallelism", run: runTropicalCommand},
		{name: "/plan", desc: "toggle plan mode (read-only turns that produce a plan)", run: runPlanCommand},
		{name: "/theme", desc: "switch CLI theme (menu or id)", run: runThemeCommand},
		{name: "/sessions", desc: "list & resume past sessions", run: func(m *appModel, _ slashInvocation) tea.Cmd { return m.openSessionPicker() }},
		{name: "/resume", desc: "resume a session by id or unique prefix", run: runResumeCommand},
		{name: "/new", desc: "start a fresh thread", run: runNewCommand},
		{name: "/delete", desc: "delete the current thread (two-step: /delete confirm)", run: runDeleteCommand},
		{name: "/title", desc: "set the current session's title", run: func(m *appModel, in slashInvocation) tea.Cmd { return m.setTitle(in.tail) }},
		{name: "/fork", aliases: []string{"/branch"}, desc: "branch the current session", requirement: slashLocalStore, run: func(m *appModel, _ slashInvocation) tea.Cmd { return m.forkSession() }},
		{name: "/compress", aliases: []string{"/compact"}, desc: "compress context now", requirement: slashLocalAgent, run: func(m *appModel, _ slashInvocation) tea.Cmd { return m.compressNow() }},
		{name: "/undo", desc: "remove the last exchange", run: func(m *appModel, _ slashInvocation) tea.Cmd { return m.undoLast() }},
		{name: "/search", desc: "search past conversations", requirement: slashLocalStore, run: func(m *appModel, in slashInvocation) tea.Cmd { return m.searchMessages(in.tail) }},
		{name: "/clear", desc: "clear the screen + start fresh", run: runClearCommand},
		{name: "/noformat", desc: "re-print the last response as raw text", run: runNoformatCommand},
		{name: "/redraw", desc: "repaint (recover from render drift)", run: func(_ *appModel, _ slashInvocation) tea.Cmd { return tea.ClearScreen }},
		{name: "/help", aliases: []string{"/?"}, desc: "command reference", run: runHelpCommand},
		{name: "/quit", aliases: []string{"/q", "/exit"}, desc: "exit", run: runQuitCommand},
	}
}

func runThemeCommand(m *appModel, in slashInvocation) tea.Cmd {
	if len(in.args) == 0 {
		return m.openThemePicker()
	}
	if in.args[0] == "list" {
		return m.printLine("\n" + formatThemeList() + "\n")
	}
	return m.setTheme(in.args[0], true)
}

func runEffortCommand(m *appModel, in slashInvocation) tea.Cmd {
	usage := "usage: /effort low | medium | high | tropical | default"
	if len(in.args) == 0 {
		return m.openEffortPicker()
	}
	switch in.args[0] {
	case "low", "medium", "high":
		m.sess.tropical = false
		m.sess.effort = in.args[0]
		return m.printLine("\n" + sty(cAccent).Render("  ◈ effort set to "+in.args[0]+" (applies from the next message)") + "\n")
	case "tropical":
		return m.setTropical(!m.sess.tropical)
	case "default", "off", "none":
		m.sess.tropical = false
		m.sess.effort = ""
		return m.printLine("\n" + sty(cAccent).Render("  ◈ effort reset to provider default") + "\n")
	default:
		return m.printLine("\n  unknown effort " + in.args[0] + "\n  " + usage + "\n")
	}
}

func runTropicalCommand(m *appModel, _ slashInvocation) tea.Cmd {
	return m.setTropical(!m.sess.tropical)
}

func runPlanCommand(m *appModel, _ slashInvocation) tea.Cmd {
	m.sess.planMode = !m.sess.planMode
	if m.sess.planMode {
		return m.printLine("\n" + sty(cAccent).Render("  ◈ plan mode ON — the next turn is read-only and ends with a plan you can approve, edit, or cancel (/plan again to exit)") + "\n")
	}
	return m.printLine("\n" + sty(cAccent).Render("  ◈ plan mode OFF — turns execute normally") + "\n")
}

func runResumeCommand(m *appModel, in slashInvocation) tea.Cmd {
	if len(in.args) > 0 {
		return m.resumeSession(in.args[0])
	}
	return m.openSessionPicker()
}

func runNewCommand(m *appModel, _ slashInvocation) tea.Cmd {
	m.sess.threadID = ""
	m.ctxUsed, m.ctxMax = 0, 0
	m.costSeg = ""
	m.todos = nil
	return tea.Printf("\n%s\n", sty(cAccent).Render("  ◈ new thread"))
}

// runDeleteCommand implements the two-step /delete: the first invocation
// explains the confirmation step; "/delete confirm" performs the deletion.
func runDeleteCommand(m *appModel, in slashInvocation) tea.Cmd {
	if m.sess.threadID == "" {
		return tea.Printf("\n%s\n", sty(cMuted).Render("  no active thread — nothing to delete"))
	}
	if len(in.args) == 0 || in.args[0] != "confirm" {
		return m.printLine("\n" + sty(cWarn).Render("  ⚠ this deletes the whole thread permanently") +
			"\n  " + sty(cAccent).Render("run /delete confirm to delete this thread") + "\n")
	}
	if m.sess.backend == nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ no backend configured"))
	}
	if err := m.sess.backend.DeleteThread(m.sess.threadID); err != nil {
		return tea.Printf("\n%s\n", sty(cErr).Render("  ⚠ "+err.Error()))
	}
	deleted := shortID(m.sess.threadID)
	// Reset to the same clean state /new leaves behind, then offer the
	// session picker so the next conversation is one keystroke away.
	m.sess.threadID = ""
	m.ctxUsed, m.ctxMax = 0, 0
	m.costSeg = ""
	m.draft = ""
	m.todos = nil
	cmds := []tea.Cmd{
		m.printLine("\n" + sty(cAccent).Render("  ◈ thread "+deleted+" deleted") + "\n"),
		m.openSessionPicker(),
	}
	return tea.Sequence(cmds...)
}

func runClearCommand(m *appModel, _ slashInvocation) tea.Cmd {
	m.sess.threadID = ""
	m.ctxUsed, m.ctxMax = 0, 0
	m.costSeg = ""
	m.draft = ""
	m.todos = nil
	return tea.ClearScreen
}

// runNoformatCommand re-prints the previous response completely unformatted:
// raw markdown source, no styling, no content margin, no blank-line
// collapsing. Only hard wrapping at printWidth remains — that keeps long
// lines from physically wrapping and desyncing the inline renderer (terminal
// physics, not formatting); normal prose is unaffected by it.
func runNoformatCommand(m *appModel, _ slashInvocation) tea.Cmd {
	if strings.TrimSpace(m.lastResponseRaw) == "" {
		return m.printLine("\n" + sty(cMuted).Render("  no previous response in this session") + "\n")
	}
	return tea.Sequence(
		m.printLine("\n"+sty(cMuted).Render("◈ previous response, unformatted:")+"\n"),
		tea.Printf("%s\n", wrapForPrint(m.lastResponseRaw, m.printWidth())),
	)
}

func runQuitCommand(m *appModel, _ slashInvocation) tea.Cmd {
	saveHistory(m.history)
	return tea.Quit
}

func runHelpCommand(m *appModel, _ slashInvocation) tea.Cmd {
	return tea.Printf("\n%s", m.helpText())
}

func (c slashCommand) matches(name string) bool {
	if c.name == name {
		return true
	}
	for _, alias := range c.aliases {
		if alias == name {
			return true
		}
	}
	return false
}

// matchesPrefix reports whether the partial input p is a prefix of the command
// name or of any of its aliases.
func (c slashCommand) matchesPrefix(p string) bool {
	if strings.HasPrefix(c.name, p) {
		return true
	}
	for _, alias := range c.aliases {
		if strings.HasPrefix(alias, p) {
			return true
		}
	}
	return false
}

func (c slashCommand) available(m *appModel) bool {
	if c.requirement == slashBackend {
		// These commands are part of every Sandbar frontend. Individual handlers
		// still report a missing backend defensively for hand-built test models.
		return true
	}
	if m == nil || m.sess == nil || m.sess.local == nil {
		return false
	}
	switch c.requirement {
	case slashLocalStore:
		return m.sess.local.store != nil
	case slashLocalAgent:
		return m.sess.local.ag != nil
	default:
		return false
	}
}

func findSlashCommand(name string) (*slashCommand, bool) {
	for i := range slashCommands {
		if slashCommands[i].matches(name) {
			return &slashCommands[i], true
		}
	}
	return nil, false
}

func parseSlashInvocation(input string) (string, slashInvocation, bool) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", slashInvocation{}, false
	}
	name := parts[0]
	return name, slashInvocation{
		raw:  input,
		args: parts[1:],
		tail: strings.TrimSpace(strings.TrimPrefix(input, name)),
	}, true
}

func (m *appModel) slashCmd(input string) tea.Cmd {
	name, invocation, ok := parseSlashInvocation(input)
	if !ok {
		return nil
	}
	command, ok := findSlashCommand(name)
	if !ok {
		// Unknown commands fall through to prompt templates: "/name args"
		// expands a matching template (persona/templates.go) and submits it
		// as the user message. Registered commands keep precedence.
		if cmd := m.runTemplateCommand(name, invocation); cmd != nil {
			return cmd
		}
		return tea.Printf("\n%s\n", sty(cWarn).Render(fmt.Sprintf("  ⚠ unknown command %q — /help for list", name)))
	}
	if !command.available(m) {
		return tea.Printf("\n%s\n", sty(cWarn).Render(fmt.Sprintf("  ⚠ %s requires the local session services", command.name)))
	}
	return command.run(m, invocation)
}

// commandLabel renders a command plus its aliases for the help list, e.g.
// "/quit" → "/quit, /q, /exit".
func commandLabel(c slashCommand) string {
	if len(c.aliases) == 0 {
		return c.name
	}
	return c.name + ", " + strings.Join(c.aliases, ", ")
}

func (m appModel) helpText() string {
	var b strings.Builder
	b.WriteString(sty(cAccent).Render("  ◈ commands") + "\n")
	for _, command := range slashCommands {
		if !command.available(&m) {
			continue
		}
		b.WriteString(fmt.Sprintf("  %s  %s\n", sty(cLavender).Render(fmt.Sprintf("%-26s", commandLabel(command))), sty(cMuted).Render(command.desc)))
	}
	b.WriteString("\n" + sty(cAccent).Render("  ◈ keys") + "\n")
	for _, k := range [][2]string{
		{"Ctrl+R", "reverse-search history"},
		{"Esc", "interrupt turn / dismiss popup"},
		{"! prefix", "shell escape (! <command>)"},
		{"@path", "include file in message"},
		{"Tab", "complete path/command"},
		{"↑↓", "history"},
		{"Alt+Enter, \\ then Enter", "newline"},
		{"Ctrl+L", "clear screen"},
		{"Ctrl+C", "stop turn / quit (twice)"},
		{"Ctrl+D", "quit"},
	} {
		b.WriteString(fmt.Sprintf("  %s  %s\n", sty(cLavender).Render(fmt.Sprintf("%-26s", k[0])), sty(cMuted).Render(k[1])))
	}
	return b.String()
}
