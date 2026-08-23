package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoRunner is a fake SubagentRunner that emits the goal back as events, so a
// sink can verify it only received events from its own delegation.
type echoRunner struct{}

func (echoRunner) SpawnSubagent(ctx context.Context, goal, contextStr string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 2)
	ch <- SubagentEvent{Type: "token", Content: goal}
	ch <- SubagentEvent{Type: "done", Content: goal}
	close(ch)
	return ch, nil
}

func (echoRunner) ResumeSubagent(ctx context.Context, taskID string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "done", Content: "resumed: " + taskID}
	close(ch)
	return ch, nil
}

// TestDelegateTask_SinkIsolation runs many concurrent delegations, each with its
// own context-scoped event sink, and asserts that no sink ever observes another
// delegation's events. Under the previous package-global SubagentEventCallback
// this raced and leaked events across concurrent chats; the context-scoped sink
// fixes it. Run with -race to also catch the data race on the old global.
func TestDelegateTask_SinkIsolation(t *testing.T) {
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			goal := fmt.Sprintf("goal-%d", i)

			var mu sync.Mutex
			var seen []string
			ctx := WithEventSink(context.Background(), func(ev SubagentEvent) {
				mu.Lock()
				value := ev.Content
				if ev.Type == "start" {
					value = ev.Goal
				}
				seen = append(seen, value)
				mu.Unlock()
			})

			if _, err := delegateTask(echoRunner{}, ctx, map[string]interface{}{"goal": goal}); err != nil {
				errs[i] = err
				return
			}

			mu.Lock()
			defer mu.Unlock()
			if len(seen) == 0 {
				errs[i] = fmt.Errorf("%s: sink received no events", goal)
				return
			}
			for _, c := range seen {
				if c != goal {
					errs[i] = fmt.Errorf("%s: sink saw foreign event %q", goal, c)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

func TestDelegateTaskTagsEventsWithParentToolCallID(t *testing.T) {
	var seen []SubagentEvent
	ctx := WithToolCallID(context.Background(), "call_delegate_1")
	ctx = WithEventSink(ctx, func(ev SubagentEvent) {
		seen = append(seen, ev)
	})

	if _, err := delegateTask(echoRunner{}, ctx, map[string]interface{}{"goal": "inspect"}); err != nil {
		t.Fatalf("delegate task: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("events: got %d, want 3", len(seen))
	}
	var taskID string
	for _, ev := range seen {
		if ev.ToolCallID != "call_delegate_1" {
			t.Errorf("event %q tool call id: got %q", ev.Type, ev.ToolCallID)
		}
		if ev.TaskID == "" {
			t.Errorf("event %q has no task id", ev.Type)
		} else if taskID == "" {
			taskID = ev.TaskID
		} else if ev.TaskID != taskID {
			t.Errorf("event %q task id: got %q, want %q", ev.Type, ev.TaskID, taskID)
		}
	}
}

// failingRunner emits one token and then a terminal error event, mirroring a
// sub-agent that produced partial output before failing.
type failingRunner struct {
	partial string
}

func (r failingRunner) SpawnSubagent(ctx context.Context, goal, contextStr string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 3)
	ch <- SubagentEvent{Type: "token", Content: "streamed so far"}
	ch <- SubagentEvent{Type: "error", Content: "subagent LLM error: boom", Partial: r.partial}
	close(ch)
	return ch, nil
}

func (r failingRunner) ResumeSubagent(ctx context.Context, taskID string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "error", Content: "subagent LLM error: boom", Partial: r.partial}
	close(ch)
	return ch, nil
}

func TestDelegateTaskErrorEventReturnsFailedResult(t *testing.T) {
	res, err := delegateTask(failingRunner{partial: "half the answer"}, context.Background(), map[string]interface{}{"goal": "inspect"})
	if err != nil {
		t.Fatalf("delegate task: %v", err)
	}
	if !strings.Contains(res, "[subagent failed: subagent LLM error: boom]") {
		t.Errorf("missing failed marker: %q", res)
	}
	if !strings.Contains(res, "Partial output before interruption:\nhalf the answer") {
		t.Errorf("missing event-carried partial output: %q", res)
	}
	if strings.Contains(res, "You may retry") {
		t.Errorf("failed outcome should not carry the retry hint: %q", res)
	}
}

func TestDelegateTaskErrorEventFallsBackToStreamedPartial(t *testing.T) {
	res, err := delegateTask(failingRunner{}, context.Background(), map[string]interface{}{"goal": "inspect"})
	if err != nil {
		t.Fatalf("delegate task: %v", err)
	}
	if !strings.Contains(res, "[subagent failed:") {
		t.Errorf("missing failed marker: %q", res)
	}
	if !strings.Contains(res, "Partial output before interruption:\nstreamed so far") {
		t.Errorf("missing streamed partial output: %q", res)
	}
}

// stallingRunner emits one token and then blocks until the context is done,
// mirroring a sub-agent stuck mid-run when the caller cancels.
type stallingRunner struct{}

func (stallingRunner) SpawnSubagent(ctx context.Context, goal, contextStr string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "token", Content: "partial work"}
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func (stallingRunner) ResumeSubagent(ctx context.Context, taskID string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "token", Content: "partial work"}
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

func TestDelegateTaskContextCancelReturnsInterruptedResult(t *testing.T) {
	sawToken := make(chan struct{})
	ctx := WithEventSink(context.Background(), func(ev SubagentEvent) {
		if ev.Type == "token" {
			close(sawToken)
		}
	})
	ctx, cancel := context.WithCancel(ctx)

	done := make(chan struct{})
	var res string
	var err error
	go func() {
		defer close(done)
		res, err = delegateTask(stallingRunner{}, ctx, map[string]interface{}{"goal": "inspect"})
	}()

	<-sawToken // ensure the token is consumed before the cancel lands
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delegateTask did not return after context cancel")
	}
	if err != nil {
		t.Fatalf("delegate task: %v", err)
	}
	if !strings.Contains(res, "[subagent interrupted: context canceled]") {
		t.Errorf("missing interrupted marker: %q", res)
	}
	if !strings.Contains(res, "Partial output before interruption:\npartial work") {
		t.Errorf("missing accumulated partial output: %q", res)
	}
	if !strings.Contains(res, "You may retry by issuing a new delegate_task call.") {
		t.Errorf("missing retry hint: %q", res)
	}
}

type resumeMetadataRunner struct {
	shell      *ShellExec
	seenTaskID string
	asyncErr   error
}

func (r *resumeMetadataRunner) SpawnSubagent(context.Context, string, string) (<-chan SubagentEvent, error) {
	return nil, fmt.Errorf("unexpected spawn")
}

func (r *resumeMetadataRunner) SubagentTaskGoal(_ context.Context, taskID string) (string, error) {
	if taskID != "task-resume" {
		return "", fmt.Errorf("unexpected task id %q", taskID)
	}
	return "persisted investigation goal", nil
}

func (r *resumeMetadataRunner) ResumeSubagent(ctx context.Context, taskID string) (<-chan SubagentEvent, error) {
	r.seenTaskID = SubagentTaskIDFromContext(ctx)
	_, r.asyncErr = r.shell.Execute(ctx, map[string]interface{}{"command": "true", "async": true})
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "done", Content: "resumed result"}
	close(ch)
	return ch, nil
}

