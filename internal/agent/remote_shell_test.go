package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandbar/internal/llm"
	"sandbar/internal/tools"
)

// TestAgentRemoteShellExecExecutesOnFakeSSH proves the main chat loop accepts
// shell_exec with a host argument and routes it through the ssh transport: a
// fake ssh shim on PATH answers the call, and the model receives its stdout as
// a normal tool result. The model supplied a plain command — no nested quoting.
func TestAgentRemoteShellExecExecutesOnFakeSSH(t *testing.T) {
	// Fake ssh: whatever it receives, it prints the marker the model asked for.
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\necho remote-marker\n"), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		switch callCount {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"remote-1","type":"function","function":{"name":"shell_exec","arguments":"{\"command\":\"echo remote-marker\",\"host\":\"some-host\"}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"remote work done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = server.URL
	a.cfg.Persona.TitleModel = "missing-title-model"
	a.cfg.MaxTurns = 0
	a.tools = tools.NewRegistry(workspace, "", "", nil)

	var toolResults []string
	var terminal string
	_, err := a.Chat(context.Background(), Request{
		ModelAlias: "test-model", UserMessage: "run remotely", Workspace: workspace,
	}, func(event llm.StreamEvent) error {
		if event.Type == "tool_result" {
			toolResults = append(toolResults, event.Content)
		}
		if event.Type == "token" {
			terminal += event.Content
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(toolResults) != 1 || !strings.Contains(toolResults[0], "remote-marker") {
		t.Fatalf("remote tool result missing marker: %q", toolResults)
	}
	if terminal != "remote work done" {
		t.Fatalf("terminal content: got %q", terminal)
	}
}
