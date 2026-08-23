.PHONY: fmt test build build-cli clean run install

# Stamped into the binary via -X; short commit sha until the first tag exists.
# main.version is the linker symbol for package main's version var — it is
# module-path-independent, so the stamp survives the module flip unchanged.
VERSION ?= $(shell git describe --tags --always --dirty)

fmt:
	go fmt ./...

test:
	go test -race -count=1 ./...

build: build-cli

build-cli:
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o sandbar ./cmd/sandbar

clean:
	rm -f sandbar

run:
	go run ./cmd/sandbar

install: build-cli
	install -Dm755 sandbar $(HOME)/.local/bin/sandbar
