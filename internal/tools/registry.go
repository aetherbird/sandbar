package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Tool describes a callable tool.
type Tool struct {
	Name         string
	Description  string
	Schema       map[string]interface{}
	ParallelSafe bool // handler is read-only or isolated and may share a batch
	Metadata     ToolMetadata
	Execute      func(ctx context.Context, args map[string]interface{}) (string, error)
}

// Registry holds all registered tools.
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	order    []string
	shell    *ShellExec     // reference for CancelActiveTool
	files    *FileTools     // reference for agent:// transcript wiring
	runner   SubagentRunner // reference for delegate_task
	plans    PlanStore      // durable per-thread todo plans
	approval ApprovalConfig
	handler  ApprovalHandler
	observer ExecutionObserver
}

type registryOptions struct {
	subagents bool
}

// RegistryOption customizes the built-in tools registered for one runtime.
// Options are invocation-scoped; the default registry remains unchanged.
type RegistryOption func(*registryOptions)

// WithoutSubagents omits both subagent entry points from the registry. Because
// schemas and execution share this registry, delegate_task and resume_task are
// neither advertised to the model nor dispatchable by that runtime.
func WithoutSubagents() RegistryOption {
	return func(options *registryOptions) {
		options.subagents = false
	}
}

// NewRegistry creates a tool registry with all built-in tools.
func NewRegistry(workspace, braveAPIKey, openrouterAPIKey string, blockedCommands []string, options ...RegistryOption) *Registry {
	opts := registryOptions{subagents: true}
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	r := &Registry{
		tools:    make(map[string]Tool),
		approval: DefaultApprovalConfig(),
	}
	ft := NewFileTools(workspace)
	r.files = ft
	se := NewShellExec(workspace, blockedCommands)
	r.shell = se

	r.Register(Tool{
		Name:         "file_read",
		Description:  "Read a file and return its full-content SHA-256; each line is prefixed with an 8-hex content hash usable as a file_patch anchor. URL-like paths resolve before file reads: pr://<n> and issue://<n> (GitHub via the user's gh CLI, owner/repo-qualified forms accepted) and agent://<task-id> (a persisted subagent transcript)",
		ParallelSafe: true,
		Metadata:     fileToolMetadata(TierRead, "read"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string"},
				"max_bytes": map[string]interface{}{
					"type": "integer", "minimum": 1, "maximum": absoluteMaxBytes,
					"description": "Maximum displayed content bytes. The SHA-256 always covers the full file.",
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
		Execute: ft.FileRead,
	})
	r.Register(Tool{
		Name:        "file_write",
		Description: "Atomically write a file when its SHA-256 precondition still matches",
		Metadata:    fileToolMetadata(TierWrite, "write"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":            map[string]interface{}{"type": "string"},
				"content":         map[string]interface{}{"type": "string"},
				"expected_sha256": filePreconditionSchema(),
			},
			"required":             []string{"path", "content", "expected_sha256"},
			"additionalProperties": false,
		},
		Execute: ft.FileWrite,
	})
	r.Register(Tool{
		Name:        "file_append",
		Description: "Atomically append to a file when its SHA-256 precondition still matches",
		Metadata:    fileToolMetadata(TierWrite, "append"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":            map[string]interface{}{"type": "string"},
				"content":         map[string]interface{}{"type": "string"},
				"expected_sha256": filePreconditionSchema(),
			},
			"required":             []string{"path", "content", "expected_sha256"},
			"additionalProperties": false,
		},
		Execute: ft.FileAppend,
	})
	r.Register(Tool{
		Name:        "file_patch",
		Description: "Atomically edit a file by replacing text when its SHA-256 precondition still matches. When old_str is hashline-formatted (lines copied verbatim from file_read output), lines are selected by content anchor and stale anchors are rejected with current hashes",
		Metadata:    fileToolMetadata(TierWrite, "patch"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":            map[string]interface{}{"type": "string"},
				"old_str":         map[string]interface{}{"type": "string"},
				"new_str":         map[string]interface{}{"type": "string"},
				"expected_sha256": existingFilePreconditionSchema(),
			},
			"required":             []string{"path", "old_str", "new_str", "expected_sha256"},
			"additionalProperties": false,
		},
		Execute: ft.FilePatch,
	})
	r.Register(Tool{
		Name:        "shell_exec",
		Description: "Run a shell command in the workspace and return its exit code, standard output, and standard error.",
		Metadata:    shellToolMetadata(),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"minLength":   1,
					"description": "Shell command to execute with bash in the workspace.",
				},
				"host": map[string]interface{}{
					"type": "string",
					"description": "Run the command on a remote host over SSH instead of locally. " +
						"The command executes as-is under bash on that host — pass a plain command; " +
						"do not embed ssh, nested quotes, or python -c wrappers in the command.",
				},
				"timeout_seconds": map[string]interface{}{
					"type":        "integer",
					"minimum":     1,
					"description": "Maximum execution time in seconds. Defaults to 30.",
				},
				"async": map[string]interface{}{
					"type":        "boolean",
					"description": "Start an explicitly managed background job and return its job ID immediately.",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
		Execute: se.Execute,
	})
	r.Register(Tool{
		Name:        "job",
		Description: "Inspect, wait for, tail, or cancel explicitly managed background shell jobs.",
		Metadata:    jobToolMetadata(),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type": "string", "enum": []string{"list", "status", "tail", "wait", "cancel"},
					"description": "Job operation. job_id is required for every action except list.",
				},
				"job_id": map[string]interface{}{
					"type": "string", "description": "Managed job ID returned by shell_exec with async:true.",
				},
				"max_bytes": map[string]interface{}{
					"type": "integer", "minimum": 1, "description": "Maximum retained tail bytes to return.",
				},
				"timeout_seconds": map[string]interface{}{
					"type": "number", "exclusiveMinimum": 0, "description": "Maximum time to wait for a job state change.",
				},
			},
			"required":             []string{"action"},
			"additionalProperties": false,
		},
		Execute: se.JobTool,
	})
	r.Register(Tool{
		Name:        "git",
		Description: "Inspect or modify the Git repository in the workspace. For add, paths is required; for commit, message is required.",
		Metadata:    gitToolMetadata(),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"status", "diff", "add", "commit"},
					"description": "Git operation to perform.",
				},
				"repo_path": map[string]interface{}{
					"type":        "string",
					"description": "Workspace-relative path to the repository. Defaults to the workspace root.",
				},
				"staged": map[string]interface{}{
					"type":        "boolean",
					"description": "For diff only, show staged changes instead of unstaged changes.",
				},
				"paths": map[string]interface{}{
					"type":        "array",
					"minItems":    1,
					"description": "For add only, non-empty repository-relative paths to stage.",
					"items": map[string]interface{}{
						"type":      "string",
						"minLength": 1,
					},
				},
				"message": map[string]interface{}{
					"type":        "string",
					"minLength":   1,
					"description": "For commit only, the commit message.",
				},
			},
			"required":             []string{"action"},
			"additionalProperties": false,
		},
		Execute: gitDispatchInWorkspace(workspace),
	})
	r.Register(Tool{
		Name:         "web_search",
		Description:  "Search the web",
		ParallelSafe: true,
		Metadata:     argumentToolMetadata(TierRead, "search", "query"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string"},
			},
			"required": []string{"query"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			args["brave_api_key"] = braveAPIKey
			return WebSearch(ctx, args)
		},
	})
	r.Register(Tool{
		Name:         "search_files",
		Description:  "Search workspace files by regular-expression content or case-insensitive filename using ripgrep. Respects .gitignore.",
		ParallelSafe: true,
		Metadata:     argumentToolMetadata(TierRead, "search", "path"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"minLength":   1,
					"description": "Regular expression to match for content searches, or filename substring for file searches.",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Directory to search, relative to the workspace unless absolute. Defaults to the workspace root.",
				},
				"target": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"content", "files"},
					"description": "Search file contents or filenames. Defaults to content.",
				},
				"file_glob": map[string]interface{}{
					"type":        "string",
					"description": "Ripgrep glob limiting files included in either search mode, such as '*.go'.",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"minimum":     1,
					"maximum":     200,
					"description": "Maximum number of result lines to return. Defaults to 50.",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
		Execute: searchFilesInWorkspace(workspace),
	})
	r.Register(Tool{
		Name:         "web_fetch",
		Description:  "Fetch a web page and return its readable text content.",
		ParallelSafe: true,
		Metadata:     argumentToolMetadata(TierRead, "fetch", "url"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"format":      "uri",
					"description": "Absolute HTTP or HTTPS URL to fetch.",
				},
				"max_chars": map[string]interface{}{
					"type":        "integer",
					"minimum":     1000,
					"maximum":     200000,
					"description": "Maximum extracted characters to return. Defaults to 50000.",
				},
			},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
		Execute: WebFetch,
	})
	r.Register(Tool{
		Name:        "todo",
		Description: "Manage the task list for the current conversation. The items array is required for create, update, complete, and cancel; omit it only for list.",
		Metadata:    todoToolMetadata(),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"create", "update", "complete", "cancel", "list"},
					"description": "Operation to perform. Use list without items; all other actions require a non-empty items array.",
				},
				"items": map[string]interface{}{
					"type":        "array",
					"minItems":    1,
					"description": "Todo entries. create requires content; update requires id plus content and/or status; complete and cancel require id.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id": map[string]interface{}{
								"type":        "string",
								"description": "Existing todo ID; required for update, complete, and cancel.",
							},
							"content": map[string]interface{}{
								"type":        "string",
								"minLength":   1,
								"description": "Todo text; required for create and optional for update.",
							},
							"status": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"pending", "in_progress", "completed", "cancelled"},
								"description": "New status; optional for update.",
							},
						},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"action"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			return r.todo(ctx, args)
		},
	})
	if opts.subagents {
		r.Register(Tool{
			Name:         "delegate_task",
			Description:  "Delegate an independent subtask to a sub-agent and wait for its summary. Send multiple delegate_task calls in ONE batch when the subtasks are independent — they run concurrently. Do not delegate small bounded lookups you can do in one or two calls, and do not fan out multiple sub-agents onto one small task. The sub-agent sees nothing of this conversation: give it a self-contained briefing (objective, relevant paths/commands, constraints, and the exact shape of the summary you want back). Trust but verify its summary against the actual files or output. If a sub-agent is interrupted, resume_task with its task_id instead of re-delegating from scratch.",
			ParallelSafe: true,
			Metadata:     argumentToolMetadata(TierExec, "delegate", "goal"),
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"goal": map[string]interface{}{
						"type":        "string",
						"minLength":   1,
						"description": "Self-contained objective for the sub-agent: what to do, the paths/commands to use, constraints to honor, and the exact shape of the summary to return.",
					},
					"context": map[string]interface{}{
						"type":        "string",
						"description": "Relevant background, constraints, or findings the sub-agent should use.",
					},
				},
				"required":             []string{"goal"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return r.delegate(ctx, args)
			},
		})
		r.Register(Tool{
			Name:        "resume_task",
			Description: "Resume a previously-interrupted or failed sub-agent task by its task_id. Use this to continue delegated work that was cut off.",
			Metadata:    argumentToolMetadata(TierExec, "resume", "task_id"),
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "string",
						"description": "The task ID returned by a previous delegate_task call that was interrupted or failed.",
					},
				},
				"required":             []string{"task_id"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return r.resume(ctx, args)
			},
		})
	}
	r.Register(Tool{
		Name:        "image_generate",
		Description: "Generate an image from a prompt",
		Metadata:    argumentToolMetadata(TierExec, "generate", "prompt"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prompt": map[string]interface{}{"type": "string"},
			},
			"required": []string{"prompt"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			args["openrouter_api_key"] = openrouterAPIKey
			// The agent injects the request-scoped workspace into ctx; only
			// fall back to the registry-configured one when it didn't.
			if WorkspaceFromContext(ctx) == "" {
				ctx = WithWorkspace(ctx, workspace)
			}
			return ImageGenerate(ctx, args)
		},
	})
	r.Register(Tool{
		Name:         "vision_analyze",
		Description:  "Send an image to the configured vision provider for analysis",
		ParallelSafe: true,
		// Vision analysis sends local image content to an external provider.
		Metadata: argumentToolMetadata(TierExec, "analyze-external", "image_path"),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"image_path": map[string]interface{}{"type": "string"},
			},
			"required": []string{"image_path"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			args["openrouter_api_key"] = openrouterAPIKey
			return VisionAnalyze(ctx, args)
		},
	})
	return r
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	r.tools[t.Name] = t
	r.order = append(r.order, t.Name)
}

