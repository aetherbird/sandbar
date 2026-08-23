package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sandbar/internal/cliadmin"
)

func TestRunAdminCommandLeavesChatArgumentsAlone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(context.Background(), []string{"explain", "this"}, &stdout, &stderr)
	if handled || code != 0 {
		t.Fatalf("runAdminCommand() = (%v, %d), want (false, 0)", handled, code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestConfigCommandRoundTripAndRedaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	run := func(args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		handled, code := runAdminCommand(context.Background(), append([]string{"config"}, args...), &stdout, &stderr)
		if !handled {
			t.Fatalf("config command was not handled")
		}
		return stdout.String(), stderr.String(), code
	}

	stdout, stderr, code := run("set", "theme", "dracula")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "theme = dracula") {
		t.Fatalf("set theme: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = run("get", "theme")
	if code != 0 || stderr != "" || stdout != "dracula\n" {
		t.Fatalf("get theme: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	wantPath := filepath.Join(home, ".config", "sandbar", "client.yaml")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("client config was not persisted at %s: %v", wantPath, err)
	}
}

func TestConfigCommandHelpIsSuccessful(t *testing.T) {
	for _, help := range []string{"-h", "--help"} {
		t.Run(help, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			handled, code := runAdminCommand(context.Background(), []string{"config", help}, &stdout, &stderr)
			if !handled || code != 0 {
				t.Fatalf("runAdminCommand() = (%v, %d), want (true, 0)", handled, code)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: sandbar config") {
				t.Fatalf("unexpected help output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestConfigCoreMutationFailsClosed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(
		context.Background(),
		[]string{"config", "set", "--scope", "core", "workspace", "/tmp/example"},
		&stdout,
		&stderr,
	)
	if !handled || code != 1 {
		t.Fatalf("runAdminCommand() = (%v, %d), want (true, 1)", handled, code)
	}
	if !strings.Contains(stderr.String(), cliadmin.ErrCoreMutationUnsupported.Error()) {
		t.Fatalf("stderr %q does not explain core mutation policy", stderr.String())
	}
}

func TestCompletionCommandUsesPublicDescriptor(t *testing.T) {
	if err := cliadmin.ValidateCommandSpec(sandbarCommandSpec()); err != nil {
		t.Fatalf("invalid Sandbar command descriptor: %v", err)
	}

	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			handled, code := runAdminCommand(context.Background(), []string{"completion", shell}, &stdout, &stderr)
			if !handled || code != 0 || stderr.Len() != 0 {
				t.Fatalf("completion %s: handled=%v code=%d stderr=%q", shell, handled, code, stderr.String())
			}
			for _, want := range []string{"config", "doctor", "completion", "--list-themes"} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("%s completion does not contain %q", shell, want)
				}
			}
		})
	}
}

func TestRootUsageIncludesAdminCommandsAndPromptEscape(t *testing.T) {
	var output bytes.Buffer
	fs := flag.NewFlagSet("sandbar", flag.ContinueOnError)
	fs.SetOutput(&output)
	fs.Bool("list-themes", false, "List CLI themes")
	writeRootUsage(&output, fs)

	usage := output.String()
	for _, want := range []string{"sandbar <command>", "config", "doctor", "completion", "-list-themes", "sandbar -- <message>"} {
		if !strings.Contains(usage, want) {
			t.Errorf("root usage missing %q:\n%s", want, usage)
		}
	}
}

func TestAdminCommandDescriptorMatchesPublicFlags(t *testing.T) {
	root := sandbarCommandSpec()
	rootFlags := make(map[string]bool)
	for _, commandFlag := range root.Flags {
		for _, name := range commandFlag.Names {
			rootFlags[name] = true
		}
	}
	for _, name := range []string{"-disable-subagents", "--disable-subagents"} {
		if !rootFlags[name] {
			t.Errorf("root descriptor missing public flag %q; flags=%v", name, rootFlags)
		}
	}
	assertCommandFlags(t, findCommandSpec(t, root, "config"), "-h", "--help")
	assertCommandFlags(t, findCommandSpec(t, root, "config", "set"), "--scope", "--json", "-h", "--help")
	assertCommandFlags(t, findCommandSpec(t, root, "config", "reset"), "--scope", "--json", "-h", "--help")
	assertCommandFlags(t, findCommandSpec(t, root, "doctor"),
		"--config", "--model", "--workspace", "--theme", "--color", "--json", "-h", "--help")
	assertCommandFlags(t, findCommandSpec(t, root, "completion"), "-h", "--help")
}

func TestDoctorHelpListsThemeAndColorFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(context.Background(), []string{"doctor", "--help"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("runAdminCommand() = (%v, %d), want (true, 0)", handled, code)
	}
	for _, want := range []string{"--theme THEME", "--color auto|always|never"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("doctor help missing %q: %q", want, stderr.String())
		}
	}
}

