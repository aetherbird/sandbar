package cliadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/aetherbird/sandbar/internal/catalog"
	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/llm"
	uxtheme "github.com/aetherbird/sandbar/internal/ui/theme"
)

// CheckStatus is the outcome of one doctor probe.
type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// DoctorCheck contains a secret-safe diagnostic result. Details must never
// contain raw credentials; cliadmin's built-in checks use only presence flags
// and the redactedValue marker.
type DoctorCheck struct {
	Name    string         `json:"name"`
	Status  CheckStatus    `json:"status"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details,omitempty"`
}

// DoctorReport is a complete doctor run in a stable JSON-friendly shape.
// Version carries the CLI build version passed in by the caller (empty when
// unknown).
type DoctorReport struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Version     string        `json:"version,omitempty"`
	Healthy     bool          `json:"healthy"`
	Checks      []DoctorCheck `json:"checks"`
}

// DoctorOptions configures a doctor run. The function fields are optional
// dependency seams for deterministic tests; normal callers only need the first
// five fields.
type DoctorOptions struct {
	ConfigPath string
	Model      string
	Workspace  string
	Theme      string
	ColorMode  string
	// Version is the CLI build version echoed into the report ("" omits it).
	Version string

	LookupPath func(string) (string, error)
	Stat       func(string) (os.FileInfo, error)
	Getenv     func(string) string
	IsTerminal func(uintptr) bool
	Output     *os.File
	Now        func() time.Time
}

// RunDoctor performs read-only environment checks. The one exception is the
// existing config.LoadClientConfig behavior: if the client config is absent it
// creates the documented default file.
func RunDoctor(ctx context.Context, options DoctorOptions) DoctorReport {
	deps := normalizeDoctorOptions(options)
	report := DoctorReport{GeneratedAt: deps.Now(), Version: strings.TrimSpace(options.Version), Healthy: true}
	add := func(check DoctorCheck) {
		report.Checks = append(report.Checks, check)
		if check.Status == CheckFail {
			report.Healthy = false
		}
	}

	var core *config.Config
	var zeroConfig bool
	corePath, err := Path(CoreConfig, options.ConfigPath)
	if err != nil {
		// The REPL boots zero-config from $OPENAI_API_KEY when no config file
		// exists (cmd/sandbar/main.go loadCoreConfig); doctor mirrors that so
		// a zero-config machine reports as healthy as it boots.
		if zero, ok := config.DefaultFromEnv(); ok {
			core = zero
			zeroConfig = true
			add(zeroConfigCheck(deps.Stat))
		} else {
			add(failCheck("config", "could not resolve core config", map[string]any{
				"error": err.Error(),
				"hint":  "set $OPENAI_API_KEY for a zero-config boot with OpenAI defaults",
			}))
		}
	} else {
		loaded, loadErr := config.Load(corePath)
		if loadErr != nil {
			add(failCheck("config", "core config did not parse or validate", map[string]any{
				"path": corePath, "error": loadErr.Error(),
			}))
		} else {
			core = loaded
			add(passCheck("config", "core config parsed and validated", map[string]any{"path": corePath}))
		}
	}

	clientPath, pathErr := Path(ClientConfig, "")
	var client *config.ClientConfig
	if pathErr != nil {
		add(warnCheck("client_config", "could not resolve client config", map[string]any{"error": pathErr.Error()}))
	} else {
		loaded, loadErr := loadClientStrict(clientPath)
		if loadErr != nil {
			add(warnCheck("client_config", "client config did not parse", map[string]any{
				"path": clientPath, "error": loadErr.Error(),
			}))
		} else if validateErr := validateClient(loaded); validateErr != nil {
			client = loaded
			add(warnCheck("client_config", "client config has an invalid preference", map[string]any{
				"path": clientPath, "error": validateErr.Error(),
			}))
		} else {
			client = loaded
			add(passCheck("client_config", "client config parsed and validated", map[string]any{"path": clientPath}))
		}
	}

	if core != nil {
		workspace := core.Workspace
		if strings.TrimSpace(options.Workspace) != "" {
			workspace = options.Workspace
		}
		add(checkWorkspace(workspace, zeroConfig, deps.Stat))
		add(checkDatabase(core.Database, deps.Stat))
		modelCheck, authCheck := checkModel(core, chooseModel(options.Model, client))
		add(modelCheck)
		add(authCheck)
	}

	for _, binary := range []string{"bash", "git"} {
		add(checkBinary(binary, deps.LookupPath))
	}
	// rg is optional: search_files falls back to a pure-Go walker without it.
	add(checkOptionalBinary("rg", deps.LookupPath))
	add(checkTerminal(deps))
	add(checkTheme(options, client, deps.Getenv))
	if core != nil {
		add(checkModelsJSON(core, corePath, deps.Stat))
		add(checkMCPServers(core))
		add(checkSkillsPrompts(core, workspaceOption(options), deps.Stat, deps.Getenv))
		add(checkCatalog())
	}

	return report
}

