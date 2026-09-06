// Package capi is the Go port of the Capi driver
// (.lok8s/drivers/capi/{main,generate}): production clusters via Cluster
// API. clusterctl/kubectl/kind stay execs behind the execx.Runner seam; the
// resource generation (templates + whitelist envsubst) is native.
//
// Work directory: none of its own — state lives in the management cluster;
// kubeconfigs land under .kubeconfig/ (workload under metadata.name,
// management under the management DOMAIN).
//
// The hosted-kubehz branches (kubehz::read_config, kubehz::provision_hosted,
// kubehz::destroy_hosted) are injectable Hooks — nil means "not wired yet",
// exactly like the kubeone driver's.
package capi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Name is the driver's registry name (spec `kind: Capi`).
const Name = "capi"

func init() {
	driver.Register(Name, func(deps *driver.Deps) (driver.Driver, error) {
		return New(deps), nil
	})
}

// Pinned provider versions for the LOCAL kind management cluster
// (capi::ensure_local_mgmt): pinned so the example is reproducible (and
// matches what the driver was validated against) rather than tracking
// clusterctl's latest.
const (
	localMgmtCAPIVersion    = "v1.13.2"
	localMgmtHetznerVersion = "v1.1.7"
)

// bootstrapName is the temporary kind bootstrap cluster's fixed name.
const bootstrapName = "lok8s-bootstrap"

// Hooks are the driver's injectable seams. nil = "not wired yet".
type Hooks struct {
	// ReadKubehzConfig is kubehz::read_config: reads spec.kubehz and
	// returns the hosting mode ("hosted", "self", ""). nil = the kubehz lib
	// is not wired → the hosted branch never fires (the bash tests stub it
	// as a no-op with the same effect). The call is GUARDED (`|| return 1`
	// in the bash): unguarded, a failed read left the hosting mode unset
	// and the != "hosted" branch told the operator of a HOSTED cluster to
	// set spec.managementCluster.domain — the wrong advice for their
	// configuration, hiding the real error.
	ReadKubehzConfig func(clusterYAML string) (hosting string, err error)
	// ProvisionHosted is kubehz::provision_hosted (delegate to the kubehz
	// API). Required once ReadKubehzConfig reports "hosted".
	ProvisionHosted func(ctx context.Context, domain, clusterYAML string) error
	// DestroyHosted is kubehz::destroy_hosted — same rules. Without it a
	// HOSTED cluster must never get the self-hosted teardown.
	DestroyHosted func(ctx context.Context, domain, clusterYAML string) error
}

// Driver is the Capi driver. Implements driver.Driver and driver.Exporter.
type Driver struct {
	deps  *driver.Deps
	Hooks Hooks

	// sleep is the wait seam (tests stub it; prod = time.Sleep) — the
	// apply-retry, kubeconfig-extraction, readyz and wait_ready loops all
	// wait through it.
	sleep func(time.Duration)
}

// New builds the driver over its dispatch-provided dependencies.
func New(deps *driver.Deps) *Driver {
	return &Driver{deps: deps, sleep: time.Sleep}
}

func (d *Driver) stderr() io.Writer {
	if d.deps.Stderr != nil {
		return d.deps.Stderr
	}
	return os.Stderr
}

func (d *Driver) sleepSeconds(n int) { d.sleep(time.Duration(n) * time.Second) }

func (d *Driver) clusterYAML(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
}

func (d *Driver) kubeconfigPath(name string) string {
	return filepath.Join(d.deps.Paths.Base, ".kubeconfig", name+".yaml")
}

// writeKubeconfigFile writes a cluster-admin kubeconfig owner-only. Deviation
// D20: the bash driver's `… > "${kc}"` redirect left the file at the umask
// default (0644), readable by every local user; the hosted and kind paths
// already wrote 0600. WriteFile sets the mode on a NEW file only, so the
// Chmod tightens one that already exists (the bash-faithful "the redirect
// truncates on every attempt" call sites rewrite the same path).
func writeKubeconfigFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// readHosting evaluates kubehz::read_config through the hook. nil hook =
// hosting unknown ("") — the self-hosted branches fire, matching the bash
// tests' no-op stub.
func (d *Driver) readHosting(clusterYAML string) (string, error) {
	if d.Hooks.ReadKubehzConfig == nil {
		return "", nil
	}
	return d.Hooks.ReadKubehzConfig(clusterYAML)
}

