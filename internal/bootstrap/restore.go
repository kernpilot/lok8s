package bootstrap

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
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
func (e *Engine) restoreD(ctx context.Context, domain, kubeconfig string) {
	stderr := e.stderr()
	rdir := e.Paths.Clusters + "/" + domain + "/restore.d"
	if !dirExists(rdir) {
		return
	}
	files, _ := filepath.Glob(rdir + "/*.sops.yaml")
	sort.Strings(files)
	applied, skipped := 0, 0
	for _, f := range files {
		// Nullglob-style guard; continue (not break) so one dangling entry
		// can't abort the remaining valid restores.
		if !fileExists(f) {
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
		if fileExists(plain) {
			errOut, rc := e.restoreApply(ctx, kubeconfig, "", plain)
			if rc == 0 {
				applied++
				ui.Debugf(stderr, "bootstrap: restore.d applied %s (store working copy)", base)
				continue
			}
			ui.Debugf(stderr, "bootstrap: restore.d %s store-copy apply failed: %s", base, firstLine(errOut))
		}
		// Decrypt and apply as SEPARATE steps with separate stderr: piping
		// `sops 2>&1 | kubectl` would inject sops warnings into kubectl's
		// stdin (corrupting the YAML). The Go port decrypts IN MEMORY via
		// the sops library — no pipe exists to get this wrong.
		dec, err := e.sopsDecrypt(f)
		if err != nil {
			skipped++
			ui.Debugf(stderr, "bootstrap: restore.d %s decrypt failed: %s", base, firstLine(err.Error()))
			ui.Warnf(stderr, "bootstrap: restore.d could not decrypt/apply %s — skipping (its owner will re-create/re-issue)", base)
			continue
		}
		errOut, rc := e.restoreApply(ctx, kubeconfig, string(dec)+"\n", "-")
		if rc == 0 {
			applied++
			ui.Debugf(stderr, "bootstrap: restore.d applied %s (sops)", base)
		} else {
			skipped++
			// First line only — kubectl errors can quote input fragments.
			ui.Debugf(stderr, "bootstrap: restore.d %s apply failed: %s", base, firstLine(errOut))
			ui.Warnf(stderr, "bootstrap: restore.d could not decrypt/apply %s — skipping (its owner will re-create/re-issue)", base)
		}
	}
	if applied != 0 || skipped != 0 {
		fmt.Fprintf(stderr, "\033[32m✓\033[0m bootstrap: restore.d — %d applied, %d skipped\n", applied, skipped)
	}
}

// restoreApply runs one server-side apply for restore.d, stdout discarded
// and stderr captured (bash: `_err="$(kubectl … 2>&1 >/dev/null)"`).
func (e *Engine) restoreApply(ctx context.Context, kubeconfig, stdin, file string) (string, int) {
	var errBuf strings.Builder
	c := execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", kubeconfig, "apply", "--server-side", "--force-conflicts", "-f", file},
		Stdout: io.Discard,
		Stderr: &errBuf,
	}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
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
