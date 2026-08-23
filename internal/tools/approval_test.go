package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aetherbird/sandbar/internal/config"
)

func newApprovalTestRegistry(t *testing.T, tool Tool) *Registry {
	t.Helper()
	r := NewRegistry(t.TempDir(), "", "", nil)
	r.Clear()
	r.Register(tool)
	return r
}

func TestAccessTierOrdering(t *testing.T) {
	if !(TierRead.Rank() < TierWrite.Rank() && TierWrite.Rank() < TierExec.Rank()) {
		t.Fatalf("tier ranks are not read < write < exec: %d, %d, %d", TierRead.Rank(), TierWrite.Rank(), TierExec.Rank())
	}
	if AccessTier("invalid").Rank() <= TierExec.Rank() {
		t.Fatal("unknown tier must rank above exec")
	}
}

func TestApprovalRequestIDsAreOpaqueAndUnique(t *testing.T) {
	const requests = 256
	seen := make(map[string]struct{}, requests)
	for range requests {
		id := newApprovalRequestID()
		if !strings.HasPrefix(id, "approval-") || len(id) != len("approval-")+36 {
			t.Fatalf("approval ID %q is not an opaque UUID", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate approval ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestApprovalModeSemantics(t *testing.T) {
	tests := []struct {
		mode ApprovalMode
		tier AccessTier
		want ApprovalPolicy
	}{
		{ApprovalModeYolo, TierRead, PolicyAllow},
		{ApprovalModeYolo, TierWrite, PolicyAllow},
		{ApprovalModeYolo, TierExec, PolicyAllow},
		{ApprovalModeWrite, TierRead, PolicyAllow},
		{ApprovalModeWrite, TierWrite, PolicyAllow},
		{ApprovalModeWrite, TierExec, PolicyPrompt},
		{ApprovalModeAlwaysAsk, TierRead, PolicyAllow},
		{ApprovalModeAlwaysAsk, TierWrite, PolicyPrompt},
		{ApprovalModeAlwaysAsk, TierExec, PolicyPrompt},
	}
	for _, tc := range tests {
		if got := defaultPolicy(tc.mode, tc.tier); got != tc.want {
			t.Errorf("defaultPolicy(%q, %q) = %q, want %q", tc.mode, tc.tier, got, tc.want)
		}
	}
}

func TestApprovalPolicyPrecedence(t *testing.T) {
	var calls atomic.Int32
	r := newApprovalTestRegistry(t, Tool{
		Name: "danger", Metadata: ToolMetadata{Tier: TierExec},
		Execute: func(context.Context, map[string]interface{}) (string, error) {
			calls.Add(1)
			return "ok", nil
		},
	})

	// Tier policy overrides mode.
	if err := r.SetApprovalConfig(ApprovalConfig{
		Mode:         ApprovalModeYolo,
		TierPolicies: map[AccessTier]ApprovalPolicy{TierExec: PolicyDeny},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), "danger", nil); !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("tier policy error = %v, want denial", err)
	}

	// Exact tool policy overrides tier policy.
	if err := r.SetApprovalConfig(ApprovalConfig{
		Mode:         ApprovalModeYolo,
		TierPolicies: map[AccessTier]ApprovalPolicy{TierExec: PolicyDeny},
		ToolPolicies: map[string]ApprovalPolicy{"danger": PolicyAllow},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), "danger", nil); err != nil {
		t.Fatalf("tool policy should allow: %v", err)
	}

	// The argument-aware resolver is most specific and overrides the tool.
	if err := r.SetApprovalConfig(ApprovalConfig{
		Mode:         ApprovalModeYolo,
		ToolPolicies: map[string]ApprovalPolicy{"danger": PolicyAllow},
		Resolver: ApprovalPolicyResolverFunc(func(_ context.Context, req ApprovalRequest) (ApprovalPolicy, bool, error) {
			if req.Arguments["deny"] == true {
				return PolicyDeny, true, nil
			}
			return "", false, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), "danger", map[string]interface{}{"deny": true}); !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("resolver error = %v, want denial", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want only the explicitly allowed call", calls.Load())
	}
}

func TestMissingAndMalformedMetadataDefaultToExec(t *testing.T) {
	for _, tier := range []AccessTier{"", "destructive"} {
		t.Run(string(tier), func(t *testing.T) {
			called := false
			var observed ToolExecutionResult
			r := newApprovalTestRegistry(t, Tool{
				Name: "unclassified", Metadata: ToolMetadata{Tier: tier},
				Execute: func(context.Context, map[string]interface{}) (string, error) {
					called = true
					return "should not run", nil
				},
			})
			if err := r.SetApprovalConfig(ApprovalConfig{Mode: ApprovalModeWrite}); err != nil {
				t.Fatal(err)
			}
			r.SetExecutionObserver(ExecutionObserverFunc(func(_ context.Context, result ToolExecutionResult) {
				observed = result
			}))
			_, err := r.Execute(context.Background(), "unclassified", nil)
			if !errors.Is(err, ErrApprovalUnavailable) {
				t.Fatalf("Execute error = %v, want unavailable approval", err)
			}
			if called {
				t.Fatal("unclassified tool executed")
			}
			if observed.Request.Tier != TierExec || observed.Request.MetadataValid {
				t.Fatalf("request classification = tier %q valid=%v, want exec/false", observed.Request.Tier, observed.Request.MetadataValid)
			}
		})
	}
}

func TestMetadataResolverCanRepairAndSupplyEffectiveArguments(t *testing.T) {
	original := map[string]interface{}{"path": "raw"}
	var handlerArgs map[string]interface{}
	var executorArgs map[string]interface{}
	r := newApprovalTestRegistry(t, Tool{
		Name: "normalize",
		Schema: map[string]interface{}{
			"required": []string{"path", "normalized"},
		},
		Metadata: ToolMetadata{
			// Missing static metadata is safe because this resolver classifies
			// every action and returns the final argument map.
			Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
				target.Tier = TierWrite
				target.Action = "replace"
				target.Resource = target.Arguments["path"].(string)
				target.Arguments["path"] = "approved"
				target.Arguments["normalized"] = true
				return target, nil
			},
		},
		Execute: func(_ context.Context, args map[string]interface{}) (string, error) {
			executorArgs = cloneArguments(args)
			return args["path"].(string), nil
		},
	})
	if err := r.SetApprovalConfig(ApprovalConfig{Mode: ApprovalModeAlwaysAsk}); err != nil {
		t.Fatal(err)
	}
	ctx := WithApprovalHandler(context.Background(), ApprovalHandlerFunc(func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
		handlerArgs = cloneArguments(req.Arguments)
		// A handler cannot change the already-approved execution snapshot.
		req.Arguments["path"] = "handler-mutated"
		return ApprovalDecision{Policy: PolicyAllow, Reason: "test approval"}, nil
	}))
	out, err := r.Execute(ctx, "normalize", original)
	if err != nil {
		t.Fatal(err)
	}
	if out != "approved" || handlerArgs["path"] != "approved" || executorArgs["path"] != "approved" {
		t.Fatalf("effective args mismatch: out=%q handler=%#v executor=%#v", out, handlerArgs, executorArgs)
	}
	if handlerArgs["normalized"] != true || executorArgs["normalized"] != true {
		t.Fatalf("resolver-supplied args missing: handler=%#v executor=%#v", handlerArgs, executorArgs)
	}
	if original["path"] != "raw" {
		t.Fatalf("caller arguments mutated: %#v", original)
	}
}

func TestPromptFailsClosedWithoutHandler(t *testing.T) {
	called := false
	r := newApprovalTestRegistry(t, Tool{
		Name: "exec", Metadata: ToolMetadata{Tier: TierExec},
		Execute: func(context.Context, map[string]interface{}) (string, error) {
			called = true
			return "", nil
		},
	})
	if err := r.SetApprovalConfig(ApprovalConfig{Mode: ApprovalModeWrite}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Execute(context.Background(), "exec", nil)
	if !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("Execute error = %v, want ErrApprovalUnavailable", err)
	}
	var approvalErr *ApprovalError
	if !errors.As(err, &approvalErr) || approvalErr.Decision.Source != "no-handler" || !approvalErr.Decision.Prompted {
		t.Fatalf("approval error = %#v", approvalErr)
	}
	if called {
		t.Fatal("executor ran without approval handler")
	}
}

func TestCancellationAfterApprovalPreventsExecution(t *testing.T) {
	called := false
	r := newApprovalTestRegistry(t, Tool{
		Name: "exec", Metadata: ToolMetadata{Tier: TierExec},
		Execute: func(context.Context, map[string]interface{}) (string, error) {
			called = true
			return "unexpected", nil
		},
	})
	if err := r.SetApprovalConfig(ApprovalConfig{Mode: ApprovalModeWrite}); err != nil {
		t.Fatal(err)
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := WithApprovalHandler(baseCtx, ApprovalHandlerFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		cancel()
		return ApprovalDecision{Policy: PolicyAllow}, nil
	}))
	_, err := r.Execute(ctx, "exec", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context cancellation", err)
	}
	if called {
		t.Fatal("executor ran after its request was cancelled")
	}
}

func TestExecutionObserverIsCancellationBounded(t *testing.T) {
	r := newApprovalTestRegistry(t, Tool{
		Name: "read", Metadata: ToolMetadata{Tier: TierRead},
		Execute: func(context.Context, map[string]interface{}) (string, error) { return "ok", nil },
	})
	observerStarted := make(chan struct{})
	r.SetExecutionObserver(ExecutionObserverFunc(func(ctx context.Context, _ ToolExecutionResult) {
		close(observerStarted)
		<-ctx.Done()
	}))
	startedAt := time.Now()
	out, err := r.Execute(context.Background(), "read", nil)
	if err != nil || out != "ok" {
		t.Fatalf("Execute = %q, %v", out, err)
	}
	select {
	case <-observerStarted:
	default:
		t.Fatal("observer was not invoked")
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("observer blocked Execute for %s", elapsed)
	}
}

func TestNetworkAndHookExecutingToolsUseExecTier(t *testing.T) {
	metadata := gitToolMetadata()
	for action, want := range map[string]AccessTier{
		"status": TierRead,
		"diff":   TierRead,
		"add":    TierWrite,
		"commit": TierExec,
	} {
		t.Run("git_"+action, func(t *testing.T) {
			target, err := metadata.Resolver(context.Background(), ApprovalTarget{
				Tool: "git", Tier: metadata.Tier,
				Arguments: map[string]interface{}{"action": action, "repo_path": "."},
			})
			if err != nil {
				t.Fatal(err)
			}
			if target.Tier != want {
				t.Fatalf("git %s tier = %q, want %q", action, target.Tier, want)
			}
		})
	}

	r := NewRegistry(t.TempDir(), "", "", nil)
	vision, err := r.Get("vision_analyze")
	if err != nil {
		t.Fatal(err)
	}
	target, err := vision.Metadata.Resolver(context.Background(), ApprovalTarget{
		Tool: vision.Name, Tier: vision.Metadata.Tier,
		Arguments: map[string]interface{}{"image_path": "/tmp/private.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.Tier != TierExec || target.Action != "analyze-external" {
		t.Fatalf("vision approval target = %#v, want external exec", target)
	}
}

func TestContextApprovalHandlerAndObserverTakePrecedence(t *testing.T) {
	r := newApprovalTestRegistry(t, Tool{
		Name: "exec", Metadata: ToolMetadata{Tier: TierExec},
		Execute: func(context.Context, map[string]interface{}) (string, error) { return "done", nil },
	})
	if err := r.SetApprovalConfig(ApprovalConfig{Mode: ApprovalModeWrite}); err != nil {
		t.Fatal(err)
	}
	r.SetApprovalHandler(ApprovalHandlerFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDecision{Policy: PolicyDeny, Reason: "global"}, nil
	}))
	var globalObserved atomic.Int32
	r.SetExecutionObserver(ExecutionObserverFunc(func(context.Context, ToolExecutionResult) {
		globalObserved.Add(1)
	}))

	var contextResult ToolExecutionResult
	ctx := WithApprovalHandler(context.Background(), ApprovalHandlerFunc(func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
		if req.Tool != "exec" || req.Tier != TierExec {
			t.Errorf("unexpected request: %#v", req)
		}
		return ApprovalDecision{Policy: PolicyAllow, Reason: "request owner"}, nil
	}))
	ctx = WithExecutionObserver(ctx, ExecutionObserverFunc(func(_ context.Context, result ToolExecutionResult) {
		contextResult = result
	}))
	out, err := r.Execute(ctx, "exec", nil)
	if err != nil || out != "done" {
		t.Fatalf("Execute = %q, %v", out, err)
	}
	if globalObserved.Load() != 0 {
		t.Fatal("global observer ran despite request-scoped observer")
	}
	if contextResult.Outcome != OutcomeSucceeded || contextResult.Decision.Source != "handler" || !contextResult.Decision.Prompted {
		t.Fatalf("context result = %#v", contextResult)
	}
}

func TestApprovalResolverAndHandlerErrorsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     ApprovalConfig
		handler ApprovalHandler
	}{
		{
			name: "policy resolver error",
			cfg: ApprovalConfig{Mode: ApprovalModeYolo, Resolver: ApprovalPolicyResolverFunc(func(context.Context, ApprovalRequest) (ApprovalPolicy, bool, error) {
				return "", false, errors.New("resolver broke")
			})},
		},
		{
			name: "invalid resolver policy",
			cfg: ApprovalConfig{Mode: ApprovalModeYolo, Resolver: ApprovalPolicyResolverFunc(func(context.Context, ApprovalRequest) (ApprovalPolicy, bool, error) {
				return ApprovalPolicy("maybe"), true, nil
			})},
		},
		{
			name: "handler error",
			cfg:  ApprovalConfig{Mode: ApprovalModeWrite},
			handler: ApprovalHandlerFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
				return ApprovalDecision{}, errors.New("prompt disconnected")
			}),
		},
		{
			name: "handler returns prompt",
			cfg:  ApprovalConfig{Mode: ApprovalModeWrite},
			handler: ApprovalHandlerFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
				return ApprovalDecision{Policy: PolicyPrompt}, nil
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			r := newApprovalTestRegistry(t, Tool{
				Name: "exec", Metadata: ToolMetadata{Tier: TierExec},
				Execute: func(context.Context, map[string]interface{}) (string, error) {
					called = true
					return "", nil
				},
			})
			if err := r.SetApprovalConfig(tc.cfg); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.handler != nil {
				ctx = WithApprovalHandler(ctx, tc.handler)
			}
			_, err := r.Execute(ctx, "exec", nil)
			if !errors.Is(err, ErrInvalidApproval) {
				t.Fatalf("Execute error = %v, want ErrInvalidApproval", err)
			}
			if called {
				t.Fatal("executor ran after malformed approval")
			}
		})
	}
}

