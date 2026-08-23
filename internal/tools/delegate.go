package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// SubagentRunner is the interface the agent provides for spawning sub-agents.
// This avoids a circular import between tools and agent packages.
type SubagentRunner interface {
	SpawnSubagent(ctx context.Context, goal, contextStr string) (<-chan SubagentEvent, error)
	// ResumeSubagent resumes a previously persisted sub-agent task by ID.
	ResumeSubagent(ctx context.Context, taskID string) (<-chan SubagentEvent, error)
}

// SubagentTaskGoalProvider optionally exposes persisted task metadata before a
// resume starts, allowing frontends to render an accurate lifecycle entry.
type SubagentTaskGoalProvider interface {
	SubagentTaskGoal(ctx context.Context, taskID string) (string, error)
}

// SubagentEvent represents an event from a running sub-agent.
type SubagentEvent struct {
	Type       string // "token", "tool_call", "tool_result", "thinking", "notice", "error", "done"
	Content    string
	Tool       string
	Args       string
	ToolCallID string // parent delegate_task call, used to group concurrent subagents
	TaskID     string // stable delegated task identifier
	Goal       string // goal supplied when the task was created, including resumes when persisted metadata is available
	Status     string // running, completed, failed, or interrupted
	// Partial carries the sub-agent's accumulated assistant text on the
	// terminal "error" event, so the parent can act on work produced before
	// the run ended. Err carries the underlying cause when one is known.
	Partial string
	Err     error
}

// eventSinkKey is the context key under which a subagent event sink is stored.
type eventSinkKey struct{}

// WithEventSink returns a context carrying fn as the subagent event sink.
// Each event emitted by a sub-agent spawned within this context is delivered to
// fn. Scoping the sink to the context (rather than a package global) lets
// concurrent chats route subagent events to their own frontends without leaking
// events into each other or racing on a shared callback.
func WithEventSink(ctx context.Context, fn func(SubagentEvent)) context.Context {
	return context.WithValue(ctx, eventSinkKey{}, fn)
}

// eventSink returns the subagent event sink carried by ctx, or nil if none is set.
func eventSink(ctx context.Context) func(SubagentEvent) {
	fn, _ := ctx.Value(eventSinkKey{}).(func(SubagentEvent))
	return fn
}

// maxSubagentPartialLen bounds how much partial sub-agent output is inlined
// into the delegate_task tool result.
const maxSubagentPartialLen = 4000

// subagentOutcome formats the tool result for a sub-agent run that ended
// without a "done" event. It is returned as normal tool output (not a Go
// error) so the parent model sees a typed outcome it can act on rather than a
// fatal error string.
func subagentOutcome(kind, reason, partial string) string {
	if partial == "" {
		partial = "(none)"
	} else if len(partial) > maxSubagentPartialLen {
		partial = partial[:maxSubagentPartialLen] + "..."
	}
	out := fmt.Sprintf("[subagent %s: %s]\n\nPartial output before interruption:\n%s", kind, reason, partial)
	if kind == "interrupted" {
		out += "\n\nYou may retry by issuing a new delegate_task call."
	}
	return out
}

// delegateTask spawns a sub-agent to complete a goal and returns the result.
// The runner is passed explicitly (stored on the Registry) rather than read
// from a package global, so concurrent Agent instances don't clobber each other.
func delegateTask(runner SubagentRunner, ctx context.Context, args map[string]interface{}) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("subagent runner not configured")
	}

	goal, _ := args["goal"].(string)
	if goal == "" {
		return "", fmt.Errorf("goal is required")
	}

	contextStr, _ := args["context"].(string)

	// Generate a task_id and make it available to the agent via context so
	// SpawnSubagent can persist initial state for resumability.
	taskID := uuid.New().String()
	ctx = WithSubagentTaskID(ctx, taskID)
	sink := eventSink(ctx)
	toolCallID := ToolCallIDFromContext(ctx)
	if sink != nil {
		sink(SubagentEvent{
			Type:       "start",
			ToolCallID: toolCallID,
			TaskID:     taskID,
			Goal:       goal,
			Status:     "running",
		})
	}

	events, err := runner.SpawnSubagent(ctx, goal, contextStr)
	if err != nil {
		if sink != nil {
			sink(SubagentEvent{Type: "error", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "failed", Content: err.Error(), Err: err})
		}
		return "", fmt.Errorf("spawn subagent: %w", err)
	}

	var result string
	var partial strings.Builder
