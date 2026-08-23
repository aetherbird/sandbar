package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
)

func TestChatPlanTurnMarksPendingApproval(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"the plan\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	a, store, cleanup := setupTestAgent(t, false)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	chat := func(req Request) string {
		t.Helper()
		threadID, err := a.Chat(context.Background(), req, func(llm.StreamEvent) error { return nil })
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		return threadID
	}
	planModeOf := func(threadID string) string {
		t.Helper()
		thread, err := store.GetThread(threadID)
		if err != nil {
			t.Fatalf("get thread: %v", err)
		}
		return thread.PlanMode
	}

	// A plan turn leaves the thread awaiting the user's decision.
	threadID := chat(Request{ModelAlias: "test-model", UserMessage: "plan it", PlanOnly: true})
	if got := planModeOf(threadID); got != memory.PlanModePendingApproval {
		t.Fatalf("after plan turn: plan_mode = %q, want %q", got, memory.PlanModePendingApproval)
	}

	// Approval marks the thread for the one-shot execution directive.
	if err := a.ApprovePlan(threadID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := planModeOf(threadID); got != memory.PlanModeApproved {
		t.Fatalf("after approve: plan_mode = %q, want %q", got, memory.PlanModeApproved)
	}

	// The next (normal) turn consumes the approval: buildMessages clears it.
	chat(Request{ThreadID: threadID, ModelAlias: "test-model", UserMessage: "go"})
	if got := planModeOf(threadID); got != memory.PlanModeOff {
		t.Fatalf("after execution turn: plan_mode = %q, want off", got)
	}

	// A normal turn abandons an unfinished plan decision.
	chat(Request{ThreadID: threadID, ModelAlias: "test-model", UserMessage: "plan again", PlanOnly: true})
	chat(Request{ThreadID: threadID, ModelAlias: "test-model", UserMessage: "never mind"})
	if got := planModeOf(threadID); got != memory.PlanModeOff {
		t.Fatalf("after abandoning normal turn: plan_mode = %q, want off", got)
	}
}

func TestApproveRejectPlan(t *testing.T) {
	a := newTestAgentWithFakeSummarizer(t, &fakeSummarizer{})
	thread := seedThreadWithTodos(t, a.store)

	// Nothing pending → approve fails with the sentinel; reject still works.
	if err := a.ApprovePlan(thread.ID); !errors.Is(err, ErrNoPendingPlan) {
		t.Fatalf("approve without pending plan: %v", err)
	}
	if err := a.RejectPlan(thread.ID); err != nil {
		t.Fatalf("reject without pending plan: %v", err)
	}

	if err := a.store.SetThreadPlanMode(thread.ID, memory.PlanModePendingApproval); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := a.ApprovePlan(thread.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Double approve no longer has a pending plan.
	if err := a.ApprovePlan(thread.ID); !errors.Is(err, ErrNoPendingPlan) {
		t.Fatalf("double approve: %v", err)
	}

	if err := a.store.SetThreadPlanMode(thread.ID, memory.PlanModePendingApproval); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := a.RejectPlan(thread.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	loaded, err := a.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if loaded.PlanMode != memory.PlanModeOff {
		t.Fatalf("after reject: plan_mode = %q, want off", loaded.PlanMode)
	}

	// Unknown threads surface the store's not-found error.
	if err := a.ApprovePlan("no-such-thread"); err == nil {
		t.Fatal("approve on missing thread: expected error")
	}
	if err := a.RejectPlan("no-such-thread"); err == nil {
		t.Fatal("reject on missing thread: expected error")
	}
}

func TestBuildMessagesPlanApprovedInjection(t *testing.T) {
	a := newTestAgentWithFakeSummarizer(t, &fakeSummarizer{})
	thread := seedThreadWithTodos(t, a.store)
	_, beforeMsgs, err := a.store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	if err := a.store.SetThreadPlanMode(thread.ID, memory.PlanModeApproved); err != nil {
		t.Fatalf("seed approved: %v", err)
	}

	msgs := mustBuildMessages(t, a, thread.ID, a.cfg.Workspace, "")
	if len(msgs) < 2 || msgs[1].Kind != "plan_approved" || !msgs[1].Synthetic {
		t.Fatalf("expected synthetic plan_approved at index 1: %+v", msgs)
	}
	content := msgs[1].Msg.Content
	if !strings.Contains(content, "The user approved this plan") {
		t.Fatalf("approval directive missing: %q", content)
	}
	// The durable todo list rides along so the model sees the plan's steps.
	if !strings.Contains(content, "draft the plan") {
		t.Fatalf("todo list missing from approval block: %q", content)
	}

	// Injected exactly once: the persisted state was cleared, and the block
	// itself was never written to the store.
	loaded, err := a.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if loaded.PlanMode != memory.PlanModeOff {
		t.Fatalf("approval state not consumed: %q", loaded.PlanMode)
	}
	msgs = mustBuildMessages(t, a, thread.ID, a.cfg.Workspace, "")
	for _, m := range msgs {
		if m.Kind == "plan_approved" || strings.Contains(m.Msg.Content, "The user approved this plan") {
			t.Fatalf("approval block injected twice: %+v", m)
		}
	}
	_, afterMsgs, err := a.store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("reload messages: %v", err)
	}
	if len(afterMsgs) != len(beforeMsgs) {
		t.Fatalf("approval block persisted: before=%d after=%d", len(beforeMsgs), len(afterMsgs))
	}
}

func TestBuildMessagesRejectedPlanInjectsNothing(t *testing.T) {
	a := newTestAgentWithFakeSummarizer(t, &fakeSummarizer{})
	thread := seedThreadWithTodos(t, a.store)
	if err := a.store.SetThreadPlanMode(thread.ID, memory.PlanModePendingApproval); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := a.RejectPlan(thread.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	msgs := mustBuildMessages(t, a, thread.ID, a.cfg.Workspace, "")
	for _, m := range msgs {
		if m.Kind == "plan_approved" || strings.Contains(m.Msg.Content, "The user approved this plan") {
			t.Fatalf("approval block injected after rejection: %+v", m)
		}
	}
}
