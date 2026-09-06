// Package driver defines the Go driver contract — the port of the bash
// driver contract (.lok8s/drivers/README.md): every cluster-architecture
// driver implements provision/destroy/status/kubeconfig, plus the optional
// export and post-provision hooks. The dispatch layer (internal/provision)
// looks drivers up by name in the Registry and drives them.
//
// # Return-code semantics (the bash dispatch contract)
//
// The bash implementation communicates through process return codes; the Go
// port preserves them as sentinel errors:
//
//   - rc 3 → ErrDeclined: the real-infrastructure gate's decline sentinel
//     (provision::confirm_infra). Lets callers tell "operator said no" apart
//     from a real failure. Two consumers depend on the distinction:
//     provision::dispatch_destroy REMAPS a driver's OWN rc 3 to rc 1 — a
//     subprocess exit 3 inside driver::destroy (e.g. curl) must not read as
//     "operator said no" (DispatchDestroy does the same via ExitCode);
//     main::down turns a gate decline into a silent return 1 (no
//     orphaned-infra warning — the operator chose to stop).
//   - rc 100 → ErrFullLifecycle: driver::provision reports "remote CI
//     handled the full lifecycle" (remote VM ran provision + bootstrap
//     itself). Dispatch skips every local post-provision step and reports
//     success.
package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// Sentinel errors of the dispatch contract. See the package comment for the
// bash rc mapping.
var (
	// ErrDeclined is the rc-3 gate decline: the operator refused (or could
	// not be asked for) the real-infrastructure confirmation.
	ErrDeclined = errors.New("declined")
	// ErrFullLifecycle is the rc-100 remote-CI sentinel: the driver handled
	// the full lifecycle itself and local post-provision must be skipped.
	ErrFullLifecycle = errors.New("driver handled full lifecycle")
)

// Driver is the required contract (bash: driver::provision, driver::destroy,
// driver::status, driver::kubeconfig).
//
// Status returns the single status word the bash contract printed to stdout
// ("Running", "NotFound", "Healthy", "Degraded", "NotProvisioned",
// "Unknown", …) — the dispatch prints it.
//
// Kubeconfig returns the path of the cluster's kubeconfig under
// .kubeconfig/ (materializing it first when the driver can).
type Driver interface {
	Provision(ctx context.Context, domain string) error
	Destroy(ctx context.Context, domain string) error
	Status(ctx context.Context, domain string) (string, error)
	Kubeconfig(ctx context.Context, domain string) (string, error)
}

// Exporter is the optional driver::export hook: export the spec-derived env
// that spec.bootstrap addons consume (LOK8S_SPEC_*, …). MUST be idempotent —
// dispatch calls it on BOTH the full provision and the --bootstrap path, so
// a re-applied bootstrap graph renders with the same env a fresh provision
// would set.
type Exporter interface {
	Export(ctx context.Context, domain string) error
}

// PostProvisioner is the optional driver::post_provision hook: driver
// side-effects that need already-provisioned infrastructure (rare). Runs on
// the full-provision path only, never under --bootstrap.
type PostProvisioner interface {
	PostProvision(ctx context.Context, domain string) error
}

// Provider is the infrastructure-provider contract
// (.lok8s/utils/provider.sh): the seam between drivers and clouds. The
// actual hetzner provider port comes later; until then implementations are
// test fakes or exec wrappers.
//
// Output returns the standard inventory JSON (api/access/nodes/network —
// see the schema in utils/provider.sh); every provider produces the same
// shape, every driver reads it.
type Provider interface {
	Validate(ctx context.Context, configFile string) error
	CredentialData(ctx context.Context, configFile string) (map[string]string, error)
	Provision(ctx context.Context, configFile, workDir string) error
	Destroy(ctx context.Context, configFile, workDir string) error
	Output(ctx context.Context, configFile string) ([]byte, error)
}

// ProviderStatuser is the optional provider::status hook
// (Running | Partial | NotFound). The bash also documents optional
// provider::rebuild and provider::doctor hooks — those belong to the
// recover/doctor port, not this layer.
type ProviderStatuser interface {
	ProviderStatus(ctx context.Context, configFile string) (string, error)
}

// Deps is what the dispatch hands a driver factory. Provider fields are nil
// or empty until the dispatch loads a provider (bash sources the provider
// into globals AFTER sourcing the driver; the shared pointer mirrors that —
// the dispatch may fill the provider fields after construction, before
// Provision runs).
type Deps struct {
	Paths  *config.Paths
	Runner execx.Runner
	Stderr io.Writer

	// Provider is the loaded infrastructure provider (bash: the sourced
	// provider:: functions), nil when none was loaded.
	Provider Provider
	// ProviderName is the loaded provider's name (bash: PROVIDER_NAME).
	ProviderName string
	// ProviderConfigFile is the resolved provider config path (bash:
	// PROVIDER_CONFIG_FILE — a configRef file or an inline-config temp file).
	ProviderConfigFile string
}

// ExitError carries an explicit process exit code through the dispatch,
// preserving the wrapped cause.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode maps an error to the bash process exit code: nil → 0,
// ErrDeclined → 3, ErrFullLifecycle → 100, an explicit ExitError → its
// code, a subprocess *exec.ExitError → the subprocess's code, anything
// else → 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if errors.Is(err, ErrDeclined) {
		return 3
	}
	if errors.Is(err, ErrFullLifecycle) {
		return 100
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return xe.ExitCode()
	}
	return 1
}
