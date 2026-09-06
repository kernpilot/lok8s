package bootstrap

// restore_test.go — the Go port of the bootstrap::_restore_d bats block:
// the pre-bootstrap DR restore of sops-encrypted manifests. Decryption is
// IN MEMORY via the seam (never a sops|kubectl pipe — the round-1 bash
// regression this design forecloses entirely); kubectl runs through the
// fake runner.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestRestoreDNoDirIsSilentNoop(t *testing.T) {
	e, f, _, errOut, _ := testEngine(t)
	e.restoreD(context.Background(), "test.lok8s.dev", "/kc")
	if len(f.calls) != 0 {
		t.Errorf("kubectl called with no restore.d: %v", f.calls)
	}
	if errOut.Len() != 0 {
		t.Errorf("output on a no-op: %s", errOut.String())
	}
}

func TestRestoreDPlaintextStoreCopyWins(t *testing.T) {
	// Cross-domain-skew regression: a stale PATH_SECRETS (another domain's
	// store) must be IGNORED — the store resolves from the domain argument.
	e, f, _, errOut, p := testEngine(t)
	t.Setenv("PATH_SECRETS", filepath.Join(p.Clusters, "other.lok8s.dev", "secrets"))
	writeFile(t, filepath.Join(p.Clusters, "other.lok8s.dev", "secrets", "restore.d", "tls-a.yaml"), "kind: WrongDomainSecret\n")
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-a.sops.yaml"), "cipher\n")
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "secrets", "restore.d", "tls-a.yaml"), "kind: Secret\n")
	sopsCalled := false
	e.SopsDecrypt = func(path string) ([]byte, error) {
		sopsCalled = true
		return nil, fmt.Errorf("must not be called")
	}
	e.restoreD(context.Background(), "test.lok8s.dev", "/kc/kubeconfig")
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
	// Pin the args: right kubeconfig + THIS domain's store file.
	if !strings.Contains(f.log(), "--kubeconfig /kc/kubeconfig") {
		t.Errorf("kubeconfig not threaded:\n%s", f.log())
	}
	if !strings.Contains(f.log(), "test.lok8s.dev/secrets/restore.d/tls-a.yaml") {
		t.Errorf("store working copy not applied:\n%s", f.log())
	}
	if strings.Contains(f.log(), "other.lok8s.dev") {
		t.Errorf("stale PATH_SECRETS store used:\n%s", f.log())
	}
	if sopsCalled {
		t.Error("sops decrypt ran despite the plaintext working copy")
	}
}

func TestRestoreDSopsFallbackDecryptsInMemory(t *testing.T) {
	e, _, _, errOut, p := testEngine(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-b.sops.yaml"), "cipher\n")
	var decrypted string
	e.SopsDecrypt = func(path string) ([]byte, error) {
		decrypted = path
		return []byte("kind: Secret"), nil
	}
	var gotStdin, gotArgs string
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error {
		gotArgs = strings.Join(c.Args, " ")
		if c.Stdin != nil {
			raw, _ := io.ReadAll(c.Stdin)
			gotStdin = string(raw)
		}
		return nil
	})
	e.restoreD(context.Background(), "test.lok8s.dev", "/kc/kubeconfig")
	if !strings.HasSuffix(decrypted, "restore.d/tls-b.sops.yaml") {
		t.Errorf("decrypted wrong file: %q", decrypted)
	}
	if !strings.Contains(gotArgs, "--kubeconfig /kc/kubeconfig") || !strings.Contains(gotArgs, "-f -") {
		t.Errorf("apply args = %q", gotArgs)
	}
	// The plaintext reaches kubectl EXACTLY — no sops warnings, no pipe
	// contamination (the bash round-1 regression).
	if gotStdin != "kind: Secret\n" {
		t.Errorf("kubectl stdin = %q, want the pure payload", gotStdin)
	}
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
}

func TestRestoreDDecryptFailureWarnsAndContinues(t *testing.T) {
	e, _, _, errOut, p := testEngine(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-c.sops.yaml"), "cipher\n")
	e.SopsDecrypt = func(path string) ([]byte, error) { return nil, fmt.Errorf("no key") }
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error { return &rcError{1} })
	// Never fatal — DR must not wedge on a missing age key.
	e.restoreD(context.Background(), "test.lok8s.dev", "/kc")
	if !strings.Contains(errOut.String(), "could not decrypt/apply tls-c") {
		t.Errorf("missing warn: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "restore.d — 0 applied, 1 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
}

func TestRestoreDApplyFailureFallsThroughStoreCopyToSops(t *testing.T) {
	// A failing store-copy apply falls through to the sops path (bash: the
	// store-copy failure only debugs, then the .sops.yaml decrypt runs).
	e, _, _, errOut, p := testEngine(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-d.sops.yaml"), "cipher\n")
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "secrets", "restore.d", "tls-d.yaml"), "kind: Secret\n")
	e.SopsDecrypt = func(path string) ([]byte, error) { return []byte("kind: Secret"), nil }
	call := 0
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error {
		call++
		if call == 1 { // the store-copy apply fails
			fmt.Fprintln(c.Stderr, "error: connection refused")
			return &rcError{1}
		}
		return nil // the sops-path apply succeeds
	})
	e.restoreD(context.Background(), "test.lok8s.dev", "/kc")
	if call != 2 {
		t.Fatalf("kubectl calls = %d, want 2 (store copy, then sops)", call)
	}
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
}
