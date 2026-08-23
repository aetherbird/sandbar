package agent

import (
	"context"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/llm"
)

// warmupTimeout is the generous timeout for the initial cold-start request.
// Local models may need several minutes to load weights from disk into GPU
// memory on first use.
const warmupTimeout = 10 * time.Minute

// keepaliveInterval is how often we ping the compression model to prevent the
// serving framework (e.g. Ollama, which evicts idle models after 5 minutes)
// from unloading the weights. 4 minutes stays just under that eviction window.
const keepaliveInterval = 4 * time.Minute

// keepalivePingTimeout caps each background ping. If the model is already
// warm this completes in <1s. If it somehow went cold, we don't want the
// ping goroutine blocking for minutes.
const keepalivePingTimeout = 60 * time.Second

// WarmupCompressionModel sends a tiny dummy request to the configured
// compression model so its weights load at startup, not mid-conversation.
// Best-effort: background goroutine, errors ignored. Only warms a dedicated
// compression model — the chat model fallback warms on first use anyway.
func (a *Agent) WarmupCompressionModel() {
	if !a.cfg.Compression.Enabled {
		return
	}
	modelAlias := a.cfg.Compression.Model
	if modelAlias == "" {
		return // no dedicated compression model; chat model warms on first use
	}
	resolved, err := a.registry.ResolveModel(modelAlias)
	if err != nil {
		return
	}
	go a.pingModel(resolved, warmupTimeout)
}

// StartKeepalive periodically pings the compression model to keep its weights
// resident (Ollama evicts idle models after 5 minutes). Call StopKeepalive on
// shutdown.
func (a *Agent) StartKeepalive() {
	if !a.cfg.Compression.Enabled {
		return
	}
	modelAlias := a.cfg.Compression.Model
	if modelAlias == "" {
		return
	}
	resolved, err := a.registry.ResolveModel(modelAlias)
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.keepaliveCancel = cancel

	go func() {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.pingModel(resolved, keepalivePingTimeout)
			}
		}
	}()
}

// StopKeepalive stops the background keepalive goroutine. Safe to call even
// if StartKeepalive was never called.
func (a *Agent) StopKeepalive() {
	if a.keepaliveCancel != nil {
		a.keepaliveCancel()
	}
}

// pingModel sends a minimal 1-token request to the resolved model. Silently
// ignores all errors — this is best-effort warm-up, not a health check.
func (a *Agent) pingModel(resolved llm.ResolvedModel, timeout time.Duration) {
	client := llm.NewWireClient(resolved)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _ = client.Complete(ctx, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "OK"},
	}, nil)
}
