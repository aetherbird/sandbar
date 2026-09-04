package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadValid(t *testing.T) {
	cfg, err := Load("../../tests/fixtures/config.valid.yaml")
	if err != nil {
		t.Fatalf("load valid config: %v", err)
	}
	if cfg.Workspace != "./workspace" {
		t.Errorf("workspace: got %q", cfg.Workspace)
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("providers: got %d, want 2", len(cfg.Providers))
	}
	if cfg.MaxTurns != 0 {
		t.Errorf("omitted max_turns: got %d, want unlimited (0)", cfg.MaxTurns)
	}
	if cfg.Subagent.MaxTurns != 0 {
		t.Errorf("omitted subagent.max_turns: got %d, want unlimited (0)", cfg.Subagent.MaxTurns)
	}
	if cfg.Compression.PostCompressionRatio != 0.50 {
		t.Errorf("omitted compression.post_compression_ratio: got %v, want 0.50", cfg.Compression.PostCompressionRatio)
	}
	if cfg.Tools.Approval.Mode != "yolo" {
		t.Errorf("omitted tools.approval.mode: got %q, want yolo", cfg.Tools.Approval.Mode)
	}
}

func TestToolApprovalConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "valid modes and policies",
			yaml: `
tools:
  approval:
    mode: always-ask
    rules:
      shell_exec: deny
      file_write: prompt
    tiers:
      read: allow
      exec: prompt
`,
		},
		{name: "invalid mode", yaml: "tools:\n  approval:\n    mode: permissive\n", wantErr: true},
		{name: "invalid rule policy", yaml: "tools:\n  approval:\n    rules:\n      shell_exec: sometimes\n", wantErr: true},
		{name: "invalid tier", yaml: "tools:\n  approval:\n    tiers:\n      destructive: deny\n", wantErr: true},
		{name: "invalid tier policy", yaml: "tools:\n  approval:\n    tiers:\n      write: sometimes\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected approval config validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Tools.Approval.Mode != "always-ask" || cfg.Tools.Approval.Rules["shell_exec"] != "deny" || cfg.Tools.Approval.Tiers["exec"] != "prompt" {
				t.Fatalf("approval config = %#v", cfg.Tools.Approval)
			}
		})
	}
}

func TestShellTimeoutValidation(t *testing.T) {
	for _, timeout := range []string{"not-a-duration", "0s", "-1s"} {
		t.Run(timeout, func(t *testing.T) {
			cfg := Config{Tools: ToolsConfig{Shell: ShellConfig{Timeout: timeout}}}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("timeout %q unexpectedly validated", timeout)
			}
		})
	}
}

