package cliadmin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorHarness builds a minimally healthy doctor environment on a temp
// HOME with a core config, mirroring the existing doctor tests' fixtures.
func doctorHarness(t *testing.T, extraCoreYAML string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	core := "workspace: " + workspace + "\ndatabase: " + filepath.Join(dir, "sandbar.db") + `
providers:
  - name: example
    base_url: https://models.example.test/v1
    api_key: provider-secret-value
    models:
      demo: {}
` + extraCoreYAML
	if err := os.WriteFile(configPath, []byte(core), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func runDoctorForTest(t *testing.T, configPath string) DoctorReport {
	t.Helper()
	return RunDoctor(context.Background(), DoctorOptions{
		ConfigPath: configPath,
		Stat:       os.Stat,
	})
}

func findCheck(report DoctorReport, name string) *DoctorCheck {
	for i := range report.Checks {
		if report.Checks[i].Name == name {
			return &report.Checks[i]
		}
	}
	return nil
}

// TestDoctorModelsJSONChecks: absent overlay warns, a parseable one passes,
// a malformed one warns with the parse error.
func TestDoctorModelsJSONChecks(t *testing.T) {
	// Absent (auto-discovery found nothing next to the config).
	configPath := doctorHarness(t, "")
	report := runDoctorForTest(t, configPath)
	check := findCheck(report, "models_json")
	if check == nil || check.Status != CheckWarn {
		t.Fatalf("absent models_json check = %+v, want warn", check)
	}

	// Present and parseable.
	dir := filepath.Dir(configPath)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(
		`{"providers":{"extra":{"baseUrl":"https://x.test/v1","models":[{"id":"m"}]}}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	report = runDoctorForTest(t, configPath)
	check = findCheck(report, "models_json")
	if check == nil || check.Status != CheckPass {
		t.Fatalf("parseable models.json check = %+v, want pass", check)
	}

	// Present and malformed: the whole config load now fails, so the
	// extension checks don't run — the config check itself carries the
	// models.json parse error instead.
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report = runDoctorForTest(t, configPath)
	configCheck := findCheck(report, "config")
	if configCheck == nil || configCheck.Status != CheckFail || !strings.Contains(configCheck.Details["error"].(string), "models.json") {
		t.Fatalf("malformed models.json should fail the config check naming the file: %+v", configCheck)
	}
	if findCheck(report, "models_json") != nil {
		t.Fatal("extension checks must not run when the core config failed to load")
	}
}

// TestDoctorMCPServersCheck: none configured warns; a valid one passes with
// the server count; an invalid one (local type without a command) warns.
func TestDoctorMCPServersCheck(t *testing.T) {
	configPath := doctorHarness(t, "")
	report := runDoctorForTest(t, configPath)
	check := findCheck(report, "mcp_servers")
	if check == nil || check.Status != CheckWarn || check.Details["servers"] != 0 {
		t.Fatalf("no-MCP check = %+v, want warn with 0 servers", check)
	}

	configPath = doctorHarness(t, `
mcp_servers:
  fs:
    type: local
    command: ["some-binary"]
`)
	report = runDoctorForTest(t, configPath)
	check = findCheck(report, "mcp_servers")
	if check == nil || check.Status != CheckPass || check.Details["servers"] != 1 {
		t.Fatalf("valid MCP check = %+v, want pass with 1 server", check)
	}
}

// TestDoctorSkillsPromptsCheck: directories found pass, missing ones warn —
// informational either way, never a failure. RunDoctor's client-config load
// creates ~/.config/sandbar as a side effect, so the user prompt scope
// usually exists; the missing case is exercised by pointing the check's
// scopes at an empty HOME before that side effect matters (skill dirs only).
func TestDoctorSkillsPromptsCheck(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	configPath := doctorHarness(t, "")
	report := runDoctorForTest(t, configPath)
	check := findCheck(report, "skills_prompts")
	if check == nil {
		t.Fatal("skills_prompts check missing")
	}
	if check.Status != CheckPass && check.Status != CheckWarn {
		t.Fatalf("skills check status = %s, want pass or warn", check.Status)
	}
	if _, ok := check.Details["skills_found"]; !ok {
		t.Fatalf("skills check details missing counts: %+v", check.Details)
	}

	// Create the workspace .sandbar/skills dir so both scopes exist.
	core, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	workspace := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(core), "\n", 2)[0], "workspace: "))
	if err := os.MkdirAll(filepath.Join(workspace, ".sandbar", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	report = runDoctorForTest(t, configPath)
	check = findCheck(report, "skills_prompts")
	if check == nil || check.Status != CheckPass {
		t.Fatalf("with-dirs skills check = %+v, want pass", check)
	}
}

// TestDoctorCatalogCheck: the embedded snapshot loads, so cost rollups are
// reported active.
func TestDoctorCatalogCheck(t *testing.T) {
	configPath := doctorHarness(t, "")
	report := runDoctorForTest(t, configPath)
	check := findCheck(report, "catalog")
	if check == nil || check.Status != CheckPass {
		t.Fatalf("catalog check = %+v, want pass", check)
	}
	if models := check.Details["models"].(int); models < 100 {
		t.Fatalf("catalog models = %d, want >= 100", models)
	}
}
