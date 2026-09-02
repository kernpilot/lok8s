GO      ?= go
BINARY  ?= bin/lo
LDFLAGS ?= -s -w -X github.com/kernpilot/lok8s/internal/cli.version=$(shell cat .lok8s/VERSION)

.PHONY: build test vet lint clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/lo

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/
