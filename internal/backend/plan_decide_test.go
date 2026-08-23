package backend

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"sandbar/internal/agent"
	"sandbar/internal/config"
	"sandbar/internal/llm"
	"sandbar/internal/memory"
	"sandbar/internal/tools"
)

func TestLocalBackendDecidePlan(t *testing.T) {
	workspace := t.TempDir()
	cfg := &config.Config{
		Workspace: workspace,
		Persona:   config.PersonaConfig{Name: "Sandbar", SystemPrompt: "test", TitleModel: "missing-title-model"},
		Providers: []config.ProviderConfig{{
			Name: "test", BaseURL: "http://localhost:9999", APIKey: "test",
			Models: map[string]config.ModelConfig{"test-model": {}},
		}},
	}
	store, err := memory.OpenWithMigrations(filepath.Join(t.TempDir(), "local.db"), "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	agentRuntime := agent.New(cfg, store, llm.NewRegistry(cfg), tools.NewRegistry(workspace, "", "", nil))
	local := NewLocalBackend(cfg, store, agentRuntime, []string{"test-model"})

	thread, err := local.CreateThread("test-model")
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := local.DecidePlan(context.Background(), thread.ID, "approve"); !errors.Is(err, agent.ErrNoPendingPlan) {
		t.Fatalf("approve without pending plan: %v", err)
	}
	if err := local.DecidePlan(context.Background(), thread.ID, "bogus"); err == nil {
		t.Fatal("unknown action accepted")
	}
	if err := store.SetThreadPlanMode(thread.ID, memory.PlanModePendingApproval); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := local.DecidePlan(context.Background(), thread.ID, "approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	loaded, err := store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if loaded.PlanMode != memory.PlanModeApproved {
		t.Fatalf("after approve: plan_mode = %q, want %q", loaded.PlanMode, memory.PlanModeApproved)
	}
	if err := local.DecidePlan(context.Background(), thread.ID, "reject"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	loaded, _ = store.GetThread(thread.ID)
	if loaded.PlanMode != memory.PlanModeOff {
		t.Fatalf("after reject: plan_mode = %q, want off", loaded.PlanMode)
	}
}
