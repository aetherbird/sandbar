package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientConfig holds per-user CLI/TUI preferences.
type ClientConfig struct {
	DefaultModel string `yaml:"default_model"`
	Theme        string `yaml:"theme"`
	ColorMode    string `yaml:"color_mode"`
	FontSize     int    `yaml:"font_size"`
}

const (
	ColorModeAuto   = "auto"
	ColorModeAlways = "always"
	ColorModeNever  = "never"
)

// clientConfigPath returns the path to the client config file.
func clientConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sandbar", "client.yaml")
}

// defaultClientConfig returns a zero-valued config with sensible defaults.
func defaultClientConfig() *ClientConfig {
	return &ClientConfig{
		Theme:     "system",
		ColorMode: ColorModeAuto,
		FontSize:  15,
	}
}

func normalizeColorMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ColorModeAlways:
		return ColorModeAlways
	case ColorModeNever:
		return ColorModeNever
	default:
		return ColorModeAuto
	}
}

// LoadClientConfig reads the client config, creating it with commented defaults if missing.
// If the file exists but is malformed, it prints a warning to stderr and returns defaults.
func LoadClientConfig() (*ClientConfig, error) {
	path := clientConfigPath()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, fmt.Errorf("create config dir: %w", err)
		}
		template := `# Sandbar Client Config
# This file is auto-created on first run. Edit to set defaults.

default_model: ""       # Preferred model alias (e.g. "gemma4")

# TUI preferences
theme: "system"         # --theme > SANDBAR_THEME > this preference > system light/dark
color_mode: "auto"      # auto | always | never (NO_COLOR also disables color in auto mode)
font_size: 15           # px
`
		if err := os.WriteFile(path, []byte(template), 0644); err != nil {
			return nil, fmt.Errorf("write default client config: %w", err)
		}
		return defaultClientConfig(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client config: %w", err)
	}

	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s is malformed: %v. Using defaults.\n", path, err)
		return defaultClientConfig(), nil
	}

	// Apply defaults for empty fields.
	if cfg.Theme == "" {
		cfg.Theme = "system"
	}
	cfg.ColorMode = normalizeColorMode(cfg.ColorMode)
	if cfg.FontSize == 0 {
		cfg.FontSize = 15
	}

	return &cfg, nil
}

// Save writes the client config to disk.
func (c *ClientConfig) Save() error {
	path := clientConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	c.Theme = strings.TrimSpace(c.Theme)
	if c.Theme == "" {
		c.Theme = "system"
	}
	c.ColorMode = normalizeColorMode(c.ColorMode)
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal client config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
