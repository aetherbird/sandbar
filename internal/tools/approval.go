package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AccessTier describes the maximum kind of effect a tool invocation can have.
// Tiers are ordered from least to most privileged: read < write < exec.
type AccessTier string

const (
	TierRead  AccessTier = "read"
	TierWrite AccessTier = "write"
	TierExec  AccessTier = "exec"
)

// Valid reports whether the tier is one of Sandbar's known access tiers.
func (t AccessTier) Valid() bool {
	switch t {
	case TierRead, TierWrite, TierExec:
		return true
	default:
		return false
	}
}

// Rank returns a stable ordering for valid tiers. Unknown tiers sort above
// exec so callers that compare tiers fail conservatively.
func (t AccessTier) Rank() int {
	switch t {
	case TierRead:
		return 1
	case TierWrite:
		return 2
	case TierExec:
		return 3
	default:
		return 4
	}
}

// ApprovalPolicy is the action selected by approval policy evaluation.
type ApprovalPolicy string

const (
	PolicyAllow  ApprovalPolicy = "allow"
	PolicyDeny   ApprovalPolicy = "deny"
	PolicyPrompt ApprovalPolicy = "prompt"
)

func (p ApprovalPolicy) valid() bool {
	switch p {
	case PolicyAllow, PolicyDeny, PolicyPrompt:
		return true
	default:
		return false
	}
}

// ApprovalMode supplies the default policy when no resolver, tool policy, or
// tier policy matches an invocation.
type ApprovalMode string

const (
	// ApprovalModeYolo allows every tier. It is intentionally the default for
	// backward compatibility with deployments that predate approvals.
	ApprovalModeYolo ApprovalMode = "yolo"
	// ApprovalModeWrite allows read and write tools and prompts for exec tools.
	ApprovalModeWrite ApprovalMode = "write"
	// ApprovalModeAlwaysAsk allows reads and prompts for write and exec tools.
	ApprovalModeAlwaysAsk ApprovalMode = "always-ask"
)

func (m ApprovalMode) valid() bool {
	switch m {
	case ApprovalModeYolo, ApprovalModeWrite, ApprovalModeAlwaysAsk:
		return true
	default:
		return false
	}
}

// ApprovalTarget is the security-relevant form of a tool call. A metadata
// resolver may refine the tier and display fields and may replace Arguments.
// Those effective arguments are then used for validation, approval, and final
// execution, preventing the approved call from differing from the executed
// call.
type ApprovalTarget struct {
	Tool      string                 `json:"tool"`
	Tier      AccessTier             `json:"tier"`
	Action    string                 `json:"action,omitempty"`
	Resource  string                 `json:"resource,omitempty"`
	Summary   string                 `json:"summary,omitempty"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

// ApprovalTargetResolver classifies a call using its actual arguments. It can,
// for example, classify "git status" as read and "git commit" as write. A nil
// Arguments result preserves the input arguments; a non-nil map becomes the
// effective argument map passed to both approval and execution.
type ApprovalTargetResolver func(context.Context, ApprovalTarget) (ApprovalTarget, error)

// ToolMetadata contains policy metadata that is deliberately separate from a
// provider-facing tool schema. Missing or malformed tiers are treated as exec,
// the most privileged tier, unless a resolver returns a valid tier.
type ToolMetadata struct {
	Tier     AccessTier
	Resolver ApprovalTargetResolver
}

// ApprovalRequest is an immutable snapshot of a call presented to policy and
// approval handlers. Arguments contains the effective arguments that will be
// passed to the tool if the request is approved.
type ApprovalRequest struct {
	ID            string                 `json:"id"`
	Tool          string                 `json:"tool"`
	Tier          AccessTier             `json:"tier"`
	Action        string                 `json:"action,omitempty"`
	Resource      string                 `json:"resource,omitempty"`
	Summary       string                 `json:"summary,omitempty"`
	Arguments     map[string]interface{} `json:"arguments,omitempty"`
	ThreadID      string                 `json:"thread_id,omitempty"`
	ToolCallID    string                 `json:"tool_call_id,omitempty"`
	Workspace     string                 `json:"workspace,omitempty"`
	MetadataValid bool                   `json:"metadata_valid"`
	RequestedAt   time.Time              `json:"requested_at"`
}

// ApprovalDecision records the policy decision for an approval request.
// Policy is always final (allow or deny) in ToolExecutionResult. A handler may
// return allow or deny; returning prompt is rejected as malformed.
type ApprovalDecision struct {
	RequestID string         `json:"request_id"`
	Policy    ApprovalPolicy `json:"policy"`
	Source    string         `json:"source"`
	Reason    string         `json:"reason,omitempty"`
	Prompted  bool           `json:"prompted"`
	DecidedAt time.Time      `json:"decided_at"`
}

// ExecutionOutcome is the terminal state of a Registry.Execute call.
type ExecutionOutcome string

const (
	OutcomeSucceeded ExecutionOutcome = "succeeded"
	OutcomeDenied    ExecutionOutcome = "denied"
	OutcomeFailed    ExecutionOutcome = "failed"
)

// ToolExecutionResult is a bounded, audit-friendly record. It intentionally
// records output length rather than raw output, which can be large or secret.
type ToolExecutionResult struct {
	Request     ApprovalRequest  `json:"request"`
	Decision    ApprovalDecision `json:"decision"`
	Outcome     ExecutionOutcome `json:"outcome"`
	Error       string           `json:"error,omitempty"`
	OutputBytes int              `json:"output_bytes,omitempty"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt time.Time        `json:"completed_at"`
}

