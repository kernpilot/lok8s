//go:build inprocess

package render

// inprocess.go — the `lo-full` build: `kustomize build` through the pinned
// kustomize API inside the binary, with the two exec generators served by
// the binary itself (dispatch.go, pluginhome.go, khelm.go — all gated on
// the same tag). `make build` without the tag swaps this file for core.go.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const (
	inProcessAvailable = true
	variantName        = "full"
)

// dirLocks serializes renders of the SAME directory: their overlay files
// share a name (renderenv.go), so two of them at once would read each
// other's variables. Renders of different directories run in parallel.
var dirLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func lockDir(dir string) func() {
	dirLocks.mu.Lock()
	if dirLocks.m == nil {
		dirLocks.m = map[string]*sync.Mutex{}
	}
	l, ok := dirLocks.m[dir]
	if !ok {
		l = &sync.Mutex{}
		dirLocks.m[dir] = l
	}
	dirLocks.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// buildInProcess is `kustomize build --enable-alpha-plugins [--enable-exec]
// [--load-restrictor …] <dir>` via krusty, option for option what the
// kustomize v5.8.1 build command derives from those flags
// (commands/build/build.go: HonorKustomizeFlags):
//
//   - Reorder: the flag is not passed → ReorderOptionUnspecified (the
//     kustomization's sortOptions decide, else legacy).
//   - PluginConfig: EnabledPluginConfig(BploUseStaticallyLinked) —
//     PluginRestrictionsNone + the builtin helm inflator enabled with the
//     default `helm` command; FnpLoadingOptions from --enable-exec.
//   - AddManagedbyLabel: only via KUSTOMIZE_ENABLE_MANAGEDBY_LABEL=on.
//
// The per-render overlay (Options.Env) never touches the process
// environment. The exec pipeline handed it to the kustomize child, whose
// plugin children inherited it; here the plugin children are children of
// THIS process, so the overlay is written to a file under the self-exec
// plugin home for the duration of the run (renderenv.go) and the child
// reads it before it serves a generator (dispatch.go). The two values the
// kustomize API itself reads from the environment (the managed-by label
// switch, the helm command's PATH) are taken from the overlay first.
//
// The output is ResMap.AsYaml(), the exact bytes the CLI writes.
func buildInProcess(ctx context.Context, dir string, o Options) ([]byte, error) {
	home, err := selfExecPluginHome()
	if err != nil {
		return nil, err
	}
	kOpts := krusty.MakeDefaultOptions()
	kOpts.Reorder = krusty.ReorderOptionUnspecified
	kOpts.LoadRestrictions = types.LoadRestrictionsRootOnly
	if o.LoadRestrictions == LoadRestrictionsNone {
		kOpts.LoadRestrictions = types.LoadRestrictionsNone
	}
	pc := types.EnabledPluginConfig(types.BploUseStaticallyLinked)
	pc.FnpLoadingOptions = types.FnPluginLoadingOptions{EnableExec: o.EnableExec}
	pc.HelmConfig.Command = helmCommand(o.Env)
	pc.HelmConfig.ApiVersions = []string{}
	kOpts.PluginConfig = pc
	kOpts.AddManagedbyLabel = overlayOrEnv(o.Env, konfig.EnableManagedbyLabelEnv) == "on"

	out, err := func() ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		unlock := lockDir(dir)
		defer unlock()
		remove, err := writeRenderEnv(home, dir, o.Env)
		if err != nil {
			return nil, err
		}
		defer remove()
		if os.Getenv(konfig.KustomizePluginHomeEnv) != home {
			// Something re-pointed the plugin home after the first render
			// (a caller's own Setenv). The self-exec symlinks are the only
			// plugins this build can serve, so put the constant back — a
			// process-wide value again, not a per-render one.
			os.Setenv(konfig.KustomizePluginHomeEnv, home)
		}
		m, err := krusty.MakeKustomizer(kOpts).Run(filesys.MakeFsOnDisk(), dir)
		if err != nil {
			return nil, err
		}
		return m.AsYaml()
	}()
	if err != nil {
		// cobra's error line, as the kustomize CLI child printed it.
		fmt.Fprintf(o.stderr(), "Error: %v\n", err)
		return nil, err
	}
	return out, nil
}

// overlayOrEnv reads key from the overlay (last entry wins), else from the
// process environment: what a kustomize child with the overlay appended
// to its environment would have seen.
func overlayOrEnv(overlay []string, key string) string {
	val, ok := os.LookupEnv(key)
	for _, kv := range overlay {
		if k, v, found := strings.Cut(kv, "="); found && k == key {
			val, ok = v, true
		}
	}
	if !ok {
		return ""
	}
	return val
}

// helmCommand resolves the builtin inflator's `helm` the way the kustomize
// child resolved it: through the overlay's PATH when the overlay carries
// one (the toolchain's .bin first), else the plain name for the process
// PATH.
func helmCommand(overlay []string) string {
	for _, dir := range filepath.SplitList(overlayOrEnv(overlay, "PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "helm")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	return "helm"
}
