package cliadmin

import (
	"os/exec"
	"strings"
	"testing"
)

func testCommandSpec() CommandSpec {
	return CommandSpec{
		Name:        "sandbar",
		Description: "Sandbar coding harness",
		PersistentFlags: []FlagSpec{
			{Names: []string{"-c", "--config"}, Description: "Core config path", Value: &ArgumentSpec{Name: "path", Hint: FileHint}},
		},
		Flags: []FlagSpec{{Names: []string{"--json"}, Description: "Emit JSON"}},
		Subcommands: []CommandSpec{
			{
				Name: "config", Description: "Manage configuration",
				Flags: []FlagSpec{{
					Names: []string{"--scope"}, Description: "Configuration scope",
					Value: &ArgumentSpec{Name: "scope", Choices: []string{"core", "client"}},
				}},
				Subcommands: []CommandSpec{
					{Name: "get", Description: "Read a value", Arguments: []ArgumentSpec{{Name: "key"}}},
					{Name: "set", Description: "Set a value", Arguments: []ArgumentSpec{{Name: "key"}, {Name: "value"}}},
				},
			},
			{Name: "doctor", Aliases: []string{"check"}, Description: "Check the installation"},
			{
				Name: "completion", Description: "Generate shell completion",
				Arguments: []ArgumentSpec{{
					Name: "shell", Description: "Target shell", Choices: []string{"bash", "zsh", "fish"},
				}},
			},
		},
	}
}

func TestGenerateCompletionFromSharedDescriptor(t *testing.T) {
	spec := testCommandSpec()
	for _, shell := range []CompletionShell{Bash, Zsh, Fish} {
		t.Run(string(shell), func(t *testing.T) {
			script, err := GenerateCompletion(shell, spec)
			if err != nil {
				t.Fatalf("GenerateCompletion: %v", err)
			}
			for _, want := range []string{"sandbar", "--config", "config", "doctor", "completion", "bash", "zsh", "fish"} {
				if !strings.Contains(script, want) {
					t.Errorf("%s script missing descriptor value %q:\n%s", shell, want, script)
				}
			}
			if strings.Contains(script, "hardcoded-command-not-in-spec") {
				t.Fatal("generator introduced a command absent from the descriptor")
			}
		})
	}
}

func TestGeneratedBashCompletionParses(t *testing.T) {
	script, err := GenerateCompletion(Bash, testCommandSpec())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s\nscript:\n%s", err, output, script)
	}

	probe := script + `
COMP_WORDS=(sandbar config --scope "")
COMP_CWORD=3
_sandbar_completion
printf '%s\n' "${COMPREPLY[@]}"
`
	cmd = exec.Command("bash")
	cmd.Stdin = strings.NewReader(probe)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash completion probe failed: %v\n%s", err, output)
	}
	got := string(output)
	if !strings.Contains(got, "core") || !strings.Contains(got, "client") {
		t.Fatalf("bash value completion = %q, want core and client", got)
	}
}

func TestFishDescriptionsAndMultiNameFlagCases(t *testing.T) {
	script, err := GenerateCompletion(Fish, testCommandSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "config\tManage configuration") {
		t.Fatalf("fish description is not tab-separated:\n%s", script)
	}
	if !strings.Contains(script, "case 'sandbar|-c' 'sandbar|--config'") {
		t.Fatalf("fish aliases do not share a value-taking case:\n%s", script)
	}
}

func TestValidateCommandSpecRejectsAmbiguity(t *testing.T) {
	tests := []struct {
		name string
		spec CommandSpec
	}{
		{
			name: "duplicate flag",
			spec: CommandSpec{Name: "x", Flags: []FlagSpec{{Names: []string{"--same"}}, {Names: []string{"--same"}}}},
		},
		{
			name: "duplicate child alias",
			spec: CommandSpec{Name: "x", Subcommands: []CommandSpec{{Name: "one", Aliases: []string{"shared"}}, {Name: "two", Aliases: []string{"shared"}}}},
		},
		{
			name: "choice with whitespace",
			spec: CommandSpec{Name: "x", Arguments: []ArgumentSpec{{Name: "value", Choices: []string{"not portable"}}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateCommandSpec(test.spec); err == nil {
				t.Fatal("invalid descriptor was accepted")
			}
		})
	}
	if _, err := GenerateCompletion("powershell", testCommandSpec()); err == nil {
		t.Fatal("unsupported shell was accepted")
	}
}
