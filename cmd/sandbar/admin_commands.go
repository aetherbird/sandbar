package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"sandbar/internal/cliadmin"
	"sandbar/internal/config"
)

// runAdminCommand handles Sandbar's non-interactive administration command
// tree before the chat-oriented flag parser sees the arguments. Keeping this
// seam independent of os.Exit makes the commands straightforward to test and
// lets completions use the same descriptor that documents the public surface.
func runAdminCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (handled bool, exitCode int) {
	if len(args) == 0 {
		return false, 0
	}

	switch args[0] {
	case "config":
		return true, runConfigCommand(args[1:], stdout, stderr)
	case "doctor":
		return true, runDoctorCommand(ctx, args[1:], stdout, stderr)
	case "version":
		return true, runVersionCommand(args[1:], stdout, stderr)
	case "completion", "completions":
		return true, runCompletionCommand(args[1:], stdout, stderr)
	default:
		return false, 0
	}
}

func runConfigCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeConfigUsage(stderr)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		writeConfigUsage(stderr)
		return 0
	}

	operation := args[0]
	fs := flag.NewFlagSet("sandbar config "+operation, flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultScope := "client"
	if operation == "validate" {
		defaultScope = "all"
	}
	scopeName := fs.String("scope", defaultScope, "Config scope: client, core, or all (validate only)")
	configPath := fs.String("config", "", "Explicit core config path")
	jsonOutput := fs.Bool("json", false, "Emit JSON")
	fs.Usage = func() { writeConfigOperationUsage(stderr, operation) }
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	allowAll := operation == "validate"
	scope, all, err := parseConfigScope(*scopeName, allowAll)
	if err != nil {
		return writeAdminError(stderr, err, 2)
	}
	if all {
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		return runConfigValidateAll(*configPath, *jsonOutput, stdout, stderr)
	}
	if scope == cliadmin.ClientConfig && strings.TrimSpace(*configPath) != "" {
		return writeAdminError(stderr, errors.New("--config applies only to --scope core"), 2)
	}
	explicit := ""
	if scope == cliadmin.CoreConfig {
		explicit = *configPath
	}

	switch operation {
	case "path":
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		path, err := cliadmin.Path(scope, explicit)
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		if *jsonOutput {
			return writeAdminJSON(stdout, stderr, map[string]any{"scope": scope, "path": path})
		}
		fmt.Fprintln(stdout, path)
		return 0

	case "show":
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		snapshot, err := cliadmin.Read(scope, explicit)
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		if *jsonOutput {
			return writeAdminJSON(stdout, stderr, snapshot)
		}
		fmt.Fprintf(stdout, "%s config: %s\n", snapshot.Scope, snapshot.Path)
		for _, field := range snapshot.Fields {
			fmt.Fprintf(stdout, "%s = %s\n", field.Key, displayConfigValue(field.Value))
		}
		return 0

	case "get":
		if fs.NArg() != 1 {
			fs.Usage()
			return 2
		}
		field, err := cliadmin.Get(scope, explicit, fs.Arg(0))
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		if *jsonOutput {
			return writeAdminJSON(stdout, stderr, field)
		}
		fmt.Fprintln(stdout, displayConfigValue(field.Value))
		return 0

	case "set":
		if fs.NArg() != 2 {
			fs.Usage()
			return 2
		}
		field, err := cliadmin.Set(scope, explicit, fs.Arg(0), fs.Arg(1))
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		if *jsonOutput {
			return writeAdminJSON(stdout, stderr, field)
		}
		fmt.Fprintf(stdout, "%s = %s\n", field.Key, displayConfigValue(field.Value))
		return 0

	case "reset":
		if fs.NArg() != 1 {
			fs.Usage()
			return 2
		}
		field, err := cliadmin.Reset(scope, explicit, fs.Arg(0))
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		if *jsonOutput {
			return writeAdminJSON(stdout, stderr, field)
		}
		fmt.Fprintf(stdout, "%s = %s\n", field.Key, displayConfigValue(field.Value))
		return 0

	case "validate":
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		result, err := cliadmin.Validate(scope, explicit)
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		if *jsonOutput {
			if code := writeAdminJSON(stdout, stderr, result); code != 0 {
				return code
			}
		} else {
			writeValidationResult(stdout, result)
		}
		if !result.Valid {
			return 1
		}
		return 0

	default:
		writeAdminError(stderr, fmt.Errorf("unknown config command %q", operation), 2)
		writeConfigUsage(stderr)
		return 2
	}
}

