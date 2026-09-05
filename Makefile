GO      ?= go
BINARY  ?= bin/lo
FULL    ?= bin/lo-full
# The version stamp reads the embedded mirror's VERSION (canonical since the
# eject model; .lok8s/VERSION is its synced twin — see hack/sync-legacy-assets.sh).
LDFLAGS ?= -s -w -X github.com/kernpilot/lok8s/internal/cli.version=$(shell cat internal/assets/lok8s/VERSION)

# Two builds from one tree (internal/render):
#   lo       (core, the default) — the render execs the pinned kustomize +
#            the b-managed exec plugins under .kustomize/ (lo init toolchain)
#   lo-full  (-tags inprocess)   — the kustomize API + khelm linked in; renders
#            in-process and serves both exec generators itself
FULL_TAGS := inprocess

.PHONY: build build-full test test-full vet vet-full lint lint-full clean release-check snapshot

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/lo

build-full:
	$(GO) build -trimpath -tags $(FULL_TAGS) -ldflags "$(LDFLAGS)" -o $(FULL) ./cmd/lo

test:
	$(GO) test ./...

test-full:
	$(GO) test -tags $(FULL_TAGS) ./...

vet:
	$(GO) vet ./...

vet-full:
	$(GO) vet -tags $(FULL_TAGS) ./...

lint:
	golangci-lint run

lint-full:
	golangci-lint run --build-tags $(FULL_TAGS)

clean:
	rm -rf bin/ dist/ .release/

# Release pipeline (.goreleaser.yaml). `release-check` validates the config;
# `snapshot` runs the whole pipeline locally without publishing, stamping
# .lok8s/VERSION (the tag stamps on CI). Needs goreleaser on PATH — see
# https://github.com/goreleaser/goreleaser/releases (v2.18.0 pinned in
# .github/workflows/release.yml).
release-check:
	goreleaser check

snapshot:
	LOK8S_VERSION=$$(cat internal/assets/lok8s/VERSION) goreleaser release --snapshot --clean
