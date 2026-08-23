package main

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/aetherbird/sandbar/internal/tools"
)

func TestApprovalPromptQueuesAndResolvesInOrder(t *testing.T) {
	first := make(chan tools.ApprovalDecision, 1)
	second := make(chan tools.ApprovalDecision, 1)
	m := appModel{
		width: 80, styles: defaultStyleSet(),
		approvals: []pendingApproval{
			{request: tools.ApprovalRequest{ID: "one", Tool: "shell_exec", Tier: tools.TierExec, Resource: "go test ./..."}, reply: first},
			{request: tools.ApprovalRequest{ID: "two", Tool: "file_write", Tier: tools.TierWrite, Resource: "main.go"}, reply: second},
		},
	}
	view := stripANSI(m.approvalPromptView())
	for _, want := range []string{"approval required", "shell_exec", "go test ./...", "1 more approval request"} {
		if !strings.Contains(view, want) {
			t.Fatalf("prompt %q missing %q", view, want)
		}
	}

	m.resolveCurrentApproval(tools.PolicyAllow, "test")
	if decision := <-first; decision.RequestID != "one" || decision.Policy != tools.PolicyAllow {
		t.Fatalf("first decision = %#v", decision)
	}
	if len(m.approvals) != 1 || m.approvals[0].request.ID != "two" {
		t.Fatalf("queue after first = %#v", m.approvals)
	}
	m.resolveCurrentApproval(tools.PolicyDeny, "test")
	if decision := <-second; decision.RequestID != "two" || decision.Policy != tools.PolicyDeny {
		t.Fatalf("second decision = %#v", decision)
	}
}

func TestApprovalSummaryRedactsSensitiveAndContentFields(t *testing.T) {
	request := tools.ApprovalRequest{Arguments: map[string]interface{}{
		"path":          "safe.go",
		"content":       "private body",
		"auth_token":    "secret-token",
		"password":      "hunter2",
		"to\x1b[31mken": "escape-split-secret",
	}}
	summary := approvalArgumentsSummary(request)
	if strings.Contains(summary, "private body") || strings.Contains(summary, "secret-token") || strings.Contains(summary, "hunter2") || strings.Contains(summary, "escape-split-secret") {
		t.Fatalf("approval summary leaked sensitive value: %s", summary)
	}
	if !strings.Contains(summary, "safe.go") || !strings.Contains(summary, "[redacted]") {
		t.Fatalf("approval summary lost safe/redaction context: %s", summary)
	}
}

func TestSanitizeApprovalTextStripsANSIAndControlSequences(t *testing.T) {
	input := "safe\x1b[2J\x1b]8;;https://evil.invalid\a link\x1b]8;;\a\r\nnext\t\x00end\u202e"
	got := sanitizeApprovalText(input, 200)
	if got != "safe link next end" {
		t.Fatalf("sanitized text = %q", got)
	}
	for _, r := range got {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatalf("sanitized text retained unsafe rune %U in %q", r, got)
		}
	}
}

func TestApprovalPromptBoundsUntrustedResourceAndSummary(t *testing.T) {
	arguments := map[string]interface{}{}
	for i := 0; i < approvalMaxArguments+12; i++ {
		arguments[strings.Repeat("k", 80)+string(rune('a'+i))] = strings.Repeat("v", 500)
	}
	request := tools.ApprovalRequest{
		Tool:      "shell_exec\x1b[31m",
		Tier:      tools.TierExec,
		Resource:  strings.Repeat("r", approvalResourceLimit+100) + "\x1b[2J",
		Arguments: arguments,
	}

	resource := sanitizeApprovalText(request.Resource, approvalResourceLimit)
	if got := utf8.RuneCountInString(resource); got > approvalResourceLimit {
		t.Fatalf("resource length = %d, want <= %d", got, approvalResourceLimit)
	}
	summary := approvalArgumentsSummary(request)
	if got := utf8.RuneCountInString(summary); got > approvalSummaryLimit {
		t.Fatalf("summary length = %d, want <= %d", got, approvalSummaryLimit)
	}
	if !strings.Contains(summary, "more") {
		t.Fatalf("summary does not report omitted arguments: %s", summary)
	}
	if strings.Contains(resource, "\x1b") || strings.Contains(summary, "\x1b") {
		t.Fatalf("approval output retained an escape byte: resource=%q summary=%q", resource, summary)
	}
}
