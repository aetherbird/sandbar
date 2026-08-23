package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level runtime configuration.
type Config struct {
	Workspace   string            `yaml:"workspace"`
	Database    string            `yaml:"database"`
	Persona     PersonaConfig     `yaml:"persona"`
	Providers   []ProviderConfig  `yaml:"providers"`
	Tools       ToolsConfig       `yaml:"tools"`
	Compression CompressionConfig `yaml:"compression"`
	Subagent    SubagentConfig    `yaml:"subagent"`
	MaxTurns    int               `yaml:"max_turns"` // 0 = unlimited; positive values cap main-agent turns
	// ModelsJSON overrides where a legacy-style models.json provider
	// registry is loaded from (see modelsjson.go). Empty = auto-discovery.
	ModelsJSON string `yaml:"models_json,omitempty"`
	// MCP names the Model Context Protocol servers to attach at runtime,
	// keyed by server name. Empty = no MCP servers (zero overhead).
	MCP map[string]MCPServerConfig `yaml:"mcp_servers,omitempty"`
}

// MCPServerConfig describes one MCP server. Type "local" spawns Command as a
// stdio child process; type "remote" dials URL over streamable HTTP, sending
// Headers on every request. TimeoutMS bounds connect + initialize per server
// (0 = no per-server bound; the runtime still applies an overall boot budget).
type MCPServerConfig struct {
	Type      string            `yaml:"type"`
	Command   []string          `yaml:"command"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	TimeoutMS int               `yaml:"timeout_ms"`
}

// PersonaConfig holds the agent personality.
type PersonaConfig struct {
	Name         string `yaml:"name"`
	SystemPrompt string `yaml:"system_prompt"`
	// SystemPromptFile, when set, names a Markdown file whose contents replace
	// the inline SystemPrompt. Relative paths resolve against the config
	// file's directory, so a config and its prompt travel together. A missing
	// or unreadable file is a configuration error — failing loudly beats
	// silently running a stale or default persona.
	SystemPromptFile string `yaml:"system_prompt_file"`
	TitleModel       string `yaml:"title_model"`
}

// ProviderConfig defines an OpenAI-compatible endpoint.
type ProviderConfig struct {
	Name          string                 `yaml:"name"`
	BaseURL       string                 `yaml:"base_url"`
	APIKey        string                 `yaml:"api_key"`
	Models        map[string]ModelConfig `yaml:"models"`
	ModelDefaults ModelConfig            `yaml:"model_defaults"`
	// API selects the wire protocol: "" / "openai-completions" means
	// OpenAI-compatible chat completions; "anthropic-messages" uses the native
	// Messages client (internal/llm/anthropic.go).
	API string `yaml:"api,omitempty" json:"api,omitempty"`
	// Compat carries optional per-provider quirks (models.json heritage).
	// Nil means defaults; see CompatFlags.
	Compat *CompatFlags `yaml:"compat,omitempty" json:"compat,omitempty"`
	// ReasoningStyle translates the per-turn reasoning effort into the
	// provider's dialect: "" / "openai" sends reasoning_effort (OpenAI,
	// OpenRouter, GLM); "enable_thinking" sends chat_template_kwargs
	// enable_thinking (llama.cpp-served Qwen3.x / Gemma4 — verified against
	// the local llama-swap endpoint: low disables thinking, medium/high
	// enable it); "none" ignores effort for this provider.
	ReasoningStyle string `yaml:"reasoning_style,omitempty"`
}

// ModelConfig holds per-model overrides.
type ModelConfig struct {
	ContextLength *int    `yaml:"context_length,omitempty"`
	SupportsTools *bool   `yaml:"supports_tools,omitempty"`
	ModelID       *string `yaml:"model_id,omitempty"`
	// MaxTokens caps output tokens for this model; 0/nil = provider default.
	MaxTokens *int `yaml:"max_tokens,omitempty"`
	// Name is a display name for pickers and listings.
	Name string `yaml:"name,omitempty"`
}

// ToolsConfig holds tool-specific settings.
type ToolsConfig struct {
	Shell     ShellConfig        `yaml:"shell"`
	SSH       SSHConfig          `yaml:"ssh"`
	WebSearch WebSearchConfig    `yaml:"web_search"`
	Approval  ToolApprovalConfig `yaml:"approval"`
}

// ToolApprovalConfig controls centralized tool authorization. Mode defaults to
// yolo for compatibility with deployments created before approvals. Rules are
// exact tool-name overrides; Tiers are read/write/exec overrides. Both accept
// allow, deny, or prompt.
type ToolApprovalConfig struct {
	Mode  string            `yaml:"mode"`
	Rules map[string]string `yaml:"rules,omitempty"`
	Tiers map[string]string `yaml:"tiers,omitempty"`
}

// ShellConfig holds shell_exec settings.
type ShellConfig struct {
	Timeout         string          `yaml:"timeout"`
	BlockedCommands []string        `yaml:"blocked_commands"`
	Jobs            ShellJobsConfig `yaml:"jobs"`
}

// SSHConfig holds remote shell_exec settings. When the model sets the `host`
// argument, the harness owns the ssh transport and POSIX quoting; the model
// passes a plain command and never composes nested ssh commands.
type SSHConfig struct {
	ConnectTimeout string   `yaml:"connect_timeout"` // ssh -o ConnectTimeout; default 5s
	BatchMode      *bool    `yaml:"batch_mode"`      // ssh -o BatchMode; nil = true
	AllowedHosts   []string `yaml:"allowed_hosts"`   // empty = any host
}

// SSHRuntimeConfig is the parsed form consumed by runtime wiring.
type SSHRuntimeConfig struct {
	ConnectTimeout time.Duration
	BatchMode      bool
	AllowedHosts   []string
}

// ShellJobsConfig bounds explicitly supervised background processes.
type ShellJobsConfig struct {
	MaxJobs          int    `yaml:"max_jobs"`
	MaxRunning       int    `yaml:"max_running"`
	OutputBytes      int    `yaml:"output_bytes"`
	Retention        string `yaml:"retention"`
	TerminationGrace string `yaml:"termination_grace"`
}

// ShellJobRuntimeConfig is the parsed form consumed by runtime wiring.
type ShellJobRuntimeConfig struct {
	MaxJobs          int
	MaxRunning       int
	OutputBytes      int
	Retention        time.Duration
	TerminationGrace time.Duration
}

// WebSearchConfig holds search settings.
type WebSearchConfig struct {
	Engine      string `yaml:"engine"`
	BraveAPIKey string `yaml:"brave_api_key"`
}

// CompressionConfig holds context compression settings.
type CompressionConfig struct {
	Enabled              bool    `yaml:"enabled"`
	Threshold            float64 `yaml:"threshold"`
	TargetRatio          float64 `yaml:"target_ratio"`           // auxiliary summary max-output budget as a ratio of pre-compression tokens
	PostCompressionRatio float64 `yaml:"post_compression_ratio"` // desired provider-message context after mid-loop compression
	RecentTailTokens     int     `yaml:"recent_tail_tokens"`     // 0 = derive from context (about 8-12K for a 65K+ model)
	Model                string  `yaml:"model"`                  // empty = use current model
	MinSummaryTokens     int     `yaml:"min_summary_tokens"`     // floor for computed output budget
	MaxSummaryTokens     int     `yaml:"max_summary_tokens"`     // ceiling for computed output budget; 0 = use provider limit
	TimeoutSeconds       int     `yaml:"timeout_seconds"`        // summarizer timeout; 0 = default 120s
}

// SubagentConfig holds sub-agent delegation settings.
type SubagentConfig struct {
	Model    string   `yaml:"model"`     // model alias for sub-agents (default: deepseek/deepseek-v4-flash)
	MaxTurns int      `yaml:"max_turns"` // 0 = unlimited; positive values cap sub-agent turns
	Tools    []string `yaml:"tools"`     // tools sub-agents can use (default: all read-only tools + shell)
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return finalizeConfig(data, filepath.Dir(path))
}

// finalizeConfig parses interpolated YAML bytes, applies defaults, and
// validates. configDir is the base for resolving a persona
// system_prompt_file; the zero-config synthesized config passes "" because it
// never references one.
func finalizeConfig(data []byte, configDir string) (*Config, error) {
	data = interpolateEnv(data)

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Workspace == "" {
		cfg.Workspace = "./workspace"
	}
	if cfg.Database == "" {
		cfg.Database = "./sandbar.db"
	}
	if cfg.Persona.Name == "" {
		cfg.Persona.Name = "Sandbar"
	}
	if cfg.Persona.SystemPromptFile != "" {
		promptPath := cfg.Persona.SystemPromptFile
		if !filepath.IsAbs(promptPath) {
			promptPath = filepath.Join(configDir, promptPath)
		}
		promptData, err := os.ReadFile(promptPath)
		if err != nil {
			return nil, fmt.Errorf("read persona system_prompt_file: %w", err)
		}
		content := strings.TrimSpace(string(promptData))
		if content == "" {
			return nil, fmt.Errorf("persona system_prompt_file %q is empty", cfg.Persona.SystemPromptFile)
		}
		cfg.Persona.SystemPrompt = content
	}
	if cfg.Persona.SystemPrompt == "" {
		cfg.Persona.SystemPrompt = "You are Sandbar, a precise agentic assistant. You help the user get real work done: inspect, build, repair, explain, document, and carry the task forward. Use the available tools proactively, batch independent calls, prefer dedicated tools over shell equivalents, verify non-trivial work before claiming completion, and keep working until the task is complete. When using tools, output ONLY structured tool_calls."
	}
	if cfg.Compression.Threshold == 0 {
		cfg.Compression.Threshold = 0.80
	}
	if cfg.Compression.TargetRatio == 0 {
		cfg.Compression.TargetRatio = 0.20
	}
	if cfg.Compression.PostCompressionRatio == 0 {
		cfg.Compression.PostCompressionRatio = 0.50
	}
	if cfg.Compression.MinSummaryTokens == 0 {
		cfg.Compression.MinSummaryTokens = 1000
	}
	if cfg.Compression.MaxSummaryTokens == 0 {
		cfg.Compression.MaxSummaryTokens = 12000
	}
	if cfg.Compression.TimeoutSeconds == 0 {
		cfg.Compression.TimeoutSeconds = 120
	}
	if cfg.Subagent.Model == "" {
		cfg.Subagent.Model = "deepseek/deepseek-v4-flash"
	}
	if len(cfg.Subagent.Tools) == 0 {
		cfg.Subagent.Tools = []string{"file_read", "search_files", "web_search", "web_fetch", "shell_exec"}
	}
	if cfg.Tools.Approval.Mode == "" {
		cfg.Tools.Approval.Mode = "yolo"
	}
	if cfg.Tools.Shell.Jobs.MaxJobs == 0 {
		cfg.Tools.Shell.Jobs.MaxJobs = 128
	}
	if cfg.Tools.Shell.Jobs.MaxRunning == 0 {
		cfg.Tools.Shell.Jobs.MaxRunning = 16
	}
	if cfg.Tools.Shell.Jobs.OutputBytes == 0 {
		cfg.Tools.Shell.Jobs.OutputBytes = 64 * 1024
	}
	if cfg.Tools.Shell.Jobs.Retention == "" {
		cfg.Tools.Shell.Jobs.Retention = "30m"
	}
	if cfg.Tools.Shell.Jobs.TerminationGrace == "" {
		cfg.Tools.Shell.Jobs.TerminationGrace = "750ms"
	}

	// Overlay any models.json provider registry before validation so name
	// clashes are already replaced (validation rejects duplicates).
	if err := mergeModelsJSON(&cfg, configDir); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

// Validate checks configuration rules.
func (c *Config) Validate() error {
	for _, p := range c.Providers {
		switch p.ReasoningStyle {
		case "", "openai", "enable_thinking", "none":
		default:
			return fmt.Errorf("provider %s: reasoning_style %q must be openai, enable_thinking, or none", p.Name, p.ReasoningStyle)
		}
		switch p.API {
		case "", "openai-completions", "anthropic-messages":
		default:
			return fmt.Errorf("provider %s: api %q must be anthropic-messages or openai-completions (empty = openai-compatible)", p.Name, p.API)
		}
	}
	if c.MaxTurns < 0 {
		return fmt.Errorf("max_turns must be zero (unlimited) or positive, got %d", c.MaxTurns)
	}
	if c.Subagent.MaxTurns < 0 {
		return fmt.Errorf("subagent.max_turns must be zero (unlimited) or positive, got %d", c.Subagent.MaxTurns)
	}
	if err := c.Tools.Approval.Validate(); err != nil {
		return err
	}
	if timeout, err := c.ShellTimeout(); err != nil {
		return fmt.Errorf("tools.shell.timeout must be a duration: %w", err)
	} else if timeout <= 0 {
		return fmt.Errorf("tools.shell.timeout must be positive, got %s", timeout)
	}
	if _, err := c.ShellJobSettings(); err != nil {
		return err
	}
	// sudo is never blockable (deployment hosts grant the agent passwordless
	// sudo; see neverBlocked in internal/tools/shell.go). Reject such entries
	// at validation instead of silently dropping them at runtime.
	for _, entry := range c.Tools.Shell.BlockedCommands {
		if fields := strings.Fields(entry); len(fields) > 0 && fields[0] == "sudo" {
			return fmt.Errorf("tools.shell.blocked_commands entry %q is ignored: sudo can never be blocklisted (deployment policy requires passwordless sudo to stay runnable)", entry)
		}
	}

	// MCP servers: type selects the transport and dictates the required
	// fields. Sorted names keep the first reported error deterministic.
	for _, name := range slices.Sorted(maps.Keys(c.MCP)) {
		srv := c.MCP[name]
		switch srv.Type {
		case "local":
			if len(srv.Command) == 0 {
				return fmt.Errorf("mcp_servers.%s: local server needs a command", name)
			}
		case "remote":
			if srv.URL == "" {
				return fmt.Errorf("mcp_servers.%s: remote server needs a url", name)
			}
		default:
			return fmt.Errorf("mcp_servers.%s: unknown type %q (want \"local\" or \"remote\")", name, srv.Type)
		}
		if srv.TimeoutMS < 0 {
			return fmt.Errorf("mcp_servers.%s: timeout_ms must not be negative, got %d", name, srv.TimeoutMS)
		}
	}

	providerNames := make(map[string]bool)
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider name is required")
		}
		if providerNames[p.Name] {
			return fmt.Errorf("duplicate provider name: %s", p.Name)
		}
		providerNames[p.Name] = true
	}

	// Positive context lengths. Collect both bare and provider-qualified
	// alias sets so config fields (e.g. compression.model) can reference
	// either form. Duplicate bare aliases across providers are allowed.
	modelAliases := make(map[string]bool)
	qualifiedAliases := make(map[string]bool)
	for _, p := range c.Providers {
		for alias, m := range p.Models {
			modelAliases[alias] = true
			qualifiedAliases[p.Name+"/"+alias] = true
			if m.ContextLength != nil && *m.ContextLength <= 0 {
				return fmt.Errorf("model %s context_length must be positive", alias)
			}
		}
	}

	if c.Compression.Enabled {
		if c.Compression.Threshold <= 0 || c.Compression.Threshold >= 1 {
			return fmt.Errorf("compression threshold must be between 0 and 1 (exclusive), got %v", c.Compression.Threshold)
		}
		if c.Compression.TargetRatio <= 0 || c.Compression.TargetRatio > 1 {
			return fmt.Errorf("compression target_ratio must be between 0 and 1 (exclusive), got %v", c.Compression.TargetRatio)
		}
		if c.Compression.PostCompressionRatio <= 0 || c.Compression.PostCompressionRatio >= c.Compression.Threshold {
			return fmt.Errorf("compression post_compression_ratio must be greater than 0 and less than threshold (%v), got %v", c.Compression.Threshold, c.Compression.PostCompressionRatio)
		}
		if c.Compression.MinSummaryTokens > c.Compression.MaxSummaryTokens {
			return fmt.Errorf("compression min_summary_tokens (%d) must not exceed max_summary_tokens (%d)", c.Compression.MinSummaryTokens, c.Compression.MaxSummaryTokens)
		}
		if c.Compression.RecentTailTokens < 0 {
			return fmt.Errorf("compression recent_tail_tokens must not be negative, got %d", c.Compression.RecentTailTokens)
		}
		if c.Compression.Model != "" && !modelAliases[c.Compression.Model] && !qualifiedAliases[c.Compression.Model] {
			return fmt.Errorf("compression model %q is not a known model alias (bare or provider-qualified)", c.Compression.Model)
		}
	}

	return nil
}

// Validate rejects approval configuration typos rather than silently falling
// back to a less restrictive policy.
func (c ToolApprovalConfig) Validate() error {
	mode := c.Mode
	if mode == "" {
		mode = "yolo"
	}
	switch mode {
	case "yolo", "write", "always-ask":
	default:
		return fmt.Errorf("tools.approval.mode must be yolo, write, or always-ask, got %q", mode)
	}
	validPolicy := func(policy string) bool {
		return policy == "allow" || policy == "deny" || policy == "prompt"
	}
	for name, policy := range c.Rules {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("tools.approval.rules contains an empty tool name")
		}
		if !validPolicy(policy) {
			return fmt.Errorf("tools.approval.rules.%s must be allow, deny, or prompt, got %q", name, policy)
		}
	}
	for tier, policy := range c.Tiers {
		switch tier {
		case "read", "write", "exec":
		default:
			return fmt.Errorf("tools.approval.tiers contains invalid tier %q", tier)
		}
		if !validPolicy(policy) {
			return fmt.Errorf("tools.approval.tiers.%s must be allow, deny, or prompt, got %q", tier, policy)
		}
	}
	return nil
}

// ShellTimeout parses the shell timeout string.
func (c *Config) ShellTimeout() (time.Duration, error) {
	if c.Tools.Shell.Timeout == "" {
		return 30 * time.Second, nil
	}
	return time.ParseDuration(c.Tools.Shell.Timeout)
}

// ShellJobSettings validates and parses supervised background-job limits.
func (c *Config) ShellJobSettings() (ShellJobRuntimeConfig, error) {
	jobs := c.Tools.Shell.Jobs
	if jobs.MaxJobs == 0 {
		jobs.MaxJobs = 128
	}
	if jobs.MaxRunning == 0 {
		jobs.MaxRunning = 16
	}
	if jobs.OutputBytes == 0 {
		jobs.OutputBytes = 64 * 1024
	}
	if jobs.Retention == "" {
		jobs.Retention = "30m"
	}
	if jobs.TerminationGrace == "" {
		jobs.TerminationGrace = "750ms"
	}
	if jobs.MaxJobs < 0 {
		return ShellJobRuntimeConfig{}, fmt.Errorf("tools.shell.jobs.max_jobs must be positive, got %d", jobs.MaxJobs)
	}
	if jobs.MaxRunning < 0 || jobs.MaxRunning > jobs.MaxJobs {
		return ShellJobRuntimeConfig{}, fmt.Errorf("tools.shell.jobs.max_running must be positive and no greater than max_jobs, got %d", jobs.MaxRunning)
	}
	if jobs.OutputBytes < 0 {
		return ShellJobRuntimeConfig{}, fmt.Errorf("tools.shell.jobs.output_bytes must be positive, got %d", jobs.OutputBytes)
	}
	retention, err := time.ParseDuration(jobs.Retention)
	if err != nil || retention <= 0 {
		return ShellJobRuntimeConfig{}, fmt.Errorf("tools.shell.jobs.retention must be a positive duration, got %q", jobs.Retention)
	}
	grace, err := time.ParseDuration(jobs.TerminationGrace)
	if err != nil || grace <= 0 {
		return ShellJobRuntimeConfig{}, fmt.Errorf("tools.shell.jobs.termination_grace must be a positive duration, got %q", jobs.TerminationGrace)
	}
	return ShellJobRuntimeConfig{
		MaxJobs: jobs.MaxJobs, MaxRunning: jobs.MaxRunning, OutputBytes: jobs.OutputBytes,
		Retention: retention, TerminationGrace: grace,
	}, nil
}

// SSHSettings parses the remote-execution settings. Returns an error only for
// an invalid duration; allowed_hosts entries are validated at tool wiring.
func (c *Config) SSHSettings() (SSHRuntimeConfig, error) {
	sshCfg := c.Tools.SSH
	timeout := 5 * time.Second
	if sshCfg.ConnectTimeout != "" {
		parsed, err := time.ParseDuration(sshCfg.ConnectTimeout)
		if err != nil || parsed <= 0 {
			return SSHRuntimeConfig{}, fmt.Errorf("tools.ssh.connect_timeout must be a positive duration, got %q", sshCfg.ConnectTimeout)
		}
		timeout = parsed
	}
	batchMode := true
	if sshCfg.BatchMode != nil {
		batchMode = *sshCfg.BatchMode
	}
	return SSHRuntimeConfig{
		ConnectTimeout: timeout,
		BatchMode:      batchMode,
		AllowedHosts:   append([]string(nil), sshCfg.AllowedHosts...),
	}, nil
}

var envInterpPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolateEnv replaces ${VAR} patterns in raw YAML bytes with os.Getenv values.
func interpolateEnv(data []byte) []byte {
	return envInterpPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		varName := string(match[2 : len(match)-1])
		return []byte(os.Getenv(varName))
	})
}
