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

// runMu serializes in-process renders: the plugin children read the
// process environment, so the per-render overlay (Options.Env) has to be
// installed in it for the duration of a run.
var runMu sync.Mutex

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
	pc.HelmConfig.Command = "helm"
	pc.HelmConfig.ApiVersions = []string{}
	kOpts.PluginConfig = pc

	overlay := append(append([]string{}, o.Env...), konfig.KustomizePluginHomeEnv+"="+home)

	var out []byte
	err = withEnv(overlay, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The managed-by label switch is read from the environment the
		// same way the CLI reads it, after the overlay is in place.
		kOpts.AddManagedbyLabel = os.Getenv(konfig.EnableManagedbyLabelEnv) == "on"
		m, err := krusty.MakeKustomizer(kOpts).Run(filesys.MakeFsOnDisk(), dir)
		if err != nil {
			return err
		}
		out, err = m.AsYaml()
		return err
	})
	if err != nil {
		// cobra's error line, as the kustomize CLI child printed it.
		fmt.Fprintf(o.stderr(), "Error: %v\n", err)
		return nil, err
	}
	return out, nil
}

// withEnv installs overlay (KEY=VALUE) in the process environment, runs
// fn, and restores every touched key — under runMu.
func withEnv(overlay []string, fn func() error) error {
	runMu.Lock()
	defer runMu.Unlock()
	type saved struct {
		value string
		set   bool
	}
	prior := map[string]saved{}
	for _, kv := range overlay {
		k, v, _ := strings.Cut(kv, "=")
		if _, seen := prior[k]; !seen {
			old, ok := os.LookupEnv(k)
			prior[k] = saved{old, ok}
		}
		os.Setenv(k, v)
	}
	defer func() {
		for k, s := range prior {
			if s.set {
				os.Setenv(k, s.value)
			} else {
				os.Unsetenv(k)
			}
		}
	}()
	return fn()
}
