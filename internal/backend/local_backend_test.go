package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/tools"
)

func TestLocalBackendSendMessageCancellationDoesNotBlockOnFullEventChannel(t *testing.T) {
	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		close(requestStarted)
		for i := 0; i < 256; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token-%d \"},\"finish_reason\":null}]}\n\n", i)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	supportsTools := false
	workspace := t.TempDir()
	cfg := &config.Config{
		Workspace: workspace,
		Persona:   config.PersonaConfig{Name: "Sandbar", SystemPrompt: "test", TitleModel: "missing-title-model"},
		Providers: []config.ProviderConfig{{
			Name: "test", BaseURL: server.URL, APIKey: "test",
			Models: map[string]config.ModelConfig{"test-model": {SupportsTools: &supportsTools}},
		}},
	}
	store, err := memory.OpenWithMigrations(filepath.Join(t.TempDir(), "local.db"), "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	toolRegistry := tools.NewRegistry(workspace, "", "", nil)
	agentRuntime := agent.New(cfg, store, llm.NewRegistry(cfg), toolRegistry)
	defer func() {
		_ = agentRuntime.Close(context.Background())
		_ = store.Close()
	}()
	local := NewLocalBackend(cfg, store, agentRuntime, []string{"test-model"})
	thread, err := local.CreateThread("test-model")
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	events, err := local.SendMessage(ctx, thread.ID, "test-model", "stream many events", "", false)
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}
	// Leave the 32-slot result channel unread until the producer is blocked,
	// then cancel. Forwarding must observe cancellation instead of waiting
	// forever for a consumer that has gone away.
	time.Sleep(50 * time.Millisecond)
	cancel()

	drained := make(chan struct{})
	go func() {
		for range events {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("local event stream did not close after cancellation")
	}
}
