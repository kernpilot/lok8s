package build

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks a build failure whose message was already printed in the
// bash implementation's own [error] format; callers exit non-zero without
// printing anything further.
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// Options carries one build invocation's inputs.
type Options struct {
	Paths  *config.Paths
	Domain string
	// SplitOverride is the LOK8S_BUILD_SPLIT debug override: "" lets the
	// spec decide, "1" forces the split emit, "0" skips it.
	SplitOverride string
	// NoSecrets is the effective --no-secrets/LOK8S_BUILD_NO_SECRETS bit
	// (see NoSecretsEffective): the CI render path — the render never
	// touches the secrets store and the split leaves committed
	// Secret.*.sops.yaml wholly inert.
	NoSecrets bool
	// Stderr receives the human-facing stream (banner, [error]/[warn]/
	// [debug] lines). Defaults to os.Stderr.
	Stderr io.Writer
}

func (o *Options) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

// Artifacts builds the domain's composed artifact via kustomize alpha
// plugins (bash: build::artifacts).
//
// Renders the DOMAIN kustomization (clusters/<domain>/kustomization.yaml) —
// which composes the domain's targets (resources: [./targets/foo,
// ../../.targets/bar]; local + shared) — into ONE file
// clusters/<domain>/artifacts.yaml.
//
// Same pipeline the CLI has always used — `kustomize build
// --enable-alpha-plugins` with the secrets/khelm exec generators, now run
// in-process by internal/render (LO_RENDER=exec is the binary pipeline) —
// with KHELM_TRUST_ANY_REPO=true,
// per-domain secret isolation (exportSecretsPath), API-endpoint resolution
// (resolveAPI), kubeconfig resolution for cluster-aware plugins (the khelm
// ChartRenderer kubeVersion check), and the LOK8S_* envsubst pass.
//
// There is no per-target loop and no artifacts/<target>/ output: target
// ordering and selection live in the domain kustomization the user authors.
func Artifacts(o Options) error {
	stderr := o.stderr()
	domainDir := filepath.Join(o.Paths.Clusters, o.Domain)

	ui.Debugf(stderr, "Build artifacts via kustomize alpha plugins for %s", o.Domain)

	// The domain composes its targets in its own kustomization.yaml. Without
	// it there is nothing to build — fail clearly with the fix.
	if !hasKustomization(domainDir) {
		ui.Errorf(stderr, "domain %s has no kustomization.yaml — compose its targets there, e.g. resources: [./targets/foo, ../../.targets/bar]", o.Domain)
		return ErrHandled
	}

	// Per-domain secret isolation + API endpoint exports (LOK8S_USER_API_*),
	// so ${LOK8S_USER_*}/${LOK8S_SPEC_*} in target manifests resolve at
	// envsubst time. Exported into the process env so they reach both the
	// kustomize child and the whitelist below.
	exportSecretsPath(domainDir)
	resolveAPI(o.Paths, domainDir)

	// Resolve kubeconfig (some targets need it for cluster-aware kustomize
	// plugins like the khelm ChartRenderer kubeVersion check).
	kubeconfig := renderKubeconfig(o.Paths, o.Domain)

	// envsubst whitelist: rebuilt on every call so new LOK8S_SPEC_* vars in
	// lo::export_spec_envs are automatically picked up.
	whitelist := EnvsubstWhitelist()

	// Clean stale per-target artifact dirs from the pre-domain-build era so
	// `lo status`/hooks don't read phantom outputs. Only framework-generated
	// subdirs (each held an artifacts.yaml) are removed; env's overlay
	// kustomization.yaml, capi.yaml and .cache-queue live as files directly
	// under artifacts/ and are preserved.
	pruneStaleArtifactDirs(filepath.Join(domainDir, "artifacts"))

	ui.Debugf(stderr, "Building domain kustomization: %s", filepath.Join(domainDir, "kustomization.yaml"))

	// Render to a temp file first, then promote on success. A direct write
	// to artifacts.yaml would truncate the target BEFORE the render runs, so
	// a kustomize failure would leave a truncated/empty artifact and clobber
	// a prior good one. Only a fully successful build is moved into place; a
	// failure removes the temp and leaves the old artifact. The temp lives
	// IN domain_dir: same filesystem as the target, so the rename below is
	// atomic (a cross-fs move is copy+unlink — non-atomic, and an
	// interrupted one can leave a partial artifacts.yaml).
	tmp, err := os.CreateTemp(domainDir, "tmp.")
	if err != nil {
		ui.Errorf(stderr, "kustomize build failed for %s", o.Domain)
		return ErrHandled
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()

	rendered, err := runKustomize(context.Background(), o, domainDir, kubeconfig, stderr)
	if err != nil {
		_ = os.Remove(tmpPath)
		ui.Errorf(stderr, "kustomize build failed for %s", o.Domain)
		return ErrHandled
	}
	rendered = Envsubst(rendered, whitelist)
	if err := os.WriteFile(tmpPath, rendered, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		ui.Errorf(stderr, "kustomize build failed for %s", o.Domain)
		return ErrHandled
	}

	// A render that produced NOTHING must not replace a good artifact.
	//
	// kustomize exits 0 for an empty result — `resources: []`, a target list
	// that lost its entries, a base that stopped resolving — so the render
	// above succeeds and the atomic promote below would install a 0-byte
	// artifacts.yaml over the previous good one (measured: an emptied
	// `resources:` gives rc=0, 0 documents, 0 bytes, no other output). That
	// is not a failure anywhere it can be noticed: on the prod path
	// render.yml COMMITS the artifacts and Flux applies them with prune:
	// true, so an empty render deletes every resource it used to manage.
	//
	// Refuse only where it would DESTROY something. An empty render over an
	// empty/absent artifact is a new or genuinely empty domain — warn, but
	// nothing is lost.
	artifactPath := filepath.Join(domainDir, "artifacts.yaml")
	docs := countKindLines(rendered)
	prior := 0
	existing, existingErr := os.ReadFile(artifactPath)
	if existingErr == nil {
		prior = countKindLines(existing)
	}
	// A SPLIT-mode domain keeps its committed state in artifacts/, not in
	// artifacts.yaml — so a domain whose artifacts.yaml is absent or already
	// empty can still have a full split layout to lose, and counting only
	// the single file would wave the empty render through. Counted with the
	// same uppercase-Kind ownership rule the swap prunes by, so the two
	// agree about what "generated" means.
	outDir := filepath.Join(domainDir, "artifacts")
	if prior == 0 {
		if info, err := os.Stat(outDir); err == nil && info.IsDir() {
			prior = len(generatedFiles(outDir))
		}
	}
	if docs == 0 && prior > 0 {
		_ = os.Remove(tmpPath)
		ui.Errorf(stderr, "refusing to overwrite %s's rendered output (%d existing document(s)/file(s)) with an EMPTY render", o.Domain, prior)
		ui.Errorf(stderr, "  kustomize succeeded but produced nothing — check %s resources:", filepath.Join(domainDir, "kustomization.yaml"))
		ui.Errorf(stderr, "  applying an empty artifact would prune everything it manages (Flux prune: true)")
		return ErrHandled
	}
	if docs == 0 {
		ui.Warnf(stderr, "%s rendered 0 documents (no prior artifact, so nothing was lost) — is its kustomization.yaml composing any targets?", o.Domain)
	}

	// Promote ONLY when the content changed. An unconditional move refreshes
	// the mtime of a byte-identical artifact, and everything that WATCHES
	// the file treats mtime as change — a Tiltfile that both reads
	// artifacts.yaml and rewrites it during eval self-triggered a reload
	// loop that starved every queued build for hours (2026-08-07). An
	// unchanged render is a no-op and the watch stays quiet.
	if existingErr == nil && bytes.Equal(existing, rendered) {
		_ = os.Remove(tmpPath)
		ui.Debugf(stderr, "%s: render unchanged (%d document(s)) — artifacts.yaml left untouched", o.Domain, docs)
	} else {
		if err := os.Rename(tmpPath, artifactPath); err != nil {
			_ = os.Remove(tmpPath)
			ui.Errorf(stderr, "kustomize build failed for %s", o.Domain)
			return ErrHandled
		}
		// Say what was produced. A silent success cannot distinguish a full
		// render from one that quietly lost most of its resources; the count
		// is the only cheap thing an operator can check against what they
		// expected.
		fmt.Fprintf(stderr, "lo build: %s rendered %d document(s) -> %s\n", o.Domain, docs, artifactPath)
	}

	// Split emit rides EVERY successful build of a split-mode domain (spec-
	// declared, or forced/skipped via LOK8S_BUILD_SPLIT=1/0 by the debug
	// flags) — so the committed GitOps layout can never silently stale
	// behind artifacts.yaml. (The --no-secrets modifier shapes WHAT the
	// split emits, not WHETHER.)
	splitMode := o.SplitOverride
	if splitMode != "0" && splitMode != "1" {
		if ArtifactsMode(domainDir) == "split" {
			splitMode = "1"
		} else {
			splitMode = "0"
		}
	}
	if splitMode == "1" {
		return Split(o)
	}
	return nil
}

// exportSecretsPath implements per-instance secret isolation. The kustomize
// secrets plugin reads $PATH_SECRETS; when a domain keeps its own secrets
// (clusters/<domain>/secrets/) point the plugin there so each instance
// generates + reads ONLY its own secrets — never a shared flat store, so dev
// secrets can't bleed into prod (and vice versa). A domain opts in simply by
// having that dir (the `lo secrets --domain <d> …` commands write it).
// Without it, the inherited flat $PATH_SECRETS is used (single-instance
// projects). Bash: build::_export_secrets_path.
func exportSecretsPath(domainDir string) {
	secretsDir := filepath.Join(domainDir, "secrets")
	if info, err := os.Stat(secretsDir); err == nil && info.IsDir() {
		os.Setenv("PATH_SECRETS", secretsDir)
	}
}

// pruneStaleArtifactDirs removes artifacts/*/ subdirs that contain an
// artifacts.yaml (pre-domain-build per-target outputs); everything else is
// preserved.
func pruneStaleArtifactDirs(artifactsDir string) {
	entries, err := os.ReadDir(artifactsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(artifactsDir, e.Name())
		if fileExists(filepath.Join(sub, "artifacts.yaml")) {
			_ = os.RemoveAll(sub)
		}
	}
}

// runKustomize renders `kustomize build --enable-alpha-plugins <domainDir>`
// through internal/render (in-process by default, the pinned binary under
// LO_RENDER=exec) with the exact env the bash pipeline set: KUBECONFIG
// (pass B), KHELM_TRUST_ANY_REPO=true, and LOK8S_SECRETS_DISABLE.
//
// --no-secrets store-free wiring: in no-secrets mode the render itself must
// not touch the secrets store. The split-side guards leave the committed
// Secret.*.sops.yaml inert, but the split runs AFTER this kustomize build —
// the render still invokes the secrets.lok8s.dev exec generator, which would
// read/mint $PATH_SECRETS. LOK8S_SECRETS_DISABLE=1 makes that generator emit
// nothing and never read the store, so the whole render needs no store or
// key (this — not the split shaping alone — is what makes --no-secrets truly
// store-free). Explicit 0 otherwise (unambiguous off).
func runKustomize(ctx context.Context, o Options, domainDir, kubeconfig string, stderr io.Writer) ([]byte, error) {
	secretsDisable := "0"
	if o.NoSecrets {
		secretsDisable = "1"
	}
	return render.Build(ctx, domainDir, render.Options{
		Paths:  o.Paths,
		Env:    kustomizeEnv(o.Paths, kubeconfig, secretsDisable),
		Stderr: stderr,
	})
}

// kustomizeEnv is the per-render environment overlay, the way the argsh
// runtime had it for the kustomize child: the toolchain on PATH (the
// Secret generator's `bash:` scripts and any KRM exec function inherit
// it) and the per-render overrides. KUSTOMIZE_PLUGIN_HOME is the render
// package's business: the self-exec home in-process, the
// $PATH_BASE/.kustomize default in exec mode.
func kustomizeEnv(p *config.Paths, kubeconfig, secretsDisable string) []string {
	path := os.Getenv("PATH")
	for _, dir := range []string{p.Lok8s, p.Bin} {
		if !containsPathEntry(path, dir) {
			path = dir + string(os.PathListSeparator) + path
		}
	}
	return []string{
		"PATH=" + path,
		"KUBECONFIG=" + kubeconfig,
		"KHELM_TRUST_ANY_REPO=true",
		"LOK8S_SECRETS_DISABLE=" + secretsDisable,
	}
}

// countKindLines counts lines starting with "kind:" — the same literal
// `grep -c '^kind:'` the bash guard used (a LINE count, deliberately not a
// YAML parse, so the two implementations agree on malformed input too).
func countKindLines(data []byte) int {
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "kind:") {
			count++
		}
	}
	return count
}

// generatedFiles lists the framework-owned files at depth 1 of dir: names
// matching the uppercase-Kind ownership rule ([A-Z]*.yaml), regular files
// only (bash: find -maxdepth 1 -type f -name '[A-Z]*.yaml').
func generatedFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' && strings.HasSuffix(name, ".yaml") {
			out = append(out, name)
		}
	}
	return out
}

func containsPathEntry(path, dir string) bool {
	for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
		if entry == dir {
			return true
		}
	}
	return false
}
