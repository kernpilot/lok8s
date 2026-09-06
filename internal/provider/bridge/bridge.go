// Package bridge runs the BASH infrastructure providers
// (.lok8s/providers/<name>/main) as children of the Go dispatch.
//
// Why a bridge and not a port: the only provider today is hetzner — ~1650
// lines of argsh driving the hcloud CLI, the Robot REST API (curl) and a
// cloud-init generator with its own template tree. That is a task of its
// own; until it lands, the Go provision.Dispatcher needs SOME
// provision.ProviderLoader or every cloud domain fails with "provider not
// found". So each provider contract call becomes a `bash -c` child over
// the ORIGINAL libs — the precedent is internal/recover/bridge.go (and `lo
// doctor`'s provider section). Every child goes through execx.Runner, so
// the dispatch stays hermetic under a fake.
//
// The provider is loaded ONCE per call (a fresh bash has no memory): the
// state a provider needs lives at the cloud (hcloud labels) or on disk
// (<work_dir>/hetzner.dump.json), never in shell variables, which is what
// makes per-call loading correct. PROVIDER_CONFIG_FILE / PROVIDER_NAME ride
// the inherited environment (the dispatch exports them, like bash did) and
// are ALSO passed positionally, so a provider that reads either works.
//
// The kubeone driver's inventory + pre-apply seams (kubeone.Hooks
// {AppendInventory, PrepareApply}) are hetzner-inventory bound — the bash
// functions call provider::output and the Robot naming helpers — so they
// ride this bridge too (KubeoneAppendInventory / KubeonePrepareApply). A Go
// hetzner port replaces all of it in one place.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

// nameRe is the provider-name allowlist (bash: provider::read_name) — the
// name lands in a path the child sources.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Loader implements provision.ProviderLoader over the bash providers.
type Loader struct {
	Paths  *config.Paths
	Runner execx.Runner
	// Stdout/Stderr are where a provider's streamed output (provision,
	// destroy, validate) lands; nil = the process streams.
	Stdout io.Writer
	Stderr io.Writer
}

func (l *Loader) out() io.Writer {
	if l.Stdout != nil {
		return l.Stdout
	}
	return os.Stdout
}

func (l *Loader) errOut() io.Writer {
	if l.Stderr != nil {
		return l.Stderr
	}
	return os.Stderr
}

func (l *Loader) runner() execx.Runner {
	if l.Runner != nil {
		return l.Runner
	}
	return execx.NewRunner(l.Paths)
}

// PATH prepends .lok8s + .bin to PATH when missing (cli.shimEnv).
func PATH(p *config.Paths) string {
	path := os.Getenv("PATH")
	for _, dir := range []string{p.Lok8s, p.Bin} {
		found := false
		for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
			if entry == dir {
				found = true
				break
			}
		}
		if !found {
			path = dir + string(os.PathListSeparator) + path
		}
	}
	return path
}

// Env is the environment the argsh entrypoint would have derived: the
// prepared PATH plus every PATH_* the libs read.
func Env(p *config.Paths) []string {
	secretsVal := p.SecretsEnv
	if secretsVal == "" {
		secretsVal = filepath.Join(p.Base, ".secrets")
	}
	return []string{
		"PATH=" + PATH(p),
		"PATH_BASE=" + p.Base,
		"PATH_BIN=" + p.Bin,
		"PATH_LOK8S=" + p.Lok8s,
		"PATH_CLUSTERS=" + p.Clusters,
		"PATH_SECRETS=" + secretsVal,
		"PATH_SCRIPTS=" + p.Lok8s,
	}
}

// probeScript is provider::load with its diagnostics visible (bash:
// provision::dispatch's provider::load — the not-found and missing-contract
// errors print there).
const probeScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^utils/provider
provider::load "${1}" >/dev/null`

// callScript re-loads the provider quietly (it already announced itself
// during the probe) and calls ONE contract function with the remaining
// argv.
const callScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^utils/provider
provider::load "${1}" >/dev/null 2>&1
fn="${2}"; shift 2
"${fn}" "${@}"`

// kubeoneScript sources the bash kubeone driver over a loaded provider
// (empty name = no provider) and calls ONE of its functions.
const kubeoneScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^utils/provider
if [[ -n "${1}" ]]; then provider::load "${1}" >/dev/null 2>&1; fi
source "${PATH_LOK8S}/drivers/kubeone/main"
fn="${2}"; shift 2
"${fn}" "${@}"`

// kubeonePrepareApplyScript is the pre-apply trio inside kubeone::apply,
// with its exact guards: the stale-worker cleanup is fail-ignored (a plain
// statement under the apply's suppressed errexit), the Robot naming and the
// addon render abort (`|| return 1`).
const kubeonePrepareApplyScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^utils/provider
if [[ -n "${1}" ]]; then provider::load "${1}" >/dev/null 2>&1; fi
source "${PATH_LOK8S}/drivers/kubeone/main"
kubeone::_clean_reinstalled_workers "${2}" || true
kubeone::_name_robot_workers "${2}/kubeone.yaml"
kubeone::render_addons "${2}" "${3}"`

// Load probes the plugin once (bash: provider::load + provider::check_contract
// — its diagnostics print on Stderr) and returns the bridged provider.
func (l *Loader) Load(name string) (driver.Provider, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("bridge: invalid provider name %q", name)
	}
	err := l.runner().Run(context.Background(), execx.Cmd{
		Name:   "bash",
		Args:   []string{"-c", probeScript, "lo-provider", name},
		Dir:    l.Paths.Base,
		Env:    l.env(name),
		Stdout: io.Discard,
		Stderr: l.errOut(),
	})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", name, err)
	}
	return &Provider{l: l, name: name}, nil
}

