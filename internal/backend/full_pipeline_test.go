package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sandbar/internal/agent"
	"sandbar/internal/config"
	"sandbar/internal/llm"
	"sandbar/internal/memory"
	"sandbar/internal/tools"
)

// TestFullTuiPipeline simulates the exact TUI flow:
// createThread -> startSend -> pumpStream -> events -> done.
// Requires OPENROUTER_API_KEY set in environment.
// Uses a temporary database so production data is never touched.
func TestFullTuiPipeline(t *testing.T) {
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	cfg, store, ag, models := setupFromConfigWithTempDB(t)
	defer store.Close()

	backend := NewLocalBackend(cfg, store, ag, models)

	// Step 1: Create a thread (like TUI Init does)
	thread, err := backend.CreateThread("deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	t.Logf("created thread: %s", thread.ID)

	// Step 2: Send a message (like startSend does)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Logf("sending message via SendMessage...")
	ch, err := backend.SendMessage(ctx, thread.ID, "deepseek/deepseek-v4-flash", "say exactly: pong", "", false)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Step 3: Drain events (like pumpStream does)
	deadline := time.After(30 * time.Second)
	eventCount := 0
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Logf("channel closed after %d events", eventCount)
				if eventCount == 0 {
					t.Fatal("ZERO events received -- agent never emitted anything")
				}
				return
			}
			eventCount++
			t.Logf("event %d: type=%s", eventCount, ev.Type)
			if ev.Type == "error" {
				t.Fatalf("BACKEND ERROR: %s", ev.Content)
			}
			if ev.Type == "done" {
				t.Logf("SUCCESS: %d events received, done received", eventCount)
				return
			}
		case <-deadline:
			t.Fatalf("TIMEOUT after 30s -- only %d events received", eventCount)
		}
	}
}

// pipelineCfgPath is the system-wide config location used to source API keys
// and model definitions for the live pipeline test.
const pipelineCfgPath = "/etc/sandbar/config.yaml"

// setupFromConfigWithTempDB loads the production config to get API keys and
// model definitions, overriding the database to a temp file.
func setupFromConfigWithTempDB(t *testing.T) (*config.Config, *memory.Store, *agent.Agent, []string) {
	t.Helper()

	cfgPath := pipelineCfgPath
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// Use a temporary database so production data is never touched.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := memory.OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	registry := llm.NewRegistry(cfg)
	apiKey := ""
	if len(cfg.Providers) > 0 {
		apiKey = cfg.Providers[0].APIKey
	}
	t.Logf("API key present: %v", apiKey != "")

	toolReg := tools.NewRegistry(cfg.Workspace, cfg.Tools.WebSearch.BraveAPIKey, apiKey, cfg.Tools.Shell.BlockedCommands)
	ag := agent.New(cfg, store, registry, toolReg)
	return cfg, store, ag, registry.ListModels()
}
