// Package gitops is the Go port of .lok8s/libs/gitops — DEFERRED
// (post-refactor redesign), exactly like the bash.
//
// The previous implementation emitted Flux Kustomization CRs with dependsOn
// chains and Argo sync-wave annotations, driven by spec.syncWave. That field
// was removed as part of the bootstrap/targets refactor; gitops output will
// be redesigned to work from the new services.yaml + targets model and
// re-enabled in a follow-up. Until then the module exists so `lo gitops`
// emits a clear error instead of a "command not found", and the dispatch
// tail's gitops::bootstrap stays a warn-only no-op. No exec happens
// anywhere in this package (nothing to capture via execx).
package gitops

import (
	"context"
	"errors"
	"io"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrDeferred is the non-zero exit of the deferred subcommands (bash:
// `error …; return 1`) — the message was already printed.
var ErrDeferred = errors.New("gitops: deferred")

// Bootstrap is gitops::bootstrap, the provision-dispatch tail hook called
// when spec.gitops.provider is set: a warn-only no-op that always succeeds.
func Bootstrap(stderr io.Writer, domain, provider string) error {
	ui.Warnf(stderr, "lo gitops is being redesigned post-refactor; no-op for now")
	return nil
}

// BootstrapHook returns Bootstrap in the shape of
// provision.Hooks.GitopsBootstrap.
func BootstrapHook(stderr io.Writer) func(ctx context.Context, domain, provider string) error {
	return func(ctx context.Context, domain, provider string) error {
		return Bootstrap(stderr, domain, provider)
	}
}

// Flux is gitops::flux (deferred): prints the error, returns ErrDeferred.
func Flux(stderr io.Writer) error {
	ui.Errorf(stderr, "lo gitops flux is deferred (redesign after services.yaml targets lands)")
	return ErrDeferred
}

// Argo is gitops::argo (deferred): prints the error, returns ErrDeferred.
func Argo(stderr io.Writer) error {
	ui.Errorf(stderr, "lo gitops argo is deferred (redesign after services.yaml targets lands)")
	return ErrDeferred
}
