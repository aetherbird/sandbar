package tools

import "context"

// threadIDKey carries the active thread id through tool execution so per-thread
// tool state (e.g. the todo list) stays isolated between concurrent sessions
// sharing one process (the server). Without it, package-level tool state leaks
// across conversations.
type threadIDKey struct{}
type toolCallIDKey struct{}
type workspaceKey struct{}
type subagentTaskIDKey struct{}

// WithThreadID returns a context carrying the active thread id. The agent sets
// this before running tools so per-thread tools can scope their state.
func WithThreadID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, threadIDKey{}, id)
}

// threadIDFromContext returns the active thread id, or "" if none is set.
func threadIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(threadIDKey{}).(string)
	return id
}

// WithToolCallID scopes tool execution to the provider call that initiated it.
// Nested tools such as delegate_task use this to tag their progress events.
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFromContext returns the active provider tool-call id, if any.
func ToolCallIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}

// WithWorkspace carries the workspace selected for the active request through
// nested tool execution. This keeps delegated work in the same workspace even
// when it differs from the process-wide configured default.
func WithWorkspace(ctx context.Context, workspace string) context.Context {
	return context.WithValue(ctx, workspaceKey{}, workspace)
}

// WorkspaceFromContext returns the active request workspace, if one was set.
func WorkspaceFromContext(ctx context.Context) string {
	workspace, _ := ctx.Value(workspaceKey{}).(string)
	return workspace
}

// WithSubagentTaskID returns a context carrying a subagent task identifier.
// delegate_task sets this before spawning so the subagent runner can persist
// and resume task state across sessions.
func WithSubagentTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, subagentTaskIDKey{}, id)
}

// SubagentTaskIDFromContext returns the subagent task identifier, if any.
func SubagentTaskIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(subagentTaskIDKey{}).(string)
	return id
}

type effortKey struct{}

// WithEffort carries the active turn's reasoning effort through nested tool
// execution so subagents inherit the parent's effort rather than defaulting to
// the provider's own.
func WithEffort(ctx context.Context, effort string) context.Context {
	return context.WithValue(ctx, effortKey{}, effort)
}

// EffortFromContext returns the active turn's reasoning effort, or "" if unset.
func EffortFromContext(ctx context.Context) string {
	effort, _ := ctx.Value(effortKey{}).(string)
	return effort
}