func TestResumeTaskPreservesTaskContextAndGoalLifecycle(t *testing.T) {
	shell := NewShellExec(t.TempDir(), nil)
	defer func() { _ = shell.Close(context.Background()) }()
	runner := &resumeMetadataRunner{shell: shell}
	var events []SubagentEvent
	ctx := WithEventSink(context.Background(), func(ev SubagentEvent) {
		events = append(events, ev)
	})

	out, err := resumeTask(runner, ctx, map[string]interface{}{"task_id": "task-resume"})
	if err != nil {
		t.Fatalf("resume task: %v", err)
	}
	if !strings.Contains(out, "resumed result") {
		t.Fatalf("resume output = %q", out)
	}
	if runner.seenTaskID != "task-resume" {
		t.Fatalf("resume context task id = %q, want task-resume", runner.seenTaskID)
	}
	if runner.asyncErr == nil || !strings.Contains(runner.asyncErr.Error(), "unavailable inside subagents") {
		t.Fatalf("async shell on resume error = %v, want subagent rejection", runner.asyncErr)
	}
	if len(events) != 2 {
		t.Fatalf("resume lifecycle events = %d, want start and done: %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.TaskID != "task-resume" || ev.Goal != "persisted investigation goal" {
			t.Fatalf("resume lifecycle event lost task metadata: %+v", ev)
		}
	}
}