// Clear removes all tools from the registry.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = make(map[string]Tool)
	r.order = nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return Tool{}, fmt.Errorf("unknown tool: %s", name)
	}
	return t, nil
}

// Execute is the single policy and dispatch choke point for registered tools.
// It resolves effective arguments, enforces required fields, evaluates
// approval policy, optionally prompts, and only then invokes the handler.
// Tool handlers remain responsible for type- and action-specific validation.
// planModeKey marks a context whose tool dispatch is restricted to read-tier
// tools — Sandbar's plan mode. Enforcement happens here at the dispatch
// chokepoint, after per-argument tier resolution, so an argument that would
// escalate a tool to write/exec (e.g. git add) is denied even when the tool's
// base tier is read.
type planModeCtxKey struct{}

// WithPlanMode marks the context as read-only for tool dispatch.
func WithPlanMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, planModeCtxKey{}, true)
}

// PlanModeFrom reports whether plan mode is active for this dispatch.
func PlanModeFrom(ctx context.Context) bool {
	v, _ := ctx.Value(planModeCtxKey{}).(bool)
	return v
}

func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	tool, err := r.Get(name)
	if err != nil {
		return "", err
	}
	startedAt := time.Now().UTC()
	approval, fallbackHandler, fallbackObserver := r.approvalSnapshot()
	handler := ApprovalHandlerFromContext(ctx)
	if handler == nil {
		handler = fallbackHandler
	}
	observer := ExecutionObserverFromContext(ctx)
	if observer == nil {
		observer = fallbackObserver
	}

	target := ApprovalTarget{
		Tool:      tool.Name,
		Tier:      tool.Metadata.Tier,
		Action:    tool.Name,
		Summary:   tool.Description,
		Arguments: cloneArguments(args),
	}
	metadataValid := target.Tier.Valid()
	if !metadataValid {
		target.Tier = TierExec
	}
	if tool.Metadata.Resolver != nil {
		resolved, resolveErr := tool.Metadata.Resolver(ctx, cloneApprovalTarget(target))
		if resolveErr != nil {
			req := requestFromTarget(ctx, target, false)
			decision := denialDecision(req.ID, "metadata-resolver", resolveErr.Error(), false)
			approvalErr := &ApprovalError{
				Request:  req,
				Decision: decision,
				Cause:    errors.Join(ErrInvalidApproval, fmt.Errorf("resolve approval target: %w", resolveErr)),
			}
			observeExecution(ctx, observer, ToolExecutionResult{
				Request: req, Decision: decision, Outcome: OutcomeDenied,
				Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
			})
			return "", approvalErr
		}
		if resolved.Arguments == nil {
			resolved.Arguments = target.Arguments
		}
		resolved.Tool = tool.Name
		if resolved.Action == "" {
			resolved.Action = target.Action
		}
		if resolved.Summary == "" {
			resolved.Summary = target.Summary
		}
		target = cloneApprovalTarget(resolved)
		metadataValid = target.Tier.Valid()
		if !metadataValid {
			target.Tier = TierExec
		}
	}
	// The todo tool is exempt: its list is conversation metadata, not a
	// filesystem write, so planning turns may create and update their plan.
	if PlanModeFrom(ctx) && target.Tier != TierRead && target.Tool != "todo" {
		return "", fmt.Errorf("plan mode is active: %q is a %s-tier action; only read-tier tools (file_read, search_files, web lookups) are permitted while planning. Present your plan; the user will approve it before any changes run", target.Tool, target.Tier)
	}

	req := requestFromTarget(ctx, target, metadataValid)

	var missing []string
	for _, field := range requiredFields(tool.Schema) {
		value, ok := target.Arguments[field]
		if !ok || value == nil {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		label := "argument"
		if len(missing) > 1 {
			label = "arguments"
		}
		validationErr := fmt.Errorf("tool %s: missing required %s: %s", name, label, strings.Join(missing, ", "))
		decision := denialDecision(req.ID, "validation", validationErr.Error(), false)
		observeExecution(ctx, observer, ToolExecutionResult{
			Request: req, Decision: decision, Outcome: OutcomeFailed,
			Error: validationErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		return "", validationErr
	}
	if err := validateToolArguments(tool.Schema, target.Arguments); err != nil {
		validationErr := fmt.Errorf("tool %s: invalid arguments: %w", name, err)
		decision := denialDecision(req.ID, "validation", validationErr.Error(), false)
		observeExecution(ctx, observer, ToolExecutionResult{
			Request: req, Decision: decision, Outcome: OutcomeFailed,
			Error: validationErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		return "", validationErr
	}
	if tool.Execute == nil {
		executorErr := fmt.Errorf("tool %s has no executor", name)
		decision := denialDecision(req.ID, "validation", executorErr.Error(), false)
		observeExecution(ctx, observer, ToolExecutionResult{
			Request: req, Decision: decision, Outcome: OutcomeFailed,
			Error: executorErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		return "", executorErr
	}

	policy, source, policyErr := resolveApprovalPolicy(ctx, approval, req)
	if policyErr != nil {
		decision := denialDecision(req.ID, source, policyErr.Error(), false)
		approvalErr := &ApprovalError{
			Request: req, Decision: decision,
			Cause: errors.Join(ErrInvalidApproval, policyErr),
		}
		observeExecution(ctx, observer, ToolExecutionResult{
			Request: req, Decision: decision, Outcome: OutcomeDenied,
			Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		return "", approvalErr
	}

	decision := ApprovalDecision{
		RequestID: req.ID,
		Policy:    policy,
		Source:    source,
		DecidedAt: time.Now().UTC(),
	}
	switch policy {
	case PolicyDeny:
		decision.Reason = "denied by approval policy"
		approvalErr := &ApprovalError{Request: req, Decision: decision, Cause: ErrApprovalDenied}
		observeExecution(ctx, observer, ToolExecutionResult{
			Request: req, Decision: decision, Outcome: OutcomeDenied,
			Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		return "", approvalErr
	case PolicyPrompt:
		if handler == nil {
			decision = denialDecision(req.ID, "no-handler", ErrApprovalUnavailable.Error(), true)
			approvalErr := &ApprovalError{Request: req, Decision: decision, Cause: ErrApprovalUnavailable}
			observeExecution(ctx, observer, ToolExecutionResult{
				Request: req, Decision: decision, Outcome: OutcomeDenied,
				Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
			})
			return "", approvalErr
		}
		handlerDecision, handlerErr := handler.RequestApproval(ctx, cloneApprovalRequest(req))
		if handlerErr != nil {
			decision = denialDecision(req.ID, "handler", handlerErr.Error(), true)
			approvalErr := &ApprovalError{
				Request: req, Decision: decision,
				Cause: errors.Join(ErrInvalidApproval, fmt.Errorf("approval handler: %w", handlerErr)),
			}
			observeExecution(ctx, observer, ToolExecutionResult{
				Request: req, Decision: decision, Outcome: OutcomeDenied,
				Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
			})
			return "", approvalErr
		}
		if handlerDecision.Policy != PolicyAllow && handlerDecision.Policy != PolicyDeny {
			decision = denialDecision(req.ID, "handler", fmt.Sprintf("handler returned invalid final policy %q", handlerDecision.Policy), true)
			approvalErr := &ApprovalError{Request: req, Decision: decision, Cause: ErrInvalidApproval}
			observeExecution(ctx, observer, ToolExecutionResult{
				Request: req, Decision: decision, Outcome: OutcomeDenied,
				Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
			})
			return "", approvalErr
		}
		decision = ApprovalDecision{
			RequestID: req.ID,
			Policy:    handlerDecision.Policy,
			Source:    "handler",
			Reason:    handlerDecision.Reason,
			Prompted:  true,
			DecidedAt: time.Now().UTC(),
		}
		if decision.Policy == PolicyDeny {
			approvalErr := &ApprovalError{Request: req, Decision: decision, Cause: ErrApprovalDenied}
			observeExecution(ctx, observer, ToolExecutionResult{
				Request: req, Decision: decision, Outcome: OutcomeDenied,
				Error: approvalErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
			})
			return "", approvalErr
		}
	}

	executionArgs := cloneArguments(target.Arguments)
	// Approval handlers may block while the owning request is cancelled. Check
	// again after the final decision and immediately before crossing into the
	// tool implementation so an approval cannot revive a cancelled operation.
	if ctxErr := ctx.Err(); ctxErr != nil {
		executeErr := fmt.Errorf("tool %s canceled before execution: %w", name, ctxErr)
		observeExecution(ctx, observer, ToolExecutionResult{
			Request: req, Decision: decision, Outcome: OutcomeFailed,
			Error: executeErr.Error(), StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
		return "", executeErr
	}
	output, executeErr := tool.Execute(ctx, executionArgs)
	result := ToolExecutionResult{
		Request: req, Decision: decision, StartedAt: startedAt, CompletedAt: time.Now().UTC(),
		OutputBytes: len(output), Outcome: OutcomeSucceeded,
	}
	if executeErr != nil {
		result.Outcome = OutcomeFailed
		result.Error = executeErr.Error()
	}
	observeExecution(ctx, observer, result)
	return output, executeErr
}

func cloneApprovalTarget(target ApprovalTarget) ApprovalTarget {
	target.Arguments = cloneArguments(target.Arguments)
	return target
}

func requestFromTarget(ctx context.Context, target ApprovalTarget, metadataValid bool) ApprovalRequest {
	return ApprovalRequest{
		ID:            newApprovalRequestID(),
		Tool:          target.Tool,
		Tier:          target.Tier,
		Action:        target.Action,
		Resource:      target.Resource,
		Summary:       target.Summary,
		Arguments:     cloneArguments(target.Arguments),
		ThreadID:      threadIDFromContext(ctx),
		ToolCallID:    ToolCallIDFromContext(ctx),
		Workspace:     WorkspaceFromContext(ctx),
		MetadataValid: metadataValid,
		RequestedAt:   time.Now().UTC(),
	}
}

func denialDecision(requestID, source, reason string, prompted bool) ApprovalDecision {
	return ApprovalDecision{
		RequestID: requestID, Policy: PolicyDeny, Source: source,
		Reason: reason, Prompted: prompted, DecidedAt: time.Now().UTC(),
	}
}

func resolveApprovalPolicy(ctx context.Context, cfg ApprovalConfig, req ApprovalRequest) (ApprovalPolicy, string, error) {
	if cfg.Resolver != nil {
		policy, matched, err := cfg.Resolver.ResolveApproval(ctx, cloneApprovalRequest(req))
		if err != nil {
			return PolicyDeny, "resolver", fmt.Errorf("resolve approval policy: %w", err)
		}
		if matched {
			if !policy.valid() {
				return PolicyDeny, "resolver", fmt.Errorf("approval resolver returned invalid policy %q", policy)
			}
			return policy, "resolver", nil
		}
	}
	if policy, ok := cfg.ToolPolicies[req.Tool]; ok {
		return policy, "tool-policy", nil
	}
	if policy, ok := cfg.TierPolicies[req.Tier]; ok {
		return policy, "tier-policy", nil
	}
	return defaultPolicy(cfg.Mode, req.Tier), "mode", nil
}

const (
	maxConcurrentExecutionObservers = 32
	executionObserverTimeout        = 250 * time.Millisecond
)

var executionObserverSlots = make(chan struct{}, maxConcurrentExecutionObservers)

func observeExecution(ctx context.Context, observer ExecutionObserver, result ToolExecutionResult) {
	if observer == nil {
		return
	}

	// Observers are audit/event sinks, not part of authorization or execution.
	// Give well-behaved observers a short cancellation-safe delivery window,
	// while bounding both caller delay and the number of misbehaving observer
	// goroutines that can remain blocked after ignoring their context.
	observerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), executionObserverTimeout)
	defer cancel()
	select {
	case executionObserverSlots <- struct{}{}:
	case <-observerCtx.Done():
		return
	}

	done := make(chan struct{})
	snapshot := cloneExecutionResult(result)
	go func() {
		defer func() {
			_ = recover() // an observer must never crash tool execution
			<-executionObserverSlots
			close(done)
		}()
		observer.ObserveToolExecution(observerCtx, snapshot)
	}()
	select {
	case <-done:
	case <-observerCtx.Done():
	}
}

func requiredFields(schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}
	switch fields := schema["required"].(type) {
	case []string:
		return fields
	case []interface{}:
		out := make([]string, 0, len(fields))
		for _, field := range fields {
			if name, ok := field.(string); ok {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

// List returns all registered tool names in registration order.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// RestrictTo removes every registered tool whose name is not in names, so the
// removed tools disappear from the advertised schemas entirely — the model
// never sees them, and a call to a removed tool fails as unknown rather than
// being merely denied at dispatch. Every entry in names must name a currently
// registered tool: a typo fails closed instead of silently narrowing the run.
// An empty names slice removes every tool (a plain chat turn needs none).
func (r *Registry) RestrictTo(names []string) error {
	allow := make(map[string]struct{}, len(names))
	for _, name := range names {
		allow[name] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			return fmt.Errorf("restrict tools: %q is not a registered tool", name)
		}
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if _, ok := allow[name]; ok {
			kept = append(kept, name)
			continue
		}
		delete(r.tools, name)
	}
	r.order = kept
	return nil
}

// CanRunConcurrently reports whether a tool explicitly opts into concurrent
// batch execution. Unknown and unannotated tools default to sequential.
func (r *Registry) CanRunConcurrently(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return ok && tool.ParallelSafe
}

// CancelActiveTool cancels the currently executing shell command (if any)
// by sending SIGINT to its process group. Returns nil if no tool is active.
func (r *Registry) CancelActiveTool() error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return nil
	}
	return shell.Cancel()
}

// CancelActiveToolFor cancels only synchronous shell commands owned by ctx's
// thread and workspace. Shared servers should prefer this over the legacy
// process-wide CancelActiveTool method.
func (r *Registry) CancelActiveToolFor(ctx context.Context) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return nil
	}
	return shell.CancelOwner(ctx)
}

// CancelThreadJobs tears down all shell work owned by a thread before its
// durable conversation state is deleted.
func (r *Registry) CancelThreadJobs(ctx context.Context, threadID string) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return nil
	}
	return shell.CancelThread(ctx, threadID)
}

// Close tears down shell processes and rejects future shell/job execution.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return nil
	}
	return shell.Close(ctx)
}

// SetSSHConfig applies remote-execution settings to shell_exec's host mode.
func (r *Registry) SetSSHConfig(cfg SSHRuntimeConfig) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return fmt.Errorf("shell tool is not configured")
	}
	return shell.SetSSHConfig(cfg)
}

// SetShellTimeout applies the runtime's configured default to shell_exec.
func (r *Registry) SetShellTimeout(timeout time.Duration) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return fmt.Errorf("shell tool is not configured")
	}
	return shell.SetDefaultTimeout(timeout)
}

