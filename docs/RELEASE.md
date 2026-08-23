# Release Guide

How Sandbar releases work today. The module is `github.com/aetherbird/sandbar`
(public repo, root layout: `cmd/sandbar`, `internal/`, `migrations/`).

## Current state

- **Tags: `v0.1.1` (first public release) and `v0.3.0` (current).** The
  intermediate tags (`v0.1.2`, `v0.2.0`, `v0.2.1`) were deleted on
  2026-08-23 to keep the public release history minimal; they are
  `retract`ed in `go.mod` so any Go-proxy-cached copies resolve with a
  clear "superseded" error instead of installing stale code.
- **Module** is `github.com/aetherbird/sandbar`; `go install
  github.com/aetherbird/sandbar/cmd/sandbar@latest` installs the binary as
  `sandbar`.
- **No goreleaser-published release exists yet**, so there are no prebuilt
  binaries: `install.sh` and `SANDBAR_VERSION=...` pinning will work once a
  release is cut. Until then, users build from source or use `go install`.

## Cutting a release

From the repo root:

```bash
goreleaser release --snapshot --clean    # local full dry run, no network
goreleaser check                         # config validates
```

On the release tag (tag first, then publish):

```bash
git tag -a vX.Y.Z -m "release vX.Y.Z"
git push origin vX.Y.Z
GITHUB_TOKEN=<token> goreleaser release --clean
```

goreleaser builds `./cmd/sandbar` as `sandbar` (CGO off, stripped,
version-stamped with `-X main.version={{ .Version }}`) for
linux/darwin/windows/freebsd × amd64/arm64 and publishes the archives plus a
checksums file on the GitHub release. Snapshot builds stamp
`X.Y.(Z+1)-dev` via `snapshot.version_template`.

## install.sh ↔ goreleaser coupling invariants

`install.sh` derives asset URLs from goreleaser's output names. If you change
one, change the other in the same commit — the installer verifies checksums
and dies loudly on a mismatch, so a drift breaks installs on release day.

The two names, exactly as they appear in both files:

| Artifact | `.goreleaser.yaml` | `install.sh` |
|---|---|---|
| Checksums file | `checksum.name_template: sandbar_checksums.txt` (versionless on purpose, so `releases/latest/download` redirects work) | `checksums_name="sandbar_checksums.txt"` |
| Archive | `archives[].name: sandbar_{{ .Version }}_{{ .Os }}_{{ .Arch }}` (goreleaser strips the leading `v` from the tag) | `archive_for()` prints `sandbar_<ver>_<os>_<arch>.tar.gz` with the `v` stripped from the requested version |

Windows archives are `.zip` (`format_overrides`); all other platforms are
`.tar.gz`. Both files document the dependency in a comment; keep those
comments in sync too.

## Pre-release checklist

- [ ] `go test -race -count=1 -skip TestFullTuiPipeline ./...` green (the
      skipped test dials a live API when `OPENROUTER_API_KEY` is set)
- [ ] `gofmt -l .` empty (CI enforces this) and `go vet ./...` green
- [ ] gitleaks job green in CI; no secrets or internal hostnames in the tree
- [ ] `cmd/sandbar/version.go` current (docstring, fallback logic); the
      `-X main.version` stamp path remains first priority
- [ ] `make build && ./sandbar version` prints the stamped version
- [ ] `README.md` install section matches the release being cut
      (`SANDBAR_VERSION=vX.Y.Z` examples)

## Post-release checklist

- [ ] `sandbar doctor` passes on a zero-config machine (only
      `OPENAI_API_KEY` set, no config file) — the `zero_config` check must be
      present and the report healthy
- [ ] `install.sh` smoke-tested on a clean Linux and macOS box (no repo
      checkout): `curl -fsSL
      https://raw.githubusercontent.com/aetherbird/sandbar/main/install.sh |
      bash`, then `sandbar version`; a tampered `SANDBAR_RELEASES_URL` must
      fail the checksum gate, and a `BIN_DIR` off `PATH` must print the
      `export PATH=...` fix line
- [ ] `go install github.com/aetherbird/sandbar/cmd/sandbar@vX.Y.Z` works
      from the module proxy and `sandbar version` reports `vX.Y.Z` (build
      info fallback in `cmd/sandbar/version.go`)

## Deferred

Homebrew tap and scoop bucket via goreleaser pipes once a first
goreleaser-published release exists (the commented `brews`/`scoop` blocks in
`.goreleaser.yaml` are ready to fill in with the aetherbird repo URLs).
