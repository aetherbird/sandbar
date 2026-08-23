package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/persona"

	openai "github.com/sashabaranov/go-openai"
)

// ThreadSummary is a lightweight thread representation.
type ThreadSummary struct {
	ID        string
	Title     string
	UpdatedAt int64
	Model     string
	Workspace string
	// PlanMode is the thread's plan-approval lifecycle state ("" = off); lets
	// frontends restore plan state when resuming a session.
	PlanMode string
}

// ThreadDetail includes messages.
type ThreadDetail struct {
	ThreadSummary
	Messages []Message
}

// Message is a UI-friendly message.
type Message struct {
	Role    string
	Content string
	Seq     int
}

// Backend abstracts the CLI's chat backend.
type Backend interface {
	ListThreads() ([]ThreadSummary, error)
	GetThread(id string) (*ThreadDetail, error)
	CreateThread(model string) (*ThreadSummary, error)
	DeleteThread(id string) error
	RenameThread(id, title string) error
	UndoThread(id string, seq int) error
	SendMessage(ctx context.Context, threadID, model, message, effort string, plan bool) (<-chan llm.StreamEvent, error)
	ListModels() []string
	DefaultModel() string
	Workspace() string
	GetContextInfo(threadID string) (used, max int, err error)
}

// ModelsProvider is an optional error-returning replacement for ListModels.
// Backend keeps ListModels for compatibility with existing callers.
type ModelsProvider interface {
	Models(ctx context.Context) ([]string, error)
}

// MessageQueuer is an optional interface for mid-turn steering: queueing a user
// message for delivery at the next tool boundary, and interrupting an in-flight
// turn. Callers type-assert a Backend to this rather than expanding the stable
// interface (same pattern as ModelsProvider).
type MessageQueuer interface {
	EnqueueUserMessage(ctx context.Context, threadID, text string) error
	InterruptThread(ctx context.Context, threadID string) error
}

// TodoLister is an optional interface for reading a thread's durable plan
// todos. Callers type-assert a Backend to this rather than expanding the
// stable interface (same pattern as ModelsProvider).
type TodoLister interface {
	ListTodos(ctx context.Context, threadID string) ([]memory.TodoItem, error)
}

// PlanDecider is an optional interface for approving or rejecting a thread's
// pending plan (the plan-mode lifecycle). Callers type-assert a Backend to
// this rather than expanding the stable interface (same pattern as
// MessageQueuer / TodoLister).
type PlanDecider interface {
	// DecidePlan applies action ("approve" or "reject") to threadID's pending
	// plan. Errors include agent.ErrNoPendingPlan (nothing awaiting a
	// decision).
	DecidePlan(ctx context.Context, threadID, action string) error
}

// LocalBackend uses in-process agent and store.
type LocalBackend struct {
	cfg    *config.Config
	store  *memory.Store
	agent  *agent.Agent
	models []string
}

func NewLocalBackend(cfg *config.Config, store *memory.Store, ag *agent.Agent, models []string) *LocalBackend {
	return &LocalBackend{cfg: cfg, store: store, agent: ag, models: models}
}

func (b *LocalBackend) ListThreads() ([]ThreadSummary, error) {
	threads, err := b.store.ListThreads()
	if err != nil {
		return nil, err
	}
	var out []ThreadSummary
	for _, t := range threads {
		title := "Untitled"
		if t.Title != nil && *t.Title != "" {
			title = *t.Title
		}
		model := ""
		if t.Model != nil {
			model = *t.Model
		}
		out = append(out, ThreadSummary{
			ID:        t.ID,
			Title:     title,
			UpdatedAt: t.UpdatedAt.Unix(),
			Model:     model,
			Workspace: t.Workspace,
			PlanMode:  t.PlanMode,
		})
	}
	return out, nil
}

