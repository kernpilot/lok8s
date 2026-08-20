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

# Pick a `go` that PROVABLY compiles. A candidate can lie: a goenv/mise
# install can print a version yet fail every build with "compile: version X
# does not match go tool version Y" (stale or partial toolchain), and an
# unset mise/goenv PATH shim errors with "No version is set for shim: go".
# Version-sorting candidates cannot see either failure, so the picker
# VALIDATES instead: each candidate must compile a trivial CGO_ENABLED=0
# program under this module's `go` directive — the same toolchain resolution
# the real build performs. First working candidate wins; the PATH `go` (what
# the developer's shell uses) is tried first, then goenv/mise installs,
# newest first. Override with `make build GO=…` — a make ARGUMENT; an
# environment prefix does not beat `GO :=`.
GO := $(shell \
  tmp=$$(mktemp -d); \
  { echo 'module probe'; grep '^go ' go.mod; } > $$tmp/go.mod; \
  printf 'package main\n\nfunc main() {}\n' > $$tmp/main.go; \
  { command -v go; \
    for d in $(HOME)/.goenv/versions $(HOME)/.local/share/mise/installs/go; do \
      [ -d $$d ] && ls -d $$d/*/bin/go 2>/dev/null | sort -rV; \
    done; } | while read -r g; do \
      [ -x "$$g" ] || continue; \
      if ( cd $$tmp && CGO_ENABLED=0 GOFLAGS= "$$g" build -o /dev/null . ) >/dev/null 2>&1; then \
        echo "$$g"; break; \
      fi; \
    done; \
  rm -rf $$tmp)
GO := $(if $(GO),$(GO),go)

# --- Plugin: secrets.lok8s.dev/v1/Secret ---
SECRET_PLUGIN_PATH := $(BIN_ROOT)/secrets.lok8s.dev/v1/secret
SECRET_BIN         := $(SECRET_PLUGIN_PATH)/Secret

# All plugin binaries (extend when adding more cmd/<name>)
BINS := $(SECRET_BIN)

GO_FILES := $(shell find . -name '*.go' -not -path './testdata/*')

.PHONY: all build install test lint tidy clean dist help

all: build  ## build all plugins (default)

build: $(BINS)  ## build all plugin binaries

$(SECRET_BIN): $(GO_FILES) go.mod
	@mkdir -p $(SECRET_PLUGIN_PATH)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/secret
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

# Cross-platform release builds (not needed for local dev).
# Asset naming is a CONTRACT: kustomize-secret-<os>-<arch> is what release
# consumers address via `b` (asset: "kustomize-secret-*"), so the release
# workflow attaches these files verbatim. Don't rename without bumping the
# consumers (kubehz-cluster/.bin/b.yaml + b.ci.yaml).
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
dist:  ## build cross-platform release binaries (kustomize-secret-<os>-<arch>)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  building $$os/$$arch..."; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
	      -o dist/kustomize-secret-$$os-$$arch ./cmd/secret ; \
	done

help:  ## show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