// infoLine mirrors the bash `echo "info: …" >&2` family (NOT the [error]/
// [warn] verbose.sh helpers — the driver deliberately used both).
func (d *Driver) infoLine(format string, a ...any) {
	fmt.Fprintf(d.stderr(), "info: "+format+"\n", a...)
}

// rawErrorLine mirrors the bash `echo "error: …" >&2` family used by
// capi::bootstrap (distinct from verbose.sh's colored "[error]").
func (d *Driver) rawErrorLine(format string, a ...any) {
	fmt.Fprintf(d.stderr(), "error: "+format+"\n", a...)
}

// ── Driver contract ───────────────────────────────────────

// Provision provisions a CAPI cluster (bash: driver::provision).
func (d *Driver) Provision(ctx context.Context, domain string) error {
	stderr := d.stderr()
	cy := d.clusterYAML(domain)
	spec := loadSpec(cy)

	// 1. Determine management cluster.
	mgmtDomain := spec.or("", "spec", "managementCluster", "domain")
	mgmtLocal := spec.or("false", "spec", "managementCluster", "local")
	namespace := spec.or("default", "spec", "cluster", "namespace")

	if mgmtDomain == "" {
		// Guarded read (see Hooks.ReadKubehzConfig): a failed read must
		// surface ITS error, not the managementCluster.domain advice below.
		hosting, err := d.readHosting(cy)
		if err != nil {
			return err
		}
		if hosting != "hosted" {
			ui.Errorf(stderr, "spec.managementCluster.domain is required for self-hosted CAPI")
			ui.Errorf(stderr, "set spec.kubehz.hosting: hosted to use the kubehz seed cluster")
			return fmt.Errorf("capi: spec.managementCluster.domain is required for self-hosted CAPI")
		}
		if d.Hooks.ProvisionHosted == nil {
			return errors.New("capi: hosted provisioning is not wired (Hooks.ProvisionHosted)")
		}
		return d.Hooks.ProvisionHosted(ctx, domain, cy)
	}

	// 2. Detect the infra provider (needed to set up the management
	// cluster). Guarded: an undetected provider is an EMPTY string that
	// flows into ensure_local_mgmt, ensure_credentials and generate, each
	// of which then builds the wrong thing rather than refusing.
	provider, err := d.DetectProvider(cy)
	if err != nil {
		return err
	}

	// 3. Ensure a management cluster exists.
	mgmtKubeconfig := d.kubeconfigPath(mgmtDomain)
	if domain == mgmtDomain && !fileExists(mgmtKubeconfig) {
		// The domain IS the management cluster — bootstrap it on the
		// provider.
		d.infoLine("bootstrapping management cluster %s", mgmtDomain)
		return d.Bootstrap(ctx, domain)
	}
	if !fileExists(mgmtKubeconfig) {
		if mgmtLocal == "true" {
			// Cheap, self-contained model: a local kind cluster as the CAPI
			// management cluster (real workload nodes still on the cloud
			// provider).
			if err := d.ensureLocalMgmt(ctx, mgmtDomain, provider); err != nil {
				return err
			}
		} else {
			ui.Errorf(stderr, "management cluster kubeconfig not found: %s", mgmtKubeconfig)
			ui.Errorf(stderr, "provision it first ('lo provision %s'), or set spec.managementCluster.local: true", mgmtDomain)
			return fmt.Errorf("capi: management cluster kubeconfig not found: %s", mgmtKubeconfig)
		}
	}

	// 4. Ensure the cluster namespace + credential Secret exist on the mgmt
	// cluster. Guarded on BOTH: without the namespace the credential Secret
	// and the CAPI resources land nowhere; without the credentials the
	// apply below still SUCCEEDS — applying CAPI custom resources is only
	// writing CRs — and then CAPH cannot authenticate to the provider, so
	// no Machine is ever created. That failure surfaces, if at all, fifteen
	// minutes later in WaitReady and is attributed to the wrong thing, on a
	// cluster the operator was told was being provisioned.
	//
	// The namespace manifest is rendered into a variable rather than piped,
	// so BOTH halves are checked: a `create … | apply` pipeline reports
	// only the LAST command's status (the bash could not assume pipefail).
	// Provider emptiness is checked HERE, not at detection: this is the
	// first place an empty provider does damage, and checking earlier would
	// pre-empt the "management cluster kubeconfig not found" diagnosis
	// kind_contract_test.bats pins as the intended message. (The Go
	// DetectProvider can no longer return empty-and-nil, but the guard is
	// the incident record and stays.)
	if provider == "" {
		ui.Errorf(stderr, "could not detect the infrastructure provider from %s", cy)
		return fmt.Errorf("capi: could not detect the infrastructure provider from %s", cy)
	}

	var nsManifest strings.Builder
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "kubectl",
		Args: []string{
			"--kubeconfig", mgmtKubeconfig, "create", "namespace", namespace,
			"--dry-run=client", "-o", "yaml",
		},
		Stdout: &nsManifest,
	}); err != nil {
		return err
	}
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", mgmtKubeconfig, "apply", "-f", "-"},
		Stdin:  strings.NewReader(nsManifest.String() + "\n"),
		Stdout: stderr, // bash: … | kubectl apply -f - >&2
	}); err != nil {
		return err
	}
	if err := d.EnsureCredentialsSecret(ctx, cy, provider, mgmtKubeconfig); err != nil {
		return err
	}

	// 5. Generate + apply the CAPI resources. On a freshly-initialized
	// management cluster the provider (CAPH) admission webhooks may still
	// be wiring up cert injection when clusterctl init returns, so the
	// first apply can fail with "connection refused" to the webhook
	// service. Retry with backoff until they serve (apply is idempotent, so
	// already-created objects are unchanged).
	resources, err := d.Generate(cy, provider)
	if err != nil {
		return err
	}
	for applyTry := 1; ; applyTry++ {
		err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "kubectl",
			Args: []string{"apply", "--kubeconfig", mgmtKubeconfig, "-f", "-"},
			// bash: -f <(echo "${resources}") — a process substitution; the
			// Go port feeds stdin, same stream.
			Stdin:  strings.NewReader(resources),
			Stdout: stderr, // bash: >&2
		})
		if err == nil {
			break
		}
		if applyTry == 10 {
			ui.Errorf(stderr, "failed to apply CAPI resources after %d attempts (provider webhooks not ready?)", applyTry)
			return fmt.Errorf("capi: failed to apply CAPI resources after %d attempts", applyTry)
		}
		d.infoLine("apply failed — provider webhooks may still be starting; retry %d/10 in 15s", applyTry)
		d.sleepSeconds(15)
	}

	// 6. Wait for the workload cluster to provision. Cloud nodes install
	// the kubeadm stack via cloud-init, so allow generous time (15m).
	// Guarded: a cluster that never became ready is the whole reason this
	// step exists. Unguarded it fell through to the kubeconfig extraction,
	// which then failed on its own 5 minutes later with "could not extract
	// workload kubeconfig" — a symptom reported as the cause. The CAPI
	// resources were already applied above, so CAPH may have created real
	// Hetzner resources by now — say so, like the destroy path does,
	// instead of leaving the operator to guess whether anything is billing.
	clusterName := spec.raw("metadata", "name")
	if err := d.WaitReady(ctx, mgmtKubeconfig, clusterName, namespace, 900); err != nil {
		ui.Errorf(stderr, "CAPI resources were applied — Hetzner servers and a load balancer may exist and keep billing")
		ui.Errorf(stderr, "  run 'lo down' to tear down, or inspect: kubectl --kubeconfig %s get cluster,machine -n %s", mgmtKubeconfig, namespace)
		return err
	}

	// 7. Extract the workload kubeconfig under the cluster's metadata.name
	// — the path the framework's bootstrap step (CNI + CCM) and the harness
	// expect. The kubeconfig secret appears once the control plane is
	// initialized, so retry briefly.
	if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
		return err
	}
	kc := d.kubeconfigPath(clusterName)
	for i := 1; i <= 30; i++ {
		var out strings.Builder
		err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "clusterctl",
			Args: []string{
				"get", "kubeconfig", clusterName,
				"--namespace", namespace,
				"--kubeconfig", mgmtKubeconfig,
			},
			Stdout: &out,
			Stderr: io.Discard, // bash: 2>/dev/null
		})
		// bash: `clusterctl … > "${kc}"` — the redirect truncates/writes the
		// file on EVERY attempt, whatever the exit status.
		if werr := writeKubeconfigFile(kc, out.String()); werr != nil {
			return werr
		}
		if err == nil && out.Len() > 0 {
			break
		}
		d.sleepSeconds(10)
	}
	if !fileNonEmpty(kc) {
		ui.Errorf(stderr, "could not extract workload kubeconfig for %s", clusterName)
		return fmt.Errorf("capi: could not extract workload kubeconfig for %s", clusterName)
	}

	// 8. Wait for the workload API server to answer before the framework
	// applies the CNI + CCM (the control-plane node still has to finish
	// kubeadm init). Fail (so the harness tears down) if it never comes up
	// — otherwise the framework would apply bootstrap addons against a dead
	// cluster.
	d.infoLine("waiting for the workload API server to become reachable")
	reachable := false
	for i := 1; i <= 60; i++ {
		if err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name:   "kubectl",
			Args:   []string{"--kubeconfig", kc, "get", "--raw=/readyz"},
			Stdout: io.Discard, Stderr: io.Discard, // bash: &>/dev/null
		}); err == nil {
			reachable = true
			break
		}
		d.sleepSeconds(10)
	}
	if !reachable {
		ui.Errorf(stderr, "workload API server for %s did not become reachable", clusterName)
		return fmt.Errorf("capi: workload API server for %s did not become reachable", clusterName)
	}
	return nil
}

