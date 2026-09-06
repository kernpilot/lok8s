// Package render is the single kustomize entry point for the Go binary.
//
// Build renders a kustomization directory the way `kustomize build
// --enable-alpha-plugins [--enable-exec] <dir>` does. Two builds of the
// binary exist, selected by the `inprocess` build tag (see core.go and
// inprocess.go):
//
//   - lo (CORE, the default build): the render EXECS the pinned kustomize
//     binary from the project's toolchain (.bin first, then PATH) with
//     KUSTOMIZE_PLUGIN_HOME defaulted to <project>/.kustomize, where the
//     b-managed exec generators live — khelm's ChartRenderer and the
//     kustomize-secret Secret plugin (`lo init toolchain` installs all
//     three, pinned). No kustomize API, no khelm, no helm are linked in.
//     The secrets.lok8s.dev generator itself IS still part of core
//     (kustomize/plugins/secret, imported) — the registry TLS mint calls
//     it in-process, it is ours and small.
//   - lo-full (build tag `inprocess`): the pinned kustomize API
//     (sigs.k8s.io/kustomize/api — the module behind the kustomize
//     binary the toolchain pins) runs inside the binary, and both exec
//     generators are served by the binary ITSELF through a per-process
//     plugin home of symlinks pointing at the running executable
//     (DispatchPlugin routes argv[0]). No kustomize, khelm or .kustomize/
//     is needed. LO_RENDER=exec restores the subprocess pipeline for an
//     A/B comparison.
//
// Byte parity is the contract: both pipelines produce the same bytes. The
// pins that make that true — the kustomize API version and the kustomize
// CLI release it corresponds to, the khelm library/binary version (+ its
// helm) — live in internal/toolchain (pins.go) and are drift-tested
// against go.mod and the generated .bin/b.yaml template.
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

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// ModeEnv selects the render pipeline. lo-full: unset/"inprocess" (the
// default) runs kustomize in-process, "exec" restores the subprocess
// pipeline. lo core: unset/"exec" run the subprocess pipeline (the only
// one it has) and "inprocess" is rejected with a pointer to lo-full. Any
// other value is rejected (fail closed).
const ModeEnv = "LO_RENDER"

// Mode is the resolved value of ModeEnv.
type Mode string

const (
	// ModeInProcess runs the kustomize API inside lo (lo-full's default).
	ModeInProcess Mode = "inprocess"
	// ModeExec execs the pinned kustomize binary (core's only mode;
	// lo-full's escape hatch).
	ModeExec Mode = "exec"
)

// kustomizePluginHomeEnv is kustomize's plugin-home variable
// (konfig.KustomizePluginHomeEnv), spelled out so core does not link the
// kustomize API for one string.
const kustomizePluginHomeEnv = "KUSTOMIZE_PLUGIN_HOME"

// Variant names the build: "core" (exec render) or "full" (in-process).
func Variant() string { return variantName }

// InProcessAvailable reports whether this build links the in-process
// renderer (lo-full). Tests use it to skip in-process assertions on core.
func InProcessAvailable() bool { return inProcessAvailable }

// CurrentMode reads LO_RENDER. An unknown value is an error so a typo can
// never silently pick a pipeline; on core an explicit "inprocess" is an
// error too, naming the build that has it.
func CurrentMode() (Mode, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(ModeEnv)))
	switch v {
	case "":
		if inProcessAvailable {
			return ModeInProcess, nil
		}
		return ModeExec, nil
	case string(ModeExec):
		return ModeExec, nil
	case string(ModeInProcess):
		if inProcessAvailable {
			return ModeInProcess, nil
		}
		return "", fmt.Errorf("%s=inprocess: this is lo core (the render execs the pinned kustomize; only \"exec\" or unset is valid) — install lo-full for the in-process renderer", ModeEnv)
	default:
		if inProcessAvailable {
			return "", fmt.Errorf("%s: unknown value %q (want \"exec\" or unset)", ModeEnv, v)
		}
		return "", fmt.Errorf("%s: unknown value %q (want \"exec\" or unset; this is lo core)", ModeEnv, v)
	}
}

// SecretInProcess reports whether the imported secrets.lok8s.dev generator
// runs inside this process for the registry TLS mint: true unless
// LO_RENDER=exec asks for the subprocess pipeline explicitly. Independent
// of the build variant — the generator is linked into core as well — so
// core mints the dev registry cert without the plugin binary, exactly like
// lo-full, and only the explicit escape hatch reproduces the bash
// behaviour (exec the built plugin, fail when it is missing).
func SecretInProcess() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv(ModeEnv))) != string(ModeExec)
}

// LoadRestrictions is `--load-restrictor`: RootOnly (the default, like the
// CLI) or None. Declared here rather than borrowed from the kustomize API
// so core does not link it.
type LoadRestrictions int

const (
	// LoadRestrictionsRootOnly forbids files outside the kustomization root.
	LoadRestrictionsRootOnly LoadRestrictions = iota
	// LoadRestrictionsNone allows them.
	LoadRestrictionsNone
)

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
	LoadRestrictions LoadRestrictions
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
		// Callers report a failed render generically ("kustomize build
		// failed for <domain>") and rely on this stream for the cause —
		// the same way the kustomize child's `Error:` line reaches it — so
		// a rejected LO_RENDER (an unknown value, or "inprocess" on lo
		// core) is spelled out here.
		fmt.Fprintf(o.stderr(), "Error: %v\n", err)
		return nil, err
	}
	if mode == ModeExec {
		return buildExec(ctx, dir, o)
	}
	return buildInProcess(ctx, dir, o)
}

// buildExec is the subprocess pipeline (core's only one; lo-full's
// LO_RENDER=exec): the pinned kustomize binary (.bin first, then PATH)
// with the overlay appended to its environment and, when Paths is known
// and the caller's environment has no plugin home, KUSTOMIZE_PLUGIN_HOME
// defaulted to <Base>/.kustomize.
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
	if o.LoadRestrictions == LoadRestrictionsNone {
		args = append(args, "--load-restrictor", "LoadRestrictionsNone")
	}
	args = append(args, dir)
	env := append([]string{}, o.Env...)
	if o.Paths != nil && os.Getenv(kustomizePluginHomeEnv) == "" && !hasKey(env, kustomizePluginHomeEnv) {
		env = append(env, kustomizePluginHomeEnv+"="+filepath.Join(o.Paths.Base, ".kustomize"))
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
