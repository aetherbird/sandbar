package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/persona"
	"github.com/aetherbird/sandbar/internal/tools"
)

func turnLimitReached(turn, maxTurns int) bool {
	return maxTurns > 0 && turn >= maxTurns
}

const planModePrompt = "You are in PLAN MODE for this turn. Investigate the task with read-only tools (file_read, search_files, web lookups, non-mutating inspection) and produce a concrete plan: what you will change, where, in what order, and how you will verify it. Do not attempt any file modification, shell command that changes state, or other write action — those are blocked and will error. End your turn with the plan; execution happens after the user approves it."

// defaultResumeTurns is the turn budget granted to a resumed subagent task that
// has already exhausted (or come within one turn of) its original max_turns
// cap. It keeps a turn-capped task resumable without making it unbounded.
const defaultResumeTurns = 10

const subagentsUnavailablePrompt = "Runtime capability: sub-agent delegation is unavailable in this run. Do not call delegate_task or resume_task."

// cliFormattingPrompt is appended to the system prompt for CLI-originated
// turns (Request.Source == "cli"); both surfaces render Markdown, but the
// CLI terminal renderer is narrow — keep Markdown simple there.
const cliFormattingPrompt = "Formatting: the CLI renders standard Markdown — use headings, lists, emphasis, and fenced code blocks; avoid HTML, images, and wide tables."

// todoNudgeAfterRounds is the number of consecutive tool-call rounds in one
// turn without a todo-tool call after which the model gets a one-time reminder
// to keep its durable task list current.
const todoNudgeAfterRounds = 12

const todoNudgePrompt = "[Reminder: you have an active task list for this thread — update it or mark items complete as you make progress.]"

// planApprovedPrompt is injected once, on the turn immediately after the user
// approves a pending plan (memory.PlanModeApproved), then cleared.
const planApprovedPrompt = "[The user approved this plan. Execute it now, updating the task list as you complete each step.]"

// Agent is the shared reasoning core used by both server and CLI.
type Agent struct {
	cfg             *config.Config
	store           *memory.Store
	registry        *llm.Registry
	tools           *tools.Registry
	summarizers     SummarizerFactory // nil falls back to productionSummarizerFactory
	keepaliveCancel context.CancelFunc
	// turnLocks holds one semaphore per thread ID so concurrent Chat calls on the
	// same thread serialize instead of interleaving history writes.
	turnLocks sync.Map
	// resumeMu guards resuming so two concurrent ResumeSubagent calls on the same
	// task cannot interleave their re-execution of the same work.
	resumeMu sync.Mutex
	resuming map[string]bool
	// steering holds the per-thread mid-turn message queues.
	steering *steeringQueues
	// turnCancels maps a thread ID to the cancel func of its active turn, so the
	// interrupt endpoint can cancel it out-of-band.
	turnCancels sync.Map
	// compressionLoadNoticeOnce caps the "cannot load saved compression
	// summary" notice at one per agent lifetime: a persistently broken store
	// would otherwise repeat it on every turn.
	compressionLoadNoticeOnce sync.Once
	// injectedInstr tracks instruction files (AGENTS.md/CLAUDE.md) already
	// appended to a tool result, so a subdirectory's instructions are injected
	// once per agent instead of on every file tool call under it.
	instrMu       sync.Mutex
	injectedInstr map[string]bool
}

type threadTurnLock struct {
	token chan struct{}
}