// Export is driver::export — spec-derived env consumed by spec.bootstrap
// addons. Idempotent, no side effects (beyond the one env export). Called
// by the dispatch on BOTH the full provision and `--bootstrap` paths,
// before bootstrap::apply.
//
// The `hcloud` secret the Hetzner CCM mounts is generated by the `ccm`
// bootstrap addon (Secret.hcloud.kube-system.yaml — the SAME single source
// every driver uses), so we don't mint it here; we only export the one
// CAPI-specific input the generator can't infer — the private-network NAME
// (CAPH names it after the cluster) when networking mode is on.
// HCLOUD_TOKEN is exported by the dispatch.
func (d *Driver) Export(ctx context.Context, domain string) error {
	cy := d.clusterYAML(domain)
	spec := loadSpec(cy)
	clusterName := spec.raw("metadata", "name")
	// Best-effort (the bash call was unguarded under a disabled errexit): a
	// failed detection prints its diagnostic and leaves the provider empty.
	provider, _ := d.DetectProvider(cy)
	netEnabled := spec.or("false", "spec", "provider", "config", "network", "enabled")

	// The CCM reads the private network from the hcloud secret's `network`
	// key in networking mode; the addon generator picks it up from this env
	// var.
	if provider == "hetzner" && netEnabled == "true" {
		os.Setenv("HCLOUD_CCM_NETWORK", clusterName)
	}
	return nil
}