func (b *LocalBackend) GetThread(id string) (*ThreadDetail, error) {
	thread, msgs, err := b.store.GetThreadWithMessages(id)
	if err != nil {
		return nil, err
	}
	title := "Untitled"
	if thread.Title != nil && *thread.Title != "" {
		title = *thread.Title
	}
	model := ""
	if thread.Model != nil {
		model = *thread.Model
	}
	detail := &ThreadDetail{
		ThreadSummary: ThreadSummary{
			ID:        thread.ID,
			Title:     title,
			UpdatedAt: thread.UpdatedAt.Unix(),
			Model:     model,
			Workspace: thread.Workspace,
			PlanMode:  thread.PlanMode,
		},
	}
	for _, m := range msgs {
		msg := Message{Role: m.Role, Seq: m.Seq}
		if m.Content != nil {
			msg.Content = *m.Content
		}
		detail.Messages = append(detail.Messages, msg)
	}
	return detail, nil
}

func (b *LocalBackend) CreateThread(model string) (*ThreadSummary, error) {
	var m *string
	if model != "" {
		m = &model
	}
	t, err := b.store.CreateThreadWithWorkspace(nil, m, b.cfg.Workspace)
	if err != nil {
		return nil, err
	}
	return &ThreadSummary{
		ID:        t.ID,
		Title:     "Untitled",
		UpdatedAt: unixOrZero(t.UpdatedAt),
		Model:     model,
		Workspace: t.Workspace,
	}, nil
}

func (b *LocalBackend) DeleteThread(id string) error {
	if b.agent == nil {
		return b.store.DeleteThread(id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return b.agent.DeleteThread(ctx, id)
}

func (b *LocalBackend) RenameThread(id, title string) error {
	return b.store.UpdateThreadTitle(id, title)
}

func (b *LocalBackend) UndoThread(id string, seq int) error {
	return b.store.DeleteMessagesAfter(id, seq)
}

func (b *LocalBackend) SendMessage(ctx context.Context, threadID, model, message, effort string, plan bool) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 32)
	go func() {
		defer close(ch)
		// Derive a cancellable turn context and register it so an out-of-band
		// interrupt can stop this turn. Only existing threads can be
		// interrupted (a new thread's ID is not known until Chat creates it).
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if threadID != "" && b.agent != nil {
			unregister := b.agent.RegisterTurnCancel(threadID, cancel)
			defer unregister()
		}
		send := func(ev llm.StreamEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-turnCtx.Done():
				return false
			}
		}
		defer func() {
			if r := recover(); r != nil {
				_ = send(llm.StreamEvent{Type: "error", Content: fmt.Sprintf("panic: %v", r)})
			}
		}()
		req := agent.Request{
			ThreadID:    threadID,
			ModelAlias:  model,
			UserMessage: message,
			Workspace:   b.cfg.Workspace,
			Effort:      effort,
			PlanOnly:    plan,
		}
		// The agent already emits canonical events — forward them as-is.
		tid, err := b.agent.Chat(turnCtx, req, func(ev llm.StreamEvent) error {
			if !send(ev) {
				return turnCtx.Err()
			}
			return nil
		})
		// Belt-and-braces: if the agent's turn-start "thread" announcement was
		// never delivered (e.g. the context was cancelled before the first
		// emission), still surface the returned thread ID so callers can resume
		// the conversation instead of silently starting a new thread.
		if tid != "" {
			_ = send(llm.StreamEvent{Type: "thread", ThreadID: tid})
		}
		if err != nil && turnCtx.Err() == nil {
			_ = send(llm.StreamEvent{Type: "error", Content: err.Error()})
		}
	}()
	return ch, nil
}

// EnqueueUserMessage queues a user message for delivery at the next tool
// boundary of an active turn (see agent.EnqueueUserMessage).
func (b *LocalBackend) EnqueueUserMessage(ctx context.Context, threadID, text string) error {
	if b.agent == nil {
		return agent.ErrNoActiveTurn
	}
	return b.agent.EnqueueUserMessage(threadID, text)
}

// InterruptThread cancels the active turn for threadID (see agent.InterruptThread).
func (b *LocalBackend) InterruptThread(ctx context.Context, threadID string) error {
	if b.agent == nil {
		return agent.ErrNoActiveTurn
	}
	return b.agent.InterruptThread(threadID)
}

