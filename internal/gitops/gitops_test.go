package gitops

// gitops_test.go ports tests/unit/gitops_test.bats: the stub behaves —
// deferred commands emit a clear error, bootstrap is a no-op.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kernpilot/lok8s/internal/provision"
)

// Compile-time pin: BootstrapHook IS the dispatch tail's gitops seam.
var _ = provision.Hooks{GitopsBootstrap: BootstrapHook(io.Discard)}

// bats: "gitops::flux is deferred and returns error"
func TestFluxDeferred(t *testing.T) {
	var errBuf bytes.Buffer
	if err := Flux(&errBuf); !errors.Is(err, ErrDeferred) {
		t.Fatalf("err = %v", err)
	}
	if want := "\033[0;31m[error]\033[0m lo gitops flux is deferred (redesign after services.yaml targets lands)\n"; errBuf.String() != want {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// bats: "gitops::argo is deferred and returns error"
func TestArgoDeferred(t *testing.T) {
	var errBuf bytes.Buffer
	if err := Argo(&errBuf); !errors.Is(err, ErrDeferred) {
		t.Fatalf("err = %v", err)
	}
	if want := "\033[0;31m[error]\033[0m lo gitops argo is deferred (redesign after services.yaml targets lands)\n"; errBuf.String() != want {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// bats: "gitops::bootstrap is a no-op stub" (+ the hook shape the dispatch calls)
func TestBootstrapNoOp(t *testing.T) {
	var errBuf bytes.Buffer
	if err := BootstrapHook(&errBuf)(context.Background(), "test.lok8s.dev", "flux"); err != nil {
		t.Fatal(err)
	}
	if want := "\033[0;33m[warn]\033[0m lo gitops is being redesigned post-refactor; no-op for now\n"; errBuf.String() != want {
		t.Errorf("stderr = %q", errBuf.String())
	}
}