// Destroy tears down a CAPI cluster (bash: driver::destroy).
func (d *Driver) Destroy(ctx context.Context, domain string) error {
	stderr := d.stderr()
	cy := d.clusterYAML(domain)
	spec := loadSpec(cy)

	mgmtDomain := spec.or("", "spec", "managementCluster", "domain")
	clusterName := spec.raw("metadata", "name")
	namespace := spec.or("default", "spec", "cluster", "namespace")
	mgmtLocal := spec.or("false", "spec", "managementCluster", "local")

	if mgmtDomain == "" {
		// Guarded read — same reason as Provision's: without it a failed
		// config read skipped the hosted branch and a HOSTED cluster fell
		// through to the "managementCluster.domain is required" error — the
		// wrong diagnosis for the wrong path.
		hosting, err := d.readHosting(cy)
		if err != nil {
			return err
		}
		if hosting == "hosted" {
			if d.Hooks.DestroyHosted == nil {
				return errors.New("capi: hosted destroy is not wired (Hooks.DestroyHosted)")
			}
			return d.Hooks.DestroyHosted(ctx, domain, cy)
		}
		ui.Errorf(stderr, "spec.managementCluster.domain is required for self-hosted CAPI destroy")
		return fmt.Errorf("capi: spec.managementCluster.domain is required for self-hosted CAPI destroy")
	}

	mgmtKubeconfig := d.kubeconfigPath(mgmtDomain)
	workloadKubeconfig := d.kubeconfigPath(clusterName)

	// If the mgmt kubeconfig was lost but the local kind cluster is still
	// up, recover it — otherwise we would delete the kind cluster below
	// with a live workload (and its billed servers) still behind it.
	if !fileExists(mgmtKubeconfig) && mgmtLocal == "true" {
		kindCluster := MgmtKindName(mgmtDomain)
		if d.kindClusterExists(ctx, kindCluster) {
			if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
				return err
			}
			var out strings.Builder
			// Best-effort (bash: `… > file 2>/dev/null || true` — the
			// redirect creates the file whatever the exit status).
			_ = d.deps.Runner.Run(ctx, execx.Cmd{
				Name:   "kind",
				Args:   []string{"get", "kubeconfig", "--name", kindCluster},
				Stdout: &out, Stderr: io.Discard,
			})
			_ = writeKubeconfigFile(mgmtKubeconfig, out.String())
		}
	}

	// A management kubeconfig we cannot find is a destroy we cannot
	// perform. Without this guard the delete below is silently SKIPPED, the
	// workload kubeconfig is removed anyway, and the function returns
	// success — the same silent-success class as the delete guard at the
	// bottom, just entered one step earlier. The remote case always
	// hard-fails. The local case is exempt ONLY when the workload
	// kubeconfig is gone too: a missing mgmt kubeconfig AND a missing
	// workload kubeconfig is what a completed destroy looks like (re-runs
	// of 'lo down' stay idempotent) — but a workload kubeconfig still on
	// disk means the previous destroy never finished, and reporting success
	// here would delete the only handle while Hetzner servers may still be
	// billing.
	if !fileExists(mgmtKubeconfig) {
		if mgmtLocal != "true" {
			ui.Errorf(stderr, "management kubeconfig %s not found — cannot reach management cluster %s to delete workload cluster %s", mgmtKubeconfig, mgmtDomain, clusterName)
			ui.Errorf(stderr, "  KEEPING %s — Hetzner servers and load balancer may still be running and billing", workloadKubeconfig)
			ui.Errorf(stderr, "  restore the management cluster kubeconfig, then re-run 'lo down'")
			return fmt.Errorf("capi: management kubeconfig %s not found", mgmtKubeconfig)
		} else if fileExists(workloadKubeconfig) {
			ui.Errorf(stderr, "local management kubeconfig is gone but workload cluster %s still has a kubeconfig — the previous destroy never completed", clusterName)
			ui.Errorf(stderr, "  KEEPING %s — Hetzner servers and load balancer may still be running and billing", workloadKubeconfig)
			ui.Errorf(stderr, "  recreate the management cluster ('lo up' on %s) and re-run 'lo down', or clean up via 'hcloud server list'", mgmtDomain)
			return fmt.Errorf("capi: previous destroy of %s never completed", clusterName)
		}
	}

	// Delete the workload Cluster and BLOCK until CAPH has deprovisioned
	// the Hetzner servers + load balancer (finalizers complete). Deleting
	// the management cluster before this finishes would orphan billed
	// infrastructure.
	var delErr error
	if fileExists(mgmtKubeconfig) {
		d.infoLine("deleting workload cluster %s — waiting for Hetzner teardown (up to 10m)", clusterName)
		delErr = d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "kubectl",
			Args: []string{
				"delete", "cluster", clusterName,
				"--namespace", namespace,
				"--kubeconfig", mgmtKubeconfig,
				"--ignore-not-found=true",
				"--wait=true", "--timeout=600s",
			},
			Stdout: stderr, // bash: >&2
		})
	}

	// Tear down the local kind management cluster we created — but only
	// once the workload teardown actually completed, so CAPH can finish if
	// it is still deleting servers.
	if mgmtLocal == "true" && delErr == nil {
		kindName := MgmtKindName(mgmtDomain)
		d.infoLine("deleting local kind management cluster '%s'", kindName)
		ensureEnvDefault("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s-example")
		_ = d.deps.Runner.Run(ctx, execx.Cmd{
			Name:   "kind",
			Args:   []string{"delete", "cluster", "--name", kindName},
			Stderr: io.Discard, // bash: 2>/dev/null || true
		})
		_ = os.Remove(mgmtKubeconfig)
	}

	// A failed teardown must not report success, and must not delete any
	// handle: without this block a timed-out delete fell through to the
	// infallible `rm -f`, returned 0, and main::down suppressed its
	// orphaned-infra warning (the case the dispatch's rc=3 remap exists to
	// preserve) — while CAPH may still be deprovisioning, or may have given
	// up with servers and a load balancer still billing, and the kubeconfig
	// that could reach them was gone. This is the ONE place that reports
	// the failure — mgmt-local only adds its extra line here.
	if delErr != nil {
		ui.Errorf(stderr, "workload cluster delete did not complete (rc=%d) — Hetzner servers and load balancer may still be running and billing", driver.ExitCode(delErr))
		ui.Errorf(stderr, "  KEEPING %s so the cluster stays reachable", workloadKubeconfig)
		if mgmtLocal == "true" {
			ui.Errorf(stderr, "  KEEPING the local kind management cluster so CAPH can finish deprovisioning")
		}
		ui.Errorf(stderr, "  check 'hcloud server list' and re-run 'lo down', or inspect: kubectl --kubeconfig %s get cluster,machine -A", mgmtKubeconfig)
		return fmt.Errorf("capi: workload cluster delete did not complete: %w", delErr)
	}

	_ = os.Remove(workloadKubeconfig)
	return nil
}

