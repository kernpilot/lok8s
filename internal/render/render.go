// Package render is the single kustomize entry point for the Go binary.
//
// Build renders a kustomization directory the way `kustomize build
// --enable-alpha-plugins [--enable-exec] <dir>` did, but IN-PROCESS: the
// pinned kustomize API (sigs.k8s.io/kustomize/api — the module behind the
// kustomize v5.8.1 binary the repo pins in .bin/b.yaml) runs inside `lo`,
// and the two exec generators every lok8s render depends on —
// secrets.lok8s.dev/v1/Secret and khelm.mgoltzsche.github.com/v2/
// ChartRenderer — are served by `lo` ITSELF: a per-process plugin home
// holds symlinks named like the plugins that point at the running binary,
// and main dispatches on argv[0] (see dispatch.go). No kustomize binary, no
// khelm binary, no .kustomize/ directory and no KUSTOMIZE_PLUGIN_HOME are
// needed by the binary. The frozen bash tree (LO_IMPL=bash) keeps using all
// of them.
//
// Byte parity is the contract: the in-process render must produce the same
// bytes the exec pipeline produced. The pins that make that true — the
// kustomize API version, the khelm library version (+ its helm), and the
// generator package imported from ./kustomize — are listed in
// docs/reference/go-migration.md. LO_RENDER=exec restores the exec
// pipeline for an A/B comparison.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// ModeEnv selects the render pipeline: unset/"inprocess" (the default) runs
// kustomize in-process; "exec" restores the subprocess pipeline (the
// pinned kustomize binary + the exec plugins under KUSTOMIZE_PLUGIN_HOME)
// for an A/B comparison. Any other value is rejected (fail closed).
const ModeEnv = "LO_RENDER"

// Mode is the resolved value of ModeEnv.
type Mode string

const (
	// ModeInProcess runs the kustomize API inside lo (default).
	ModeInProcess Mode = "inprocess"
	// ModeExec execs the pinned kustomize binary (escape hatch).
	ModeExec Mode = "exec"
)

// CurrentMode reads LO_RENDER. An unknown value is an error so a typo can
// never silently pick a pipeline.
func CurrentMode() (Mode, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(ModeEnv))); v {
	case "", string(ModeInProcess):
		return ModeInProcess, nil
	case string(ModeExec):
		return ModeExec, nil
	default:
		return "", fmt.Errorf("%s: unknown value %q (want \"exec\" or unset)", ModeEnv, v)
	}
}

// Options shapes one render.
type Options struct {
	// Paths locates the project. In-process it is unused; in exec mode it
	// resolves the kustomize binary (.bin first) and, when
	// KUSTOMIZE_PLUGIN_HOME is unset, defaults it to <Base>/.kustomize
	// the way `lo build` always did. Nil is allowed (addons pass none):
	// then exec mode needs Runner and leaves the plugin home to the
	// process environment, again as before.
	Paths *config.Paths
	// Runner runs the exec-mode kustomize child (the hermetic seam the
	// addon tests stub). Nil = execx.NewRunner(Paths).
	Runner execx.Runner
	// EnableExec is `--enable-exec` (KRM exec functions). The addon render
	// passes it; the domain build does not. Legacy exec generators are
	// enabled by --enable-alpha-plugins in both cases.
	EnableExec bool
	// LoadRestrictions is `--load-restrictor` (default RootOnly, like the
	// CLI).
	LoadRestrictions types.LoadRestrictions
	// Env is the per-render environment overlay (KEY=VALUE), the entries
	// the exec pipeline handed to the kustomize child on top of its own
	// environment: KUBECONFIG, KHELM_TRUST_ANY_REPO, LOK8S_SECRETS_DISABLE,
	// a PATH with the toolchain, per-addon LOK8S_* overrides. In-process
	// the plugins are children of THIS process, so the overlay is applied
	// to the process environment for the duration of the run (and
	// restored afterwards) under a package mutex — concurrent renders
	// (the bootstrap DAG) serialize on it.
	Env []string
	// Stderr receives what the kustomize child wrote to its stderr: in
	// exec mode the child's stream, in-process the `Error: …` line the
	// kustomize CLI prints on failure. Nil = os.Stderr.
	Stderr io.Writer
}

func (o *Options) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

// Build renders dir and returns the YAML stream `kustomize build` would
// have written to stdout. On failure the kustomize error line has already
// been written to Options.Stderr (exec: by the child; in-process: the same
// `Error: <msg>` cobra line) and the error is returned for the caller's
// own reporting — callers print their own [error] lines exactly as before.
func Build(ctx context.Context, dir string, o Options) ([]byte, error) {
	mode, err := CurrentMode()
	if err != nil {
		return nil, err
	}
	if o.LoadRestrictions == types.LoadRestrictionsUnknown {
		o.LoadRestrictions = types.LoadRestrictionsRootOnly
	}
	if mode == ModeExec {
		return buildExec(ctx, dir, o)
	}
	return buildInProcess(ctx, dir, o)
}

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
	kOpts.LoadRestrictions = o.LoadRestrictions
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

// buildExec is the pre-WP4 pipeline, kept verbatim behind LO_RENDER=exec:
// the pinned kustomize binary (.bin first, then PATH) with the overlay
// appended to its environment and, when Paths is known and the caller's
// environment has no plugin home, KUSTOMIZE_PLUGIN_HOME defaulted to
// <Base>/.kustomize.
func buildExec(ctx context.Context, dir string, o Options) ([]byte, error) {
	runner := o.Runner
	if runner == nil {
		if o.Paths == nil {
			return nil, errors.New("render: exec mode needs Paths or a Runner")
		}
		runner = execx.NewRunner(o.Paths)
	}
	args := []string{"build", "--enable-alpha-plugins"}
	if o.EnableExec {
		args = append(args, "--enable-exec")
	}
	if o.LoadRestrictions == types.LoadRestrictionsNone {
		args = append(args, "--load-restrictor", types.LoadRestrictionsNone.String())
	}
	args = append(args, dir)
	env := append([]string{}, o.Env...)
	if o.Paths != nil && os.Getenv(konfig.KustomizePluginHomeEnv) == "" && !hasKey(env, konfig.KustomizePluginHomeEnv) {
		env = append(env, konfig.KustomizePluginHomeEnv+"="+filepath.Join(o.Paths.Base, ".kustomize"))
	}
	var out bytes.Buffer
	err := runner.Run(ctx, execx.Cmd{
		Name:   "kustomize",
		Args:   args,
		Env:    env,
		Stdout: &out,
		Stderr: o.stderr(),
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func hasKey(env []string, key string) bool {
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			return true
		}
	}
	return false
}
