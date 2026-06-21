# lok8s kustomize plugins — Go build
#
# Builds exec generator plugins to ../.kustomize/<group>/<version>/<kind>/<Kind>
# (the layout kustomize expects under KUSTOMIZE_PLUGIN_HOME).

MODULE      := github.com/kernpilot/lok8s/kustomize
# Overridable so `lo plugins build` can install into a project's plugin home
# (KUSTOMIZE_PLUGIN_HOME=<project>/.kustomize) instead of lok8s/.kustomize.
BIN_ROOT    ?= ../.kustomize
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

# Prefer the goenv-managed `go` binary if a usable one exists. The system
# `go` shim on this dev box is a stale 1.22.0 wrapper pointing at a 1.26.0
# stdlib, which produces "version mismatch" errors during compilation. We
# pick the highest installed goenv version that's >= 1.22.
# Pick the highest real `go` from goenv OR mise installs, falling back to a
# `go` on PATH. (The bare PATH `go`/shim is often a stale wrapper or an unset
# mise shim — "No version is set for shim: go" — that fails to compile.)
GO := $(shell \
  for d in $(HOME)/.goenv/versions $(HOME)/.local/share/mise/installs/go; do \
    [ -d $$d ] && ls -d $$d/*/bin/go 2>/dev/null; \
  done | sort -V | tail -1)
GO := $(if $(GO),$(GO),$(shell command -v go))
GO := $(if $(GO),$(GO),go)

# --- Plugin: secrets.lok8s.dev/v1/Secret ---
SECRET_PLUGIN_PATH := $(BIN_ROOT)/secrets.lok8s.dev/v1/secret
SECRET_BIN         := $(SECRET_PLUGIN_PATH)/Secret

# certgen: a standalone CLI (NOT a kustomize plugin) the Lo driver uses to mint
# dev TLS without the mkcert binary. Installed beside the plugins, under bin/.
CERTGEN_BIN := $(BIN_ROOT)/bin/certgen

# All built binaries (extend when adding more cmd/<name>)
BINS := $(SECRET_BIN) $(CERTGEN_BIN)

GO_FILES := $(shell find . -name '*.go' -not -path './testdata/*')

.PHONY: all build install test lint tidy clean dist help

all: build  ## build all plugins (default)

build: $(BINS)  ## build all plugin binaries

$(SECRET_BIN): $(GO_FILES) go.mod
	@mkdir -p $(SECRET_PLUGIN_PATH)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/secret
	@echo "  built $@"

$(CERTGEN_BIN): $(GO_FILES) go.mod
	@mkdir -p $(dir $@)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/certgen
	@echo "  built $@"

install: build  ## alias for build (no separate install step needed)

test:  ## run all unit + integration tests
	$(GO) test -race -count=1 ./...

test-cover:  ## run tests with coverage report
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -1

lint:  ## run golangci-lint
	golangci-lint run

tidy:  ## update go.mod / go.sum
	$(GO) mod tidy

clean:  ## remove built binaries
	rm -f $(BINS)
	rm -rf dist/

# Cross-platform release builds (not needed for local dev)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
dist:  ## build cross-platform release tarballs
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  building $$os/$$arch..."; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
	      -o dist/Secret-$$os-$$arch ./cmd/secret ; \
	done

help:  ## show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