// Status reports the cluster status word (bash: driver::status).
func (d *Driver) Status(ctx context.Context, domain string) (string, error) {
	spec := loadSpec(d.clusterYAML(domain))
	mgmtDomain := spec.or("", "spec", "managementCluster", "domain")
	clusterName := spec.raw("metadata", "name")

	if mgmtDomain == "" {
		return "Unknown", nil
	}

	var out strings.Builder
	err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "kubectl",
		Args: []string{
			"get", "cluster", clusterName,
			"--kubeconfig", d.kubeconfigPath(mgmtDomain),
			"-o", "jsonpath={.status.phase}",
		},
		Stdout: &out,
		Stderr: io.Discard, // bash: 2>/dev/null
	})
	if err != nil {
		return "NotFound", nil
	}
	return out.String(), nil
}

// Kubeconfig returns the standard kubeconfig path (bash:
// driver::kubeconfig) — written under the workload cluster's metadata.name
// (see Provision).
func (d *Driver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	spec := loadSpec(d.clusterYAML(domain))
	return d.kubeconfigPath(spec.raw("metadata", "name")), nil
}

// EnsureCredentials is the optional bash contract function
// driver::ensure_credentials: the env-var gate first, then the Secret
// upsert (delegating to EnsureCredentialsSecret, which repeats the gate —
// exactly the double check the bash performed).
func (d *Driver) EnsureCredentials(ctx context.Context, clusterYAML, provider, kubeconfig string) error {
	if err := requireCredentials(provider, d.stderr()); err != nil {
		return err
	}
	return d.EnsureCredentialsSecret(ctx, clusterYAML, provider, kubeconfig)
}

