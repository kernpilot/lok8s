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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestRestoreDNoDirIsSilentNoop(t *testing.T) {
	e, f, _, errOut, _ := testEngine(t)
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
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
	testutil.WriteFile(t, filepath.Join(p.Clusters, "other.lok8s.dev", "secrets", "restore.d", "tls-a.yaml"), "kind: WrongDomainSecret\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-a.sops.yaml"), "cipher\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "secrets", "restore.d", "tls-a.yaml"), "kind: Secret\n")
	sopsCalled := false
	e.SopsDecrypt = func(path string) ([]byte, error) {
		sopsCalled = true
		return nil, fmt.Errorf("must not be called")
	}
	var gotStdin string
	f.handler = func(c execx.Cmd) error {
		raw, _ := io.ReadAll(c.Stdin)
		gotStdin = string(raw)
		return nil
	}
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc/kubeconfig")
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
	// Pin the args: right kubeconfig, and THIS domain's working copy on
	// stdin: lo applies the copy that it checked, not the file.
	if !strings.Contains(f.log(), "--kubeconfig /kc/kubeconfig") || !strings.Contains(f.log(), "-f -") {
		t.Errorf("apply args:\n%s", f.log())
	}
	if gotStdin != "kind: Secret\n" {
		t.Errorf("kubectl stdin = %q, want this domain's working copy", gotStdin)
	}
	if sopsCalled {
		t.Error("sops decrypt ran despite the plaintext working copy")
	}
}

func TestRestoreDSopsFallbackDecryptsInMemory(t *testing.T) {
	e, _, _, errOut, p := testEngine(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-b.sops.yaml"), "cipher\n")
	var decrypted string
	e.SopsDecrypt = func(path string) ([]byte, error) {
		decrypted = path
		return []byte("kind: Secret\n"), nil
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
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc/kubeconfig")
	if !strings.HasSuffix(decrypted, "restore.d/tls-b.sops.yaml") {
		t.Errorf("decrypted wrong file: %q", decrypted)
	}
	if !strings.Contains(gotArgs, "--kubeconfig /kc/kubeconfig") || !strings.Contains(gotArgs, "-f -") {
		t.Errorf("apply args = %q", gotArgs)
	}
	// The plaintext reaches kubectl EXACTLY — no sops warnings, no pipe
	// contamination (the bash round-1 regression), no added newline.
	if gotStdin != "kind: Secret\n" {
		t.Errorf("kubectl stdin = %q, want the pure payload", gotStdin)
	}
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
}

func TestRestoreDDecryptFailureWarnsAndContinues(t *testing.T) {
	e, _, _, errOut, p := testEngine(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-c.sops.yaml"), "cipher\n")
	e.SopsDecrypt = func(path string) ([]byte, error) { return nil, fmt.Errorf("no key") }
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error { return &rcError{1} })
	// Never fatal — DR must not wedge on a missing age key.
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
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
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-d.sops.yaml"), "cipher\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "secrets", "restore.d", "tls-d.yaml"), "kind: Secret\n")
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
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
	if call != 2 {
		t.Fatalf("kubectl calls = %d, want 2 (store copy, then sops)", call)
	}
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("missing summary: %s", errOut.String())
	}
}

// restoreMarked is a Secret from lo build --keys-only: its key names with
// empty values, and the keys-only marker.
const restoreMarked = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls\n  annotations:\n    secrets.lok8s.dev/keys-only: \"true\"\ndata:\n  tls.key: \"\"\n"

// bats: "restore_d: a store working copy with a keys-only Secret is not
// applied and does not fall back to sops". lo applies nothing for that
// entry and does not decrypt its .sops.yaml. The next entry still restores.
func TestRestoreDRefusesAMarkedWorkingCopyWithoutAFallback(t *testing.T) {
	e, f, _, errOut, p := testEngine(t)
	d := filepath.Join(p.Clusters, "test.lok8s.dev")
	testutil.WriteFile(t, filepath.Join(d, "restore.d", "tls-a.sops.yaml"), "cipher\n")
	testutil.WriteFile(t, filepath.Join(d, "restore.d", "tls-b.sops.yaml"), "cipher\n")
	testutil.WriteFile(t, filepath.Join(d, "secrets", "restore.d", "tls-a.yaml"), restoreMarked)
	var decrypted []string
	e.SopsDecrypt = func(path string) ([]byte, error) {
		decrypted = append(decrypted, filepath.Base(path))
		return []byte("kind: Secret\n"), nil
	}
	var applied []string
	f.handler = func(c execx.Cmd) error {
		raw, _ := io.ReadAll(c.Stdin)
		applied = append(applied, string(raw))
		return nil
	}
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
	if len(decrypted) != 1 || decrypted[0] != "tls-b.sops.yaml" {
		t.Errorf("decrypted %v, want only tls-b.sops.yaml", decrypted)
	}
	if len(applied) != 1 || applied[0] != "kind: Secret\n" {
		t.Errorf("applied %q, want only the manifest of tls-b", applied)
	}
	for _, want := range []string{
		"[error] bootstrap: restore.d: refusing to apply tls-a (store working copy): it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one\n",
		"lo does not fall back to tls-a.sops.yaml after it finds a keys-only Secret in the store working copy",
		"restore.d — 1 applied, 1 skipped",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr misses %q:\n%s", want, errOut.String())
		}
	}
}