// TestSudoBlocklistEntryRejected verifies that a `sudo …` blocklist entry is
// rejected at config validation with a clear error, instead of being silently
// dropped at runtime (sudo is never blockable — see neverBlocked in
// internal/tools/shell.go).
func TestSudoBlocklistEntryRejected(t *testing.T) {
	cfg := Config{Tools: ToolsConfig{Shell: ShellConfig{BlockedCommands: []string{"rm -rf /", "sudo rm -rf /"}}}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("sudo blocklist entry unexpectedly validated")
	}
	if !strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), "blocked_commands") {
		t.Fatalf("error should name the offending entry and field, got %v", err)
	}

	// Ordinary entries must still validate.
	ok := Config{Tools: ToolsConfig{Shell: ShellConfig{BlockedCommands: []string{"rm -rf /", "chmod 777"}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("ordinary blocklist entries should validate: %v", err)
	}
}

func TestLoadMaxTurnsSemantics(t *testing.T) {
	tests := []struct {
		name         string
		yaml         string
		wantMain     int
		wantSubagent int
		wantErr      bool
	}{
		{
			name:         "zero is unlimited",
			yaml:         "max_turns: 0\nsubagent:\n  max_turns: 0\n",
			wantMain:     0,
			wantSubagent: 0,
		},
		{
			name:         "positive is finite",
			yaml:         "max_turns: 7\nsubagent:\n  max_turns: 3\n",
			wantMain:     7,
			wantSubagent: 3,
		},
		{name: "negative main is invalid", yaml: "max_turns: -1\n", wantErr: true},
		{name: "negative subagent is invalid", yaml: "subagent:\n  max_turns: -1\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.MaxTurns != tc.wantMain {
				t.Fatalf("max_turns: got %d, want %d", cfg.MaxTurns, tc.wantMain)
			}
			if cfg.Subagent.MaxTurns != tc.wantSubagent {
				t.Fatalf("subagent.max_turns: got %d, want %d", cfg.Subagent.MaxTurns, tc.wantSubagent)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalidContext(t *testing.T) {
	_, err := Load("../../tests/fixtures/config.invalid-context.yaml")
	if err == nil {
		t.Fatal("expected error for zero context_length")
	}
}

func TestCompressionConfigValidation(t *testing.T) {
	base := `
providers:
  - name: openrouter-direct
    base_url: "https://openrouter.ai/api/v1"
    api_key: ""
    models:
      google/gemini-3.1-flash-lite:
        context_length: 262144
`
	tests := []struct {
		name      string
		injection string
		wantErr   bool
	}{
		{
			name: "valid compression config",
			injection: `
compression:
  enabled: true
  threshold: 0.80
  target_ratio: 0.20
  min_summary_tokens: 1000
  max_summary_tokens: 12000
  model: "google/gemini-3.1-flash-lite"
`,
			wantErr: false,
		},
		{
			name: "empty model falls back to current chat model",
			injection: `
compression:
  enabled: true
  threshold: 0.80
  target_ratio: 0.20
  model: ""
`,
			wantErr: false,
		},
		{
			name: "threshold one is invalid",
			injection: `
compression:
  enabled: true
  threshold: 1.0
  target_ratio: 0.20
`,
			wantErr: true,
		},
		{
			name: "threshold negative is invalid",
			injection: `
compression:
  enabled: true
  threshold: -0.1
  target_ratio: 0.20
`,
			wantErr: true,
		},
		{
			name: "target_ratio greater than one is invalid",
			injection: `
compression:
  enabled: true
  threshold: 0.80
  target_ratio: 1.5
`,
			wantErr: true,
		},
		{
			name: "post compression ratio must be below trigger threshold",
			injection: `
compression:
  enabled: true
  threshold: 0.70
  target_ratio: 0.20
  post_compression_ratio: 0.70
`,
			wantErr: true,
		},
		{
			name: "min_summary_tokens exceeds max_summary_tokens",
			injection: `
compression:
  enabled: true
  threshold: 0.80
  target_ratio: 0.20
  min_summary_tokens: 20000
  max_summary_tokens: 12000
`,
			wantErr: true,
		},
		{
			name: "negative recent tail is invalid",
			injection: `
compression:
  enabled: true
  threshold: 0.80
  target_ratio: 0.20
  recent_tail_tokens: -1
`,
			wantErr: true,
		},
		{
			name: "unknown compression model is invalid",
			injection: `
compression:
  enabled: true
  threshold: 0.80
  target_ratio: 0.20
  model: "unknown/model-name"
`,
			wantErr: true,
		},
		{
			name: "compression disabled skips validation",
			injection: `
compression:
  enabled: false
  threshold: 0
  target_ratio: 0
  model: "unknown/model-name"
`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(tmp, []byte(base+tc.injection), 0644); err != nil {
				t.Fatalf("write temp config: %v", err)
			}
			_, err := Load(tmp)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			}
		})
	}
}

func TestDuplicateAliasAcrossProviders(t *testing.T) {
	// The same model alias on multiple providers is allowed — the same
	// model can live on different hosts. This should NOT error.
	base := `
providers:
  - name: host-a
    base_url: "http://a:8080/v1"
    api_key: ""
    models:
      local/shared:
        context_length: 131072
  - name: host-b
    base_url: "http://b:8080/v1"
    api_key: ""
    models:
      local/shared:
        context_length: 262144
`
	tmp := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmp, []byte(base), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("duplicate alias across providers should be allowed: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(cfg.Providers))
	}
}

func TestShellJobSettingsDefaults(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.ShellJobSettings()
	if err != nil {
		t.Fatalf("default shell job settings: %v", err)
	}
	if got.MaxJobs != 128 || got.MaxRunning != 16 || got.OutputBytes != 64*1024 || got.Retention != 30*time.Minute || got.TerminationGrace != 750*time.Millisecond {
		t.Fatalf("unexpected default shell job settings: %+v", got)
	}
}

func TestLoadCustomShellJobSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
tools:
  shell:
    jobs:
      max_jobs: 9
      max_running: 3
      output_bytes: 4096
      retention: 45s
      termination_grace: 125ms
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	got, err := cfg.ShellJobSettings()
	if err != nil {
		t.Fatalf("parse shell job settings: %v", err)
	}
	if got.MaxJobs != 9 || got.MaxRunning != 3 || got.OutputBytes != 4096 || got.Retention != 45*time.Second || got.TerminationGrace != 125*time.Millisecond {
		t.Fatalf("unexpected custom shell job settings: %+v", got)
	}
}

func TestShellJobSettingsValidation(t *testing.T) {
	tests := []struct {
		name string
		jobs ShellJobsConfig
	}{
		{name: "negative max jobs", jobs: ShellJobsConfig{MaxJobs: -1}},
		{name: "running exceeds retained", jobs: ShellJobsConfig{MaxJobs: 2, MaxRunning: 3}},
		{name: "negative output", jobs: ShellJobsConfig{OutputBytes: -1}},
		{name: "invalid retention", jobs: ShellJobsConfig{Retention: "later"}},
		{name: "nonpositive grace", jobs: ShellJobsConfig{TerminationGrace: "-1ms"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Tools: ToolsConfig{Shell: ShellConfig{Jobs: tc.jobs}}}
			if _, err := cfg.ShellJobSettings(); err == nil {
				t.Fatal("invalid shell job settings unexpectedly succeeded")
			}
		})
	}
}