// ApprovalPolicyResolver performs argument-aware policy selection. matched=false
// continues to the next precedence level; matched=true makes policy final.
type ApprovalPolicyResolver interface {
	ResolveApproval(context.Context, ApprovalRequest) (policy ApprovalPolicy, matched bool, err error)
}

// ApprovalPolicyResolverFunc adapts a function to ApprovalPolicyResolver.
type ApprovalPolicyResolverFunc func(context.Context, ApprovalRequest) (ApprovalPolicy, bool, error)

func (f ApprovalPolicyResolverFunc) ResolveApproval(ctx context.Context, req ApprovalRequest) (ApprovalPolicy, bool, error) {
	return f(ctx, req)
}

// ApprovalHandler handles policy decisions that require an interactive prompt.
// Implementations should return only PolicyAllow or PolicyDeny.
type ApprovalHandler interface {
	RequestApproval(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

// ApprovalHandlerFunc adapts a function to ApprovalHandler.
type ApprovalHandlerFunc func(context.Context, ApprovalRequest) (ApprovalDecision, error)

func (f ApprovalHandlerFunc) RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	return f(ctx, req)
}

type approvalHandlerContextKey struct{}

// WithApprovalHandler scopes interactive approval to one request/session. A
// context handler takes precedence over the Registry fallback, preventing
// concurrent HTTP chats and sub-agents from sharing mutable prompt state.
func WithApprovalHandler(ctx context.Context, handler ApprovalHandler) context.Context {
	return context.WithValue(ctx, approvalHandlerContextKey{}, handler)
}

// ApprovalHandlerFromContext returns the request-scoped handler, if any. It is
// exported so transports can bridge approval requests without falling back to
// process-global state.
func ApprovalHandlerFromContext(ctx context.Context) ApprovalHandler {
	if ctx == nil {
		return nil
	}
	handler, _ := ctx.Value(approvalHandlerContextKey{}).(ApprovalHandler)
	return handler
}

// ExecutionObserver receives terminal execution records. Observers must be
// safe for concurrent use. Their return value cannot alter tool execution.
type ExecutionObserver interface {
	ObserveToolExecution(context.Context, ToolExecutionResult)
}

// ExecutionObserverFunc adapts a function to ExecutionObserver.
type ExecutionObserverFunc func(context.Context, ToolExecutionResult)

func (f ExecutionObserverFunc) ObserveToolExecution(ctx context.Context, result ToolExecutionResult) {
	f(ctx, result)
}

type executionObserverContextKey struct{}

// WithExecutionObserver scopes audit/event observation to one request/session.
// A context observer takes precedence over the Registry fallback.
func WithExecutionObserver(ctx context.Context, observer ExecutionObserver) context.Context {
	return context.WithValue(ctx, executionObserverContextKey{}, observer)
}

// ExecutionObserverFromContext returns the request-scoped observer, if any.
func ExecutionObserverFromContext(ctx context.Context) ExecutionObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(executionObserverContextKey{}).(ExecutionObserver)
	return observer
}

// ApprovalConfig controls policy selection. Precedence is:
// Resolver > exact ToolPolicies entry > TierPolicies entry > Mode.
// Maps are copied when installed on a Registry, so callers may safely reuse or
// mutate their input after SetApprovalConfig returns.
type ApprovalConfig struct {
	Mode         ApprovalMode
	ToolPolicies map[string]ApprovalPolicy
	TierPolicies map[AccessTier]ApprovalPolicy
	Resolver     ApprovalPolicyResolver
}

// DefaultApprovalConfig preserves the behavior of deployments created before
// the approval system: all registered tools execute without prompting.
func DefaultApprovalConfig() ApprovalConfig {
	return ApprovalConfig{Mode: ApprovalModeYolo}
}

