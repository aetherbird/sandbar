package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sashabaranov/go-openai"

	"sandbar/internal/llm"
)

// ErrNoActiveTurn reports that a message or interrupt was requested for a
// thread with no in-flight turn. Callers map this to a 409/404 at the HTTP
// boundary and to a silent fallback in the CLI.
var ErrNoActiveTurn = errors.New("no active turn for thread")

// ErrSteeringQueueFull reports that a thread's steering queue reached its cap.
var ErrSteeringQueueFull = errors.New("steering queue is full")

// steeringQueueCap bounds queued mid-turn messages per thread so a runaway
// frontend cannot grow the queue without bound.
const steeringQueueCap = 8

// steeringWrapper prefixes a queued user message when it is injected into the
// running conversation. It is byte-stable: tests pin it, and it must not change
// without updating them.
const steeringWrapper = "[The user sent this message while you were working. It takes priority and supersedes earlier instructions where they conflict. Address it as you continue the current turn.]\n\n"

// steeringQueues holds the per-thread mid-turn message queues and the set of
// threads with an active turn. A single mutex makes the active-check-and-append
// in EnqueueUserMessage atomic with respect to beginTurn/endTurn, so a message
// enqueued before a turn's exit critical section is always drained, and one
// enqueued after gets a clean ErrNoActiveTurn.
type steeringQueues struct {
	mu     sync.Mutex
	active map[string]bool
	queues map[string][]string
}

func newSteeringQueues() *steeringQueues {
	return &steeringQueues{
		active: make(map[string]bool),
		queues: make(map[string][]string),
	}
}

// EnqueueUserMessage records a user message to be injected at the next tool
// boundary of the thread's active turn. It trims and rejects empty input, and
// fails when the thread has no active turn or its queue is full.
func (q *steeringQueues) EnqueueUserMessage(threadID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("message is empty")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.active[threadID] {
		return ErrNoActiveTurn
	}
	if len(q.queues[threadID]) >= steeringQueueCap {
		return ErrSteeringQueueFull
	}
	q.queues[threadID] = append(q.queues[threadID], text)
	return nil
}

// beginTurn marks threadID as having an active turn.
func (q *steeringQueues) beginTurn(threadID string) {
	q.mu.Lock()
	q.active[threadID] = true
	q.mu.Unlock()
}

// endTurn clears the active flag and drains the queue. It returns the drained
// messages (clearing the queue) so the caller can persist undelivered ones.
func (q *steeringQueues) endTurn(threadID string) []string {
	q.mu.Lock()
	delete(q.active, threadID)
	msgs := q.queues[threadID]
	q.queues[threadID] = nil
	q.mu.Unlock()
	return msgs
}

// drain returns and clears the queued messages for threadID without clearing
// the active flag — used at the mid-turn tool boundary.
func (q *steeringQueues) drain(threadID string) []string {
	q.mu.Lock()
	msgs := q.queues[threadID]
	q.queues[threadID] = nil
	q.mu.Unlock()
	return msgs
}

// EnqueueUserMessage queues a user message for delivery at the next tool
// boundary of an active turn.
func (a *Agent) EnqueueUserMessage(threadID, text string) error {
	if a.steering == nil {
		return ErrNoActiveTurn
	}
	return a.steering.EnqueueUserMessage(threadID, text)
}

// turnCancelEntry wraps a cancel func in a comparable pointer so sync.Map can
// hold it (func values are not comparable) and unregister can verify it still
// owns the slot before deleting.
type turnCancelEntry struct {
	cancel context.CancelFunc
}

// RegisterTurnCancel registers the cancel func for a thread's active turn and
// returns an unregister func. InterruptThread looks the cancel up by thread ID.
// The caller derives the cancel (context.WithCancel) and passes it here before
// running Chat; unregister must run when the turn ends.
func (a *Agent) RegisterTurnCancel(threadID string, cancel context.CancelFunc) (unregister func()) {
	entry := &turnCancelEntry{cancel: cancel}
	a.turnCancels.Store(threadID, entry)
	return func() {
		// Only remove the slot if we still own it — a newer turn on the same
		// thread may have replaced the entry.
		if v, ok := a.turnCancels.Load(threadID); ok && v == entry {
			a.turnCancels.Delete(threadID)
		}
	}
}

// InterruptThread cancels the active turn for threadID, if one is registered.
// It returns ErrNoActiveTurn when the thread has no in-flight turn.
func (a *Agent) InterruptThread(threadID string) error {
	v, ok := a.turnCancels.Load(threadID)
	if !ok {
		return ErrNoActiveTurn
	}
	v.(*turnCancelEntry).cancel()
	return nil
}

// drainSteering persists each queued steering message for threadID and returns
// the indexed messages to inject into the in-memory history plus the raw-text
// events to emit. Persistence errors are returned so the caller can surface
// them (a steering message that cannot be persisted must not silently vanish).
func (a *Agent) drainSteering(threadID string, sink func(llm.StreamEvent) error) ([]indexedMessage, error) {
	raw := a.steering.drain(threadID)
	if len(raw) == 0 {
		return nil, nil
	}
	indexed := make([]indexedMessage, 0, len(raw))
	for _, text := range raw {
		content := steeringWrapper + text
		msg, err := a.store.AppendMessage(threadID, "user", &content, nil)
		if err != nil {
			return indexed, fmt.Errorf("persist steering message: %w", err)
		}
		indexed = append(indexed, indexedMessage{
			Seq:  msg.Seq,
			Kind: "thread_message",
			Msg:  openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content},
		})
		// Emit the raw text so frontends confirm what was injected without
		// re-rendering the wrapper prefix.
		if err := sink(llm.StreamEvent{Type: "user_message", Content: text, ThreadID: threadID}); err != nil {
			return indexed, err
		}
	}
	return indexed, nil
}

// beginTurn marks a thread as actively running; pair with endSteeringTurn.
func (a *Agent) beginTurn(threadID string) {
	a.steering.beginTurn(threadID)
}

// endSteeringTurn clears the active flag and persists any queued-but-undelivered
// messages. A turn with no tool boundary (a pure streaming model, or a turn that
// errored before any tool call) never drains mid-turn; persisting here keeps the
// user's message from being lost. Best-effort: a store failure here must not
// mask the turn's own result.
func (a *Agent) endSteeringTurn(threadID string) {
	msgs := a.steering.endTurn(threadID)
	for _, text := range msgs {
		content := steeringWrapper + text
		_, _ = a.store.AppendMessage(threadID, "user", &content, nil)
	}
}
