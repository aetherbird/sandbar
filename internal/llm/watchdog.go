package llm

import (
	"context"
	"time"

	"github.com/sashabaranov/go-openai"
)

// streamWatchdog closes a stalled streaming response so its reader unblocks.
// Providers emit deltas or keepalives continuously while generating; silence
// for streamIdleTimeout means the connection is dead (wedged gateway, silently
// dropped TCP). Without this, Recv blocks forever — the agent turn hangs with
// no error and no recovery except the user pressing Ctrl+C.
type streamWatchdog struct {
	idle    *time.Timer
	firedCh chan struct{}
	done    chan struct{}
}

// newStreamWatchdog arms the watchdog for stream. Call reset after every
// successful Recv; the returned value stays usable after firing.
func newStreamWatchdog(ctx context.Context, stream *openai.ChatCompletionStream) *streamWatchdog {
	w := &streamWatchdog{
		idle:    time.NewTimer(streamIdleTimeout),
		firedCh: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		select {
		case <-w.idle.C:
			// Closing the response body unblocks a concurrent Recv with an
			// error — the same mechanism ctx cancellation uses.
			_ = stream.Close()
			close(w.firedCh)
		case <-ctx.Done():
		case <-w.done:
		}
	}()
	return w
}

// reset pushes the idle window forward after received data.
func (w *streamWatchdog) reset() {
	w.idle.Reset(streamIdleTimeout)
}

// fired reports whether the watchdog has closed the stream.
func (w *streamWatchdog) fired() bool {
	select {
	case <-w.firedCh:
		return true
	default:
		return false
	}
}

// stop disarms the watchdog once reading has finished.
func (w *streamWatchdog) stop() {
	w.idle.Stop()
	close(w.done)
}