// ListTodos returns the durable plan todos for threadID in plan order. An
// unknown thread is an error; a thread without a plan returns an empty list.
func (b *LocalBackend) ListTodos(ctx context.Context, threadID string) ([]memory.TodoItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := b.store.GetThread(threadID); err != nil {
		return nil, err
	}
	return b.store.ListTodos(threadID)
}

// DecidePlan approves or rejects the thread's pending plan via the agent.
func (b *LocalBackend) DecidePlan(ctx context.Context, threadID, action string) error {
	if b.agent == nil {
		return agent.ErrNoActiveTurn
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch action {
	case "approve":
		return b.agent.ApprovePlan(threadID)
	case "reject":
		return b.agent.RejectPlan(threadID)
	default:
		return fmt.Errorf("unknown plan action %q (use approve or reject)", action)
	}
}

func (b *LocalBackend) ListModels() []string {
	models, _ := b.Models(context.Background())
	return models
}

// Models returns a defensive copy and provides the error-returning optional
// model-provider API.
func (b *LocalBackend) Models(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]string(nil), b.models...), nil
}
func (b *LocalBackend) DefaultModel() string {
	if len(b.models) > 0 {
		return b.models[0]
	}
	return ""
}
func (b *LocalBackend) Workspace() string { return b.cfg.Workspace }

func (b *LocalBackend) GetContextInfo(threadID string) (used, max int, err error) {
	thread, err := b.store.GetThread(threadID)
	if err != nil {
		return 0, 0, err
	}
	contextMax := 8192
	if thread.Model != nil {
		if resolved, err := llm.NewRegistry(b.cfg).ResolveModel(*thread.Model); err == nil && resolved.ContextLength > 0 {
			contextMax = resolved.ContextLength
		}
	}

	// Build a compression-aware message list matching what buildMessages
	// produces, so the status bar and compression display agree.
	var msgs []openai.ChatCompletionMessage

	// System prompt (always first). The active model comes from the thread so
	// the environment block reflects what is running.
	pp := persona.Persona{
		Name:         b.cfg.Persona.Name,
		SystemPrompt: b.cfg.Persona.SystemPrompt,
	}
	model := ""
	if thread.Model != nil {
		model = *thread.Model
	}
	sysPrompt := pp.BuildSystemPrompt(b.cfg.Workspace, model)
	msgs = append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: sysPrompt})

	// Compression summary, if one exists.
	rec, err := b.store.GetLatestCompression(threadID)
	if err != nil {
		return 0, 0, fmt.Errorf("get latest compression: %w", err)
	}
	if rec != nil && rec.FirstKeptSeq > 0 {
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: "[Compressed context from earlier: " + rec.Summary + "]",
		})
		_, history, histErr := b.store.GetThreadWithMessagesFromSeq(threadID, rec.FirstKeptSeq)
		if histErr != nil {
			return 0, 0, fmt.Errorf("get compressed thread messages: %w", histErr)
		}
		for _, m := range history {
			msg := openai.ChatCompletionMessage{Role: m.Role}
			if m.Content != nil {
				msg.Content = *m.Content
			}
			msgs = append(msgs, msg)
		}
	} else {
		_, history, histErr := b.store.GetThreadWithMessages(threadID)
		if histErr != nil {
			return 0, 0, fmt.Errorf("get thread messages: %w", histErr)
		}
		for _, m := range history {
			msg := openai.ChatCompletionMessage{Role: m.Role}
			if m.Content != nil {
				msg.Content = *m.Content
			}
			msgs = append(msgs, msg)
		}
	}

	contextUsed := llm.NewTokenCounter().CountMessages(msgs)
	return contextUsed, contextMax, nil
}

// unixOrZero normalizes a zero time.Time to Unix epoch 0 so an unset
// timestamp reads as 0 rather than year 1 in Unix seconds.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