func TestApprovalConfigValidationAndDefensiveCopy(t *testing.T) {
	for _, cfg := range []ApprovalConfig{
		{Mode: "unsafe"},
		{Mode: ApprovalModeYolo, ToolPolicies: map[string]ApprovalPolicy{"": PolicyAllow}},
		{Mode: ApprovalModeYolo, ToolPolicies: map[string]ApprovalPolicy{"tool": "maybe"}},
		{Mode: ApprovalModeYolo, TierPolicies: map[AccessTier]ApprovalPolicy{"network": PolicyAllow}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", cfg)
		}
	}

	r := NewRegistry(t.TempDir(), "", "", nil)
	input := ApprovalConfig{
		Mode:         ApprovalModeYolo,
		ToolPolicies: map[string]ApprovalPolicy{"file_read": PolicyDeny},
	}
	if err := r.SetApprovalConfig(input); err != nil {
		t.Fatal(err)
	}
	input.ToolPolicies["file_read"] = PolicyAllow
	snapshot := r.ApprovalConfig()
	if snapshot.ToolPolicies["file_read"] != PolicyDeny {
		t.Fatal("registry policy aliases caller map")
	}
	snapshot.ToolPolicies["file_read"] = PolicyAllow
	if r.ApprovalConfig().ToolPolicies["file_read"] != PolicyDeny {
		t.Fatal("ApprovalConfig result aliases registry map")
	}
}

