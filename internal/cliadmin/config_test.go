package cliadmin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aetherbird/sandbar/internal/config"
)

func writeCoreConfig(t *testing.T, dir, workspace, database string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	content := `workspace: ` + workspace + `
database: ` + database + `
providers:
  - name: example
    base_url: https://models.example.test/v1?private=query
    api_key: provider-secret-value
    model_defaults:
      context_length: 65536
      supports_tools: true
    models:
      demo: {}
tools:
  web_search:
    brave_api_key: brave-secret-value
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func useClientHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".config", "sandbar", "client.yaml")
}

func TestCoreConfigReadIsTypedAndRedacted(t *testing.T) {
	dir := t.TempDir()
	path := writeCoreConfig(t, dir, dir, filepath.Join(dir, "sandbar.db"))

	snapshot, err := Read(CoreConfig, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snapshot.Path != path || snapshot.Scope != CoreConfig {
		t.Fatalf("snapshot target = %+v", snapshot)
	}

	databaseField, err := Get(CoreConfig, path, "database")
	if err != nil {
		t.Fatalf("Get database: %v", err)
	}
	if databaseField.Type != StringValue || databaseField.Writable {
		t.Fatalf("database = %+v", databaseField)
	}

	for _, key := range []string{"providers.0.api_key", "tools.web_search.brave_api_key"} {
		field, err := Get(CoreConfig, path, key)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if !field.Redacted || field.Value != redactedValue {
			t.Errorf("%s was not redacted: %+v", key, field)
		}
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"provider-secret-value", "brave-secret-value"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, encoded)
		}
	}
}

func TestClientConfigSetResetAndPath(t *testing.T) {
	wantPath := useClientHome(t)
	gotPath, err := Path(ClientConfig, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != wantPath {
		t.Fatalf("client path = %q, want %q", gotPath, wantPath)
	}

	theme, err := Set(ClientConfig, "", "theme", " tokyo-night ")
	if err != nil {
		t.Fatalf("Set theme: %v", err)
	}
	if theme.Value != "tokyo-night" || !theme.Writable {
		t.Fatalf("theme field = %+v", theme)
	}
	font, err := Set(ClientConfig, "", "font_size", "18")
	if err != nil {
		t.Fatalf("Set font_size: %v", err)
	}
	if font.Type != IntegerValue || font.Value != 18 {
		t.Fatalf("font field = %+v", font)
	}

	loaded, err := config.LoadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Theme != "tokyo-night" || loaded.FontSize != 18 {
		t.Fatalf("persisted client config = %+v", loaded)
	}

	if _, err := Set(ClientConfig, "", "theme", "not-a-theme"); err == nil {
		t.Fatal("invalid theme was accepted")
	}
	if _, err := Set(ClientConfig, "", "font_size", "zero"); err == nil {
		t.Fatal("non-integer font size was accepted")
	}
}

func TestCoreConfigMutationIsExplicitlyUnsupported(t *testing.T) {
	if _, err := Set(CoreConfig, "/unused", "server.port", "8081"); !errors.Is(err, ErrCoreMutationUnsupported) {
		t.Fatalf("Set error = %v", err)
	}
	if _, err := Reset(CoreConfig, "/unused", "server.port"); !errors.Is(err, ErrCoreMutationUnsupported) {
		t.Fatalf("Reset error = %v", err)
	}
}

func TestValidateCoreAndClient(t *testing.T) {
	dir := t.TempDir()
	validPath := writeCoreConfig(t, dir, dir, filepath.Join(dir, "sandbar.db"))
	valid, err := Validate(CoreConfig, validPath)
	if err != nil || !valid.Valid {
		t.Fatalf("valid core result = %+v, err=%v", valid, err)
	}

	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte("providers: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid, err := Validate(CoreConfig, badPath)
	if err != nil {
		t.Fatalf("Validate malformed core returned operational error: %v", err)
	}
	if invalid.Valid || !strings.Contains(invalid.Message, "parse config") {
		t.Fatalf("malformed core result = %+v", invalid)
	}

	clientPath := useClientHome(t)
	if err := os.MkdirAll(filepath.Dir(clientPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientPath, []byte("theme: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	clientResult, err := Validate(ClientConfig, "")
	if err != nil {
		t.Fatalf("Validate malformed client returned operational error: %v", err)
	}
	if clientResult.Valid || !strings.Contains(clientResult.Message, "parse client config") {
		t.Fatalf("malformed client result = %+v", clientResult)
	}
}