func runConfigValidateAll(configPath string, jsonOutput bool, stdout, stderr io.Writer) int {
	results := make([]cliadmin.ValidationResult, 0, 2)
	for _, scope := range []cliadmin.ConfigScope{cliadmin.CoreConfig, cliadmin.ClientConfig} {
		explicit := ""
		if scope == cliadmin.CoreConfig {
			explicit = configPath
		}
		result, err := cliadmin.Validate(scope, explicit)
		if err != nil {
			return writeAdminError(stderr, err, 1)
		}
		results = append(results, result)
	}
	if jsonOutput {
		if code := writeAdminJSON(stdout, stderr, results); code != 0 {
			return code
		}
	} else {
		for _, result := range results {
			writeValidationResult(stdout, result)
		}
	}
	for _, result := range results {
		if !result.Valid {
			return 1
		}
	}
	return 0
}

func runDoctorCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbar doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "Explicit core config path")
	model := fs.String("model", "", "Model alias to validate")
	workspace := fs.String("workspace", "", "Workspace to validate")
	theme := fs.String("theme", "", "CLI theme to validate")
	colorMode := fs.String("color", "", "Color mode: auto, always, or never")
	jsonOutput := fs.Bool("json", false, "Emit JSON")
	fs.Usage = func() { writeDoctorUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	clientCfg := &config.ClientConfig{
		Theme:     "system",
		ColorMode: config.ColorModeAuto,
	}
	if loaded, err := config.LoadClientConfig(); err == nil {
		clientCfg = loaded
	}
	effectiveTheme := preferredTheme(*theme, os.Getenv("SANDBAR_THEME"), clientCfg.Theme)
	effectiveColor := strings.ToLower(strings.TrimSpace(*colorMode))
	if effectiveColor == "" {
		effectiveColor = clientCfg.ColorMode
	}
	report := cliadmin.RunDoctor(ctx, cliadmin.DoctorOptions{
		ConfigPath: *configPath,
		Model:      *model,
		Workspace:  *workspace,
		Theme:      effectiveTheme,
		ColorMode:  effectiveColor,
		Version:    version,
	})
	if *jsonOutput {
		data, err := report.JSON()
		if err != nil {
			return writeAdminError(stderr, fmt.Errorf("encode doctor report: %w", err), 1)
		}
		fmt.Fprintln(stdout, string(data))
	} else {
		fmt.Fprintln(stdout, report.Human())
	}
	if !report.Healthy {
		return 1
	}
	return 0
}

func runVersionCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbar version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeVersionUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	fmt.Fprintln(stdout, "sandbar", version)
	return 0
}

func runCompletionCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbar completion", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeCompletionUsage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	script, err := cliadmin.GenerateCompletion(cliadmin.CompletionShell(fs.Arg(0)), sandbarCommandSpec())
	if err != nil {
		return writeAdminError(stderr, err, 2)
	}
	fmt.Fprint(stdout, script)
	return 0
}

func parseConfigScope(value string, allowAll bool) (cliadmin.ConfigScope, bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(cliadmin.ClientConfig), "":
		return cliadmin.ClientConfig, false, nil
	case string(cliadmin.CoreConfig):
		return cliadmin.CoreConfig, false, nil
	case "all":
		if allowAll {
			return "", true, nil
		}
	}
	want := "client or core"
	if allowAll {
		want += ", or all"
	}
	return "", false, fmt.Errorf("invalid config scope %q (want %s)", value, want)
}

func writeAdminJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return writeAdminError(stderr, fmt.Errorf("encode JSON: %w", err), 1)
	}
	return 0
}

func displayConfigValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func writeValidationResult(output io.Writer, result cliadmin.ValidationResult) {
	status := "valid"
	if !result.Valid {
		status = "invalid: " + result.Message
	}
	fmt.Fprintf(output, "%s config: %s (%s)\n", result.Scope, status, result.Path)
}

func writeAdminError(output io.Writer, err error, code int) int {
	fmt.Fprintf(output, "error: %v\n", err)
	return code
}

func writeConfigUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: sandbar config <path|show|get|set|reset|validate> [options]")
	fmt.Fprintln(output, "Run 'sandbar config <command> --help' for command-specific help.")
}

func writeConfigOperationUsage(output io.Writer, operation string) {
	usage := map[string]string{
		"path":     "Usage: sandbar config path [--scope client|core] [--config PATH] [--json]",
		"show":     "Usage: sandbar config show [--scope client|core] [--config PATH] [--json]",
		"get":      "Usage: sandbar config get [--scope client|core] [--config PATH] [--json] KEY",
		"set":      "Usage: sandbar config set [--scope client] [--json] KEY VALUE",
		"reset":    "Usage: sandbar config reset [--scope client] [--json] KEY",
		"validate": "Usage: sandbar config validate [--scope client|core|all] [--config PATH] [--json]",
	}
	if line, ok := usage[operation]; ok {
		fmt.Fprintln(output, line)
		return
	}
	writeConfigUsage(output)
}

func writeDoctorUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: sandbar doctor [--config PATH] [--model ALIAS] [--workspace DIR] [--theme THEME] [--color auto|always|never] [--json]")
}

func writeCompletionUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: sandbar completion <bash|zsh|fish>")
}

func writeVersionUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: sandbar version")
}

func writeRootUsage(output io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(output, "Usage:")
	fmt.Fprintln(output, "  sandbar [options] [message ...]")
	fmt.Fprintln(output, "  sandbar <command> [options]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Commands:")
	for _, command := range sandbarCommandSpec().Subcommands {
		name := command.Name
		if len(command.Aliases) > 0 {
			name += " (" + strings.Join(command.Aliases, ", ") + ")"
		}
		fmt.Fprintf(output, "  %-24s %s\n", name, command.Description)
	}
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Options:")
	if fs != nil {
		fs.PrintDefaults()
	}
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Use 'sandbar -- <message>' when a prompt begins with a command name.")
}