func TestDoctorContinuesWhenClientConfigCannotBeLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clientPath := filepath.Join(home, ".config", "sandbar", "client.yaml")
	if err := os.MkdirAll(clientPath, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := writeAdminCoreConfig(t)

	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(context.Background(), []string{
		"doctor", "--config", configPath, "--model", "example/demo", "--json",
	}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("doctor: handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("doctor wrote an early client config error instead of a report: %q", stderr.String())
	}
	var report cliadmin.DoctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor report: %v\n%s", err, stdout.String())
	}
	if len(report.Checks) < 10 {
		t.Fatalf("doctor stopped early; got only %d checks: %+v", len(report.Checks), report.Checks)
	}
	for _, check := range report.Checks {
		if check.Name == "client_config" {
			if check.Status != cliadmin.CheckWarn {
				t.Fatalf("client_config status = %q, want warn", check.Status)
			}
			return
		}
	}
	t.Fatal("doctor report did not include client_config check")
}

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(context.Background(), []string{"version"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("runAdminCommand(version) = (%v, %d), want (true, 0)", handled, code)
	}
	if want := "sandbar " + version + "\n"; stdout.String() != want {
		t.Fatalf("version output = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(context.Background(), []string{"version", "extra"}, &stdout, &stderr)
	if !handled || code != 2 {
		t.Fatalf("runAdminCommand(version extra) = (%v, %d), want (true, 2)", handled, code)
	}
	if !strings.Contains(stderr.String(), "Usage: sandbar version") {
		t.Fatalf("stderr %q missing usage", stderr.String())
	}
}

func TestDoctorReportCarriesVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := writeAdminCoreConfig(t)

	var stdout, stderr bytes.Buffer
	handled, code := runAdminCommand(context.Background(),
		[]string{"doctor", "--config", configPath, "--model", "example/demo", "--json"},
		&stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("doctor: handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	var report cliadmin.DoctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor report: %v", err)
	}
	if report.Version != version {
		t.Fatalf("doctor report version = %q, want %q", report.Version, version)
	}
}

func TestRootUsageListsVersionCommandAndFlag(t *testing.T) {
	var output bytes.Buffer
	fs := flag.NewFlagSet("sandbar", flag.ContinueOnError)
	fs.SetOutput(&output)
	fs.Bool("version", false, "Print version and exit")
	writeRootUsage(&output, fs)
	usage := output.String()
	for _, want := range []string{"version", "-version"} {
		if !strings.Contains(usage, want) {
			t.Errorf("root usage missing %q:\n%s", want, usage)
		}
	}
}

func findCommandSpec(t *testing.T, root cliadmin.CommandSpec, path ...string) cliadmin.CommandSpec {
	t.Helper()
	current := root
	for _, name := range path {
		found := false
		for _, child := range current.Subcommands {
			if child.Name == name {
				current = child
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("command %q not found below %q", name, current.Name)
		}
	}
	return current
}

func assertCommandFlags(t *testing.T, command cliadmin.CommandSpec, want ...string) {
	t.Helper()
	got := make(map[string]bool)
	for _, flag := range command.Flags {
		for _, name := range flag.Names {
			got[name] = true
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s flag count = %d (%v), want %d (%v)", command.Name, len(got), got, len(want), want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s descriptor missing public flag %q; flags=%v", command.Name, name, got)
		}
	}
}

func writeAdminCoreConfig(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "sandbar.db")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	contents := `server:
  host: 127.0.0.1
workspace: ` + workspace + `
database: ` + database + `
providers:
  - name: example
    base_url: http://127.0.0.1:12345/v1
    api_key: test-provider-secret
    model_defaults:
      context_length: 32768
      supports_tools: true
    models:
      demo: {}
`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}
