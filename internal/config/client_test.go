package config

import (
	"os"
	"path/filepath"
	"testing"
)

func useTempClientConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".config", "sandbar", "client.yaml")
}

func TestLoadClientConfigCreatesThemeDefaults(t *testing.T) {
	path := useTempClientConfig(t)
	cfg, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.Theme != "system" || cfg.ColorMode != ColorModeAuto || cfg.FontSize != 15 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default client config was not created: %v", err)
	}
}

func TestLoadClientConfigReadsThemeAndColorMode(t *testing.T) {
	path := useTempClientConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("default_model: provider/model\ntheme: nord\ncolor_mode: always\nfont_size: 17\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if cfg.DefaultModel != "provider/model" || cfg.Theme != "nord" || cfg.ColorMode != ColorModeAlways || cfg.FontSize != 17 {
		t.Fatalf("loaded config = %+v", cfg)
	}
}

func TestClientConfigSaveRoundTripAndNormalizesAppearance(t *testing.T) {
	useTempClientConfig(t)
	cfg := &ClientConfig{
		DefaultModel: "provider/model",
		Theme:        "  tokyo-night  ",
		ColorMode:    "not-a-mode",
		FontSize:     16,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if cfg.Theme != "tokyo-night" || cfg.ColorMode != ColorModeAuto {
		t.Fatalf("normalized saved config = %+v", cfg)
	}
	loaded, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Theme != "tokyo-night" || loaded.ColorMode != ColorModeAuto {
		t.Fatalf("round-trip config = %+v", loaded)
	}
}

func TestClientConfigShowCostDefaultsAndRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ShowCost {
		t.Fatalf("show_cost must default to false (opt-in)")
	}
	cfg.ShowCost = true
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.ShowCost {
		t.Fatalf("show_cost round-trip: got false, want true")
	}
}
