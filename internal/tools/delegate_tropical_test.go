package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// scriptedSubagentRunner answers delegate_task spawns immediately with canned
// output, recording the tropical flag and effort each spawn observed.
type scriptedSubagentRunner struct {
	mu          sync.Mutex
	spawns      int
	sawTropical []bool
	sawEffort   []string
}

func (r *scriptedSubagentRunner) SpawnSubagent(ctx context.Context, goal, _ string) (<-chan SubagentEvent, error) {
	r.mu.Lock()
	r.spawns++
	r.sawTropical = append(r.sawTropical, TropicalFromContext(ctx))
	r.sawEffort = append(r.sawEffort, EffortFromContext(ctx))
	r.mu.Unlock()
	ch := make(chan SubagentEvent, 2)
	ch <- SubagentEvent{Type: "done", Content: "result for " + goal}
	close(ch)
	return ch, nil
}

func (r *scriptedSubagentRunner) ResumeSubagent(_ context.Context, taskID string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "done", Content: "resumed " + taskID}
	close(ch)
	return ch, nil
}

func spawnBackground(t *testing.T, reg *Registry, ctx context.Context, goal string) (string, error) {
	t.Helper()
	return reg.Execute(ctx, "delegate_task", map[string]interface{}{"goal": goal, "background": true})
}

// TestDelegateTaskEnforcesTropicalConcurrencyCap pins the thread-scoped
// limiter through the real tool path: with both slots saturated, a tropical
// background spawn errors; a non-tropical spawn is unaffected.
func TestDelegateTaskEnforcesTropicalConcurrencyCap(t *testing.T) {
	reg := NewRegistry(t.TempDir(), "", "", nil)
	defer func() { _ = reg.Close(context.Background()) }()
	reg.SetSubagentRunner(&scriptedSubagentRunner{})

	lim := NewTropicalLimiter(1)
	if err := lim.TryAcquire(); err != nil {
		t.Fatalf("saturate limiter: %v", err)
	}
	// The saturated slot is released at test end so later tests (shared
	// process, but fresh limiter) are unaffected — this limiter is local.
	defer lim.Release()

	ctx := WithTropicalLimiter(WithTropicalTotal(context.Background(), NewTropicalTotal(100)), lim)
	if _, err := spawnBackground(t, reg, ctx, "capped task"); err == nil {
		t.Fatal("spawn past concurrency cap must fail")
	} else if !strings.Contains(err.Error(), "at cap") {
		t.Fatalf("error should name the cap, got: %v", err)
	}

	// Non-tropical context with no limiter sails through.
	if _, err := spawnBackground(t, reg, context.Background(), "plain task"); err != nil {
		t.Fatalf("non-tropical spawn must not be capped: %v", err)
	}
}

// TestDelegateTaskEnforcesTropicalTotalCap pins the per-turn spawn budget
// through the real tool path.
func TestDelegateTaskEnforcesTropicalTotalCap(t *testing.T) {
	reg := NewRegistry(t.TempDir(), "", "", nil)
	defer func() { _ = reg.Close(context.Background()) }()
	reg.SetSubagentRunner(&scriptedSubagentRunner{})

	ctx := WithTropicalLimiter(WithTropicalTotal(context.Background(), NewTropicalTotal(1)), NewTropicalLimiter(100))
	if _, err := spawnBackground(t, reg, ctx, "first"); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if _, err := spawnBackground(t, reg, ctx, "second"); err == nil {
		t.Fatal("second spawn past total of 1 must fail")
	} else if !strings.Contains(err.Error(), "budget spent") {
		t.Fatalf("error should guide the model, got: %v", err)
	}
}

// failingSubagentRunner fails every spawn, exercising the spawn-error path.
type failingSubagentRunner struct {
	calls int
}

var errTestSpawnFailure = errors.New("test spawn failure")

func (r *failingSubagentRunner) SpawnSubagent(context.Context, string, string) (<-chan SubagentEvent, error) {
	r.calls++
	return nil, errTestSpawnFailure
}