// ── Local management cluster ──────────────────────────────

// MgmtKindName ports capi::mgmt_kind_name: the deterministic kind cluster
// name for a management domain (kind forbids dots). ensureLocalMgmt
// (create) and Destroy (delete) must agree, so the name is derived in one
// place. e.g. capi-mgmt.lok8s.dev -> capi-mgmt-lok8s-dev
func MgmtKindName(domain string) string {
	return strings.Map(func(r rune) rune {
		if r == '.' || r == '_' {
			return '-'
		}
		return r
	}, domain)
}

// kindClusterExists mirrors `kind get clusters 2>/dev/null | grep -qx name`.
func (d *Driver) kindClusterExists(ctx context.Context, name string) bool {
	var out strings.Builder
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "kind", Args: []string{"get", "clusters"},
		Stdout: &out, Stderr: io.Discard,
	}); err != nil {
		return false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if line == name {
			return true
		}
	}
	return false
}

// ensureLocalMgmt ports capi::ensure_local_mgmt: create (or reuse) a LOCAL
// kind cluster as the CAPI management cluster and install Cluster API + the
// infra provider on it. Used when the cluster spec sets
// managementCluster.local: true — the cheap, self-contained dev model (kind
// management locally, real workload nodes on the cloud provider).
func (d *Driver) ensureLocalMgmt(ctx context.Context, mgmtDomain, provider string) error {
	stderr := d.stderr()
	mgmtKubeconfig := d.kubeconfigPath(mgmtDomain)
	kindName := MgmtKindName(mgmtDomain)

	var infra, infraVersion string
	switch provider {
	case "hetzner":
		infra, infraVersion = "hetzner", localMgmtHetznerVersion
	default:
		ui.Errorf(stderr, "local management cluster: unsupported provider '%s'", provider)
		return fmt.Errorf("capi: local management cluster: unsupported provider %q", provider)
	}

	d.infoLine("creating local kind management cluster '%s'", kindName)
	ensureEnvDefault("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s-example")
	if !d.kindClusterExists(ctx, kindName) {
		if err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name:   "kind",
			Args:   []string{"create", "cluster", "--name", kindName},
			Stdout: stderr, // bash: >&2
		}); err != nil {
			// bash: unguarded under the caller's disabled errexit — the
			// create failure flows on; the kubeconfig read below then
			// captures nothing and clusterctl init reports the real state.
			ui.Debugf(stderr, "kind create cluster failed: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
		return err
	}
	var kc strings.Builder
	// Unguarded in the bash (errexit disabled): the redirect writes the
	// file whatever the exit status.
	_ = d.deps.Runner.Run(ctx, execx.Cmd{
		Name:   "kind",
		Args:   []string{"get", "kubeconfig", "--name", kindName},
		Stdout: &kc,
	})
	if err := writeKubeconfigFile(mgmtKubeconfig, kc.String()); err != nil {
		return err
	}

	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "kubectl",
		Args: []string{
			"--kubeconfig", mgmtKubeconfig, "-n", "capi-system",
			"get", "deployment", "capi-controller-manager",
		},
		Stdout: io.Discard, Stderr: io.Discard, // bash: &>/dev/null
	}); err == nil {
		d.infoLine("Cluster API already installed on '%s'", kindName)
		return nil
	}
	d.infoLine("installing Cluster API %s + %s %s (clusterctl init)", localMgmtCAPIVersion, infra, infraVersion)
	// The function's exit status IS clusterctl init's (its last command).
	return d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "clusterctl",
		Args: []string{
			"init",
			"--kubeconfig", mgmtKubeconfig,
			"--core", "cluster-api:" + localMgmtCAPIVersion,
			"--bootstrap", "kubeadm:" + localMgmtCAPIVersion,
			"--control-plane", "kubeadm:" + localMgmtCAPIVersion,
			"--infrastructure", infra + ":" + infraVersion,
			"--wait-providers",
		},
		Stdout: stderr, // bash: >&2
	})
}

