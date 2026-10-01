GO ?= go

.PHONY: all build test lint fmt
all: lint test build

build:
	$(GO) build -o bin/sourced-resolver ./cmd/sourced-resolver

test:
	$(GO) test -race ./...

# gofmt and go vet. staticcheck isn't downloaded yet: ask before adding it.
lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
	$(GO) vet ./...

fmt:
	gofmt -w .
