package main

// version is the build version reported by `sandbar --version`,
// `sandbar version`, and `sandbar doctor`. It is a var (not a const) so
// release builds can stamp it at link time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/cli
//
// main.version is the linker symbol for this package (package main), so the
// stamp works regardless of the module path. The Makefile and .goreleaser.yaml
// both stamp it from the git tag; unstamped builds report "dev".
var version = "dev"