// Validate rejects configuration typos rather than silently weakening policy.
func (c ApprovalConfig) Validate() error {
	mode := c.Mode
	if mode == "" {
		mode = ApprovalModeYolo
	}
	if !mode.valid() {
		return fmt.Errorf("invalid approval mode %q", mode)
	}
	for name, policy := range c.ToolPolicies {
		if strings.TrimSpace(name) == "" {
			return errors.New("approval tool policy has an empty tool name")
		}
		if !policy.valid() {
			return fmt.Errorf("invalid approval policy %q for tool %q", policy, name)
		}
	}
	for tier, policy := range c.TierPolicies {
		if !tier.Valid() {
			return fmt.Errorf("invalid approval tier policy key %q", tier)
		}
		if !policy.valid() {
			return fmt.Errorf("invalid approval policy %q for tier %q", policy, tier)
		}
	}
	return nil
}

// RewritePromptToAllow returns a copy of cfg with every prompt-tier policy
// rewritten to allow. It is used when a sub-run forces Yolo mode with no
// approval handler installed: a "prompt" policy is unanswerable there, so it
// degrades to allow rather than failing every covered call. Explicit denies
// are preserved — deny stays deny.
func RewritePromptToAllow(cfg ApprovalConfig) ApprovalConfig {
	for name, p := range cfg.ToolPolicies {
		if p == PolicyPrompt {
			cfg.ToolPolicies[name] = PolicyAllow
		}
	}
	for tier, p := range cfg.TierPolicies {
		if p == PolicyPrompt {
			cfg.TierPolicies[tier] = PolicyAllow
		}
	}
	return cfg
}

func (c ApprovalConfig) clone() ApprovalConfig {
	if c.Mode == "" {
		c.Mode = ApprovalModeYolo
	}
	toolPolicies := make(map[string]ApprovalPolicy, len(c.ToolPolicies))
	for name, policy := range c.ToolPolicies {
		toolPolicies[name] = policy
	}
	tierPolicies := make(map[AccessTier]ApprovalPolicy, len(c.TierPolicies))
	for tier, policy := range c.TierPolicies {
		tierPolicies[tier] = policy
	}
	c.ToolPolicies = toolPolicies
	c.TierPolicies = tierPolicies
	return c
}

var (
	// ErrApprovalDenied identifies a policy or user denial.
	ErrApprovalDenied = errors.New("tool execution denied")
	// ErrApprovalUnavailable identifies a required prompt in a headless caller.
	ErrApprovalUnavailable = errors.New("tool approval required but no approval handler is configured")
	// ErrInvalidApproval identifies malformed policy, metadata resolver, or
	// handler output. These errors always fail closed.
	ErrInvalidApproval = errors.New("invalid tool approval")
)

// ApprovalError includes the request and decision while remaining compatible
// with errors.Is for the sentinel approval errors above.
type ApprovalError struct {
	Request  ApprovalRequest
	Decision ApprovalDecision
	Cause    error
}

func (e *ApprovalError) Error() string {
	if e == nil {
		return "tool approval failed"
	}
	if e.Cause == nil {
		return fmt.Sprintf("tool %s approval failed", e.Request.Tool)
	}
	return fmt.Sprintf("tool %s: %v", e.Request.Tool, e.Cause)
}

func (e *ApprovalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func newApprovalRequestID() string {
	// UUID v4 IDs are backed by crypto/rand. Besides avoiding collisions across
	// processes, an opaque ID prevents one requester from predicting another
	// request's identifier while it is pending.
	return "approval-" + uuid.NewString()
}

func defaultPolicy(mode ApprovalMode, tier AccessTier) ApprovalPolicy {
	switch mode {
	case ApprovalModeAlwaysAsk:
		if tier.Rank() >= TierWrite.Rank() {
			return PolicyPrompt
		}
		return PolicyAllow
	case ApprovalModeWrite:
		if tier.Rank() >= TierExec.Rank() {
			return PolicyPrompt
		}
		return PolicyAllow
	case ApprovalModeYolo:
		return PolicyAllow
	default:
		// An invalid mode should have been rejected by SetApprovalConfig. Keep
		// this fail-closed in case an uninitialized Registry is constructed.
		return PolicyPrompt
	}
}

func cloneArguments(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(args))
	for key, value := range args {
		out[key] = cloneArgumentValue(value)
	}
	return out
}

func cloneArgumentValue(value interface{}) interface{} {
	switch value := value.(type) {
	case map[string]interface{}:
		return cloneArguments(value)
	case []interface{}:
		out := make([]interface{}, len(value))
		for i := range value {
			out[i] = cloneArgumentValue(value[i])
		}
		return out
	case []string:
		return append([]string(nil), value...)
	case map[string]string:
		out := make(map[string]string, len(value))
		for key, item := range value {
			out[key] = item
		}
		return out
	default:
		// Provider arguments originate as JSON, whose scalar values are
		// immutable. Keep unfamiliar application-defined values as-is.
		return value
	}
}

func cloneApprovalRequest(req ApprovalRequest) ApprovalRequest {
	req.Arguments = cloneArguments(req.Arguments)
	return req
}

func cloneExecutionResult(result ToolExecutionResult) ToolExecutionResult {
	result.Request = cloneApprovalRequest(result.Request)
	return result
}