loop:
	for {
		select {
		case <-ctx.Done():
			// The sub-agent goroutine may still be running; drain its events
			// so it can finish instead of leaking on a blocked send.
			go func() {
				for range events {
				}
			}()
			if sink != nil {
				sink(SubagentEvent{Type: "error", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "interrupted", Content: ctx.Err().Error(), Err: ctx.Err()})
			}
			return subagentOutcome("interrupted", ctx.Err().Error(), partial.String()) + taskIDSuffix(taskID), nil
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			if ev.ToolCallID == "" {
				ev.ToolCallID = toolCallID
			}
			if ev.TaskID == "" {
				ev.TaskID = taskID
			}
			if ev.Goal == "" {
				ev.Goal = goal
			}
			switch ev.Type {
			case "done":
				ev.Status = "completed"
			case "error":
				if ctx.Err() != nil || errors.Is(ev.Err, context.Canceled) {
					ev.Status = "interrupted"
				} else {
					ev.Status = "failed"
				}
			default:
				if ev.Status == "" {
					ev.Status = "running"
				}
			}
			if sink != nil {
				sink(ev)
			}
			switch ev.Type {
			case "token":
				partial.WriteString(ev.Content)
			case "error":
				p := ev.Partial
				if p == "" {
					p = partial.String()
				}
				// Distinguish interruption (caller went away) from a real
				// sub-agent failure; both surface as actionable tool results.
				if ctx.Err() != nil || errors.Is(ev.Err, context.Canceled) {
					return subagentOutcome("interrupted", ev.Content, p) + taskIDSuffix(taskID), nil
				}
				return subagentOutcome("failed", ev.Content, p) + taskIDSuffix(taskID), nil
			case "done":
				result = ev.Content
			}
		}
	}

	// A cancel that raced the channel closing still reports as interrupted.
	if result == "" && ctx.Err() != nil {
		if sink != nil {
			sink(SubagentEvent{Type: "error", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "interrupted", Content: ctx.Err().Error(), Err: ctx.Err()})
		}
		return subagentOutcome("interrupted", ctx.Err().Error(), partial.String()) + taskIDSuffix(taskID), nil
	}
	if result == "" {
		if sink != nil {
			sink(SubagentEvent{Type: "done", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "completed"})
		}
		return "[Subagent completed with no output]" + taskIDSuffix(taskID), nil
	}
	return result + taskIDSuffix(taskID), nil
}

// taskIDSuffix appends a structured task identifier to the delegate_task result
// so the parent model can refer to it with resume_task if needed.
func taskIDSuffix(taskID string) string {
	return fmt.Sprintf("\n\n---\nTask ID: %s\nTo resume this task if it was interrupted, call resume_task with this task_id.", taskID)
}

// resumeTask resumes a previously-persisted subagent task by ID.
func resumeTask(runner SubagentRunner, ctx context.Context, args map[string]interface{}) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("subagent runner not configured")
	}

	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return "", fmt.Errorf("task_id is required")
	}

	ctx = WithSubagentTaskID(ctx, taskID)
	goal := ""
	if provider, ok := runner.(SubagentTaskGoalProvider); ok {
		goal, _ = provider.SubagentTaskGoal(ctx, taskID)
	}
	sink := eventSink(ctx)
	toolCallID := ToolCallIDFromContext(ctx)
	if sink != nil {
		sink(SubagentEvent{Type: "start", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "running"})
	}

	events, err := runner.ResumeSubagent(ctx, taskID)
	if err != nil {
		if sink != nil {
			sink(SubagentEvent{Type: "error", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "failed", Content: err.Error(), Err: err})
		}
		return "", fmt.Errorf("resume subagent: %w", err)
	}

	// Reuse the same event collection and outcome logic as delegateTask.
	var result string
	var partial strings.Builder
loop:
	for {
		select {
		case <-ctx.Done():
			go func() {
				for range events {
				}
			}()
			if sink != nil {
				sink(SubagentEvent{Type: "error", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "interrupted", Content: ctx.Err().Error(), Err: ctx.Err()})
			}
			return subagentOutcome("interrupted", ctx.Err().Error(), partial.String()) + taskIDSuffix(taskID), nil
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			if ev.ToolCallID == "" {
				ev.ToolCallID = toolCallID
			}
			if ev.TaskID == "" {
				ev.TaskID = taskID
			}
			if ev.Goal == "" {
				ev.Goal = goal
			}
			switch ev.Type {
			case "done":
				ev.Status = "completed"
			case "error":
				if ctx.Err() != nil || errors.Is(ev.Err, context.Canceled) {
					ev.Status = "interrupted"
				} else {
					ev.Status = "failed"
				}
			default:
				if ev.Status == "" {
					ev.Status = "running"
				}
			}
			if sink != nil {
				sink(ev)
			}
			switch ev.Type {
			case "token":
				partial.WriteString(ev.Content)
			case "error":
				p := ev.Partial
				if p == "" {
					p = partial.String()
				}
				if ctx.Err() != nil || errors.Is(ev.Err, context.Canceled) {
					return subagentOutcome("interrupted", ev.Content, p) + taskIDSuffix(taskID), nil
				}
				return subagentOutcome("failed", ev.Content, p) + taskIDSuffix(taskID), nil
			case "done":
				result = ev.Content
			}
		}
	}

	if result == "" && ctx.Err() != nil {
		if sink != nil {
			sink(SubagentEvent{Type: "error", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "interrupted", Content: ctx.Err().Error(), Err: ctx.Err()})
		}
		return subagentOutcome("interrupted", ctx.Err().Error(), partial.String()) + taskIDSuffix(taskID), nil
	}
	if result == "" {
		if sink != nil {
			sink(SubagentEvent{Type: "done", ToolCallID: toolCallID, TaskID: taskID, Goal: goal, Status: "completed"})
		}
		return "[Resumed subagent completed with no output]" + taskIDSuffix(taskID), nil
	}
	return result + taskIDSuffix(taskID), nil
}
