package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aetherbird/sandbar/internal/agent"
	"github.com/aetherbird/sandbar/internal/backend"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/mcp"
	"github.com/aetherbird/sandbar/internal/memory"
	"github.com/aetherbird/sandbar/internal/tools"
)

// parseToolAllowlist converts the --tools flag value into the canonical-name
// allowlist consumed by openLocalRuntime. Empty input means no restriction;
// empty entries (e.g. "file_read,,shell_exec") are rejected rather than
// silently ignored, and names are validated against the registry later.
func parseToolAllowlist(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	names := strings.Split(value, ",")
	allow := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("--tools contains an empty entry; pass a comma-separated list of tool names or omit the flag")
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		allow = append(allow, name)
	}
	return allow, nil
}

func backendModels(ctx context.Context, be backend.Backend) ([]string, error) {
	if be == nil {
		return nil, fmt.Errorf("backend is not configured")
	}
	if provider, ok := be.(backend.ModelsProvider); ok {
		return provider.Models(ctx)
	}
	return append([]string(nil), be.ListModels()...), nil
}

func chooseInitialModel(ctx context.Context, be backend.Backend, requested string) (string, error) {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested, nil
	}
	if fallback := strings.TrimSpace(be.DefaultModel()); fallback != "" {
		return fallback, nil
	}
	models, err := backendModels(ctx, be)
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", fmt.Errorf("backend returned no models and no default model is configured")
	}
	sort.Strings(models)
	return models[0], nil
}

func printWorkspaceWarning(be backend.Backend, threadID, workspace string, output io.Writer) error {
	if be == nil {
		return fmt.Errorf("backend is not configured")
	}
	if threadID == "" {
		return nil
	}
	detail, err := be.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("load thread %q: %w", threadID, err)
	}
	if warning := workspaceMismatchWarning(detail.Workspace, workspace); warning != "" && output != nil {
		if _, err := fmt.Fprintln(output, sty(cWarn).Render("  ⚠ "+warning)); err != nil {
			return fmt.Errorf("write workspace warning: %w", err)
		}
	}
	return nil
}

type localRuntime struct {
	cfg     *config.Config
	store   *memory.Store
	agent   *agent.Agent
	backend backend.Backend
	mcp     *mcp.Manager
}

type localRuntimeOptions struct {
	DisableSubagents bool
	// AllowedTools restricts the advertised registry to exactly these tool
	// names (canonical spelling). Nil means no restriction; an explicitly
	// empty (non-nil) slice strips every tool for a plain chat turn.
	AllowedTools []string
}

func openLocalRuntime(cfg *config.Config, dbPath, workspace string, options localRuntimeOptions) (*localRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("local configuration is required")
	}
	// Backend.LocalBackend obtains the active workspace from its config. Keep a
	// private runtime copy so a CLI cwd/--workspace override reaches both the
	// backend and Agent without mutating the loaded configuration shared by tests.
	runtimeCfg := *cfg
	runtimeCfg.Database = dbPath
	runtimeCfg.Workspace = workspace

	store, err := memory.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	registry := llm.NewRegistry(&runtimeCfg)
	// image_generate/vision_analyze talk to openrouter.ai directly; select the
	// key from the first provider actually pointing there rather than assuming
	// provider 0. No OpenRouter provider means an empty key, which those tools
	// report as an actionable configuration error.
	apiKey := ""
	for _, p := range runtimeCfg.Providers {
		if strings.Contains(p.BaseURL, "openrouter.ai") {
			apiKey = p.APIKey
			break
		}
	}
	var registryOptions []tools.RegistryOption
	if options.DisableSubagents {
		registryOptions = append(registryOptions, tools.WithoutSubagents())
	}
	toolRegistry := tools.NewRegistry(
		workspace,
		runtimeCfg.Tools.WebSearch.BraveAPIKey,
		apiKey,
		runtimeCfg.Tools.Shell.BlockedCommands,
		registryOptions...,
	)
	shellTimeout, err := runtimeCfg.ShellTimeout()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring shell timeout: %w", err)
	}
	if err := toolRegistry.SetShellTimeout(shellTimeout); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring shell timeout: %w", err)
	}
	jobSettings, err := runtimeCfg.ShellJobSettings()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring shell jobs: %w", err)
	}
	if err := toolRegistry.SetJobSupervisorConfig(tools.JobSupervisorConfig{
		MaxJobs: jobSettings.MaxJobs, MaxRunning: jobSettings.MaxRunning, OutputBytes: jobSettings.OutputBytes,
		Retention: jobSettings.Retention, TerminationGrace: jobSettings.TerminationGrace,
	}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring shell jobs: %w", err)
	}
	sshSettings, err := runtimeCfg.SSHSettings()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring remote shell: %w", err)
	}
	if err := toolRegistry.SetSSHConfig(tools.SSHRuntimeConfig{
		ConnectTimeout: sshSettings.ConnectTimeout,
		BatchMode:      sshSettings.BatchMode,
		AllowedHosts:   sshSettings.AllowedHosts,
	}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring remote shell: %w", err)
	}
	// MCP servers attach only when configured; zero entries means zero
	// overhead. Failures are isolated per server and surface once as boot
	// warnings on stderr — only context cancellation is fatal. Registration
	// happens before the allowlist restriction so --tools strips MCP tools
	// like any other.
	var mcpManager *mcp.Manager
	if len(runtimeCfg.MCP) > 0 {
		manager, warnings, err := mcp.Attach(context.Background(), toolRegistry, runtimeCfg.MCP)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("connecting MCP servers: %w", err)
		}
		for _, warning := range warnings {
			fmt.Fprintf(os.Stderr, "sandbar: %s\n", warning)
		}
		mcpManager = manager
	}
	if options.AllowedTools != nil {
		if err := toolRegistry.RestrictTo(options.AllowedTools); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("restricting tools: %w", err)
		}
	}
	approvalConfig, err := tools.ApprovalConfigFromToolConfig(runtimeCfg.Tools.Approval)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring tool approvals: %w", err)
	}
	if err := toolRegistry.SetApprovalConfig(approvalConfig); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configuring tool approvals: %w", err)
	}
	ag := agent.New(&runtimeCfg, store, registry, toolRegistry)
	models := registry.ListModels()
	be := backend.NewLocalBackend(&runtimeCfg, store, ag, models)
	return &localRuntime{cfg: &runtimeCfg, store: store, agent: ag, backend: be, mcp: mcpManager}, nil
}

func (r *localRuntime) close() {
	if r == nil {
		return
	}
	if r.agent != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = r.agent.Close(ctx)
		cancel()
	}
	// After the agent drains, tear down MCP sessions (and any stdio child
	// processes they own).
	if r.mcp != nil {
		_ = r.mcp.Close()
	}
	if r.store != nil {
		_ = r.store.Close()
	}
}
