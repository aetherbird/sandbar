package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

type slowFakeRunner struct {
	events  chan SubagentEvent
	spawned chan struct{}
}

func (r *slowFakeRunner) SpawnSubagent(ctx context.Context, goal, contextStr string) (<-chan SubagentEvent, error) {
	r.spawned <- struct{}{}
	return r.events, nil
}

func (r *slowFakeRunner) ResumeSubagent(ctx context.Context, taskID string) (<-chan SubagentEvent, error) {
	return r.events, nil
}

// TestDelegateTaskBackgroundReturnsImmediately pins the fire-and-return
// contract: with background: true the tool result comes back at once (it does
// not wait on the sub-agent's event stream), carries the task ID, and the
// start event reports "background" so frontends can track it.
func TestDelegateTaskBackgroundReturnsImmediately(t *testing.T) {
	runner := &slowFakeRunner{events: make(chan SubagentEvent), spawned: make(chan struct{}, 1)}
	var sinkEvents []SubagentEvent
	ctx := WithEventSink(context.Background(), func(ev SubagentEvent) { sinkEvents = append(sinkEvents, ev) })
	ctx = WithWorkspace(ctx, "/w")
	ctx = WithEffort(ctx, "high")

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := delegateTask(runner, ctx, map[string]interface{}{"goal": "research", "background": true})
		done <- result{out, err}
	}()

	select {
	case <-runner.spawned:
	case <-time.After(2 * time.Second):
		t.Fatal("subagent never spawned")
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("background delegate: %v", r.err)
		}
		if !strings.Contains(r.out, "Delegated in background") || !strings.Contains(r.out, "Task ID") {
			t.Fatalf("result must carry the background contract and task ID:\n%s", r.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background delegate blocked on the sub-agent stream — must return immediately")
	}

	found := false
	for _, ev := range sinkEvents {
		if ev.Type == "start" && ev.Status == "background" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no background start event in sink: %+v", sinkEvents)
	}
}

// TestDelegateTaskBlockingStillWaits pins the default path: without
// background, delegateTask waits for the sub-agent's done event.
func TestDelegateTaskBlockingStillWaits(t *testing.T) {
	runner := &slowFakeRunner{events: make(chan SubagentEvent), spawned: make(chan struct{}, 1)}
	ctx := context.Background()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := delegateTask(runner, ctx, map[string]interface{}{"goal": "research"})
		done <- result{out, err}
	}()

	<-runner.spawned
	select {
	case <-done:
		t.Fatal("blocking delegate returned before the sub-agent finished")
	case <-time.After(150 * time.Millisecond):
	}
	runner.events <- SubagentEvent{Type: "done", Content: "summary"}
	close(runner.events) // the event loop terminates on channel close
	r := <-done
	if !strings.Contains(r.out, "summary") || !strings.Contains(r.out, "Task ID") {
		t.Fatalf("blocking result = %q", r.out)
	}
}
