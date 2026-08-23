package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestSteeringEnqueueRequiresActiveTurn(t *testing.T) {
	q := newSteeringQueues()
	if err := q.EnqueueUserMessage("t1", "hello"); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("enqueue before turn = %v, want ErrNoActiveTurn", err)
	}
	q.beginTurn("t1")
	if err := q.EnqueueUserMessage("t1", "hello"); err != nil {
		t.Fatalf("enqueue during turn: %v", err)
	}
	q.endTurn("t1")
	if err := q.EnqueueUserMessage("t1", "again"); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("enqueue after turn = %v, want ErrNoActiveTurn", err)
	}
}

func TestSteeringEnqueueRejectsEmpty(t *testing.T) {
	q := newSteeringQueues()
	q.beginTurn("t1")
	if err := q.EnqueueUserMessage("t1", "   "); err == nil {
		t.Fatal("empty message should be rejected")
	}
}

func TestSteeringEnqueueCap(t *testing.T) {
	q := newSteeringQueues()
	q.beginTurn("t1")
	for i := 0; i < steeringQueueCap; i++ {
		if err := q.EnqueueUserMessage("t1", "msg"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := q.EnqueueUserMessage("t1", "overflow"); !errors.Is(err, ErrSteeringQueueFull) {
		t.Fatalf("overflow enqueue = %v, want ErrSteeringQueueFull", err)
	}
}

func TestSteeringDrainClearsQueueKeepsActive(t *testing.T) {
	q := newSteeringQueues()
	q.beginTurn("t1")
	_ = q.EnqueueUserMessage("t1", "a")
	_ = q.EnqueueUserMessage("t1", "b")
	got := q.drain("t1")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("drain = %v, want [a b]", got)
	}
	if len(q.drain("t1")) != 0 {
		t.Fatal("second drain should be empty")
	}
	if !q.active["t1"] {
		t.Fatal("drain must not clear the active flag")
	}
}

func TestSteeringEndTurnDrainsAndClearsActive(t *testing.T) {
	q := newSteeringQueues()
	q.beginTurn("t1")
	_ = q.EnqueueUserMessage("t1", "a")
	got := q.endTurn("t1")
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("endTurn = %v, want [a]", got)
	}
	if q.active["t1"] {
		t.Fatal("endTurn must clear the active flag")
	}
}

func TestSteeringWrapperStability(t *testing.T) {
	// The wrapper is byte-stable: frontends rely on it being a fixed prefix and
	// tests pin its exact text so an accidental change is caught here.
	const want = "[The user sent this message while you were working. It takes priority and supersedes earlier instructions where they conflict. Address it as you continue the current turn.]\n\n"
	if steeringWrapper != want {
		t.Fatalf("steeringWrapper changed:\n got %q\nwant %q", steeringWrapper, want)
	}
}

func TestSteeringInjectionPassesProviderValidation(t *testing.T) {
	msgs := []indexedMessage{
		{Seq: 0, Synthetic: true, Kind: "system", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "sys"}},
		{Seq: 1, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "task"}},
		{Seq: 2, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "", ToolCalls: []openai.ToolCall{{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "file_read", Arguments: `{"path":"x"}`}}}}},
		{Seq: 3, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_1", Content: "output"}},
		// A steering message injected at the tool boundary, after a sealed group.
		{Seq: 4, Kind: "thread_message", Msg: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: steeringWrapper + "do this now"}},
	}
	raw := toRawMessages(msgs)
	if err := validateProviderPayload(raw); err != nil {
		t.Fatalf("steering injection failed provider validation: %v", err)
	}
	sanitized := sanitizeProviderMessages(raw)
	if len(sanitized) != len(raw) {
		t.Fatalf("sanitize changed message count: got %d want %d", len(sanitized), len(raw))
	}
	last := sanitized[len(sanitized)-1]
	if last.Role != openai.ChatMessageRoleUser || last.Content != steeringWrapper+"do this now" {
		t.Fatalf("injected user message altered: %+v", last)
	}
}

func TestRegisterTurnCancelAndInterrupt(t *testing.T) {
	a := New(nil, nil, nil, nil)
	if err := a.InterruptThread("none"); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("interrupt with no turn = %v, want ErrNoActiveTurn", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unregister := a.RegisterTurnCancel("t1", cancel)
	if err := a.InterruptThread("t1"); err != nil {
		t.Fatalf("interrupt registered turn: %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("interrupt did not cancel the registered context")
	}
	unregister()
	if err := a.InterruptThread("t1"); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("interrupt after unregister = %v, want ErrNoActiveTurn", err)
	}
}