func (l *Loader) env(name string) []string {
	env := Env(l.Paths)
	if name != "" {
		env = append(env, "PROVIDER_NAME="+name)
	}
	return env
}

// call runs one provider contract function in a child.
func (l *Loader) call(ctx context.Context, name, fn string, stdout, stderr io.Writer, args ...string) error {
	argv := append([]string{"-c", callScript, "lo-provider", name, fn}, args...)
	return l.runner().Run(ctx, execx.Cmd{
		Name:   "bash",
		Args:   argv,
		Dir:    l.Paths.Base,
		Env:    l.env(name),
		Stdout: stdout,
		Stderr: stderr,
	})
}

// Provider is the driver.Provider (and driver.ProviderStatuser) over one
// bash plugin.
type Provider struct {
	l    *Loader
	name string
}

// Name is the plugin's name (bash: PROVIDER_NAME).
func (p *Provider) Name() string { return p.name }

// Validate is provider::validate <config> — output passes through.
func (p *Provider) Validate(ctx context.Context, configFile string) error {
	return p.l.call(ctx, p.name, "provider::validate", p.l.out(), p.l.errOut(), configFile)
}

// CredentialData is provider::credential_data: `key=value` lines on stdout.
func (p *Provider) CredentialData(ctx context.Context, configFile string) (map[string]string, error) {
	var out strings.Builder
	if err := p.l.call(ctx, p.name, "provider::credential_data", &out, p.l.errOut(), configFile); err != nil {
		return nil, err
	}
	data := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			continue
		}
		data[k] = v
	}
	return data, nil
}

// Provision is provider::provision <config> <work_dir> — output passes
// through.
func (p *Provider) Provision(ctx context.Context, configFile, workDir string) error {
	return p.l.call(ctx, p.name, "provider::provision", p.l.out(), p.l.errOut(), configFile, workDir)
}

// Destroy is provider::destroy <config> <work_dir> — output passes through.
func (p *Provider) Destroy(ctx context.Context, configFile, workDir string) error {
	return p.l.call(ctx, p.name, "provider::destroy", p.l.out(), p.l.errOut(), configFile, workDir)
}

// Output is provider::output <config> — the inventory JSON on stdout
// (bash callers run it `2>/dev/null`).
func (p *Provider) Output(ctx context.Context, configFile string) ([]byte, error) {
	var out strings.Builder
	if err := p.l.call(ctx, p.name, "provider::output", &out, io.Discard, configFile); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

// ProviderStatus is the optional provider::status hook (Running | Partial |
// NotFound). A plugin without it fails the call — the bash driver only
// calls it after a `declare -F` probe; callers here should do the same via
// the returned error.
func (p *Provider) ProviderStatus(ctx context.Context, configFile string) (string, error) {
	var out strings.Builder
	if err := p.l.call(ctx, p.name, "provider::status", &out, p.l.errOut(), configFile); err != nil {
		return "", err
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// ErrNoProvider reports a kubeone seam invoked with no provider loaded —
// the bash _append_inventory calls provider::output unconditionally, so a
// missing provider is a hard error there too.
var ErrNoProvider = errors.New("bridge: no provider loaded")

// providerNameOf reads the loaded provider's name off the driver deps at
// CALL time — the dispatch fills deps.Provider after the driver factory
// ran, so the hooks must not snapshot it at construction.
func providerNameOf(deps *driver.Deps) string {
	if deps == nil || deps.Provider == nil {
		return ""
	}
	if bp, ok := deps.Provider.(*Provider); ok {
		return bp.name
	}
	return deps.ProviderName
}

// KubeoneAppendInventory is kubeone.Hooks.AppendInventory: the bash
// _append_inventory <config> <manifest> over the loaded provider.
func (l *Loader) KubeoneAppendInventory(deps *driver.Deps) func(ctx context.Context, configFile, manifest string) error {
	return func(ctx context.Context, configFile, manifest string) error {
		name := providerNameOf(deps)
		if name == "" {
			return ErrNoProvider
		}
		return l.runner().Run(ctx, execx.Cmd{
			Name:   "bash",
			Args:   []string{"-c", kubeoneScript, "lo-kubeone", name, "_append_inventory", configFile, manifest},
			Dir:    l.Paths.Base,
			Env:    l.env(name),
			Stdout: l.out(),
			Stderr: l.errOut(),
		})
	}
}

// KubeonePrepareApply is kubeone.Hooks.PrepareApply: the pre-apply trio
// (_clean_reinstalled_workers fail-ignored, _name_robot_workers,
// render_addons) with the bash guards. Works without a provider loaded
// (the trio reads the manifest + spec, not the cloud).
func (l *Loader) KubeonePrepareApply(deps *driver.Deps) func(ctx context.Context, workDir, clusterYAML string) error {
	return func(ctx context.Context, workDir, clusterYAML string) error {
		name := providerNameOf(deps)
		return l.runner().Run(ctx, execx.Cmd{
			Name:   "bash",
			Args:   []string{"-c", kubeonePrepareApplyScript, "lo-kubeone", name, workDir, clusterYAML},
			Dir:    l.Paths.Base,
			Env:    l.env(name),
			Stdout: l.out(),
			Stderr: l.errOut(),
		})
	}
}
