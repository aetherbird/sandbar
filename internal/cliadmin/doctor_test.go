package cliadmin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorHealthyReportIsHumanJSONAndSecretSafe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clientPath := filepath.Join(home, ".config", "sandbar", "client.yaml")
	if err := os.MkdirAll(filepath.Dir(clientPath), 0o755); err != nil {
		t.Fatal(err)
	}
	clientYAML := `default_model: example/demo
theme: nord
color_mode: auto
font_size: 15
`
	if err := os.WriteFile(clientPath, []byte(clientYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	database := filepath.Join(t.TempDir(), "sandbar.db")
	coreYAML := `workspace: ` + workspace + `
database: ` + database + `
providers:
  - name: example
    base_url: https://models.example.test/v1?private=model-query
    api_key: provider-secret-value
    model_defaults:
      context_length: 32768
      supports_tools: true
    models:
      demo: {}
`
	if err := os.WriteFile(configPath, []byte(coreYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	report := RunDoctor(context.Background(), DoctorOptions{
		ConfigPath: configPath,
		LookupPath: func(name string) (string, error) {
			return "/test/bin/" + name, nil
		},
		Getenv: func(key string) string {
			if key == "TERM" {
				return "xterm-256color"
			}
			return ""
		},
		IsTerminal: func(uintptr) bool { return true },
		Now:        func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) },
	})
	if !report.Healthy {
		t.Fatalf("doctor unexpectedly unhealthy:\n%s", report.Human())
	}

	jsonBytes, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	combined := report.Human() + "\n" + string(jsonBytes)
	for _, secret := range []string{
		"provider-secret-value",
		"private=model-query",
	} {
		if strings.Contains(combined, secret) {
			t.Fatalf("doctor output leaked %q:\n%s", secret, combined)
		}
	}
	for _, want := range []string{"Sandbar doctor: healthy", `"healthy": true`, redactedValue, "binary_rg"} {
		if !strings.Contains(combined, want) {
			t.Errorf("doctor output missing %q:\n%s", want, combined)
		}
	}
}

func TestDoctorMissingRequiredBinaryFailsButCompletesReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	configPath := writeCoreConfig(t, t.TempDir(), workspace, filepath.Join(t.TempDir(), "sandbar.db"))

	report := RunDoctor(context.Background(), DoctorOptions{
		ConfigPath: configPath,
		LookupPath: func(name string) (string, error) {
			if name == "bash" {
				return "", errors.New("not found")
			}
			return "/test/bin/" + name, nil
		},
		Getenv:     func(string) string { return "" },
		IsTerminal: func(uintptr) bool { return false },
	})
	if report.Healthy {
		t.Fatalf("doctor should be unhealthy:\n%s", report.Human())
	}
	var bash, terminal *DoctorCheck
	for i := range report.Checks {
		switch report.Checks[i].Name {
		case "binary_bash":
			bash = &report.Checks[i]
		case "terminal":
			terminal = &report.Checks[i]
		}
	}
	if bash == nil || bash.Status != CheckFail {
		t.Fatalf("bash check = %+v", bash)
	}
	if terminal == nil || terminal.Status != CheckWarn {
		t.Fatalf("terminal check = %+v", terminal)
	}
}

func TestDoctorReportEchoesVersionWhenProvided(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := writeCoreConfig(t, t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "sandbar.db"))

	report := RunDoctor(context.Background(), DoctorOptions{
		ConfigPath: configPath,
		Version:    " v0.1.0 ",
		Getenv:     func(string) string { return "" },
		IsTerminal: func(uintptr) bool { return false },
	})
	if report.Version != "v0.1.0" {
		t.Fatalf("report.Version = %q, want trimmed %q", report.Version, "v0.1.0")
	}
	if !strings.Contains(report.Human(), "Version: v0.1.0") {
		t.Errorf("human report missing version line:\n%s", report.Human())
	}
	jsonBytes, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jsonBytes), `"version": "v0.1.0"`) {
		t.Errorf("json report missing version field: %s", jsonBytes)
	}

	omitted := RunDoctor(context.Background(), DoctorOptions{Getenv: func(string) string { return "" }})
	if omitted.Version != "" || strings.Contains(omitted.Human(), "Version:") {
		t.Errorf("unset version should be omitted, got %q:\n%s", omitted.Version, omitted.Human())
	}
}
