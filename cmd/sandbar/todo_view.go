package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"sandbar/internal/memory"
)

// maxTodoPanelRows caps the sticky task panel above the input; longer lists
// collapse into a "+N more" footer row.
const maxTodoPanelRows = 8

// todoRow is one parsed line of the todo tool's "Task list:" output.
type todoRow struct {
	status  string // pending | in_progress | completed | cancelled
	id      string
	content string
}

// parseTodoList parses the todo tool's "Task list:" output into rows. It
// returns nil when the content is not a task list (validation errors,
// "(no items)") or any line deviates from the tool's stable format
// ("  [<icon>] <id> <content>") — callers fall back to the default one-line
// result rendering rather than showing a half-parsed list.
func parseTodoList(content string) []todoRow {
	const header = "Task list:\n"
	if !strings.HasPrefix(content, header) {
		return nil
	}
	body := strings.TrimSuffix(content[len(header):], "\n")
	if body == "" {
		return nil
	}

	var rows []todoRow
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "  [") {
			return nil
		}
		close := strings.Index(line, "] ")
		if close < 0 {
			return nil
		}
		icon := line[len("  ["):close]
		rest := line[close+len("] "):]
		status := ""
		switch icon {
		case " ":
			status = "pending"
		case ">":
			status = "in_progress"
		case "✓":
			status = "completed"
		case "-":
			status = "cancelled"
		default:
			return nil
		}
		id, taskContent, found := strings.Cut(rest, " ")
		if !found || id == "" || taskContent == "" {
			return nil
		}
		rows = append(rows, todoRow{status: status, id: id, content: taskContent})
	}
	return rows
}

// todoRowsFromMemory adapts persisted todos (resume-time TodoLister fetch)
// into panel rows.
func todoRowsFromMemory(items []memory.TodoItem) []todoRow {
	rows := make([]todoRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, todoRow{status: string(item.Status), id: item.ID, content: item.Content})
	}
	return rows
}

// todoDisplayOrder returns rows ranked for panel display: live work first —
// in_progress, then pending — with completed/cancelled history last. Stable
// within each rank. This is presentation-only; m.todos keeps the list's
// canonical position order.
func todoDisplayOrder(rows []todoRow) []todoRow {
	rank := func(r todoRow) int {
		switch r.status {
		case "in_progress":
			return 0
		case "pending":
			return 1
		default:
			return 2
		}
	}
	ordered := make([]todoRow, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i]) < rank(ordered[j]) })
	return ordered
}

// todoPanelView renders the sticky task list docked above the input, updating
// in place on every todo mutation instead of appending the list to scrollback.
// It returns "" when the thread has no tasks, or when every task has reached
// a terminal state (completed/cancelled) and the turn has ended — a fully
// settled list gets out of the way; the transcript keeps the final render.
func (m appModel) todoPanelView() string {
	if len(m.todos) == 0 {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	styles := m.styles
	if styles == nil {
		styles = currentStyles()
	}

	open := 0
	for _, row := range m.todos {
		if row.status == "pending" || row.status == "in_progress" {
			open++
		}
	}
	if open == 0 && !m.streaming {
		return ""
	}
	header := fmt.Sprintf(" tasks %d open · %d total ", open, len(m.todos))
	lines := []string{ansi.Truncate(styles.Style(cAccent).Bold(true).Render(header), width, "")}

	// Long lists collapse into "+N more": rank live work to the top so the
	// visible rows are the open items and the overflow hides settled history,
	// never the work in flight.
	ordered := todoDisplayOrder(m.todos)
	visible := len(ordered)
	if visible > maxTodoPanelRows {
		visible = maxTodoPanelRows
	}
	for _, row := range ordered[:visible] {
		var line string
		switch row.status {
		case "in_progress":
			line = styles.Style(cAccent).Bold(true).Render("  ◐ " + row.content)
		case "completed":
			line = styles.Style(cMuted).Render("  ☑ " + row.content)
		case "cancelled":
			line = styles.Style(cMuted).Render("  ☒ " + row.content)
		default:
			line = styles.Style(cBright).Render("  ☐ " + row.content)
		}
		lines = append(lines, ansi.Truncate(line, width, ""))
	}
	if hidden := len(ordered) - visible; hidden > 0 {
		lines = append(lines, ansi.Truncate(styles.Style(cMuted).Render(fmt.Sprintf("  +%d more", hidden)), width, ""))
	}
	return strings.Join(lines, "\n")
}