func sandbarCommandSpec() cliadmin.CommandSpec {
	value := func(name string, choices ...string) *cliadmin.ArgumentSpec {
		return &cliadmin.ArgumentSpec{Name: name, Choices: choices}
	}
	pathValue := func(name string, hint cliadmin.ValueHint) *cliadmin.ArgumentSpec {
		return &cliadmin.ArgumentSpec{Name: name, Hint: hint}
	}
	flagValue := func(names []string, description string, argument *cliadmin.ArgumentSpec) cliadmin.FlagSpec {
		return cliadmin.FlagSpec{Names: names, Description: description, Value: argument}
	}
	boolFlag := func(names []string, description string) cliadmin.FlagSpec {
		return cliadmin.FlagSpec{Names: names, Description: description}
	}
	helpFlag := func() cliadmin.FlagSpec {
		return boolFlag([]string{"-h", "--help"}, "Show help")
	}
	configFlags := func(includeConfigPath bool, scopes ...string) []cliadmin.FlagSpec {
		flags := []cliadmin.FlagSpec{
			flagValue([]string{"--scope"}, "Configuration scope", value("scope", scopes...)),
		}
		if includeConfigPath {
			flags = append(flags, flagValue([]string{"--config"}, "Explicit core config path", pathValue("path", cliadmin.FileHint)))
		}
		return append(flags, boolFlag([]string{"--json"}, "Emit JSON"), helpFlag())
	}

	return cliadmin.CommandSpec{
		Name:        "sandbar",
		Description: "Sandbar coding-agent CLI",
		Flags: []cliadmin.FlagSpec{
			flagValue([]string{"-model", "--model"}, "Model alias", value("alias")),
			flagValue([]string{"-workspace", "--workspace"}, "Workspace directory", pathValue("directory", cliadmin.DirectoryHint)),
			flagValue([]string{"-config", "--config"}, "Core config path", pathValue("path", cliadmin.FileHint)),
			flagValue([]string{"-thread", "--thread"}, "Resume thread", value("thread")),
			flagValue([]string{"-resume", "--resume"}, "Resume thread alias", value("thread")),
			flagValue([]string{"-theme", "--theme"}, "CLI theme", value("theme")),
			flagValue([]string{"-color", "--color"}, "Color output", value("mode", "auto", "always", "never")),
			boolFlag([]string{"-json", "--json"}, "Emit newline-delimited JSON events"),
			boolFlag([]string{"-summarize-context", "--summarize-context"}, "Summarize a JSON message batch"),
			boolFlag([]string{"-disable-subagents", "--disable-subagents"}, "Disable subagent tools for a local run"),
			flagValue([]string{"-tools", "--tools"}, "Restrict a local run to these tools (comma-separated)", value("names")),
			boolFlag([]string{"-list-themes", "--list-themes"}, "List CLI themes"),
			boolFlag([]string{"-version", "--version"}, "Print version and exit"),
			helpFlag(),
		},
		Arguments: []cliadmin.ArgumentSpec{{Name: "message", Repeatable: true}},
		Subcommands: []cliadmin.CommandSpec{
			{
				Name:        "config",
				Description: "Inspect and update Sandbar configuration",
				Flags:       []cliadmin.FlagSpec{helpFlag()},
				Subcommands: []cliadmin.CommandSpec{
					{Name: "path", Description: "Print a config path", Flags: configFlags(true, "client", "core")},
					{Name: "show", Description: "Show redacted effective config", Flags: configFlags(true, "client", "core")},
					{Name: "get", Description: "Read one config value", Flags: configFlags(true, "client", "core"), Arguments: []cliadmin.ArgumentSpec{{Name: "key"}}},
					{Name: "set", Description: "Set one client preference", Flags: configFlags(false, "client"), Arguments: []cliadmin.ArgumentSpec{{Name: "key"}, {Name: "value"}}},
					{Name: "reset", Description: "Reset one client preference", Flags: configFlags(false, "client"), Arguments: []cliadmin.ArgumentSpec{{Name: "key"}}},
					{Name: "validate", Description: "Validate configuration", Flags: configFlags(true, "client", "core", "all")},
				},
			},
			{
				Name:        "doctor",
				Description: "Diagnose local setup",
				Flags: []cliadmin.FlagSpec{
					flagValue([]string{"--config"}, "Explicit core config path", pathValue("path", cliadmin.FileHint)),
					flagValue([]string{"--model"}, "Model alias", value("alias")),
					flagValue([]string{"--workspace"}, "Workspace directory", pathValue("directory", cliadmin.DirectoryHint)),
					flagValue([]string{"--theme"}, "CLI theme", value("theme")),
					flagValue([]string{"--color"}, "Color mode", value("mode", "auto", "always", "never")),
					boolFlag([]string{"--json"}, "Emit JSON"),
					helpFlag(),
				},
			},
			{
				Name:        "version",
				Description: "Print version and exit",
				Flags:       []cliadmin.FlagSpec{helpFlag()},
			},
			{
				Name:        "completion",
				Aliases:     []string{"completions"},
				Description: "Generate shell completion",
				Flags:       []cliadmin.FlagSpec{helpFlag()},
				Arguments:   []cliadmin.ArgumentSpec{{Name: "shell", Choices: []string{"bash", "zsh", "fish"}}},
			},
		},
	}
}
