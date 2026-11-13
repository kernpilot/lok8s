package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/secrets"
	"github.com/kernpilot/lok8s/internal/ui"
)

// restoreD applies clusters/<domain>/restore.d/*.sops.yaml (sops-encrypted
// Kubernetes manifests) before the bootstrap DAG (bash:
// bootstrap::_restore_d). See the call site in Apply for the rationale (DR
// restore, e.g. issued TLS Secrets so cert-manager reuses instead of
// re-issuing). Per-file failures WARN and continue — DR must not wedge on a
// missing age key; the fallback (the owner controller re-creates /
// re-issues) is the pre-restore.d behavior. Never fails the run.
//
// Each input passes the keys-only apply guard before its apply: lo reads
// the working copy once and applies the bytes that it checked, and checks
// the decrypted manifest (build.HasKeysOnlyMarker,
// build.RefuseKeysOnlyStream). A working copy with a keys-only Secret is
// skipped, and lo does not fall back to the .sops.yaml file: a keys-only
// Secret in the store stops the restore of that entry, and the operator
// sees why. A working copy that cannot be read or does not parse falls
// back to the .sops.yaml file, as a failed apply does, because lo checks
// the decrypted manifest too. kubectl gets the bytes exactly.
func (e *Engine) restoreD(ctx context.Context, domain, kubeconfig string) {
	stderr := e.stderr()
	rdir := e.Paths.Clusters + "/" + domain + "/restore.d"
	if !fsutil.DirExists(rdir) {
		return
	}
	files, _ := filepath.Glob(rdir + "/*.sops.yaml")
	sort.Strings(files)
	applied, skipped := 0, 0
	for _, f := range files {
		// Nullglob-style guard; continue (not break) so one dangling entry
		// can't abort the remaining valid restores.
		if !fsutil.FileExists(f) {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(f), ".sops.yaml")
		// Plaintext-first: the per-domain secret store's working copy
		// (secrets/restore.d/<name>.yaml — gitignored, present once the
		// operator has run `lo secrets decrypt`) needs no age identity, so
		// restore works from an agent/CI sandbox where the SSH-derived key
		// isn't available. The committed .sops.yaml is the fresh-checkout
		// fallback. Resolve the store from THIS domain — not $PATH_SECRETS,
		// which may still point at a different domain's store from an
		// earlier build.
		plain := e.Paths.Clusters + "/" + domain + "/secrets/restore.d/" + base + ".yaml"
		if fsutil.FileExists(plain) {
			what := base + " (store working copy)"
			doc, rerr := os.ReadFile(plain)
			var marked bool
			var perr error
			if rerr == nil {
				marked, perr = build.HasKeysOnlyMarker(doc)
			}
			switch {
			case rerr != nil:
				ui.WarnTo(stderr, "%s: %s cannot be read; lo uses %s.sops.yaml", restoreGuard, what, base)
			case perr != nil:
				ui.WarnTo(stderr, "%s: %s does not parse as YAML; lo uses %s.sops.yaml", restoreGuard, what, base)
			case marked:
				// The message of the guard (the stream holds the marker).
				_ = build.RefuseKeysOnlyStream(stderr, restoreGuard, what, doc)
				skipped++
				ui.WarnTo(stderr, "bootstrap: restore.d skips %s — lo does not fall back to %s.sops.yaml after it finds a keys-only Secret in the store working copy", base, base)
				continue
			default:
				errOut, rc := e.restoreApply(ctx, kubeconfig, doc)
				if rc == 0 {
					applied++
					ui.DebugTo(stderr, "bootstrap: restore.d applied %s (store working copy)", base)
					continue
				}
				ui.DebugTo(stderr, "bootstrap: restore.d %s store-copy apply failed: %s", base, firstLine(errOut))
			}
		}
		// Decrypt and apply as SEPARATE steps with separate stderr: piping
		// `sops 2>&1 | kubectl` would inject sops warnings into kubectl's
		// stdin (corrupting the YAML). The Go port decrypts IN MEMORY via
		// the sops library — no pipe exists to get this wrong.
		dec, err := e.sopsDecrypt(f)
		if err != nil {
			skipped++
			ui.DebugTo(stderr, "bootstrap: restore.d %s decrypt failed: %s", base, firstLine(err.Error()))
			ui.WarnTo(stderr, "bootstrap: restore.d could not decrypt/apply %s — skipping (its owner will re-create/re-issue)", base)
			continue
		}
		if build.RefuseKeysOnlyStream(stderr, restoreGuard, base+" (sops)", dec) != nil {
			skipped++
			continue
		}
		errOut, rc := e.restoreApply(ctx, kubeconfig, dec)
		if rc == 0 {
			applied++
			ui.DebugTo(stderr, "bootstrap: restore.d applied %s (sops)", base)
		} else {
			skipped++
			// First line only — kubectl errors can quote input fragments.
			ui.DebugTo(stderr, "bootstrap: restore.d %s apply failed: %s", base, firstLine(errOut))
			ui.WarnTo(stderr, "bootstrap: restore.d could not decrypt/apply %s — skipping (its owner will re-create/re-issue)", base)
		}
	}
	if applied != 0 || skipped != 0 {
		fmt.Fprintf(stderr, "\033[32m✓\033[0m bootstrap: restore.d — %d applied, %d skipped\n", applied, skipped)
	}
}

// restoreGuard is the prefix of the keys-only guard's messages in
// restore.d.
const restoreGuard = "bootstrap: restore.d"

// restoreApply runs one server-side apply for restore.d of the checked
// manifest on stdin, byte for byte, stdout discarded and stderr captured
// (bash: `_err="$(printf '%s' "${doc}" | kubectl … -f - 2>&1 >/dev/null)"`).
func (e *Engine) restoreApply(ctx context.Context, kubeconfig string, doc []byte) (string, int) {
	var errBuf strings.Builder
	c := execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", kubeconfig, "apply", "--server-side", "--force-conflicts", "-f", "-"},
		Stdin:  bytes.NewReader(doc),
		Stdout: io.Discard,
		Stderr: &errBuf,
	}
	err := e.Runner.Run(ctx, c)
	rc := 0
	if err != nil {
		rc = 1
	}
	return strings.TrimRight(errBuf.String(), "\n"), rc
}

// sopsDecrypt resolves the decrypt seam. PATH_SECRETS note: Apply exports
// the per-domain store before rendering; restoreD deliberately ignores it
// (cross-domain-skew regression — the store resolves from the domain
// argument).
func (e *Engine) sopsDecrypt(path string) ([]byte, error) {
	if e.SopsDecrypt != nil {
		return e.SopsDecrypt(path)
	}
	return secrets.DecryptYAMLFile(path)
}
