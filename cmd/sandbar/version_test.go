package main

import "testing"

// TestResolvedVersionPrefersStampedVersion pins the ldflags contract:
// a stamped main.version (Makefile / goreleaser) always wins over the
// build-info fallbacks.
func TestResolvedVersionPrefersStampedVersion(t *testing.T) {
	original := version
	defer func() { version = original }()

	version = "v9.9.9-test"
	if got := resolvedVersion(); got != "v9.9.9-test" {
		t.Fatalf("resolvedVersion() = %q, want stamped %q", got, "v9.9.9-test")
	}
}

// TestResolvedVersionDevFallbackSane pins that an unstamped build never
// reports an empty or absurd version: it is the build-info module version,
// the short VCS revision, or the plain "dev" default.
func TestResolvedVersionDevFallbackSane(t *testing.T) {
	original := version
	defer func() { version = original }()

	version = "dev"
	got := resolvedVersion()
	if got == "" {
		t.Fatal("resolvedVersion() returned an empty version")
	}
	if got != "dev" && len(got) > 64 {
		t.Fatalf("resolvedVersion() = %q, want a short version", got)
	}
}
