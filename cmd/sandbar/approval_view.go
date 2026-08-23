package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"sandbar/internal/tools"
)

const (
	approvalResourceLimit = 240
	approvalArgumentLimit = 120
	approvalSummaryLimit  = 600
	approvalMaxArguments  = 8
	approvalScanLimit     = 4096
)

type pendingApproval struct {
	request tools.ApprovalRequest
	reply   chan<- tools.ApprovalDecision
}

func (m *appModel) resolveCurrentApproval(policy tools.ApprovalPolicy, reason string) {
	if len(m.approvals) == 0 {
		return
	}
	pending := m.approvals[0]
	m.approvals = m.approvals[1:]
	select {
	case pending.reply <- tools.ApprovalDecision{
		RequestID: pending.request.ID,
		Policy:    policy,
		Source:    "cli",
		Reason:    reason,
		Prompted:  true,
		DecidedAt: time.Now().UTC(),
	}:
	default:
		// The owning turn was cancelled between rendering and the keystroke.
	}
}

func (m *appModel) clearApprovals(reason string) {
	for len(m.approvals) > 0 {
		m.resolveCurrentApproval(tools.PolicyDeny, reason)
	}
}

func denyApprovalItem(item streamItem, reason string) {
	if item.approvalReq == nil || item.approvalReply == nil {
		return
	}
	select {
	case item.approvalReply <- tools.ApprovalDecision{
		RequestID: item.approvalReq.ID,
		Policy:    tools.PolicyDeny,
		Source:    "cli",
		Reason:    reason,
		Prompted:  true,
		DecidedAt: time.Now().UTC(),
	}:
	default:
	}
}

func (m appModel) approvalPromptView() string {
	if len(m.approvals) == 0 {
		return ""
	}
	request := m.approvals[0].request
	styles := m.styles
	if styles == nil {
		styles = currentStyles()
	}
	width := m.width
	if width <= 0 {
		width = 80
	}

	title := styles.Style(cWarn).Bold(true).Render(" approval required ")
	toolName := sanitizeApprovalText(request.Tool, approvalArgumentLimit)
	action := sanitizeApprovalText(request.Action, approvalArgumentLimit)
	operation := toolName
	if action != "" && action != toolName {
		operation += " · " + action
	}
	lines := []string{
		title,
		styles.Style(cBright).Render(operation) + styles.Style(cMuted).Render("  ["+sanitizeApprovalText(string(request.Tier), 24)+"]"),
	}
	if resource := sanitizeApprovalText(request.Resource, approvalResourceLimit); resource != "" {
		lines = append(lines, styles.Style(cMuted).Render(resource))
	}
	if summary := approvalArgumentsSummary(request); summary != "" {
		lines = append(lines, styles.Style(cMuted).Render(summary))
	}
	if queued := len(m.approvals) - 1; queued > 0 {
		lines = append(lines, styles.Style(cMuted).Render(fmt.Sprintf("%d more approval request(s) queued", queued)))
	}
	lines = append(lines,
		styles.Style(cGreen).Bold(true).Render("y")+styles.Style(cMuted).Render(" allow  ")+
			styles.Style(cErr).Bold(true).Render("n")+styles.Style(cMuted).Render(" deny"),
	)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "")
	}
	return strings.Join(lines, "\n")
}

func approvalArgumentsSummary(request tools.ApprovalRequest) string {
	if len(request.Arguments) == 0 {
		return ""
	}
	// Content bodies can be very large and may contain credentials. Prefer the
	// semantic resource above, then show only key names plus compact scalar
	// values; redact conventional secret fields.
	keys := make([]string, 0, len(request.Arguments))
	for key := range request.Arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > approvalMaxArguments {
		keys = keys[:approvalMaxArguments]
	}

	safe := make(map[string]interface{}, len(keys)+1)
	safeKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		value := request.Arguments[key]
		normalizedKey := sanitizeApprovalText(key, approvalScanLimit)
		safeKey := clip(normalizedKey, 48)
		if safeKey == "" {
			safeKey = "argument"
		}
		safeKeys = append(safeKeys, safeKey)
		// Classify the rendered key so escape/control bytes cannot split a
		// sensitive word (for example, "to<CSI>ken") and bypass redaction.
		lower := strings.ToLower(normalizedKey)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "key") || lower == "content" {
			safe[safeKey] = "[redacted]"
			continue
		}
		switch value := value.(type) {
		case string:
			safe[safeKey] = sanitizeApprovalText(value, approvalArgumentLimit)
		case float64, bool, int:
			safe[safeKey] = value
		default:
			safe[safeKey] = fmt.Sprintf("[%T]", value)
		}
	}
	for {
		if omitted := len(request.Arguments) - len(safeKeys); omitted > 0 {
			safe["…"] = fmt.Sprintf("[%d more]", omitted)
		} else {
			delete(safe, "…")
		}
		encoded, err := json.Marshal(safe)
		if err != nil {
			return ""
		}
		if len([]rune(string(encoded))) <= approvalSummaryLimit || len(safeKeys) == 0 {
			return string(encoded)
		}
		last := len(safeKeys) - 1
		delete(safe, safeKeys[last])
		safeKeys = safeKeys[:last]
	}
}

// sanitizeApprovalText makes untrusted tool metadata safe to render in a
// terminal. ANSI sequences are removed first, then remaining control/format
// characters are discarded and whitespace is collapsed to a single space.
// The scan and rendered result are both bounded so an approval cannot flood
// the prompt while the user is deciding whether to run it.
func sanitizeApprovalText(value string, limit int) string {
	if len(value) > approvalScanLimit {
		value = value[:approvalScanLimit]
	}
	value = ansi.Strip(value)

	var clean strings.Builder
	clean.Grow(min(len(value), approvalScanLimit))
	pendingSpace := false
	for _, r := range value {
		if unicode.IsSpace(r) {
			pendingSpace = clean.Len() > 0
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if pendingSpace {
			clean.WriteByte(' ')
			pendingSpace = false
		}
		clean.WriteRune(r)
	}
	return clip(clean.String(), limit)
}