func TestProviderReasoningStyleValidation(t *testing.T) {
	valid := func(style string) *Config {
		return &Config{Providers: []ProviderConfig{{Name: "p", BaseURL: "http://x", APIKey: "k", ReasoningStyle: style}}}
	}
	for _, ok := range []string{"", "openai", "enable_thinking", "none"} {
		if err := valid(ok).Validate(); err != nil {
			t.Errorf("style %q should validate: %v", ok, err)
		}
	}
	if err := valid("bogus").Validate(); err == nil || !strings.Contains(err.Error(), "reasoning_style") {
		t.Errorf("bogus style should be rejected, got %v", err)
	}
}

func TestMCPServerConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "local with command",
			yaml: `
mcp_servers:
  fs:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
`,
		},
		{
			name: "remote with url and headers",
			yaml: `
mcp_servers:
  graph:
    type: remote
    url: "https://mcp.example.com/mcp"
    headers:
      Authorization: "Bearer tok"
    timeout_ms: 2500
`,
		},
		{
			name:    "local without command",
			yaml:    "mcp_servers:\n  fs:\n    type: local\n",
			wantErr: `mcp_servers.fs: local server needs a command`,
		},
		{
			name:    "remote without url",
			yaml:    "mcp_servers:\n  graph:\n    type: remote\n",
			wantErr: `mcp_servers.graph: remote server needs a url`,
		},
		{
			name:    "unknown type",
			yaml:    "mcp_servers:\n  p:\n    type: carrier-pigeon\n",
			wantErr: `unknown type "carrier-pigeon"`,
		},
		{
			name:    "missing type",
			yaml:    "mcp_servers:\n  p:\n    command: [\"ls\"]\n",
			wantErr: `unknown type ""`,
		},
		{
			name:    "negative timeout",
			yaml:    "mcp_servers:\n  p:\n    type: local\n    command: [\"ls\"]\n    timeout_ms: -1\n",
			wantErr: "timeout_ms must not be negative",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected validation error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.MCP) != 1 {
				t.Fatalf("cfg.MCP = %#v, want 1 server", cfg.MCP)
			}
		})
	}
}

func TestTropicalLimitResolution(t *testing.T) {
	def := SubagentConfig{}
	if got := def.TropicalConcurrencyLimit(); got != DefaultTropicalMaxConcurrent {
		t.Errorf("default concurrency: got %d, want %d", got, DefaultTropicalMaxConcurrent)
	}
	if got := def.TropicalTotalLimit(); got != DefaultTropicalMaxTotalPerTurn {
		t.Errorf("default total: got %d, want %d", got, DefaultTropicalMaxTotalPerTurn)
	}
	custom := SubagentConfig{TropicalMaxConcurrent: 4, TropicalMaxTotalPerTurn: 10}
	if got := custom.TropicalConcurrencyLimit(); got != 4 {
		t.Errorf("custom concurrency: got %d, want 4", got)
	}
	if got := custom.TropicalTotalLimit(); got != 10 {
		t.Errorf("custom total: got %d, want 10", got)
	}
	unlim := SubagentConfig{TropicalMaxConcurrent: -1, TropicalMaxTotalPerTurn: -1}
	if got := unlim.TropicalConcurrencyLimit(); got != -1 {
		t.Errorf("unlimited concurrency: got %d, want -1", got)
	}
	if got := unlim.TropicalTotalLimit(); got != -1 {
		t.Errorf("unlimited total: got %d, want -1", got)
	}
}