func newThreadTurnLock() *threadTurnLock {
	lock := &threadTurnLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

// lockThread acquires the per-thread lifecycle lock while respecting request
// cancellation. Chat never recurses into itself on the same thread — spawned
// subagents use isolated clients and do not touch the parent thread store.
func (a *Agent) lockThread(ctx context.Context, threadID string) (func(), error) {
	v, _ := a.turnLocks.LoadOrStore(threadID, newThreadTurnLock())
	lock := v.(*threadTurnLock)
	select {
	case <-lock.token:
		return func() { lock.token <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func New(cfg *config.Config, store *memory.Store, registry *llm.Registry, toolReg *tools.Registry) *Agent {
	a := &Agent{
		cfg:      cfg,
		store:    store,
		registry: registry,
		tools:    toolReg,
		steering: newSteeringQueues(),
	}
	// Register as the subagent runner on the registry instance, not a package global.
	if a.tools != nil {
		a.tools.SetSubagentRunner(a)
		a.tools.SetPlanStore(store)
		a.tools.SetSubagentStore(store)
	}
	return a
}

// Close stops agent background work and tears down supervised tool processes.
func (a *Agent) Close(ctx context.Context) error {
	a.StopKeepalive()
	if a.tools == nil {
		return nil
	}
	return a.tools.Close(ctx)
}

// DeleteThread serializes with active turns, cancels every shell process owned
// by the thread, and only then removes its durable conversation state.
func (a *Agent) DeleteThread(ctx context.Context, threadID string) error {
	if a.store == nil {
		return fmt.Errorf("store not available")
	}
	unlock, err := a.lockThread(ctx, threadID)
	if err != nil {
		return fmt.Errorf("wait for active thread turn: %w", err)
	}
	defer unlock()
	if a.tools != nil {
		if err := a.tools.CancelThreadJobs(ctx, threadID); err != nil {
			return fmt.Errorf("cancel thread jobs: %w", err)
		}
	}
	return a.store.DeleteThread(threadID)
}

// SubagentTaskGoal returns persisted task metadata used by resume lifecycle
// events and the compact frontend HUD.
func (a *Agent) SubagentTaskGoal(ctx context.Context, taskID string) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("store not available")
	}
	var goal string
	if err := a.store.DB().QueryRowContext(ctx, `SELECT goal FROM subagent_tasks WHERE id = ?`, taskID).Scan(&goal); err != nil {
		return "", fmt.Errorf("load subagent task %s goal: %w", taskID, err)
	}
	return goal, nil
}

// SpawnSubagent implements tools.SubagentRunner.
func (a *Agent) SpawnSubagent(ctx context.Context, goal, contextStr string) (<-chan tools.SubagentEvent, error) {
	subagentModel := a.cfg.Subagent.Model
	if subagentModel == "" {
		subagentModel = "deepseek/deepseek-v4-flash"
	}
	resolved, err := a.registry.ResolveModel(subagentModel)
	if err != nil {
		return nil, fmt.Errorf("resolve subagent model: %w", err)
	}

	client := llm.NewWireClient(resolved)

	allowedTools := a.cfg.Subagent.Tools
	if len(allowedTools) == 0 {
		allowedTools = []string{"file_read", "search_files", "web_search", "web_fetch", "shell_exec"}
	}
	workspace := tools.WorkspaceFromContext(ctx)
	if workspace == "" {
		workspace = a.cfg.Workspace
	}
	filteredTools := tools.NewRegistry(workspace, a.cfg.Tools.WebSearch.BraveAPIKey, "", a.cfg.Tools.Shell.BlockedCommands)
	availableTools := make(map[string]tools.Tool, len(allowedTools))
	for _, name := range allowedTools {
		if tool, getErr := filteredTools.Get(name); getErr == nil {
			availableTools[name] = tool
		}
	}
	filteredTools.Clear()
	if a.tools != nil {
		approvalConfig := a.tools.ApprovalConfig()
		// delegate_task approval is the subagent's broad execution boundary.
		// Keep explicit deny rules but don't re-apply the parent mode default
		// inside the delegated run. Prompt policies are unanswerable here (no
		// handler is installed in a subagent), so they degrade to allow;
		// deny stays deny.
		approvalConfig.Mode = tools.ApprovalModeYolo
		approvalConfig = tools.RewritePromptToAllow(approvalConfig)
		_ = filteredTools.SetApprovalConfig(approvalConfig)
	}
	filteredTools.SetPlanStore(a.store)
	if shellTimeout, timeoutErr := a.cfg.ShellTimeout(); timeoutErr == nil {
		_ = filteredTools.SetShellTimeout(shellTimeout)
	}
	if jobs, jobsErr := a.cfg.ShellJobSettings(); jobsErr == nil {
		_ = filteredTools.SetJobSupervisorConfig(tools.JobSupervisorConfig{
			MaxJobs: jobs.MaxJobs, MaxRunning: jobs.MaxRunning, OutputBytes: jobs.OutputBytes,
			Retention: jobs.Retention, TerminationGrace: jobs.TerminationGrace,
		})
	}
	// Adopt the parent's job supervisor so DeleteThread's CancelThreadJobs
	// reaches shell work this subagent starts (a fresh registry would own
	// an unreachable supervisor of its own).
	if a.tools != nil {
		if parentJobs := a.tools.JobSupervisor(); parentJobs != nil {
			_ = filteredTools.SetJobSupervisor(parentJobs)
		}
	}
	if sshSettings, sshErr := a.cfg.SSHSettings(); sshErr == nil {
		_ = filteredTools.SetSSHConfig(tools.SSHRuntimeConfig{
			ConnectTimeout: sshSettings.ConnectTimeout,
			BatchMode:      sshSettings.BatchMode,
			AllowedHosts:   sshSettings.AllowedHosts,
		})
	}
	for _, name := range allowedTools {
		if tool, ok := availableTools[name]; ok {
			filteredTools.Register(tool)
		}
	}

	// Build messages after filtering so the prompt advertises exactly the
	// tools the sub-agent can call. It cannot see the parent conversation,
	// so the prompt says the goal is self-contained (mirrors the delegation
	// guidance in the parent system prompt).
	sysPrompt := fmt.Sprintf("You are an autonomous sub-agent completing a delegated goal. You do not see the parent conversation: the goal below is self-contained and carries all requirements. Use the available tools to investigate and act — you have shell_exec, so you can run commands, build, and test, not just read. Ground every claim in tool output. Keep going until the task is complete; never give up due to missing information that tools or files can provide. For long waits use the job tool (start with async, then job wait); never poll with `sleep N; …` loops. Return a clear, structured summary of what you did, what you found, and anything you could not verify.\n\nWorkspace: %s\nCurrent date: %s\nAvailable tools: %s.", workspace, time.Now().UTC().Format("2006-01-02"), strings.Join(filteredTools.List(), ", "))
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
		{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("Goal: %s\n\nContext: %s", goal, contextStr)},
	}

	maxTurns := a.cfg.Subagent.MaxTurns

	// Check for a task_id from delegate_task; persist initial state if present.
	taskID := tools.SubagentTaskIDFromContext(ctx)
	persistTask := taskID != "" && a.store != nil
	if persistTask {
		messagesJSON, _ := json.Marshal(messages)
		now := time.Now().Unix()
		_, err := a.store.DB().Exec(
			`INSERT INTO subagent_tasks (id, goal, context, model_alias, messages_json, turn, max_turns, status, result, files_read, files_written, commands_run, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, goal, contextStr, subagentModel, string(messagesJSON), 0, maxTurns, "running", "", "[]", "[]", "[]", now, now,
		)
		if err != nil {
			// Non-fatal: run without persistence.
			persistTask = false
		}
	}

	// Buffered so a slow parent cannot stall the sub-agent on every event; the
	// ctx.Done guard on each send prevents a stuck parent from leaking the
	// goroutine.
	ch := make(chan tools.SubagentEvent, 64)

	go func() {
		defer close(ch)

		// partial accumulates assistant text so a terminal error can carry the
		// work produced before the run ended; on normal completion it is what
		// the "done" event carries.
		var partial strings.Builder
		ft := newFileTracker()
		send := func(ev tools.SubagentEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		sendToken := func(content string) bool {
			partial.WriteString(content)
			return send(tools.SubagentEvent{Type: "token", Content: content})
		}
		turn := 0
		var loopGuard toolLoopGuard
		// Inherit the parent turn's reasoning effort.
		effort := tools.EffortFromContext(ctx)
		sendError := func(content string, err error) {
			partialWithManifest := partial.String()
			if manifest := ft.Manifest(); manifest != "" {
				partialWithManifest += manifest
			}
			errorPartial := sanitizeSubagentOutput(partialWithManifest)
			send(tools.SubagentEvent{Type: "error", Content: content, Partial: errorPartial, Err: err})
			if persistTask {
				status := "failed"
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					status = "interrupted"
				}
				messagesJSON, _ := json.Marshal(messages)
				resultStr := partialWithManifest
				if len(resultStr) > 4000 {
					resultStr = resultStr[:4000]
				}
				_, _ = a.store.DB().Exec(
					`UPDATE subagent_tasks SET status = ?, result = ?, messages_json = ?, turn = ?, files_read = ?, files_written = ?, updated_at = ? WHERE id = ?`,
					status, resultStr, string(messagesJSON), turn, marshalStringsJSON(ft.FilesRead()), marshalStringsJSON(ft.FilesWritten()), time.Now().Unix(), taskID,
				)
			}
		}
		// sendDone emits the done event with the final result, file manifest
		// appended, and prompt-injection sanitization applied.
		sendDone := func() bool {
			result := partial.String()
			if manifest := ft.Manifest(); manifest != "" {
				result += manifest
			}
			result = sanitizeSubagentOutput(result)
			sent := send(tools.SubagentEvent{Type: "done", Content: result})
			// The write is NOT gated on sent: when the parent cancels, ch<-ev and
			// <-ctx.Done() are both ready and Go may pick the cancel branch even
			// though the event delivered — skipping the UPDATE would leave status
			// 'running' and make a later resume re-execute completed work.
			if persistTask {
				messagesJSON, _ := json.Marshal(messages)
				resultStr := result
				if len(resultStr) > 4000 {
					resultStr = resultStr[:4000]
				}
				_, _ = a.store.DB().Exec(
					`UPDATE subagent_tasks SET status = ?, result = ?, messages_json = ?, turn = ?, files_read = ?, files_written = ?, updated_at = ? WHERE id = ?`,
					"completed", resultStr, string(messagesJSON), turn, marshalStringsJSON(ft.FilesRead()), marshalStringsJSON(ft.FilesWritten()), time.Now().Unix(), taskID,
				)
			}
			return sent
		}

		for {
			if turnLimitReached(turn, maxTurns) {
				sendError("max subagent turns reached", nil)
				return
			}
			turn++

			if resolved.SupportsTools && filteredTools != nil {
				openaiTools := buildToolSchemasFrom(filteredTools)
				messages = sanitizeProviderMessages(messages)
				var result *llm.CompletionResult
				err := runWithLLMRetry(ctx, func(ev llm.StreamEvent) error {
					// Forward retry notices to the subagent stream so a long
					// backoff never looks frozen. Delivery is best-effort.
					if ev.Type == "intermediate" {
						send(tools.SubagentEvent{Type: "notice", Content: ev.Content})
					}
					return nil
				}, func() error {
					var callErr error
					result, callErr = client.CompleteWithOptions(ctx, messages, llm.CompleteOptions{Tools: openaiTools, Effort: effort})
					return callErr
				})
				if err != nil {
					sendError(fmt.Sprintf("subagent LLM error: %v", err), err)
					return
				}

				if len(result.ToolCalls) > 0 {
					loopDecision := loopGuard.Observe(result.ToolCalls)
					messages = append(messages, openai.ChatCompletionMessage{
						Role:      openai.ChatMessageRoleAssistant,
						Content:   result.Content,
						ToolCalls: result.ToolCalls,
					})

					for _, tc := range result.ToolCalls {
						if tc.Type != openai.ToolTypeFunction {
							continue
						}
						if !send(tools.SubagentEvent{
							Type: "tool_call",
							Tool: tc.Function.Name,
							Args: tc.Function.Arguments,
						}) {
							return
						}
					}

					outputs := executeToolCallBatchWith(ctx, result.ToolCalls, loopDecision, filteredTools.CanRunConcurrently, func(callCtx context.Context, tc openai.ToolCall) string {
						if tc.Type != openai.ToolTypeFunction {
							return fmt.Sprintf("error: unsupported tool call type %q", tc.Type)
						}
						var args map[string]interface{}
						if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
							return fmt.Sprintf("error: parse tool arguments: %s", err.Error())
						}
						if args == nil {
							args = make(map[string]interface{})
						}
						callCtx = tools.WithToolCallID(callCtx, tc.ID)
						ft.trackFileOp(tc.Function.Name, args)

						if tc.Function.Name == "shell_exec" {
							if cmd, _ := args["command"].(string); cmd != "" {
								ft.trackShellCmd(cmd)
							}
						}
						if tc.Function.Name == "web_fetch" {
							if url, _ := args["url"].(string); url != "" {
								ft.trackURL(url)
							}
						}
						if tc.Function.Name == "web_search" {
							if query, _ := args["query"].(string); query != "" {
								ft.trackURL("search: " + query)
							}
						}

						output, execErr := filteredTools.Execute(callCtx, tc.Function.Name, args)
						if execErr != nil {
							return fmt.Sprintf("error: %s", execErr.Error())
						}
						return output
					})

					for i, tc := range result.ToolCalls {
						output := outputs[i]
						if !send(tools.SubagentEvent{Type: "tool_result", Tool: tc.Function.Name, Content: truncateStr(output, 300)}) {
							return
						}

						messages = append(messages, openai.ChatCompletionMessage{
							Role:       "tool",
							Content:    output,
							ToolCallID: tc.ID,
						})
					}
					if loopDecision.Abort {
						sendError(fmt.Sprintf("%s after %d consecutive rounds", ErrRepeatedToolCallLoop, loopDecision.Consecutive), ErrRepeatedToolCallLoop)
						return
					}
					continue
				}

				// A completion without tool calls is the terminal response from
				// this same tools-enabled request. Discarding it and issuing a
				// second, schemaless request would let the model call tools the
				// provider can no longer return natively.
				if result.Content != "" {
					if !sendToken(result.Content) {
						return
					}
					// Persist the final assistant text so a resumed task replays
					// a complete conversation, not just the tool history.
					messages = append(messages, openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleAssistant,
						Content: result.Content,
					})
				}
				if !sendDone() {
					return
				}
				return
			}

			// No tool support — just stream.
			messages = sanitizeProviderMessages(messages)
			stream, streamErr := client.Chat(ctx, messages)
			if streamErr != nil {
				sendError(streamErr.Error(), streamErr)
				return
			}
			for ev := range stream {
				if ev.Type == "error" {
					sendError(ev.Content, nil)
					return
				}
				if ev.Type == "token" {
					if !sendToken(ev.Content) {
						return
					}
				}
			}
			if !sendDone() {
				return
			}
			return
		}
	}()

	return ch, nil
}

func buildToolSchemasFrom(reg *tools.Registry) []openai.Tool {
	if reg == nil {
		return nil
	}
	var schemas []openai.Tool
	for _, name := range reg.List() {
		tool, _ := reg.Get(name)
		schemas = append(schemas, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Schema,
			},
		})
	}
	return schemas
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Request is a single chat request.
type Request struct {
	ThreadID    string
	ModelAlias  string
	UserMessage string
	Workspace   string
	Source      string // "cli", "web", etc. — used to tailor output formatting
	// Effort is the per-turn reasoning effort: "low", "medium", or "high".
	// Empty means the provider default. Not persisted — a follow-up turn may
	// set a different effort.
	Effort string
	// PlanOnly runs the turn in plan mode: read-tier tools only (enforced at
	// dispatch), plus a prompt directive to investigate and present a plan
	// instead of making changes. The follow-up turn executes normally.
	PlanOnly bool
}

// ValidateEffort is the exported form for request surfaces (HTTP API).
func ValidateEffort(effort string) error { return validateEffort(effort) }

// requestSourceCtxKey carries the request source ("cli", "web", …) through a
// caller's context for transports that cannot set Request.Source directly
// (e.g. the CLI's Backend.SendMessage signature has no source parameter).
type requestSourceCtxKey struct{}

// WithRequestSource annotates a context with the request source. Chat honors
// it only when Request.Source itself is empty, so an explicit field wins.
func WithRequestSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, requestSourceCtxKey{}, source)
}

// RequestSourceFrom returns the source annotated with WithRequestSource, or ""
// when the context carries none.
func RequestSourceFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestSourceCtxKey{}).(string)
	return v
}

// validateEffort rejects anything but the supported effort levels so a typo
// never silently reaches a provider as an unknown field value.
func validateEffort(effort string) error {
	switch effort {
	case "", "low", "medium", "high":
		return nil
	default:
		return fmt.Errorf("effort must be low, medium, or high (got %q)", effort)
	}
}

// Chat runs one turn of the agent loop and streams events via onEvent.
// Returns the thread ID (either existing or newly created) and any error.
func (a *Agent) Chat(ctx context.Context, req Request, onEvent func(llm.StreamEvent) error) (string, error) {
	if req.Source == "" {
		req.Source = RequestSourceFrom(ctx)
	}
	resolved, err := a.registry.ResolveModel(req.ModelAlias)
	if err != nil {
		return "", fmt.Errorf("resolve model: %w", err)
	}

	// New threads record their workspace so resume knows where a conversation belongs.
	var thread *memory.Thread
	var unlock func()
	if req.ThreadID == "" {
		thread, err = a.store.CreateThreadWithWorkspace(nil, &req.ModelAlias, req.Workspace)
		if err != nil {
			return "", fmt.Errorf("create thread: %w", err)
		}
		unlock, err = a.lockThread(ctx, thread.ID)
		if err != nil {
			return thread.ID, fmt.Errorf("lock new thread: %w", err)
		}
	} else {
		// Lock before loading so a concurrent delete cannot remove the thread
		// between load and turn execution.
		unlock, err = a.lockThread(ctx, req.ThreadID)
		if err != nil {
			return "", fmt.Errorf("wait for active thread turn: %w", err)
		}
		thread, err = a.store.GetThread(req.ThreadID)
		if err != nil {
			unlock()
			return "", fmt.Errorf("load thread: %w", err)
		}
	}

	// Serialize turns on this thread: concurrent Chat calls (CLI cancel-and-
	// resend, parallel HTTP requests) must not interleave history writes
	// between an assistant tool-call group and its tool results.
	defer unlock()
	// Register this turn with the steering queue so mid-turn messages can be
	// queued (and later injected at a tool boundary) and interrupted. The
	// endSteeringTurn defer is registered after unlock so it runs first (LIFO),
	// still holding the thread lock.
	a.beginTurn(thread.ID)
	defer a.endSteeringTurn(thread.ID)

	// Plan-mode lifecycle: a plan turn marks the thread 'planning' at start
	// (and 'pending_approval' on success, below). A normal turn abandons an
	// unfinished plan decision so a stale pending state can never fire late.
	if req.PlanOnly {
		if err := a.store.SetThreadPlanMode(thread.ID, memory.PlanModePlanning); err != nil {
			return thread.ID, fmt.Errorf("mark plan turn: %w", err)
		}
		thread.PlanMode = memory.PlanModePlanning
	} else if thread.PlanMode == memory.PlanModePlanning || thread.PlanMode == memory.PlanModePendingApproval {
		if err := a.store.SetThreadPlanMode(thread.ID, memory.PlanModeOff); err != nil {
			return thread.ID, fmt.Errorf("clear abandoned plan state: %w", err)
		}
		thread.PlanMode = memory.PlanModeOff
	}

	userContent := req.UserMessage
	if _, err := a.store.AppendMessage(thread.ID, "user", &userContent, nil); err != nil {
		return "", fmt.Errorf("persist user message: %w", err)
	}

	// Announce the thread identity before any provider call: the terminal
	// "done" event — the only other ThreadID carrier — is never emitted by a
	// cancelled turn, and a session whose first turn was interrupted would
	// otherwise keep "" as its thread ID and silently start a new thread next.
	if err := onEvent(llm.StreamEvent{Type: "thread", ThreadID: thread.ID}); err != nil {
		return thread.ID, err
	}

	// Build LLM message history. "tropical" is a session mode, not a wire
	// effort: it maps to high and injects the heavy-subagent directive.
	effort := req.Effort
	tropical := effort == "tropical"
	if tropical {
		effort = "high"
	}
	indexedMsgs, err := a.buildMessages(thread.ID, req.Workspace, req.Source, req.PlanOnly, onEvent, tropical)
	if err != nil {
		return thread.ID, fmt.Errorf("build messages: %w", err)
	}
	{
		// Emit compression_start BEFORE the summarizer runs — the event carries
		// the pre-work token snapshot, so consumers can show an in-flight state
		// instead of receiving start/end back-to-back after the fact.
		if pre := preCompressionSnapshot(indexedMsgs, req.ModelAlias, resolved.ContextLength, a.cfg.Compression); pre.willAttempt {
			_ = onEvent(llm.StreamEvent{
				Type:        "compression_start",
				Compression: buildCompressionStartEvent(pre),
			})
		}
		started := time.Now()
		comp := a.compressIfNeeded(ctx, thread.ID, indexedMsgs, req.ModelAlias, resolved.ContextLength, CompressionModeTurnStart)
		comp.ElapsedMS = time.Since(started).Milliseconds()
		indexedMsgs = comp.Messages
		if comp.Outcome != CompressionOutcomeNone {
			evType := "compression_end"
			compressionEvent := buildCompressionEndEvent(comp)
			if comp.Outcome == CompressionOutcomeError || (comp.Err != nil) {
				evType = "compression_error"
				compressionEvent = buildCompressionErrorEvent(comp)
			}
			_ = onEvent(llm.StreamEvent{
				Type:        evType,
				Compression: compressionEvent,
			})
			if usageEvent := buildCompressionAuxiliaryUsageEvent(comp); usageEvent != nil {
				_ = onEvent(*usageEvent)
			}
		}
		if errors.Is(comp.Err, ErrUnsafeProviderPayload) {
			return thread.ID, comp.Err
		}
	}
	llmMessages := toRawMessages(indexedMsgs)
	client := llm.NewWireClient(resolved)

	// Scope per-thread tool state (e.g. the todo list) to this thread so
	// concurrent sessions on a shared server don't see each other's state.
	ctx = tools.WithThreadID(ctx, thread.ID)
	ctx = tools.WithWorkspace(ctx, req.Workspace)

	// Wire subagent events to the frontend via onEvent, scoped to this request's
	// context so concurrent chats don't clobber each other's event delivery.
	// Concurrent delegate_task calls may report at the same time, so serialize
	// delivery to frontend callbacks, which are not required to be thread-safe.
	var subagentEventMu sync.Mutex
	ctx = tools.WithEventSink(ctx, func(ev tools.SubagentEvent) {
		subagentEventMu.Lock()
		defer subagentEventMu.Unlock()
		switch ev.Type {
		case "start":
			onEvent(llm.StreamEvent{Type: "subagent_start", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status})
		case "token":
			onEvent(llm.StreamEvent{Type: "subagent_token", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status, Content: ev.Content})
		case "notice":
			// A non-terminal informational line (e.g. a retry notice) from the
			// subagent. Rendered as an intermediate ↻ line so a long retry backoff
			// never looks frozen.
			onEvent(llm.StreamEvent{Type: "intermediate", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status, Content: ev.Content})
		case "tool_call":
			onEvent(llm.StreamEvent{Type: "subagent_tool_call", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status, ToolName: ev.Tool, Arguments: json.RawMessage(ev.Args), Content: ev.Args})
		case "tool_result":
			onEvent(llm.StreamEvent{Type: "subagent_tool_result", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status, ToolName: ev.Tool, Content: ev.Content})
		case "error":
			onEvent(llm.StreamEvent{Type: "subagent_error", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status, Content: ev.Content})
		case "done":
			onEvent(llm.StreamEvent{Type: "subagent_done", ToolCallID: ev.ToolCallID, TaskID: ev.TaskID, TaskGoal: ev.Goal, TaskStatus: ev.Status, Content: ev.Content})
		}
	})

	if req.PlanOnly {
		ctx = tools.WithPlanMode(ctx)
	}
	// Carry the turn's effort so subagents inherit it instead of the provider default.
	ctx = tools.WithEffort(ctx, effort)
	turn := 0
	var loopGuard toolLoopGuard
	// Counts consecutive tool-call rounds that never touch the todo tool; the
	// nudge itself fires at most once per turn.
	toolRoundsWithoutTodo := 0
	todoNudgeSent := false
	for {
		if turnLimitReached(turn, a.cfg.MaxTurns) {
			return thread.ID, onEvent(llm.StreamEvent{Type: "error", Content: "max turn limit reached"})
		}
		turn++
		// Re-render from indexed state each turn to preserve database sequence
		// metadata and transient active-turn checkpoints across repeated tool loops.
		llmMessages = toRawMessages(indexedMsgs)
		// Repair residual poison (interrupted turns, crash residue) before the
		// payload leaves the process; the validator stays on as a hard assertion.
		llmMessages = sanitizeProviderMessages(llmMessages)
		if err := validateProviderPayload(llmMessages); err != nil {
			return thread.ID, fmt.Errorf("invalid provider payload: %w", err)
		}

		if resolved.SupportsTools && a.tools != nil {
			// Tool-capable models complete with a single response that can
			// carry either native tool calls or terminal text. When the wire
			// client supports it, the completion streams live — reasoning
			// (thinking) and answer deltas reach onEvent as they arrive —
			// while assembling the same single result.
			openaiTools := a.buildToolSchemas()
			var result *llm.CompletionResult
			streamedLive := false
			err := runWithLLMRetry(ctx, onEvent, func() error {
				var callErr error
				streamedLive = false
				if lc, ok := client.(llm.LiveCompleter); ok {
					result, callErr = lc.CompleteWithOptionsLive(ctx, llmMessages, llm.CompleteOptions{Tools: openaiTools, Effort: effort}, liveEventSender(onEvent))
					streamedLive = callErr == nil
				} else {
					result, callErr = client.CompleteWithOptions(ctx, llmMessages, llm.CompleteOptions{Tools: openaiTools, Effort: effort})
				}
				return callErr
			})
			if err != nil {
				return thread.ID, fmt.Errorf("complete: %w", err)
			}

			// Emit usage per tool-loop call so the context gauge updates live as
			// the agent grows context, not just once at the end.
			if result.Usage.TotalTokens > 0 {
				_ = onEvent(llm.StreamEvent{
					Type:             "usage",
					PromptTokens:     result.Usage.PromptTokens,
					CompletionTokens: result.Usage.CompletionTokens,
					TotalTokens:      result.Usage.TotalTokens,
					CacheReadTokens:  result.Usage.CacheReadTokens,
					CacheWriteTokens: result.Usage.CacheWriteTokens,
				})
			}

			// Fallback: if no structured tool_calls, try parsing text-embedded formats
			// (DeepSeek DSML, XML function_call blocks, function= nested format).
			if len(result.ToolCalls) == 0 && result.Content != "" {
				result.ToolCalls = parseTextToolCalls(result.Content)
			}

			if len(result.ToolCalls) > 0 {
				loopDecision := loopGuard.Observe(result.ToolCalls)
				if err := validateNewToolCallGroup(indexedMsgs, result.Content, result.ToolCalls); err != nil {
					return thread.ID, fmt.Errorf("provider returned invalid tool-call group: %w", err)
				}
				// Persist the assistant turn.
				assistantMsg, err := a.persistAssistantTurn(thread.ID, result.Content, result.ToolCalls)
				if err != nil {
					return thread.ID, fmt.Errorf("persist assistant turn: %w", err)
				}

				// Append the persisted assistant message with its real sequence ID.
				indexedMsgs = append(indexedMsgs, indexedMessage{
					Seq:  assistantMsg.Seq,
					Kind: "thread_message",
					Msg: openai.ChatCompletionMessage{
						Role:      openai.ChatMessageRoleAssistant,
						Content:   result.Content,
						ToolCalls: result.ToolCalls,
					},
				})

				// Once the assistant tool-call group is durable, every call must also
				// receive a durable tool result; otherwise a cancelled request poisons
				// the next resumed provider payload with an incomplete group.
				closePending := func(start int, cause error) error {
					if _, closeErr := a.persistInterruptedToolResults(thread.ID, result.ToolCalls[start:]); closeErr != nil {
						return errors.Join(cause, fmt.Errorf("close interrupted tool-call group: %w", closeErr))
					}
					return cause
				}

				// Announce the complete provider-ordered batch before execution. The
				// coordinator may run an independent batch concurrently, but results
				// are still emitted and persisted in this original order.
				if err := onEvent(llm.StreamEvent{Type: "intermediate", Content: "Processing..."}); err != nil {
					return thread.ID, closePending(0, err)
				}
				for _, tc := range result.ToolCalls {
					if tc.Type != openai.ToolTypeFunction {
						continue
					}
					if err := onEvent(llm.StreamEvent{
						Type:       "tool_call",
						ToolCallID: tc.ID,
						ToolName:   tc.Function.Name,
						Arguments:  json.RawMessage(tc.Function.Arguments),
					}); err != nil {
						return thread.ID, closePending(0, err)
					}
				}

				parallelBatch := toolBatchCanRunConcurrently(result.ToolCalls, a.tools.CanRunConcurrently)
				var outputs []string
				if parallelBatch {
					select {
					case <-ctx.Done():
						return thread.ID, closePending(0, ctx.Err())
					default:
					}
					outputs = a.executeToolCallBatch(ctx, result.ToolCalls, req.Workspace, loopDecision)
				}

				for toolIndex, tc := range result.ToolCalls {
					var output string
					if parallelBatch {
						output = outputs[toolIndex]
					} else {
						select {
						case <-ctx.Done():
							return thread.ID, closePending(toolIndex, ctx.Err())
						default:
						}
						if loopDecision.Skip {
							output = repeatedToolCallResult(loopDecision)
						} else {
							output = a.executeOneToolCall(ctx, tc, req.Workspace)
						}
					}
					if err := onEvent(llm.StreamEvent{
						Type:       "tool_result",
						ToolCallID: tc.ID,
						ToolName:   tc.Function.Name,
						Content:    output,
					}); err != nil {
						return thread.ID, closePending(toolIndex, err)
					}

					toolMsg, err := a.store.AppendMessage(thread.ID, "tool", &output, &tc.ID)
					if err != nil {
						persistErr := fmt.Errorf("persist tool result: %w", err)
						return thread.ID, closePending(toolIndex, persistErr)
					}
					indexedMsgs = append(indexedMsgs, indexedMessage{
						Seq:  toolMsg.Seq,
						Kind: "thread_message",
						Msg: openai.ChatCompletionMessage{
							Role:       "tool",
							Content:    output,
							ToolCallID: tc.ID,
						},
					})
				}
				// A cancellation that arrived after the batch began still receives a
				// durable result for every started call before the turn exits.
				if err := ctx.Err(); err != nil {
					return thread.ID, err
				}

				// Track consecutive tool-call rounds that never touch the todo
				// tool. After enough of them — and only when the thread has a
				// durable task list — remind the model once per turn to keep
				// that list current. Synthetic and never persisted.
				todoTouched := false
				for _, tc := range result.ToolCalls {
					if tc.Function.Name == "todo" {
						todoTouched = true
						break
					}
				}
				if todoTouched {
					toolRoundsWithoutTodo = 0
				} else {
					toolRoundsWithoutTodo++
				}
				if !todoNudgeSent && toolRoundsWithoutTodo >= todoNudgeAfterRounds && a.store != nil {
					if todos, todoErr := a.store.ListTodos(thread.ID); todoErr == nil && len(todos) > 0 {
						todoNudgeSent = true
						indexedMsgs = append(indexedMsgs, indexedMessage{
							Seq:       0,
							Synthetic: true,
							Kind:      "todo_nudge",
							Msg: openai.ChatCompletionMessage{
								Role:    openai.ChatMessageRoleUser,
								Content: todoNudgePrompt,
							},
						})
					}
				}

				if loopDecision.Abort {
					loopErr := fmt.Errorf("%w after %d consecutive rounds", ErrRepeatedToolCallLoop, loopDecision.Consecutive)
					if eventErr := onEvent(llm.StreamEvent{Type: "error", Content: loopErr.Error()}); eventErr != nil {
						return thread.ID, errors.Join(loopErr, eventErr)
					}
					return thread.ID, loopErr
				}

				// Re-compress if context has grown too large from tool results.
				{
					if pre := preCompressionSnapshot(indexedMsgs, req.ModelAlias, resolved.ContextLength, a.cfg.Compression); pre.willAttempt {
						_ = onEvent(llm.StreamEvent{
							Type:        "compression_start",
							Compression: buildCompressionStartEvent(pre),
						})
					}
					started := time.Now()
					comp := a.compressIfNeeded(ctx, thread.ID, indexedMsgs, req.ModelAlias, resolved.ContextLength, CompressionModeMidLoop)
					comp.ElapsedMS = time.Since(started).Milliseconds()
					indexedMsgs = comp.Messages
					llmMessages = toRawMessages(indexedMsgs)
					if comp.Outcome != CompressionOutcomeNone {
						evType := "compression_end"
						compressionEvent := buildCompressionEndEvent(comp)
						if comp.Outcome == CompressionOutcomeError || (comp.Err != nil) {
							evType = "compression_error"
							compressionEvent = buildCompressionErrorEvent(comp)
						}
						_ = onEvent(llm.StreamEvent{
							Type:        evType,
							Compression: compressionEvent,
						})
						if usageEvent := buildCompressionAuxiliaryUsageEvent(comp); usageEvent != nil {
							_ = onEvent(*usageEvent)
						}
					}
					if errors.Is(comp.Err, ErrUnsafeProviderPayload) {
						return thread.ID, comp.Err
					}
				}

				// Drain queued steering messages at this tool boundary — after the
				// compression block, so a mid-turn message becomes the latest user
				// turn rather than being swallowed by a mid-loop compression.
				drained, drainErr := a.drainSteering(thread.ID, onEvent)
				if drainErr != nil {
					return thread.ID, drainErr
				}
				if len(drained) > 0 {
					indexedMsgs = append(indexedMsgs, drained...)
					llmMessages = toRawMessages(indexedMsgs)
				}

				continue // Loop back to the LLM with the in-memory view intact.
			}

			// No tool calls means result.Content is the terminal response from
			// the same request that received the tool schemas. Preserve it rather
			// than discarding it and issuing a second, tools-free streaming call.
			// When the deltas already streamed live, only the terminal event
			// remains to emit — re-emitting the content would duplicate it.
			err = a.emitAndPersistCompletion(thread.ID, result.Content, onEvent, streamedLive)
			if err == nil && thread.Title == nil {
				go a.maybeGenerateTitle(thread.ID, req.ModelAlias)
			}
			a.completePlanTurn(thread.ID, req.PlanOnly, err)
			return thread.ID, err
		}

		// Streaming turn (no tools or follow-up after tool execution).
		err = a.streamAndPersist(ctx, client, thread.ID, llmMessages, onEvent, effort)
		if err == nil && thread.Title == nil {
			go a.maybeGenerateTitle(thread.ID, req.ModelAlias)
		}
		a.completePlanTurn(thread.ID, req.PlanOnly, err)
		return thread.ID, err
	}
}

// completePlanTurn marks a successfully completed plan-mode turn as awaiting
// the user's approve/reject decision. Failures leave the thread in 'planning'
// so the next plan turn simply re-runs.
func (a *Agent) completePlanTurn(threadID string, planOnly bool, turnErr error) {
	if !planOnly || turnErr != nil || a.store == nil {
		return
	}
	_ = a.store.SetThreadPlanMode(threadID, memory.PlanModePendingApproval)
}

// ErrNoPendingPlan reports an approve call for a thread whose plan turn never
// completed (nothing is waiting for a decision).
var ErrNoPendingPlan = errors.New("no plan is awaiting approval for this thread")

// ApprovePlan accepts a thread's pending plan: the next turn injects the
// execution directive exactly once (see buildMessages).
func (a *Agent) ApprovePlan(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	if thread.PlanMode != memory.PlanModePendingApproval {
		return ErrNoPendingPlan
	}
	return a.store.SetThreadPlanMode(threadID, memory.PlanModeApproved)
}

// RejectPlan discards a thread's pending plan. It is unconditional and
// idempotent — cancelling a decision must never fail because the state moved.
func (a *Agent) RejectPlan(threadID string) error {
	if _, err := a.store.GetThread(threadID); err != nil {
		return err
	}
	return a.store.SetThreadPlanMode(threadID, memory.PlanModeOff)
}

func (a *Agent) streamAndPersist(ctx context.Context, client llm.WireClient, threadID string, messages []openai.ChatCompletionMessage, onEvent func(llm.StreamEvent) error, effort string) error {
	// Repair residual poison before the payload leaves the process (same
	// boundary as the turn loop in Chat).
	messages = sanitizeProviderMessages(messages)
	var stream <-chan llm.StreamEvent
	err := runWithLLMRetry(ctx, onEvent, func() error {
		var callErr error
		stream, callErr = client.ChatWithOptions(ctx, messages, llm.ChatOptions{Effort: effort})
		return callErr
	})
	if err != nil {
		return fmt.Errorf("start chat: %w", err)
	}

	var assistantContent strings.Builder
	for ev := range stream {
		if err := onEvent(ev); err != nil {
			return err
		}
		if ev.Type == "token" {
			assistantContent.WriteString(ev.Content)
		}
		if ev.Type == "error" {
			return fmt.Errorf("stream error: %s", ev.Content)
		}
	}

	content := assistantContent.String()

	// Check if the streamed content contains text-embedded tool calls.
	// This happens when the model emits <function=NAME> format during
	// streaming instead of using native OpenAI tool_calls. If found,
	// execute the tools and return — the caller will loop back if needed.
	if tcs := parseTextToolCalls(content); len(tcs) > 0 {
		if _, err := a.persistAssistantTurn(threadID, content, tcs); err != nil {
			return fmt.Errorf("persist assistant turn: %w", err)
		}
		// Once the assistant tool-call group is durable, every call must also
		// receive a durable result (mirrors closePending in the Chat tool loop);
		// otherwise an early return poisons the thread with an incomplete group.
		closePending := func(start int, cause error) error {
			if _, closeErr := a.persistInterruptedToolResults(threadID, tcs[start:]); closeErr != nil {
				return errors.Join(cause, fmt.Errorf("close interrupted tool-call group: %w", closeErr))
			}
			return cause
		}
		for i, tc := range tcs {
			if tc.Type != "function" {
				continue
			}
			select {
			case <-ctx.Done():
				return closePending(i, ctx.Err())
			default:
			}
			if err := onEvent(llm.StreamEvent{
				Type:       "tool_call",
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
				Arguments:  json.RawMessage(tc.Function.Arguments),
			}); err != nil {
				return closePending(i, err)
			}
			output, execErr := a.executeTool(ctx, tc.Function.Name, tc.Function.Arguments, "")
			if execErr != nil {
				output = fmt.Sprintf("error: %s", execErr.Error())
			}
			if err := onEvent(llm.StreamEvent{
				Type:       "tool_result",
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
				Content:    output,
			}); err != nil {
				return closePending(i, err)
			}
			if _, err := a.store.AppendMessage(threadID, "tool", &output, &tc.ID); err != nil {
				return closePending(i, fmt.Errorf("persist tool result: %w", err))
			}
		}
		return onEvent(llm.StreamEvent{Type: "done", ThreadID: threadID, Content: threadID})
	}

	if content != "" {
		if _, err := a.store.AppendMessage(threadID, "assistant", &content, nil); err != nil {
			return fmt.Errorf("persist assistant message: %w", err)
		}
	}
	// ThreadID is the canonical schema field; Content mirrors it for
	// backward-compatible consumers.
	return onEvent(llm.StreamEvent{Type: "done", ThreadID: threadID, Content: threadID})
}

// emitAndPersistCompletion finalizes a buffered completion. Tool-capable
// turns use Client.Complete so one provider response can carry native tool
// calls or terminal text; emitting the text as one token event keeps the
// public event contract without another model request. alreadyStreamed skips
// that emission when the same content reached onEvent as live deltas.
func (a *Agent) emitAndPersistCompletion(threadID, content string, onEvent func(llm.StreamEvent) error, alreadyStreamed bool) error {
	if content != "" {
		if !alreadyStreamed {
			if err := onEvent(llm.StreamEvent{Type: "token", Content: content}); err != nil {
				return err
			}
		}
		if _, err := a.store.AppendMessage(threadID, "assistant", &content, nil); err != nil {
			return fmt.Errorf("persist assistant message: %w", err)
		}
	}
	return onEvent(llm.StreamEvent{Type: "done", ThreadID: threadID, Content: threadID})
}

// liveEventSender adapts the agent's onEvent callback to the llm package's
// push-style event sender. A failed send stops forwarding, never the request.
func liveEventSender(onEvent func(llm.StreamEvent) error) func(llm.StreamEvent) bool {
	return func(ev llm.StreamEvent) bool { return onEvent(ev) == nil }
}

func (a *Agent) persistAssistantTurn(threadID, content string, toolCalls []openai.ToolCall) (*memory.Message, error) {
	pending := make([]memory.AssistantToolCall, len(toolCalls))
	for i, tc := range toolCalls {
		pending[i] = memory.AssistantToolCall{
			ID:        tc.ID,
			ToolName:  tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}
	}
	return a.store.AppendAssistantMessageWithToolCalls(threadID, strPtr(content), pending)
}

func validateNewToolCallGroup(history []indexedMessage, content string, toolCalls []openai.ToolCall) error {
	candidate := append([]openai.ChatCompletionMessage(nil), toRawMessages(history)...)
	candidate = append(candidate, openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		Content:   content,
		ToolCalls: toolCalls,
	})
	for _, tc := range toolCalls {
		candidate = append(candidate, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			ToolCallID: tc.ID,
			Content:    "pending tool-call validation",
		})
	}
	return validateProviderPayload(candidate)
}

// persistInterruptedToolResults seals every still-pending call in a durable
// assistant tool-call group. Providers require an immediately following result
// for every call ID, so returning without these records would make the thread
// impossible to resume safely. Returns the persisted messages so callers can
// mirror them into an in-memory view.
func (a *Agent) persistInterruptedToolResults(threadID string, pending []openai.ToolCall) ([]memory.Message, error) {
	var persisted []memory.Message
	for _, tc := range pending {
		toolCallID := tc.ID
		content := interruptedToolResult
		msg, err := a.store.AppendMessage(threadID, openai.ChatMessageRoleTool, &content, &toolCallID)
		if err != nil {
			return persisted, fmt.Errorf("persist closing result for tool call %q: %w", toolCallID, err)
		}
		persisted = append(persisted, *msg)
	}
	return persisted, nil
}

func (a *Agent) executeTool(ctx context.Context, name, arguments, workspace string) (string, error) {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse tool arguments: %w", err)
	}
	if args == nil {
		args = make(map[string]interface{})
	}
	// Model-generated workspace arguments are untrusted; carry the
	// request-selected workspace through context only, so tools cannot escape
	// the server/CLI boundary even if schema validation is bypassed upstream.
	delete(args, "workspace")
	if workspace != "" {
		ctx = tools.WithWorkspace(ctx, workspace)
	}
	return a.tools.Execute(ctx, name, args)
}

// buildMessages assembles the indexed message view for a turn. onEvent, when
// non-nil, receives a one-time "intermediate" notice if the saved compression
// summary cannot be loaded (the turn continues with the full history).
// tropicalModeDirective is appended to the system prompt while Tropical mode
// is engaged — sandbar's adaptation of the "ultracode" tier other harnesses
// ship: maximum effort plus explicit pressure to parallelize through
// subagents instead of grinding through work serially.
const tropicalModeDirective = `
# Tropical Mode

The user has engaged Tropical mode: treat this task as large and important.
- Reason at maximum depth; verify assumptions before acting on them.
- Parallelize aggressively with delegate_task. Spawn independent subagents
  for research, exploration, and implementation of separable components —
  several focused subagents beat one long serial pass.
- Before reporting completion, verify the combined work with a fresh
  subagent rather than trusting your own summary of it.
`

func (a *Agent) buildMessages(threadID, workspace string, source string, planOnly bool, onEvent func(llm.StreamEvent) error, tropical bool) ([]indexedMessage, error) {
	// Load latest compression record and inject summary if valid.
	var compRec *memory.CompressionRecord
	if a.store != nil {
		var loadErr error
		compRec, loadErr = a.store.GetLatestCompression(threadID)
		if loadErr != nil && onEvent != nil {
			a.compressionLoadNoticeOnce.Do(func() {
				_ = onEvent(llm.StreamEvent{Type: "intermediate", Content: "compression: " + loadErr.Error() + " (continuing with full history)"})
			})
		}
	}

	var thread *memory.Thread
	var history []memory.Message
	var err error
	if compRec != nil && compRec.FirstKeptSeq > 0 {
		thread, history, err = a.store.GetThreadWithMessagesFromSeq(threadID, compRec.FirstKeptSeq)
		if err != nil || len(history) == 0 || history[0].Seq != compRec.FirstKeptSeq {
			// Invalid or dangling compression record; fall back to full thread.
			thread, history, err = a.store.GetThreadWithMessages(threadID)
			compRec = nil
		}
	} else {
		thread, history, err = a.store.GetThreadWithMessages(threadID)
	}
	if err != nil {
		return nil, fmt.Errorf("load thread messages: %w", err)
	}

	// System prompt = persona + runtime environment block; the active model
	// comes from the thread when one is recorded.
	model := ""
	if thread != nil && thread.Model != nil {
		model = *thread.Model
	}
	// Prompt-file layering (persona/promptfiles.go): SYSTEM.md replaces the
	// configured base persona only — environment block, project context, and
	// skills are still assembled around it; APPEND_SYSTEM.md is appended after.
	// Discovery is project-first over user scope.
	promptFiles := persona.DiscoverPromptFiles(workspace, persona.UserConfigDir())
	basePrompt := a.cfg.Persona.SystemPrompt
	if promptFiles.System != "" {
		basePrompt = persona.RenderPrompt(promptFiles.System, workspace)
	}
	p := persona.Persona{
		Name:         a.cfg.Persona.Name,
		SystemPrompt: basePrompt,
	}
	sysPrompt := p.BuildSystemPrompt(workspace, model)
	if promptFiles.Append != "" {
		sysPrompt += "\n\n" + persona.RenderPrompt(promptFiles.Append, workspace)
	}
	if tropical {
		sysPrompt += "\n" + tropicalModeDirective
	}
	subagentsAvailable := false
	if a.tools != nil {
		_, delegateErr := a.tools.Get("delegate_task")
		_, resumeErr := a.tools.Get("resume_task")
		subagentsAvailable = delegateErr == nil && resumeErr == nil
	}
	if !subagentsAvailable {
		sysPrompt += "\n\n" + subagentsUnavailablePrompt
	}
	if source == "cli" {
		sysPrompt += "\n\n" + cliFormattingPrompt
	}
	if planOnly {
		sysPrompt += "\n\n" + planModePrompt
	}

	msgs := []indexedMessage{
		{
			Seq:       0,
			Synthetic: true,
			Kind:      "system",
			Msg:       openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
		},
	}

	// An approved plan gets exactly one execution directive on the turn after
	// approval; clearing the persisted state here keeps it one-shot. Synthetic
	// and never persisted, like the system message itself.
	if thread != nil && thread.PlanMode == memory.PlanModeApproved {
		content := planApprovedPrompt
		if a.store != nil {
			if todos, todoErr := a.store.ListTodos(threadID); todoErr == nil && len(todos) > 0 {
				content += "\n" + tools.RenderTodos(todos)
			}
			_ = a.store.SetThreadPlanMode(threadID, memory.PlanModeOff)
		}
		msgs = append(msgs, indexedMessage{
			Seq:       0,
			Synthetic: true,
			Kind:      "plan_approved",
			Msg: openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: content,
			},
		})
	}

	// Re-inject the durable task list at turn start as a synthetic reminder
	// right after the system prompt; long threads whose early turns were
	// compressed away otherwise lose sight of the plan. Never persisted.
	if a.store != nil {
		if todos, todoErr := a.store.ListTodos(threadID); todoErr == nil && len(todos) > 0 {
			msgs = append(msgs, indexedMessage{
				Seq:       0,
				Synthetic: true,
				Kind:      "todo_reminder",
				Msg: openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleUser,
					Content: "[Task list for this thread — keep it current as you work:\n" + tools.RenderTodos(todos) + "]",
				},
			})
		}
	}

	if compRec != nil {
		msgs = append(msgs, indexedMessage{
			Seq:       0,
			Synthetic: true,
			Kind:      "compression_summary",
			Msg: openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: "[Compressed context from earlier: " + compRec.Summary + "]",
			},
		})
	}

	for _, m := range history {
		msg := openai.ChatCompletionMessage{Role: m.Role}
		if m.Content != nil {
			msg.Content = *m.Content
		}
		if m.ToolCallID != nil {
			msg.ToolCallID = *m.ToolCallID
		}
		if m.Role == "assistant" {
			tcs, err := a.store.GetToolCallsForMessage(m.ID)
			if err != nil {
				return nil, fmt.Errorf("load tool calls for message %d: %w", m.ID, err)
			}
			for _, tc := range tcs {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.ToolName,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		msgs = append(msgs, indexedMessage{
			Seq:       m.Seq,
			Synthetic: false,
			Kind:      "thread_message",
			Msg:       msg,
		})
	}

	// Self-heal crash residue: when the final persisted group is an assistant
	// tool-call group missing results, the interrupted results are persisted
	// now, appending where they belong. Mid-history dangling groups are
	// repaired in-memory only; persisted sequences are never rewritten.
	if missing := trailingUnansweredToolCalls(msgs); len(missing) > 0 {
		closed, err := a.persistInterruptedToolResults(threadID, missing)
		if err != nil {
			return nil, fmt.Errorf("seal trailing tool-call group: %w", err)
		}
		for _, m := range closed {
			toolMsg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool}
			if m.Content != nil {
				toolMsg.Content = *m.Content
			}
			if m.ToolCallID != nil {
				toolMsg.ToolCallID = *m.ToolCallID
			}
			msgs = append(msgs, indexedMessage{
				Seq:       m.Seq,
				Synthetic: false,
				Kind:      "thread_message",
				Msg:       toolMsg,
			})
		}
	}

	// Repair the provider-facing view in-memory so every assistant tool-call
	// group is sealed and the validator never sees poison from a past turn.
	return sanitizeIndexedMessages(msgs), nil
}

func truncateMessages(msgs []openai.ChatCompletionMessage, contextLength int) []openai.ChatCompletionMessage {
	return toRawMessages(truncateIndexedMessages(wrapMessages(msgs), contextLength))
}

func (a *Agent) buildToolSchemas() []openai.Tool {
	if a.tools == nil {
		return nil
	}
	var schemas []openai.Tool
	for _, name := range a.tools.List() {
		tool, _ := a.tools.Get(name)
		schemas = append(schemas, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Schema,
			},
		})
	}
	return schemas
}

// maybeGenerateTitle names a fresh thread. When a TITLE_SYSTEM.md prompt
// file is configured (persona/promptfiles.go), the title is rendered
// locally from the template over the first user message — no model call,
// matching the offline posture of the rest of the layering. Otherwise the
// LLM summarizer runs (RegenerateTitle).
func (a *Agent) maybeGenerateTitle(threadID, modelAlias string) {
	if a.titleFromTemplate(threadID) {
		return
	}
	a.RegenerateTitle(threadID, modelAlias)
}

// titleFromTemplate renders TITLE_SYSTEM.md over the thread's first user
// message and records the result as the title. It reports whether a title
// template existed (and was applied or found nothing to render), so callers
// can skip the LLM summarizer.
func (a *Agent) titleFromTemplate(threadID string) bool {
	promptFiles := persona.DiscoverPromptFiles(a.cfg.Workspace, persona.UserConfigDir())
	if promptFiles.Title == "" {
		return false
	}
	if a.store == nil {
		return true
	}
	_, messages, err := a.store.GetThreadWithMessages(threadID)
	if err != nil {
		return true
	}
	for _, m := range messages {
		if m.Role == "user" && m.Content != nil {
			if title := persona.RenderTitle(promptFiles.Title, *m.Content, a.cfg.Workspace); title != "" {
				_ = a.store.SetThreadTitle(threadID, title)
			}
			break
		}
	}
	return true
}

// RegenerateTitle re-runs the LLM summarizer and unconditionally overwrites the thread title.
func (a *Agent) RegenerateTitle(threadID, modelAlias string) {
	titleModel := a.cfg.Persona.TitleModel
	if titleModel == "" {
		titleModel = modelAlias
	}
	resolved, err := a.registry.ResolveModel(titleModel)
	if err != nil {
		return
	}

	_, messages, err := a.store.GetThreadWithMessages(threadID)
	if err != nil {
		return
	}
	if len(messages) < 2 {
		return
	}

	var sb strings.Builder
	for _, m := range messages {
		if m.Content != nil {
			sb.WriteString(fmt.Sprintf("%s: %s\n", m.Role, *m.Content))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := llm.NewWireClient(resolved)
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "Summarize the following conversation in 5 words or less. Reply with only the title, no quotes."},
		{Role: openai.ChatMessageRoleUser, Content: sb.String()},
	}
	result, err := client.Complete(ctx, msgs, nil)
	if err != nil {
		return
	}
	title := strings.TrimSpace(result.Content)
	if title == "" {
		return
	}
	_ = a.store.SetThreadTitle(threadID, title)
}

func strPtr(s string) *string {
	return &s
}

// ResumeSubagent implements tools.SubagentRunner.
func (a *Agent) ResumeSubagent(ctx context.Context, taskID string) (<-chan tools.SubagentEvent, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not available: cannot resume subagent task without database")
	}

	var id, goal, contextStr, modelAlias, messagesJSON, status, result string
	var turn, maxTurns int
	err := a.store.DB().QueryRow(
		`SELECT id, goal, context, model_alias, messages_json, turn, max_turns, status, result
		 FROM subagent_tasks WHERE id = ?`, taskID,
	).Scan(&id, &goal, &contextStr, &modelAlias, &messagesJSON, &turn, &maxTurns, &status, &result)
	if err != nil {
		return nil, fmt.Errorf("load subagent task %s: %w", taskID, err)
	}

	if status == "completed" {
		// Resuming a finished task is not an error: hand back the stored
		// result so the parent gets the answer instead of a failure it
		// cannot recover from (which made models re-delegate identical work).
		ch := make(chan tools.SubagentEvent, 1)
		content := strings.TrimSpace(result)
		if content == "" {
			content = "[Subagent completed with no output]"
		}
		ch <- tools.SubagentEvent{Type: "done", Content: "[Task already completed — returning stored result]\n\n" + content, TaskID: taskID, Goal: goal}
		close(ch)
		return ch, nil
	}
	if status != "interrupted" && status != "running" && status != "failed" {
		return nil, fmt.Errorf("subagent task %s is in status %q and cannot be resumed", taskID, status)
	}

	resolved, err := a.registry.ResolveModel(modelAlias)
	if err != nil {
		return nil, fmt.Errorf("resolve subagent model %s: %w", modelAlias, err)
	}

	client := llm.NewWireClient(resolved)

	allowedTools := a.cfg.Subagent.Tools
	if len(allowedTools) == 0 {
		allowedTools = []string{"file_read", "search_files", "web_search", "web_fetch", "shell_exec"}
	}
	workspace := tools.WorkspaceFromContext(ctx)
	if workspace == "" {
		workspace = a.cfg.Workspace
	}
	filteredTools := tools.NewRegistry(workspace, a.cfg.Tools.WebSearch.BraveAPIKey, "", a.cfg.Tools.Shell.BlockedCommands)
	availableTools := make(map[string]tools.Tool, len(allowedTools))
	for _, name := range allowedTools {
		if tool, getErr := filteredTools.Get(name); getErr == nil {
			availableTools[name] = tool
		}
	}
	filteredTools.Clear()
	if a.tools != nil {
		approvalConfig := a.tools.ApprovalConfig()
		// Same as spawn: deny stays deny; prompt policies degrade to allow.
		approvalConfig.Mode = tools.ApprovalModeYolo
		approvalConfig = tools.RewritePromptToAllow(approvalConfig)
		_ = filteredTools.SetApprovalConfig(approvalConfig)
	}
	filteredTools.SetPlanStore(a.store)
	if shellTimeout, timeoutErr := a.cfg.ShellTimeout(); timeoutErr == nil {
		_ = filteredTools.SetShellTimeout(shellTimeout)
	}
	if jobs, jobsErr := a.cfg.ShellJobSettings(); jobsErr == nil {
		_ = filteredTools.SetJobSupervisorConfig(tools.JobSupervisorConfig{
			MaxJobs: jobs.MaxJobs, MaxRunning: jobs.MaxRunning, OutputBytes: jobs.OutputBytes,
			Retention: jobs.Retention, TerminationGrace: jobs.TerminationGrace,
		})
	}
	// Adopt the parent's job supervisor so DeleteThread's CancelThreadJobs
	// reaches shell work this subagent starts (a fresh registry would own
	// an unreachable supervisor of its own).
	if a.tools != nil {
		if parentJobs := a.tools.JobSupervisor(); parentJobs != nil {
			_ = filteredTools.SetJobSupervisor(parentJobs)
		}
	}
	if sshSettings, sshErr := a.cfg.SSHSettings(); sshErr == nil {
		_ = filteredTools.SetSSHConfig(tools.SSHRuntimeConfig{
			ConnectTimeout: sshSettings.ConnectTimeout,
			BatchMode:      sshSettings.BatchMode,
			AllowedHosts:   sshSettings.AllowedHosts,
		})
	}
	for _, name := range allowedTools {
		if tool, ok := availableTools[name]; ok {
			filteredTools.Register(tool)
		}
	}

	var messages []openai.ChatCompletionMessage
	if err := json.Unmarshal([]byte(messagesJSON), &messages); err != nil {
		return nil, fmt.Errorf("deserialize subagent messages: %w", err)
	}

	// A zero maxTurns means unbounded (remainingTurns stays 0); a positive cap
	// already exhausted gets a fresh defaultResumeTurns budget rather than a
	// single throwaway turn.
	remainingTurns := 0
	if maxTurns > 0 {
		remainingTurns = maxTurns - turn
		if remainingTurns <= 0 {
			remainingTurns = defaultResumeTurns
		}
	}

	// Guard against two concurrent resumes of the same task re-executing the
	// same work; held for the duration of the resumed run.
	a.resumeMu.Lock()
	if a.resuming == nil {
		a.resuming = make(map[string]bool)
	}
	if a.resuming[taskID] {
		a.resumeMu.Unlock()
		return nil, fmt.Errorf("subagent task %s is already resuming", taskID)
	}
	a.resuming[taskID] = true
	a.resumeMu.Unlock()

	ch := make(chan tools.SubagentEvent, 64)

	go func() {
		defer close(ch)
		defer func() {
			a.resumeMu.Lock()
			delete(a.resuming, taskID)
			a.resumeMu.Unlock()
		}()

		// runningTurn is the cumulative turn count across runs, persisted on
		// each result so repeated resumes deduct consumed budget correctly.
		runningTurn := turn
		ft := newFileTracker()
		var partial strings.Builder
		if result != "" {
			partial.WriteString(result)
		}

		send := func(ev tools.SubagentEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		sendToken := func(content string) bool {
			partial.WriteString(content)
			return send(tools.SubagentEvent{Type: "token", Content: content})
		}
		sendError := func(content string, err error) {
			partialWithManifest := partial.String()
			if manifest := ft.Manifest(); manifest != "" {
				partialWithManifest += manifest
			}
			errorPartial := sanitizeSubagentOutput(partialWithManifest)
			send(tools.SubagentEvent{Type: "error", Content: content, Partial: errorPartial, Err: err})
			status := "failed"
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				status = "interrupted"
			}
			messagesJSON, _ := json.Marshal(messages)
			resultStr := partialWithManifest
			if len(resultStr) > 4000 {
				resultStr = resultStr[:4000]
			}
			_, _ = a.store.DB().Exec(
				`UPDATE subagent_tasks SET status = ?, result = ?, messages_json = ?, turn = ?, files_read = ?, files_written = ?, updated_at = ? WHERE id = ?`,
				status, resultStr, string(messagesJSON), runningTurn, marshalStringsJSON(ft.FilesRead()), marshalStringsJSON(ft.FilesWritten()), time.Now().Unix(), taskID,
			)
		}
		sendDone := func() bool {
			resultFinal := partial.String()
			if manifest := ft.Manifest(); manifest != "" {
				resultFinal += manifest
			}
			resultFinal = sanitizeSubagentOutput(resultFinal)
			sent := send(tools.SubagentEvent{Type: "done", Content: resultFinal})
			// Persist completed state unconditionally (see the spawn path's
			// note): a cancelled parent may deliver the done event but skip the
			// UPDATE, leaving status 'running' and forcing a resume to re-execute
			// completed work.
			messagesJSON, _ := json.Marshal(messages)
			resultStr := resultFinal
			if len(resultStr) > 4000 {
				resultStr = resultStr[:4000]
			}
			_, _ = a.store.DB().Exec(
				`UPDATE subagent_tasks SET status = ?, result = ?, messages_json = ?, turn = ?, files_read = ?, files_written = ?, updated_at = ? WHERE id = ?`,
				"completed", resultStr, string(messagesJSON), runningTurn, marshalStringsJSON(ft.FilesRead()), marshalStringsJSON(ft.FilesWritten()), time.Now().Unix(), taskID,
			)
			return sent
		}

		var loopGuard toolLoopGuard
		// Inherit the parent turn's reasoning effort via the resume_task context.
		effort := tools.EffortFromContext(ctx)
		for turnCount := 0; remainingTurns == 0 || turnCount < remainingTurns; turnCount++ {
			runningTurn++
			if resolved.SupportsTools && filteredTools != nil {
				openaiTools := buildToolSchemasFrom(filteredTools)
				messages = sanitizeProviderMessages(messages)
				var compResult *llm.CompletionResult
				compErr := runWithLLMRetry(ctx, func(ev llm.StreamEvent) error {
					if ev.Type == "intermediate" {
						send(tools.SubagentEvent{Type: "notice", Content: ev.Content})
					}
					return nil
				}, func() error {
					var callErr error
					compResult, callErr = client.CompleteWithOptions(ctx, messages, llm.CompleteOptions{Tools: openaiTools, Effort: effort})
					return callErr
				})
				if compErr != nil {
					sendError(fmt.Sprintf("subagent LLM error: %v", compErr), compErr)
					return
				}

				if len(compResult.ToolCalls) > 0 {
					loopDecision := loopGuard.Observe(compResult.ToolCalls)
					messages = append(messages, openai.ChatCompletionMessage{
						Role:      openai.ChatMessageRoleAssistant,
						Content:   compResult.Content,
						ToolCalls: compResult.ToolCalls,
					})

					for _, tc := range compResult.ToolCalls {
						if tc.Type != openai.ToolTypeFunction {
							continue
						}
						if !send(tools.SubagentEvent{Type: "tool_call", Tool: tc.Function.Name, Args: tc.Function.Arguments}) {
							return
						}
					}

					outputs := executeToolCallBatchWith(ctx, compResult.ToolCalls, loopDecision, filteredTools.CanRunConcurrently, func(callCtx context.Context, tc openai.ToolCall) string {
						if tc.Type != openai.ToolTypeFunction {
							return fmt.Sprintf("error: unsupported tool call type %q", tc.Type)
						}
						var args map[string]interface{}
						if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
							return fmt.Sprintf("error: parse tool arguments: %s", err.Error())
						}
						if args == nil {
							args = make(map[string]interface{})
						}
						ft.trackFileOp(tc.Function.Name, args)
						if tc.Function.Name == "shell_exec" {
							if cmd, _ := args["command"].(string); cmd != "" {
								ft.trackShellCmd(cmd)
							}
						}
						if tc.Function.Name == "web_fetch" {
							if url, _ := args["url"].(string); url != "" {
								ft.trackURL(url)
							}
						}
						if tc.Function.Name == "web_search" {
							if query, _ := args["query"].(string); query != "" {
								ft.trackURL("search: " + query)
							}
						}
						callCtx = tools.WithToolCallID(callCtx, tc.ID)
						output, execErr := filteredTools.Execute(callCtx, tc.Function.Name, args)
						if execErr != nil {
							return fmt.Sprintf("error: %s", execErr.Error())
						}
						return output
					})

					for i, tc := range compResult.ToolCalls {
						output := outputs[i]
						if !send(tools.SubagentEvent{Type: "tool_result", Tool: tc.Function.Name, Content: truncateStr(output, 300)}) {
							return
						}
						messages = append(messages, openai.ChatCompletionMessage{
							Role:       "tool",
							Content:    output,
							ToolCallID: tc.ID,
						})
					}
					if loopDecision.Abort {
						sendError("repeated tool-call loop after resume", ErrRepeatedToolCallLoop)
						return
					}
					continue
				}

				if compResult.Content != "" {
					if !sendToken(compResult.Content) {
						return
					}
					// Persist the final assistant text so a later resume replays
					// a complete conversation.
					messages = append(messages, openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleAssistant,
						Content: compResult.Content,
					})
				}
				sendDone()
				return
			}

			// No tool support — just stream.
			messages = sanitizeProviderMessages(messages)
			stream, streamErr := client.Chat(ctx, messages)
			if streamErr != nil {
				sendError(streamErr.Error(), streamErr)
				return
			}
			for ev := range stream {
				if ev.Type == "error" {
					sendError(ev.Content, nil)
					return
				}
				if ev.Type == "token" {
					if !sendToken(ev.Content) {
						return
					}
				}
			}
			sendDone()
			return
		}

		sendError("max subagent turns reached after resume", nil)
	}()

	return ch, nil
}
