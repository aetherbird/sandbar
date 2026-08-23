package config

import (
	"path/filepath"
	"testing"
)

func TestResolveExplicitWins(t *testing.T) {
	t.Setenv("SANDBAR_CONFIG", "/from/env.yaml")
	got, err := Resolve("/explicit/flag.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/flag.yaml" {
		t.Errorf("explicit flag should win, got %q", got)
	}
}

func TestResolveEnvWhenNoFlag(t *testing.T) {
	t.Setenv("SANDBAR_CONFIG", "/from/env.yaml")
	got, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/from/env.yaml" {
		t.Errorf("env should be used when no flag, got %q", got)
	}
}

// TestSearchPathsNeverCwd is the property the user cared about: config is only
// ever read from explicit/env or a fixed absolute location — never the working
// directory.
func TestSearchPathsNeverCwd(t *testing.T) {
	for _, p := range configSearchPaths() {
		if !filepath.IsAbs(p) {
			t.Errorf("search path %q is not absolute — must not be cwd-relative", p)
		}
		if p == "config.yaml" || p == "./config.yaml" || p == "." {
			t.Errorf("search path %q resolves against the working directory", p)
		}
	}
}