// ── Management cluster bootstrap ─────────────────────────

// Bootstrap ports capi::bootstrap: bootstrap a management cluster from
// scratch. Flow: temp kind cluster -> clusterctl init -> apply mgmt CR ->
// wait -> install operator + CAPI on mgmt -> clusterctl move -> cleanup.
//
// NOTE on gating (from the bash): bootstrap ran under callers' `|| return
// 1`, so every step carries its own gate. On any failure the bootstrap kind
// cluster is KEPT: it may hold the only copy of the CAPI resources (before
// a successful move it always does), and keeping it makes the bootstrap
// re-entrant (step 1 reuses an existing cluster).
//
// The messages here are the bash's RAW `echo "error: …" >&2` family, NOT
// the colored [error] helper — the port keeps both families distinct.
func (d *Driver) Bootstrap(ctx context.Context, domain string) error {
	cy := d.clusterYAML(domain)
	bootstrapKubeconfig := d.kubeconfigPath(bootstrapName)
	mgmtKubeconfig := d.kubeconfigPath(domain)

	// Detect provider for the infrastructure flag. Unguarded in the bash
	// (errexit off): a failed detection leaves the provider empty and falls
	// into the unsupported-provider refusal below, after the detector's own
	// diagnostic.
	provider, _ := d.DetectProvider(cy)

	var infraProvider string
	switch provider {
	case "hetzner":
		infraProvider = "hetzner"
	case "aws":
		infraProvider = "aws"
	default:
		d.rawErrorLine("unsupported provider for bootstrap: %s", provider)
		return fmt.Errorf("capi: unsupported provider for bootstrap: %s", provider)
	}

	// 1. Create temporary kind bootstrap cluster.
	d.infoLine("creating bootstrap cluster")
	ensureEnvDefault("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s")
	if !d.kindClusterExists(ctx, bootstrapName) {
		if err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "kind", Args: []string{"create", "cluster", "--name", bootstrapName},
		}); err != nil {
			d.rawErrorLine("bootstrap kind cluster create failed")
			return fmt.Errorf("capi: bootstrap kind cluster create failed: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
		return err
	}
	var bkc strings.Builder
	err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name:   "kind",
		Args:   []string{"get", "kubeconfig", "--name", bootstrapName},
		Stdout: &bkc,
	})
	// The redirect writes the file whatever the exit status (bash).
	if werr := writeKubeconfigFile(bootstrapKubeconfig, bkc.String()); werr != nil {
		return werr
	}
	if err != nil {
		d.rawErrorLine("cannot read bootstrap kubeconfig")
		return fmt.Errorf("capi: cannot read bootstrap kubeconfig: %w", err)
	}

	// 2. Install CAPI core + infrastructure provider on bootstrap.
	d.infoLine("installing CAPI on bootstrap cluster")
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "clusterctl",
		Args: []string{
			"init",
			"--kubeconfig", bootstrapKubeconfig,
			"--infrastructure", infraProvider,
			"--core", "cluster-api",
			"--wait-providers",
		},
	}); err != nil {
		d.rawErrorLine("clusterctl init on bootstrap cluster failed")
		return fmt.Errorf("capi: clusterctl init on bootstrap cluster failed: %w", err)
	}

	// 3. Ensure credentials on bootstrap cluster.
	if err := d.EnsureCredentialsSecret(ctx, cy, provider, bootstrapKubeconfig); err != nil {
		d.rawErrorLine("provider credentials setup on bootstrap cluster failed")
		return fmt.Errorf("capi: provider credentials setup on bootstrap cluster failed: %w", err)
	}

	// 4. Generate and apply management cluster CAPI resources.
	d.infoLine("applying management cluster resources")
	resources, err := d.Generate(cy, provider)
	if err != nil {
		d.rawErrorLine("CAPI resource generation failed")
		return fmt.Errorf("capi: CAPI resource generation failed: %w", err)
	}
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name:  "kubectl",
		Args:  []string{"apply", "--kubeconfig", bootstrapKubeconfig, "-f", "-"},
		Stdin: strings.NewReader(resources),
	}); err != nil {
		d.rawErrorLine("applying management cluster resources failed")
		return fmt.Errorf("capi: applying management cluster resources failed: %w", err)
	}

	// 5. Wait for management cluster to become ready.
	spec := loadSpec(cy)
	clusterName := spec.raw("metadata", "name")
	d.infoLine("waiting for management cluster to become ready")
	if err := d.WaitReady(ctx, bootstrapKubeconfig, clusterName, "", 0); err != nil {
		d.rawErrorLine("management cluster %s did not become ready", clusterName)
		return fmt.Errorf("capi: management cluster %s did not become ready: %w", clusterName, err)
	}

	// 6. Extract management cluster kubeconfig.
	var mkc strings.Builder
	err = d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "clusterctl",
		Args: []string{
			"get", "kubeconfig", clusterName,
			"--kubeconfig", bootstrapKubeconfig,
		},
		Stdout: &mkc,
	})
	if werr := writeKubeconfigFile(mgmtKubeconfig, mkc.String()); werr != nil {
		return werr
	}
	if err != nil {
		d.rawErrorLine("cannot extract management cluster kubeconfig")
		return fmt.Errorf("capi: cannot extract management cluster kubeconfig: %w", err)
	}

	// 7. Install lok8s operator on management cluster (best-effort:
	// `2>/dev/null || true` in the bash).
	d.infoLine("installing lok8s operator on management cluster")
	if deployDir := filepath.Join(d.deps.Paths.Base, "operator", "deploy"); dirExists(deployDir) {
		_ = d.deps.Runner.Run(ctx, execx.Cmd{
			Name:   "kubectl",
			Args:   []string{"apply", "--kubeconfig", mgmtKubeconfig, "-f", deployDir + "/"},
			Stderr: io.Discard,
		})
	}

	// 8. Install CAPI + providers on management cluster.
	d.infoLine("installing CAPI on management cluster")
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "clusterctl",
		Args: []string{
			"init",
			"--kubeconfig", mgmtKubeconfig,
			"--infrastructure", infraProvider,
			"--core", "cluster-api",
			"--wait-providers",
		},
	}); err != nil {
		d.rawErrorLine("clusterctl init on management cluster failed")
		return fmt.Errorf("capi: clusterctl init on management cluster failed: %w", err)
	}

	// 9. Move CAPI resources from bootstrap to management cluster. A failed
	// move MUST keep the bootstrap cluster — at this point it still owns
	// the CAPI resources; deleting it would destroy the only record of the
	// just-provisioned infrastructure.
	d.infoLine("moving CAPI resources to management cluster")
	if err := d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "clusterctl",
		Args: []string{
			"move",
			"--kubeconfig", bootstrapKubeconfig,
			"--to-kubeconfig", mgmtKubeconfig,
		},
	}); err != nil {
		d.rawErrorLine("clusterctl move failed — bootstrap cluster %s kept (it still owns the CAPI resources); re-run bootstrap after fixing", bootstrapName)
		return fmt.Errorf("capi: clusterctl move failed: %w", err)
	}

	// 10. Delete bootstrap cluster (only after a successful move).
	d.infoLine("cleaning up bootstrap cluster")
	_ = d.deps.Runner.Run(ctx, execx.Cmd{
		Name:   "kind",
		Args:   []string{"delete", "cluster", "--name", bootstrapName},
		Stderr: io.Discard, // bash: 2>/dev/null || true
	})
	_ = os.Remove(bootstrapKubeconfig)

	d.infoLine("management cluster %s bootstrapped successfully", domain)
	return nil
}

// ── helpers ───────────────────────────────────────────────

// ensureEnvDefault mirrors `export VAR="${VAR:-default}"`: set only when
// unset or empty.
func ensureEnvDefault(name, def string) {
	if os.Getenv(name) == "" {
		os.Setenv(name, def)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func fileNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