// workspaceOption returns the explicitly requested workspace, if any.
func workspaceOption(options DoctorOptions) string {
	return strings.TrimSpace(options.Workspace)
}

// Human renders a compact, no-color doctor report suitable for terminals,
// logs, and NO_COLOR output.
func (r DoctorReport) Human() string {
	var out strings.Builder
	overall := "healthy"
	if !r.Healthy {
		overall = "issues found"
	}
	fmt.Fprintf(&out, "Sandbar doctor: %s\n", overall)
	if r.Version != "" {
		fmt.Fprintf(&out, "Version: %s\n", r.Version)
	}
	for _, check := range r.Checks {
		fmt.Fprintf(&out, "%-4s %-18s %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Summary)
		keys := make([]string, 0, len(check.Details))
		for key := range check.Details {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&out, "     %-18s %v\n", key+":", check.Details[key])
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// JSON renders an indented machine-readable doctor report.
func (r DoctorReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func normalizeDoctorOptions(options DoctorOptions) DoctorOptions {
	if options.LookupPath == nil {
		options.LookupPath = exec.LookPath
	}
	if options.Stat == nil {
		options.Stat = os.Stat
	}
	if options.Getenv == nil {
		options.Getenv = os.Getenv
	}
	if options.IsTerminal == nil {
		options.IsTerminal = func(fd uintptr) bool { return term.IsTerminal(int(fd)) }
	}
	if options.Output == nil {
		options.Output = os.Stdout
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return options
}

func chooseModel(explicit string, client *config.ClientConfig) string {
	if value := strings.TrimSpace(explicit); value != "" {
		return value
	}
	if client != nil {
		return strings.TrimSpace(client.DefaultModel)
	}
	return ""
}

func checkWorkspace(configured string, zeroConfig bool, stat func(string) (os.FileInfo, error)) DoctorCheck {
	path, err := filepath.Abs(configured)
	if err != nil {
		return failCheck("workspace", "workspace path could not be resolved", map[string]any{"error": err.Error()})
	}
	info, err := stat(path)
	if err != nil {
		// The zero-config default ./workspace never exists on a fresh
		// machine — the REPL boots without it and the harness creates it on
		// the first file write. That is expected, not a failure.
		if zeroConfig && configured == "./workspace" {
			return warnCheck("workspace", "default workspace ./workspace does not exist yet — it is created on the first file write", map[string]any{"path": path})
		}
		return failCheck("workspace", "workspace is unavailable", map[string]any{"path": path, "error": err.Error()})
	}
	if !info.IsDir() {
		return failCheck("workspace", "workspace is not a directory", map[string]any{"path": path})
	}
	if info.Mode().Perm()&0o222 == 0 {
		return warnCheck("workspace", "workspace has no write permission bits", map[string]any{"path": path})
	}
	return passCheck("workspace", "workspace directory is available", map[string]any{"path": path})
}

func checkDatabase(configured string, stat func(string) (os.FileInfo, error)) DoctorCheck {
	path := config.DBPath(configured)
	info, err := stat(path)
	if err == nil {
		if info.IsDir() {
			return failCheck("database", "database path is a directory", map[string]any{"path": path})
		}
		if info.Mode().Perm()&0o222 == 0 {
			return warnCheck("database", "database file has no write permission bits", map[string]any{"path": path})
		}
		return passCheck("database", "database file is available", map[string]any{"path": path, "exists": true})
	}
	if !os.IsNotExist(err) {
		return failCheck("database", "database path is unavailable", map[string]any{"path": path, "error": err.Error()})
	}
	parent := filepath.Dir(path)
	parentInfo, parentErr := stat(parent)
	if parentErr != nil {
		return failCheck("database", "database parent directory is unavailable", map[string]any{
			"path": path, "parent": parent, "error": parentErr.Error(),
		})
	}
	if !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o222 == 0 {
		return failCheck("database", "database cannot be created in its parent directory", map[string]any{
			"path": path, "parent": parent,
		})
	}
	return passCheck("database", "database will be created on first use", map[string]any{"path": path, "exists": false})
}

func checkModel(cfg *config.Config, requested string) (DoctorCheck, DoctorCheck) {
	registry := llm.NewRegistry(cfg)
	models := registry.ListModels()
	if len(models) == 0 {
		failed := failCheck("model", "no models are configured", nil)
		return failed, warnCheck("model_auth", "model authentication could not be checked", nil)
	}
	if requested == "" {
		requested = models[0]
	}
	resolved, err := registry.ResolveModel(requested)
	if err != nil {
		return failCheck("model", "model alias could not be resolved", map[string]any{
			"model": requested, "error": err.Error(), "configured_models": len(models),
		}), warnCheck("model_auth", "model authentication could not be checked", nil)
	}
	model := passCheck("model", "model alias resolves", map[string]any{
		"model": requested, "provider": resolved.ProviderName, "model_id": resolved.ModelID,
		"base_url": safeURL(resolved.BaseURL), "context_length": resolved.ContextLength,
		"supports_tools": resolved.SupportsTools, "configured_models": len(models),
	})
	authDetails := map[string]any{
		"provider": resolved.ProviderName, "configured": resolved.APIKey != "",
		"api_key": secretPresence(resolved.APIKey),
	}
	if resolved.APIKey == "" {
		return model, warnCheck("model_auth", "provider API key is not configured (valid for some local endpoints)", authDetails)
	}
	return model, passCheck("model_auth", "provider API key is configured", authDetails)
}

func checkBinary(name string, lookup func(string) (string, error)) DoctorCheck {
	path, err := lookup(name)
	if err != nil {
		return failCheck("binary_"+name, name+" was not found on PATH", map[string]any{"error": err.Error()})
	}
	return passCheck("binary_"+name, name+" is available", map[string]any{"path": path})
}

// checkOptionalBinary reports a missing soft dependency as a warning, not a
// failure: the harness has an in-process fallback for it.
func checkOptionalBinary(name string, lookup func(string) (string, error)) DoctorCheck {
	path, err := lookup(name)
	if err != nil {
		return warnCheck("binary_"+name, name+" is optional — pure-Go fallback used when absent", map[string]any{"error": err.Error()})
	}
	return passCheck("binary_"+name, name+" is available", map[string]any{"path": path})
}

func checkTerminal(options DoctorOptions) DoctorCheck {
	termName := options.Getenv("TERM")
	details := map[string]any{
		"tty": options.IsTerminal(options.Output.Fd()), "term": termName,
		"colorterm": options.Getenv("COLORTERM"), "no_color": options.Getenv("NO_COLOR") != "",
	}
	if !details["tty"].(bool) {
		return warnCheck("terminal", "output is not attached to a terminal", details)
	}
	if strings.EqualFold(termName, "dumb") {
		return warnCheck("terminal", "TERM=dumb; interactive styling will be limited", details)
	}
	return passCheck("terminal", "interactive terminal is available", details)
}

func checkTheme(options DoctorOptions, client *config.ClientConfig, getenv func(string) string) DoctorCheck {
	requested := strings.TrimSpace(options.Theme)
	if requested == "" {
		requested = strings.TrimSpace(getenv("SANDBAR_THEME"))
	}
	if requested == "" && client != nil {
		requested = client.Theme
	}
	if requested == "" {
		requested = uxtheme.System
	}
	resolvedID, err := resolveTheme(requested)
	if err != nil {
		return failCheck("theme", "configured theme is invalid", map[string]any{"theme": requested, "error": err.Error()})
	}

	colorMode := strings.ToLower(strings.TrimSpace(options.ColorMode))
	if colorMode == "" && client != nil {
		colorMode = client.ColorMode
	}
	if colorMode == "" {
		colorMode = config.ColorModeAuto
	}
	if colorMode != config.ColorModeAuto && colorMode != config.ColorModeAlways && colorMode != config.ColorModeNever {
		return failCheck("theme", "configured color mode is invalid", map[string]any{
			"theme": resolvedID, "color_mode": colorMode,
		})
	}
	details := map[string]any{"theme": resolvedID, "color_mode": colorMode}
	if palette, ok := uxtheme.Lookup(resolvedID); ok {
		details["label"] = palette.Label
		details["scheme"] = palette.Scheme
	} else {
		details["scheme"] = "automatic"
	}
	return passCheck("theme", "terminal theme is available", details)
}

func safeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func secretPresence(value string) string {
	if value == "" {
		return "<unset>"
	}
	return redactedValue
}

func passCheck(name, summary string, details map[string]any) DoctorCheck {
	return DoctorCheck{Name: name, Status: CheckPass, Summary: summary, Details: details}
}

// checkModelsJSON reports on the optional legacy-style models.json provider
// overlay. Absent (auto-discovery found nothing) is a warning, not a failure:
// providers can live in config.yaml alone. A present file that fails to parse
// is also a warning — the core config still boots YAML providers without it.
func checkModelsJSON(cfg *config.Config, configPath string, stat func(string) (os.FileInfo, error)) DoctorCheck {
	path := config.ModelsJSONCandidate(cfg, configPath)
	if path == "" {
		return warnCheck("models_json", "no models.json overlay (providers come from config.yaml)", map[string]any{"present": false})
	}
	info, err := stat(path)
	if os.IsNotExist(err) {
		// An explicitly configured missing path is a config error surfaced by
		// config.Load; doctor only reports presence here.
		summary := "models.json overlay not present"
		if cfg.ModelsJSON != "" {
			summary = "configured models.json is missing (config load will fail)"
		}
		return warnCheck("models_json", summary, map[string]any{"path": path, "present": false})
	}
	if err != nil {
		return warnCheck("models_json", "models.json path is unavailable", map[string]any{"path": path, "error": err.Error()})
	}
	if info.IsDir() {
		return warnCheck("models_json", "models.json path is a directory", map[string]any{"path": path})
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return warnCheck("models_json", "models.json could not be read", map[string]any{"path": path, "error": readErr.Error()})
	}
	if parseErr := config.ParseModelsJSON(data); parseErr != nil {
		return warnCheck("models_json", "models.json did not parse (config load will fail)", map[string]any{
			"path": path, "present": true, "error": parseErr.Error(),
		})
	}
	return passCheck("models_json", "models.json overlay parses", map[string]any{"path": path, "present": true})
}

// checkMCPServers reports how many MCP servers the config declares and
// whether their settings validate. Doctor deliberately does not dial them —
// connect status is a runtime concern, too heavy for a boot check.
func checkMCPServers(cfg *config.Config) DoctorCheck {
	if len(cfg.MCP) == 0 {
		return warnCheck("mcp_servers", "no MCP servers configured", map[string]any{"servers": 0})
	}
	names := make([]string, 0, len(cfg.MCP))
	for name := range cfg.MCP {
		names = append(names, name)
	}
	sort.Strings(names)
	details := map[string]any{"servers": len(cfg.MCP), "names": names}
	if validateErr := cfg.Validate(); validateErr != nil {
		return warnCheck("mcp_servers", "MCP server settings did not validate", map[string]any{
			"servers": len(cfg.MCP), "error": validateErr.Error(),
		})
	}
	return passCheck("mcp_servers", "MCP server settings are valid (connect status is checked at attach, not here)", details)
}

// checkSkillsPrompts reports whether the skills and prompt-file directories
// doctor knows about exist. Missing directories are informational warns:
// skills and prompt files are entirely optional.
func checkSkillsPrompts(cfg *config.Config, workspaceOpt string, stat func(string) (os.FileInfo, error), getenv func(string) string) DoctorCheck {
	workspace := cfg.Workspace
	if workspaceOpt != "" {
		workspace = workspaceOpt
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		home = ""
	}
	userConfig := getenv("XDG_CONFIG_HOME")
	if userConfig == "" && home != "" {
		userConfig = filepath.Join(home, ".config")
	}

	skillDirs := []string{
		filepath.Join(workspace, ".sandbar", "skills"),
		filepath.Join(userConfig, "sandbar", "skills"),
	}
	promptDirs := []string{
		filepath.Join(workspace, ".sandbar"),
		filepath.Join(userConfig, "sandbar"),
	}
	var foundSkills, foundPrompts []string
	for _, dir := range skillDirs {
		if _, err := stat(dir); err == nil {
			foundSkills = append(foundSkills, dir)
		}
	}
	for _, dir := range promptDirs {
		if _, err := stat(dir); err == nil {
			foundPrompts = append(foundPrompts, dir)
		}
	}
	details := map[string]any{
		"skills_found":  len(foundSkills),
		"prompts_found": len(foundPrompts),
	}
	if len(foundSkills) > 0 {
		details["skills_dirs"] = foundSkills
	}
	if len(foundPrompts) > 0 {
		details["prompts_dirs"] = foundPrompts
	}
	if len(foundSkills) == 0 && len(foundPrompts) == 0 {
		return warnCheck("skills_prompts", "no skills or prompt-file directories found (optional)", details)
	}
	return passCheck("skills_prompts", "skills/prompt-file directories found", details)
}

// checkCatalog reports the embedded pricing catalog's size so cost rollups
// can be confirmed active at a glance.
func checkCatalog() DoctorCheck {
	c := catalog.Embedded()
	size := c.Size()
	details := map[string]any{"models": size, "providers": len(c.Providers), "source": c.Source}
	if size == 0 {
		return warnCheck("catalog", "embedded pricing catalog is empty (cost rollups inactive)", details)
	}
	return passCheck("catalog", "embedded pricing catalog loaded (cost rollups active)", details)
}

// zeroConfigCheck reports the zero-config fallback that keeps a machine with
// only $OPENAI_API_KEY set booting. It mirrors the CLI boot path: write the
// commented template to the default config location, then report whether the
// write landed (WriteDefaultConfigTemplate fails silently when no
// config.yaml.example is findable, e.g. for a bare go-installed binary).
func zeroConfigCheck(stat func(string) (os.FileInfo, error)) DoctorCheck {
	templatePath := zeroConfigTemplatePath()
	config.WriteDefaultConfigTemplate()
	summary := "no config file found — using zero-config defaults from $OPENAI_API_KEY"
	if templatePath == "" {
		summary += " (template path unavailable — template not written)"
	} else if _, err := stat(templatePath); err == nil {
		summary += " (template written to " + templatePath + ")"
	} else {
		summary += " (no config.yaml.example found — template not written)"
	}
	return passCheck("zero_config", summary, map[string]any{"template_path": templatePath})
}

// zeroConfigTemplatePath mirrors config.WriteDefaultConfigTemplate's
// destination so the doctor line can name the exact file it wrote.
func zeroConfigTemplatePath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "sandbar", "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "sandbar", "config.yaml")
	}
	return ""
}

func warnCheck(name, summary string, details map[string]any) DoctorCheck {
	return DoctorCheck{Name: name, Status: CheckWarn, Summary: summary, Details: details}
}

func failCheck(name, summary string, details map[string]any) DoctorCheck {
	return DoctorCheck{Name: name, Status: CheckFail, Summary: summary, Details: details}
}
