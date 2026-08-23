package main

import "runtime/debug"

// version is the build version reported by `sandbar --version`,
// `sandbar version`, and `sandbar doctor`. It is a var (not a const) so
// release builds can stamp it at link time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/sandbar
//
// main.version is the linker symbol for this package (package main), so the
// stamp works regardless of the module path. The Makefile and .goreleaser.yaml
// both stamp it from the git tag; unstamped builds report "dev".
var version = "dev"

// resolvedVersion returns the effective build version. Precedence:
//
//  1. the linker-stamped main.version (release builds via Makefile or
//     goreleaser);
//  2. the main module version from build info — what a `go install
//     github.com/aetherbird/sandbar/cmd/sandbar@v0.1.1` build reports;
//  3. the VCS revision from build info, formatted as dev-<short-sha>, for
//     plain `go build` runs inside a checkout;
//  4. "dev".
func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		for _, setting := range bi.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				short := setting.Value
				if len(short) > 7 {
					short = short[:7]
				}
				return "dev-" + short
			}
		}
	}
	return version
}
