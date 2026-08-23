# Contributing to Sandbar

Thanks for taking the time.

## Build and test

```bash
make fmt          # go fmt ./...
make test         # go test -race -count=1 -skip TestFullTuiPipeline ./...
make build        # CGO_ENABLED=0 static build
go vet ./...
```

Go 1.25+ is required. CI runs vet, a gofmt check (`gofmt -l .` must be
empty), the race-enabled test suite, a cross-compile matrix
(linux/darwin/windows/freebsd × amd64/arm64), and a secret scan on every
push and pull request.

## Pull requests

- **Tests for behavior changes.** If you change how something behaves, add or
  adjust a test that fails without the change and passes with it.
- **Keep the diff minimal.** No drive-by reformatting, renames, or unrelated
  cleanups — mix those into their own PRs.
- **Code style:** match the surrounding code. Comment density, naming, and
  structure should look like the file you are editing.
- Run `make fmt` and `go vet ./...` before pushing; CI will reject otherwise.

## Issues

- One topic per issue, with a reproducer when applicable (version, platform,
  config snippet with secrets removed, expected vs. actual).
- Security issues do **not** go in issues — see `SECURITY.md`.