func (r *failingSubagentRunner) ResumeSubagent(_ context.Context, taskID string) (<-chan SubagentEvent, error) {
	ch := make(chan SubagentEvent, 1)
	ch <- SubagentEvent{Type: "done", Content: "resumed " + taskID}
	close(ch)
	return ch, nil
}

// TestDelegateTaskReturnsSlotOnTotalCap pins the leak fix: with the per-turn
// total already spent, a tropical background spawn must fail WITHOUT
// consuming a concurrency slot — a subsequent spawn with a fresh total must
// still find the slot free.
func TestDelegateTaskReturnsSlotOnTotalCap(t *testing.T) {
	reg := NewRegistry(t.TempDir(), "", "", nil)
	defer func() { _ = reg.Close(context.Background()) }()
	reg.SetSubagentRunner(&scriptedSubagentRunner{})

	lim := NewTropicalLimiter(1)
	spent := NewTropicalTotal(0) // total already spent
	ctx := WithTropicalLimiter(WithTropicalTotal(context.Background(), spent), lim)
	if _, err := spawnBackground(t, reg, ctx, "over budget"); err == nil {
		t.Fatal("spawn past spent total must fail")
	} else if !strings.Contains(err.Error(), "budget spent") {
		t.Fatalf("error should name the budget, got: %v", err)
	}
	// The failed spawn must not have consumed the single concurrency slot:
	// a fresh total on the same limiter must still acquire it.
	fresh := WithTropicalLimiter(WithTropicalTotal(context.Background(), NewTropicalTotal(1)), lim)
	if _, err := spawnBackground(t, reg, fresh, "slot still free"); err != nil {
		t.Fatalf("slot leaked by failed total check: %v", err)
	}
}

// TestDelegateTaskReturnsSlotOnSpawnError pins the second leak path: when the
// runner fails to spawn, the acquired concurrency slot must be returned —
// the next spawn must not report "at cap".
func TestDelegateTaskReturnsSlotOnSpawnError(t *testing.T) {
	reg := NewRegistry(t.TempDir(), "", "", nil)
	defer func() { _ = reg.Close(context.Background()) }()
	reg.SetSubagentRunner(&failingSubagentRunner{})

	lim := NewTropicalLimiter(1)
	ctx := WithTropicalLimiter(WithTropicalTotal(context.Background(), NewTropicalTotal(100)), lim)
	if _, err := spawnBackground(t, reg, ctx, "doomed"); err == nil {
		t.Fatal("failing spawn must error")
	}
	// Swap in a working runner on the same limiter: the slot must be free.
	reg.SetSubagentRunner(&scriptedSubagentRunner{})
	ctx2 := WithTropicalLimiter(WithTropicalTotal(context.Background(), NewTropicalTotal(100)), lim)
	if _, err := spawnBackground(t, reg, ctx2, "after failure"); err != nil {
		t.Fatalf("slot leaked by failed spawn: %v", err)
	}
}

// TestDelegateTaskPropagatesTropicalFlag pins Step 3 plumbing: the Tropical
// flag survives the background context rebuild so runners observe it.
func TestDelegateTaskPropagatesTropicalFlag(t *testing.T) {
	reg := NewRegistry(t.TempDir(), "", "", nil)
	defer func() { _ = reg.Close(context.Background()) }()
	runner := &scriptedSubagentRunner{}
	reg.SetSubagentRunner(runner)

	ctx := WithTropical(WithEffort(context.Background(), "high"))
	ctx = WithTropicalLimiter(WithTropicalTotal(ctx, NewTropicalTotal(100)), NewTropicalLimiter(100))
	if _, err := spawnBackground(t, reg, ctx, "flagged"); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.sawTropical) != 1 || !runner.sawTropical[0] {
		t.Fatalf("runner observed tropical=%v, want [true]", runner.sawTropical)
	}
	if len(runner.sawEffort) != 1 || runner.sawEffort[0] != "high" {
		t.Fatalf("runner observed effort=%v, want [high]", runner.sawEffort)
	}
}