func TestApprovalConfigFromToolConfig(t *testing.T) {
	cfg, err := ApprovalConfigFromToolConfig(config.ToolApprovalConfig{
		Mode:  "always-ask",
		Rules: map[string]string{"shell_exec": "deny"},
		Tiers: map[string]string{"write": "prompt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ApprovalModeAlwaysAsk || cfg.ToolPolicies["shell_exec"] != PolicyDeny || cfg.TierPolicies[TierWrite] != PolicyPrompt {
		t.Fatalf("converted config = %#v", cfg)
	}

	defaultCfg, err := ApprovalConfigFromToolConfig(config.ToolApprovalConfig{})
	if err != nil || defaultCfg.Mode != ApprovalModeYolo {
		t.Fatalf("default converted config = %#v, %v", defaultCfg, err)
	}
}

func TestRegistryApprovalConcurrentRequestsStayScoped(t *testing.T) {
	const requests = 64
	var executed atomic.Int32
	r := newApprovalTestRegistry(t, Tool{
		Name: "exec", Metadata: ToolMetadata{Tier: TierExec},
		Execute: func(_ context.Context, args map[string]interface{}) (string, error) {
			executed.Add(1)
			return fmt.Sprint(args["id"]), nil
		},
	})
	if err := r.SetApprovalConfig(ApprovalConfig{Mode: ApprovalModeWrite}); err != nil {
		t.Fatal(err)
	}
	// A global denial makes cross-request handler leakage immediately visible.
	r.SetApprovalHandler(ApprovalHandlerFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDecision{Policy: PolicyDeny}, nil
	}))

	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := range requests {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx := WithApprovalHandler(context.Background(), ApprovalHandlerFunc(func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
				if got := int(req.Arguments["id"].(int)); got != id {
					return ApprovalDecision{}, fmt.Errorf("handler %d received request %d", id, got)
				}
				return ApprovalDecision{Policy: PolicyAllow}, nil
			}))
			out, err := r.Execute(ctx, "exec", map[string]interface{}{"id": id})
			if err != nil {
				errs <- err
				return
			}
			if out != fmt.Sprint(id) {
				errs <- fmt.Errorf("request %d output = %q", id, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := executed.Load(); got != requests {
		t.Fatalf("executed = %d, want %d", got, requests)
	}
}

func TestRegistryApprovalConfigurationIsRaceSafe(t *testing.T) {
	r := newApprovalTestRegistry(t, Tool{
		Name: "read", Metadata: ToolMetadata{Tier: TierRead}, ParallelSafe: true,
		Execute: func(context.Context, map[string]interface{}) (string, error) { return "ok", nil },
	})
	var observed atomic.Int32
	r.SetExecutionObserver(ExecutionObserverFunc(func(context.Context, ToolExecutionResult) {
		observed.Add(1)
	}))

	const iterations = 100
	var wg sync.WaitGroup
	for i := range iterations {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			modes := []ApprovalMode{ApprovalModeYolo, ApprovalModeWrite, ApprovalModeAlwaysAsk}
			if err := r.SetApprovalConfig(ApprovalConfig{Mode: modes[i%len(modes)]}); err != nil {
				t.Errorf("SetApprovalConfig: %v", err)
			}
			_ = r.ApprovalConfig()
			_ = r.List()
		}(i)
		go func() {
			defer wg.Done()
			if _, err := r.Execute(context.Background(), "read", nil); err != nil {
				t.Errorf("Execute read: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := observed.Load(); got != iterations {
		t.Fatalf("observer calls = %d, want %d", got, iterations)
	}
}