// SetJobSupervisorConfig applies bounded job settings during runtime startup.
func (r *Registry) SetJobSupervisorConfig(cfg JobSupervisorConfig) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return fmt.Errorf("shell tool is not configured")
	}
	return shell.SetJobSupervisorConfig(cfg)
}

// JobSupervisor returns the shared background-job supervisor backing shell_exec,
// or nil if no shell tool is configured. Subagent registries adopt it so their
// shell work is torn down with the parent thread on DeleteThread.
func (r *Registry) JobSupervisor() *JobSupervisor {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return nil
	}
	return shell.jobs
}

// SetJobSupervisor replaces the shell tool's detached-job supervisor with a
// shared one. Subagent registries call this with the parent's supervisor so
// thread teardown cancels their background work.
func (r *Registry) SetJobSupervisor(jobs *JobSupervisor) error {
	r.mu.RLock()
	shell := r.shell
	r.mu.RUnlock()
	if shell == nil {
		return fmt.Errorf("shell tool is not configured")
	}
	return shell.SetJobSupervisor(jobs)
}

// SetApprovalConfig atomically installs approval policy after validating and
// copying it. Existing in-flight calls retain the snapshot they started with.
func (r *Registry) SetApprovalConfig(cfg ApprovalConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	r.approval = cfg.clone()
	r.mu.Unlock()
	return nil
}

