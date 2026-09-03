GO      ?= go
BINARY  ?= bin/lo
# The version stamp reads the embedded mirror's VERSION (canonical since the
# eject model; .lok8s/VERSION is its synced twin — see hack/sync-legacy-assets.sh).
LDFLAGS ?= -s -w -X github.com/kernpilot/lok8s/internal/cli.version=$(shell cat internal/assets/lok8s/VERSION)

.PHONY: build test vet lint clean release-check snapshot

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/lo

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

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