// bats: "restore_d: a store working copy that cannot be checked falls
// back to sops". A copy that cannot be read or does not parse is not
// applied; lo uses the .sops.yaml file, as after a failed apply, and checks
// the decrypted manifest. The decrypted manifest is applied.
func TestRestoreDFallsBackFromAWorkingCopyThatCannotBeChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		copy, want string
		unreadable bool
	}{
		"not YAML":   {copy: "kind: [Secret\n", want: "[warn] bootstrap: restore.d: tls-a (store working copy) does not parse as YAML; lo uses tls-a.sops.yaml\n"},
		"unreadable": {copy: "kind: Secret\n", unreadable: true, want: "[warn] bootstrap: restore.d: tls-a (store working copy) cannot be read; lo uses tls-a.sops.yaml\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.unreadable && os.Geteuid() == 0 {
				t.Skip("root reads a file without the read permission")
			}
			e, f, _, errOut, p := testEngine(t)
			d := filepath.Join(p.Clusters, "test.lok8s.dev")
			testutil.WriteFile(t, filepath.Join(d, "restore.d", "tls-a.sops.yaml"), "cipher\n")
			plain := filepath.Join(d, "secrets", "restore.d", "tls-a.yaml")
			testutil.WriteFile(t, plain, tc.copy)
			if tc.unreadable {
				if err := os.Chmod(plain, 0); err != nil {
					t.Fatal(err)
				}
			}
			var decrypted []string
			e.SopsDecrypt = func(path string) ([]byte, error) {
				decrypted = append(decrypted, filepath.Base(path))
				return []byte("kind: Secret\nmetadata:\n  name: from-sops\n"), nil
			}
			var applied []string
			f.handler = func(c execx.Cmd) error {
				raw, _ := io.ReadAll(c.Stdin)
				applied = append(applied, string(raw))
				return nil
			}
			e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
			if len(decrypted) != 1 || decrypted[0] != "tls-a.sops.yaml" {
				t.Errorf("decrypted %v, want tls-a.sops.yaml", decrypted)
			}
			if len(applied) != 1 || applied[0] != "kind: Secret\nmetadata:\n  name: from-sops\n" {
				t.Errorf("applied %q, want only the decrypted manifest", applied)
			}
			for _, want := range []string{tc.want, "restore.d — 1 applied, 0 skipped"} {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("stderr misses %q:\n%s", want, errOut.String())
				}
			}
			if strings.Contains(errOut.String(), "refusing to apply") {
				t.Errorf("a copy that falls back is refused:\n%s", errOut.String())
			}
		})
	}
}

// bats: "restore_d: kubectl gets the exact bytes of the working copy and
// of the decrypted manifest". The last literal scalar (|+) of each ends in
// several newlines; kubectl gets each byte, and no newline is added. The
// decrypted manifest differs from the working copy, so a restore that used
// the decrypted manifest in place of the working copy fails.
func TestRestoreDAppliesTheExactBytes(t *testing.T) {
	const copyDoc = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls\nstringData:\n  tls.crt: |+\n    cert\n\n\n"
	const sopsDoc = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls-from-sops\nstringData:\n  tls.crt: |+\n    sops-cert\n\n"
	for _, route := range []string{"store working copy", "sops"} {
		t.Run(route, func(t *testing.T) {
			e, f, _, errOut, p := testEngine(t)
			d := filepath.Join(p.Clusters, "test.lok8s.dev")
			testutil.WriteFile(t, filepath.Join(d, "restore.d", "tls.sops.yaml"), "cipher\n")
			doc := sopsDoc
			if route == "store working copy" {
				testutil.WriteFile(t, filepath.Join(d, "secrets", "restore.d", "tls.yaml"), copyDoc)
				doc = copyDoc
			}
			e.SopsDecrypt = func(path string) ([]byte, error) { return []byte(sopsDoc), nil }
			var applied []string
			f.handler = func(c execx.Cmd) error {
				raw, _ := io.ReadAll(c.Stdin)
				applied = append(applied, string(raw))
				return nil
			}
			e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
			if len(applied) != 1 || applied[0] != doc {
				t.Errorf("kubectl stdin = %q, want %q", applied, doc)
			}
			if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
				t.Errorf("stderr misses the summary:\n%s", errOut.String())
			}
		})
	}
}

// bats: "restore_d: a decrypted manifest with a keys-only Secret is not
// applied". Without a working copy, the guard checks the decrypted
// manifest: one with a keys-only Secret, or one that does not parse, is
// skipped.
func TestRestoreDRefusesTheDecryptedManifest(t *testing.T) {
	for name, tc := range map[string]struct{ dec, want string }{
		"keys-only Secret": {restoreMarked, "[error] bootstrap: restore.d: refusing to apply tls-a (sops): it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one\n"},
		"not YAML":         {"kind: [Secret", "[error] bootstrap: restore.d: refusing to apply tls-a (sops): it does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)\n"},
	} {
		t.Run(name, func(t *testing.T) {
			e, f, _, errOut, p := testEngine(t)
			testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-a.sops.yaml"), "cipher\n")
			e.SopsDecrypt = func(path string) ([]byte, error) { return []byte(tc.dec), nil }
			e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
			if len(f.calls) != 0 {
				t.Errorf("kubectl ran for a refused manifest:\n%s", f.log())
			}
			for _, want := range []string{tc.want, "restore.d — 0 applied, 1 skipped"} {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("stderr misses %q:\n%s", want, errOut.String())
				}
			}
		})
	}
}
