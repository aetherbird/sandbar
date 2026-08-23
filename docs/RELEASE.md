# Release Guide

How sandbar gets from this private repo to a public GitHub release. The
module path is `github.com/aetherbird/sandbar` (flipped at the first public push,
2026-08-23 — see "Module-path flip" below; the procedure is kept for the record)
the first public push. Nothing in `.goreleaser.yaml`, `Makefile`, or
`install.sh` hardcodes the public URL or embeds the module path — the version
stamp uses the module-path-independent `main.version` symbol, and every
public-URL reference in `install.sh` is parameterized and gets its real
value at release time.

Condensed from the legacy internal release plan, adapted to this repo.

## Pending owner decisions

These block the first public release; everything else can be prepared:

- [ ] **Public repo name / owner** — single personal GitHub account,
      `github.com/<owner>/sandbar` (org only if a second maintainer appears).
      Decide before the flip; the name is baked into the module path.
- [ ] **Commit author identity for the public repo** — the current private
      identity must NOT be used. Before pushing anything public:
      ```bash
      git config user.name  "<public name>"
      git config user.email "<public email>"   # repo-local, not --global
      ```
      Note: earlier commits in the history keep their original author
      identity. If that must not ship publicly, the flip commit needs to
      start a fresh history instead (`git checkout --orphan`), which loses
      provenance — owner's call.
- [ ] **Default `SANDBAR_OWNER` in `install.sh`** — replace the `OWNER`
      placeholder and update the README one-liner URL.

## Pre-push gates (every time, before any public push)

```bash
cd src
go vet ./...
go test -race -count=1 -skip TestFullTuiPipeline ./...
go build ./...                      # native
GOOS=windows GOARCH=amd64 go build ./...   # cross-compile spot check
make build-cli && ./sandbar version # stamped binary boots
```

Secrets — CI already runs gitleaks (`.github/workflows/ci.yml`); additionally:

```bash
gitleaks detect --source . --no-banner    # if installed locally
grep -rnE '([0-9]{1,3}\.){3}[0-9]{1,3}' --exclude-dir=.git --exclude='*_test.go' . \
  | grep -vE '127\.0\.0\.1|0\.0\.0\.0|169\.254'   # LAN IPs must not appear
git log --all --oneline | wc -l           # know what history you're pushing
```

No literal API keys, tokens, or internal hostnames anywhere in the tree
(gitleaks catches key shapes; the grep catches LAN references gitleaks
can't know about).

## Module-path flip

Today `module sandbar` and imports read `sandbar/internal/...`. At flip time:

1. Pick the owner/repo (decision above).
2. Decide the public layout. `go.mod` lives in `src/`, so for
   `go install` to work the module path must match the repo tree: either
   move `go.mod` to the repo root (module `github.com/<owner>/sandbar`,
   packages keep their `internal/...` paths) or keep `src/` and use
   `github.com/<owner>/sandbar/src` as the module path. Renaming
   `cmd/cli` → `cmd/sandbar` at the same time makes `go install` produce
   a binary named `sandbar` instead of `cli`. The steps below assume the
   root layout.
3. Rewrite module and imports:
   ```bash
   cd src
   go mod edit -module github.com/<owner>/sandbar
   grep -rl '"sandbar/' --include='*.go' . | xargs sed -i 's#"sandbar/#"github.com/<owner>/sandbar/#'
   go build ./... && go test -race -count=1 -skip TestFullTuiPipeline ./...
   ```
   The version stamp needs no change: Makefile and .goreleaser.yaml both
   stamp `main.version`, the linker symbol for package main, which is
   module-path-independent.
4. Add the public remote, set the repo-local author identity (above), push.
5. Tag and push the first release:
   ```bash
   git tag -a v0.1.0 -m "first public release"
   git push origin v0.1.0
   ```

## goreleaser first run

`src/.goreleaser.yaml` builds `cmd/cli` as `sandbar` — CGO off, stripped,
version-stamped — for linux/darwin/windows/freebsd × amd64/arm64, archives as
`sandbar_<version>_<os>_<arch>.tar.gz` (zip on windows), plus
`sandbar_checksums.txt` (sha256). Brew/scoop pipes are deferred as comments
until a public URL exists.

```bash
goreleaser check                          # config validates
goreleaser release --snapshot --clean     # full local dry run, no network
GITHUB_TOKEN=<token> goreleaser release --clean   # on the v0.1.0 tag
```

Snapshot builds stamp `X.Y.(Z+1)-dev` via `snapshot.version_template`.

## install.sh smoke test

On a clean Linux and macOS box (no repo checkout, minimal PATH):

```bash
SANDBAR_OWNER=<owner> sh -c \
  'curl -fsSL https://raw.githubusercontent.com/<owner>/sandbar/main/install.sh | sh'
sandbar --version        # prints sandbar v0.1.0
sandbar doctor           # passes with only env keys set
```

Check that the fetched checksums asset (`sandbar_checksums.txt`) matches
`checksum.name_template` in `src/.goreleaser.yaml`, that a tampered
`SANDBAR_RELEASES_URL` fails the checksum gate, and that a `BIN_DIR` off PATH
prints the exact `export PATH=...` fix line.

## First-release checklist

- [ ] Owner decisions resolved (repo name, public identity, default owner)
- [ ] Module flip commit landed; `go install github.com/<owner>/sandbar/cmd/sandbar@latest` works
- [ ] Pre-push gates green (vet, race suite, cross-compile, gitleaks, greps)
- [ ] Tag `v0.1.0` pushed; goreleaser published all targets + checksums
- [ ] `install.sh` smoke-tested on clean Linux and macOS
- [ ] `sandbar doctor` passes with only env keys set (zero-config claim)
- [ ] README install section: placeholder owner/URLs replaced
- [ ] Release notes credit the pi / opencode / Claude Code lineages
- [ ] Post-release (deferred): Homebrew tap + scoop bucket via goreleaser pipes
