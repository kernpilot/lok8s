package bootstrap

// isolation.go — the whole-project isolation boundary for `--dry-run`
// (review round 2, item A). A per-check isolation (one addon dir here, the
// secrets store there) cannot be patched into a read-only promise over
// arbitrary plugin execution: a cross-tree values reference, a helm cache
// write, a generator's own HOME lookup all escape a narrow copy. So a dry
// run copies the ENTIRE project into one disposable temp tree exactly once,
// then renders every entry from that copy with HOME/XDG_*/CAROOT/HELM_*/
// PATH_SECRETS redirected into it and KUBECONFIG (plus any cluster
// credential) unset — a plugin executed during the render has nowhere to
// write but the copy, and nothing to authenticate to.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
)

// dryRunSandbox is one dry run's isolation: a Paths rooted in the temp
// copy, plus the scoped-env restore and the temp-tree cleanup. Both must
// run at the end of the run — restore first (so nothing is left reading a
// dangling path for even an instant), then cleanup.
type dryRunSandbox struct {
	paths   *config.Paths
	restore func()
	cleanup func()
}

// newDryRunSandbox builds the isolation for one dry run of domain under
// orig. Everything under orig.Base is copied (symlinks resolved to their
// target's content when the target stays inside orig.Base, refused
// otherwise — see copyProjectTree); HOME and friends point at fresh empty
// dirs under the same temp root; PATH_SECRETS points at the COPY's
// clusters/<domain>/secrets, unconditionally — even when that directory
// does not exist yet, so a generator that creates one on first use creates
// it only in the disposable copy.
func newDryRunSandbox(orig *config.Paths, domainName string) (*dryRunSandbox, error) {
	root, err := os.MkdirTemp("", "lok8s-dryrun-")
	if err != nil {
		return nil, fmt.Errorf("dry-run sandbox: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()

	// .bin (the b-managed toolchain, often symlinked to a shared install
	// outside the project — PATH_BIN) is excluded from the copy, the same
	// as .git: it is read-only, never read by a render, and its symlinks
	// are EXPECTED to point outside the project, which the escaping-
	// symlink refusal below would otherwise (correctly, for anything
	// else) reject.
	projectCopy := filepath.Join(root, "project")
	if err := copyProjectTree(orig.Base, projectCopy, orig.Base, orig.Bin); err != nil {
		return nil, fmt.Errorf("dry-run sandbox: copy project: %w", err)
	}

	iso := &config.Paths{
		Base:          projectCopy,
		Bin:           orig.Bin, // the b-managed toolchain is read-only — no need to copy it
		Lok8s:         remapUnderBase(orig.Base, orig.Lok8s, projectCopy),
		Clusters:      remapUnderBase(orig.Base, orig.Clusters, projectCopy),
		SecretsEnv:    orig.SecretsEnv,
		SecretsEnvSet: orig.SecretsEnvSet,
	}

	home := filepath.Join(root, "home")
	dirs := []string{
		home,
		filepath.Join(home, ".config"), filepath.Join(home, ".config", "helm"),
		filepath.Join(home, ".local", "share"), filepath.Join(home, ".local", "share", "helm"),
		filepath.Join(home, ".cache"), filepath.Join(home, ".cache", "helm"),
		filepath.Join(home, ".caroot"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("dry-run sandbox: %w", err)
		}
	}

	var restores []func()
	set := func(k, v string) { restores = append(restores, setEnvScoped(k, v)) }
	set("HOME", home)
	set("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	set("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	set("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	set("CAROOT", filepath.Join(home, ".caroot"))
	set("HELM_CACHE_HOME", filepath.Join(home, ".cache", "helm"))
	set("HELM_CONFIG_HOME", filepath.Join(home, ".config", "helm"))
	set("HELM_DATA_HOME", filepath.Join(home, ".local", "share", "helm"))
	// ALWAYS, even when the real secrets dir does not exist yet (a fresh
	// domain, or one with no secrets store at all): a generator that mints
	// one on first use must mint it in the copy, never the project.
	set("PATH_SECRETS", filepath.Join(iso.Clusters, domainName, "secrets"))
	// Unset every cluster credential a plugin could reach the real cluster
	// with. The diff step still works: it gets the real kubeconfig through
	// an explicit --kubeconfig flag, never through $KUBECONFIG.
	for _, k := range clusterCredentialEnv() {
		set(k, "")
	}

	ok = true
	return &dryRunSandbox{
		paths: iso,
		restore: func() {
			for _, r := range slices.Backward(restores) {
				r()
			}
		},
		cleanup: func() { _ = os.RemoveAll(root) },
	}, nil
}

// clusterCredentialEnv names every environment variable a plugin could use
// to reach the real cluster: KUBECONFIG itself, the in-cluster
// KUBERNETES_SERVICE_* vars a Pod gets injected, and anything that looks
// like a bearer/API token (…_TOKEN, or the bare TOKEN some provider CLIs
// read) — review round 2, item A: "KUBECONFIG (and KUBERNETES_* / any
// token env)".
func clusterCredentialEnv() []string {
	names := []string{"KUBECONFIG"}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "KUBECONFIG" {
			continue
		}
		if strings.HasPrefix(k, "KUBERNETES_") || strings.HasSuffix(k, "_TOKEN") || k == "TOKEN" {
			names = append(names, k)
		}
	}
	return names
}

// remapUnderBase re-roots p under newBase when p lives inside origBase,
// else returns p unchanged — the embedded-asset case (Lok8s served from
// the binary's own cache, never the project) needs no copy; it is
// read-only either way.
func remapUnderBase(origBase, p, newBase string) string {
	rel, err := filepath.Rel(origBase, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join(newBase, rel)
}

// copyProjectTree copies src's content into dst, recursively. A regular
// file or directory is copied as itself. A symlink is copied AS ITS
// TARGET's content when the resolved target stays inside root (the whole
// point: a dry run must see a real file wherever the project has one,
// including a cross-directory symlink inside the SAME project); a symlink
// whose target resolves OUTSIDE root — most often an absolute path — is
// REFUSED, since that is exactly the shape of reference that could let a
// render escape the sandbox. `.git` is skipped: a render never reads it
// and it can be arbitrarily large. exclude, when non-empty, names one
// more absolute path to skip entirely (the b-managed toolchain dir,
// .bin — read-only, never read by a render, and its OWN symlinks are
// expected to point outside the project, which the escaping-symlink
// refusal below would otherwise reject for no benefit).
func copyProjectTree(src, dst, root, exclude string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, en := range entries {
		name := en.Name()
		if name == ".git" {
			continue
		}
		s := filepath.Join(src, name)
		if exclude != "" && s == exclude {
			continue
		}
		d := filepath.Join(dst, name)
		info, err := os.Lstat(s)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := resolveSymlinkInside(s, root)
			if err != nil {
				return err
			}
			rinfo, err := os.Stat(resolved)
			if err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", s, resolved, err)
			}
			if rinfo.IsDir() {
				if err := copyProjectTree(resolved, d, root, exclude); err != nil {
					return err
				}
			} else if err := copyPlainFile(resolved, d, rinfo.Mode().Perm()); err != nil {
				return err
			}
			continue
		}
		if info.IsDir() {
			if err := copyProjectTree(s, d, root, exclude); err != nil {
				return err
			}
			continue
		}
		if err := copyPlainFile(s, d, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// resolveSymlinkInside resolves link's target (relative to link's own
// directory when the target itself is relative) and refuses it when the
// result falls outside root.
func resolveSymlinkInside(link, root string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(link), resolved)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing symlink %s -> %s (escapes the project)", link, target)
	}
	return resolved, nil
}

func copyPlainFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}

// SetDryRunEnv scopes LOK8S_BOOTSTRAP_DRYRUN/LOK8S_BOOTSTRAP_ALLOW_GENERATORS
// for the caller's defer (review round 2, item D). A dispatcher calls this
// BEFORE driver.Exporter.Export so a dry run's Export never writes anything
// gated on the same sentinel (the Lo driver's .registries.json) — setting
// it only inside Engine.Apply, as the first round did, is too late: on both
// the standalone `lo bootstrap` and `lo provision --bootstrap` paths,
// Export already ran by the time Apply is reached.
func SetDryRunEnv(dryRun, allowGenerators bool) (restore func()) {
	r1 := setEnvScoped("LOK8S_BOOTSTRAP_DRYRUN", boolEnv(dryRun))
	r2 := setEnvScoped("LOK8S_BOOTSTRAP_ALLOW_GENERATORS", boolEnv(dryRun && allowGenerators))
	return func() { r2(); r1() }
}