// ApprovalConfig returns a defensive snapshot of the installed policy.
func (r *Registry) ApprovalConfig() ApprovalConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.approval.clone()
}

// SetApprovalHandler installs a process-wide fallback for callers that do not
// carry a handler with WithApprovalHandler. Request-scoped handlers take
// precedence and should be preferred by servers and interactive clients.
func (r *Registry) SetApprovalHandler(handler ApprovalHandler) {
	r.mu.Lock()
	r.handler = handler
	r.mu.Unlock()
}

// SetExecutionObserver installs a process-wide fallback observer. A
// request-scoped observer installed with WithExecutionObserver takes
// precedence.
func (r *Registry) SetExecutionObserver(observer ExecutionObserver) {
	r.mu.Lock()
	r.observer = observer
	r.mu.Unlock()
}

func (r *Registry) approvalSnapshot() (ApprovalConfig, ApprovalHandler, ExecutionObserver) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg := r.approval
	if cfg.Mode == "" {
		cfg = DefaultApprovalConfig()
	}
	return cfg, r.handler, r.observer
}

// SetSubagentRunner registers the agent's subagent spawner on this registry.
// Called by agent.New after constructing the tool registry.
func (r *Registry) SetSubagentRunner(runner SubagentRunner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runner = runner
}

// SetPlanStore enables durable per-thread todo plans. Agent.New installs its
// SQLite store here; standalone registries keep the in-memory fallback.
func (r *Registry) SetPlanStore(store PlanStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans = store
}

// SetSubagentStore enables agent:// transcript reads in file_read. Agent.New
// installs its SQLite store here; without it agent:// explains the missing
// wiring instead of failing as an unknown file.
func (r *Registry) SetSubagentStore(store SubagentStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files != nil {
		r.files.SetSubagentStore(store)
	}
}

func (r *Registry) todo(ctx context.Context, args map[string]interface{}) (string, error) {
	r.mu.RLock()
	store := r.plans
	r.mu.RUnlock()
	return todoList(store, ctx, args)
}

// delegate spawns a sub-agent via the registered runner.
func (r *Registry) delegate(ctx context.Context, args map[string]interface{}) (string, error) {
	r.mu.RLock()
	runner := r.runner
	r.mu.RUnlock()
	return delegateTask(runner, ctx, args)
}

// resume resumes a previously-persisted sub-agent task via the registered runner.
func (r *Registry) resume(ctx context.Context, args map[string]interface{}) (string, error) {
	r.mu.RLock()
	runner := r.runner
	r.mu.RUnlock()
	return resumeTask(runner, ctx, args)
}
