package tools

import (
	"context"
	"fmt"
	"sync"
)

// TropicalLimiter bounds concurrently running tropical background subagents.
// The zero value is unusable — construct with NewTropicalLimiter. A negative
// limit means unlimited.
type TropicalLimiter struct {
	mu       sync.Mutex
	limit    int
	inflight int
}

// NewTropicalLimiter builds a limiter from an effective limit as resolved by
// SubagentConfig.TropicalConcurrencyLimit: negative = unlimited.
func NewTropicalLimiter(limit int) *TropicalLimiter {
	return &TropicalLimiter{limit: limit}
}

// TryAcquire records one running subagent or returns an error when the
// concurrency cap is reached. The caller must call Release when the subagent
// reaches a terminal state.
func (l *TropicalLimiter) TryAcquire() error {
	if l == nil || l.limit < 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight >= l.limit {
		return fmt.Errorf("tropical background fan-out at cap (%d running); resume an existing task or retry after one completes", l.limit)
	}
	l.inflight++
	return nil
}

// Release records one subagent reaching a terminal state.
func (l *TropicalLimiter) Release() {
	if l == nil || l.limit < 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight > 0 {
		l.inflight--
	}
}

// TropicalTotal bounds tropical background subagent spawns within one turn.
// A negative limit means unlimited.
type TropicalTotal struct {
	mu    sync.Mutex
	limit int
	spent int
}

// NewTropicalTotal builds a per-turn spawn counter from an effective limit as
// resolved by SubagentConfig.TropicalTotalLimit: negative = unlimited.
func NewTropicalTotal(limit int) *TropicalTotal {
	return &TropicalTotal{limit: limit}
}

// TryIncrement records one spawn or returns an error when the per-turn total
// cap is reached.
func (t *TropicalTotal) TryIncrement() error {
	if t == nil || t.limit < 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spent >= t.limit {
		return fmt.Errorf("tropical background spawn budget spent (%d this turn); consolidate remaining work into running tasks or continue serially", t.limit)
	}
	t.spent++
	return nil
}

type tropicalLimiterKey struct{}
type tropicalTotalKey struct{}

// WithTropicalLimiter carries the thread-scoped tropical concurrency limiter.
func WithTropicalLimiter(ctx context.Context, l *TropicalLimiter) context.Context {
	return context.WithValue(ctx, tropicalLimiterKey{}, l)
}

// TropicalLimiterFromContext returns the thread-scoped tropical limiter, or
// nil when the turn is not a capped tropical turn.
func TropicalLimiterFromContext(ctx context.Context) *TropicalLimiter {
	l, _ := ctx.Value(tropicalLimiterKey{}).(*TropicalLimiter)
	return l
}

// WithTropicalTotal carries the per-turn tropical spawn counter.
func WithTropicalTotal(ctx context.Context, t *TropicalTotal) context.Context {
	return context.WithValue(ctx, tropicalTotalKey{}, t)
}

// TropicalTotalFromContext returns the per-turn tropical spawn counter, or
// nil when the turn is not a capped tropical turn.
func TropicalTotalFromContext(ctx context.Context) *TropicalTotal {
	t, _ := ctx.Value(tropicalTotalKey{}).(*TropicalTotal)
	return t
}
