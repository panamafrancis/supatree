BINARY     := supatree
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The version lives in workbench's pkg/version, which supatree shares.
LDFLAGS    := -s -w -X github.com/panamafrancis/workbench/pkg/version.Version=$(VERSION)

.PHONY: build install test fmt vet lint ci clean e2e hooks

build:
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/supatree-linux-amd64  ./cmd/supatree
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/supatree-darwin-arm64 ./cmd/supatree

install:
	go install -ldflags="$(LDFLAGS)" ./cmd/supatree

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

lint:
	golangci-lint run ./...

ci: fmt lint vet test

hooks:
	git config core.hooksPath .githooks

e2e:
	go build -ldflags="$(LDFLAGS)" -o dist/supatree ./cmd/supatree
	PATH="$(CURDIR)/dist:$$PATH" bash scripts/e2e-supatree.sh

clean:
	rm -rf dist/
