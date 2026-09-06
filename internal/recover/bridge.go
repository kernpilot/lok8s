package recover

// bridge.go — the bash children recover delegates to. Providers are BASH
// plugins (.lok8s/providers/<name>/main sourcing the argsh runtime) and the
// provision dispatch is still the bash lib (the Go provision.Dispatcher has
// no provider loader yet and the KubeOne driver's inventory/pre-apply hooks
// are unwired), so both run as `bash -c` children over the ORIGINAL libs —
// the precedent is `lo doctor`'s provider section (cli/cmd_doctor.go).
// Every child goes through execx.Runner (hermetic under a fake).

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// bridgePATH prepends .lok8s + .bin to PATH when missing (cli.shimEnv).
func bridgePATH(p *config.Paths) string {
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

// bridgeEnv is the environment the argsh entrypoint would have derived:
// the prepared PATH plus every PATH_* the libs read.
func bridgeEnv(p *config.Paths) []string {
	secretsVal := p.SecretsEnv
	if secretsVal == "" {
		secretsVal = filepath.Join(p.Base, ".secrets")
	}
	return []string{
		"PATH=" + bridgePATH(p),
		"PATH_BASE=" + p.Base,
		"PATH_BIN=" + p.Bin,
		"PATH_LOK8S=" + p.Lok8s,
		"PATH_CLUSTERS=" + p.Clusters,
		"PATH_SECRETS=" + secretsVal,
		"PATH_SCRIPTS=" + p.Lok8s,
	}
}

// providerProbeScript loads the provider through utils/provider.sh
// (provider::load: source + contract check, its diagnostics on stderr) and
// reports which OPTIONAL hooks it implements (bash: `declare -F`).
const providerProbeScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^utils/provider
provider::load "${1}" >/dev/null
declare -F provider::rebuild >/dev/null && echo rebuild || true
declare -F provider::doctor >/dev/null && echo doctor || true`

// providerCallScript re-loads the provider quietly (it already announced
// itself during the probe) and calls ONE contract function with the
// remaining argv.
const providerCallScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^utils/provider
provider::load "${1}" >/dev/null 2>&1
fn="${2}"; shift 2
"${fn}" "${@}"`

// provisionScript is recover::_provision: provision::dispatch with the
// same libs `lo` has loaded (the dispatch tail probes them with declare -f)
// and the gate pre-authorized (force=1, the argsh dynamic-scope idiom).
const provisionScript = `set -euo pipefail
source "${PATH_BIN}/argsh"
import ^libs/provision
import ^libs/bootstrap
import ^libs/kubehz/main
import ^libs/inventory/main
import ^libs/gitops
force=1
provision::dispatch "${1}"`

// bashProviderImpl is the Provider over the bash plugin.
type bashProviderImpl struct {
	r       *Runner
	name    string
	rebuild bool
	doctor  bool
}

// bashProvider is the default NewProvider: probe the plugin once (bash:
// provider::load + the declare -F checks in recover::_resolve).
func (r *Runner) bashProvider(ctx context.Context, name string) (Provider, error) {
	var out strings.Builder
	err := r.exec().Run(ctx, execx.Cmd{
		Name:   "bash",
		Args:   []string{"-c", providerProbeScript, "lo-recover-provider", name},
		Dir:    r.Paths.Base,
		Env:    bridgeEnv(r.Paths),
		Stdout: &out,
		Stderr: r.errOut(),
	})
	if err != nil {
		return nil, err
	}
	p := &bashProviderImpl{r: r, name: name}
	for _, line := range strings.Split(out.String(), "\n") {
		switch strings.TrimSpace(line) {
		case "rebuild":
			p.rebuild = true
		case "doctor":
			p.doctor = true
		}
	}
	return p, nil
}

func (p *bashProviderImpl) HasRebuild() bool { return p.rebuild }
func (p *bashProviderImpl) HasDoctor() bool  { return p.doctor }

func (p *bashProviderImpl) call(ctx context.Context, fn string, stdout, stderr io.Writer, args ...string) error {
	argv := append([]string{"-c", providerCallScript, "lo-recover-provider", p.name, fn}, args...)
	return p.r.exec().Run(ctx, execx.Cmd{
		Name:   "bash",
		Args:   argv,
		Dir:    p.r.Paths.Base,
		Env:    bridgeEnv(p.r.Paths),
		Stdout: stdout,
		Stderr: stderr,
	})
}

// Doctor: `provider::doctor <config> 2>/dev/null` — exit ignored.
func (p *bashProviderImpl) Doctor(ctx context.Context, configFile string) string {
	var out strings.Builder
	_ = p.call(ctx, "provider::doctor", &out, io.Discard, configFile)
	return out.String()
}

// Rebuild: `provider::rebuild <config> <work_dir>` — output passes through.
func (p *bashProviderImpl) Rebuild(ctx context.Context, configFile, workDir string) error {
	return p.call(ctx, "provider::rebuild", p.r.out(), p.r.errOut(), configFile, workDir)
}

// Output: `provider::output <config> 2>/dev/null`.
func (p *bashProviderImpl) Output(ctx context.Context, configFile string) ([]byte, error) {
	var out strings.Builder
	if err := p.call(ctx, "provider::output", &out, io.Discard, configFile); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

// bashProvision is the default Provision phase: provision::dispatch in a
// bash child (see provisionScript). Output passes through; DEBUG /
// LOK8S_NONINTERACTIVE / the loaded HCLOUD_*/HROBOT_* creds ride the
// inherited environment.
func (r *Runner) bashProvision(ctx context.Context, domainName string) error {
	err := r.exec().Run(ctx, execx.Cmd{
		Name:   "bash",
		Args:   []string{"-c", provisionScript, "lo-recover-provision", domainName},
		Dir:    r.Paths.Base,
		Env:    bridgeEnv(r.Paths),
		Stdout: r.out(),
		Stderr: r.errOut(),
	})
	if err != nil {
		return fmt.Errorf("recover: provision::dispatch: %w", err)
	}
	return nil
}
