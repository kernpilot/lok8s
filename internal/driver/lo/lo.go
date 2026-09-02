package lo

// lo.go — the driver contract implementation (.lok8s/drivers/lo/main):
// driver::provision, driver::export, driver::destroy, driver::status,
// driver::kubeconfig.
//
// EVERY step of Provision is explicitly guarded — the bash tree runs under
// libs/provision's `driver::provision || provision_rc=$?`, which disables
// errexit for the whole call, and the branch used to END on the advisory
// TLS nudge (returns 0 on every path), so every unguarded failure below was
// reported as a provisioned cluster (issue #91's family; see the per-step
// comments). Go has no errexit to lose, but the comments stay: they are the
// record of WHY each guard exists, and the ordering they pin is contractual.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Name is the driver's registry name (spec `kind: Lo`).
const Name = "lo"

func init() {
	driver.Register(Name, func(deps *driver.Deps) (driver.Driver, error) {
		return New(deps), nil
	})
}

// Hooks are the driver's injectable seams. nil = "not wired yet", exactly
// like the bash `declare -F` probes.
type Hooks struct {
	// KustomizeBuild is kustomize::build — builds the secrets plugin binary
	// on demand when registry TLS needs it and it is missing. nil = no
	// on-demand build; the mint fails with the build-it-yourself guidance.
	KustomizeBuild func(ctx context.Context) error
}

// Driver is the Lo (kind) driver. Implements driver.Driver and
// driver.Exporter.
type Driver struct {
	deps  *driver.Deps
	Hooks Hooks

	// sleep is the wait seam (tests stub it; prod = time.Sleep). The remote
	// waits and the registry retries depend on it.
	sleep func(time.Duration)

	// stdout is where progress phases print off-capture; defaults to
	// os.Stdout.
	stdout io.Writer
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

func (d *Driver) out() io.Writer {
	if d.stdout != nil {
		return d.stdout
	}
	return os.Stdout
}

// SetOutput redirects the driver's progress stdout (tests).
func (d *Driver) SetOutput(w io.Writer) { d.stdout = w }

func (d *Driver) sleepSeconds(n int) { d.sleep(time.Duration(n) * time.Second) }

func (d *Driver) clusterYAML(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
}

// Provision provisions a Lo cluster (bash: driver::provision).
func (d *Driver) Provision(ctx context.Context, domain string) error {
	stderr := d.stderr()
	stdout := d.out()
	cy := d.clusterYAML(domain)

	root := loadYAML(cy)
	runtime := ""
	if root != nil {
		runtime = yqOr(root, "kind", "spec", "runtime")
	}

	if runtime != "kind" {
		// Raw `error:` family, verbatim. A missing/unparsable spec lands
		// here with an EMPTY runtime, exactly like the bash under a failed
		// yq capture (errexit suppressed by the dispatch guard).
		fmt.Fprintf(stderr, "error: unsupported Lo runtime: %s\n", runtime)
		return fmt.Errorf("unsupported Lo runtime: %s", runtime)
	}

	clusterName := yqRaw(root, "metadata", "name")
	k8sVersion := yqRaw(root, "spec", "kubernetes", "version")

	// Remote: provision VM, wait for SSH/Docker, set DOCKER_HOST.
	if d.deps.ProviderName != "" && d.deps.Provider != nil {
		// Guarded: errexit is suppressed in the bash original (this tree
		// runs under provision's `driver::provision || rc=$?`), so an
		// unreachable remote (SSH/Docker never came up) would otherwise
		// FALL THROUGH into the local kind path below and report a
		// provisioned cluster — the wrong cluster, on the wrong machine. (A
		// provider that yields no nodes returns nil with an explicit
		// "running kind locally" — that intentional fallback is unaffected.)
		if err := d.provisionRemote(ctx, domain, cy, stderr); err != nil {
			ui.Errorf(stderr, "remote provision via provider '%s' failed — refusing to fall back to a local kind cluster", d.deps.ProviderName)
			return fmt.Errorf("remote provision failed: %w", err)
		}
	}

	// Remote CI mode: delegate everything to the VM, skip local
	// post-provision.
	if getenv("LOK8S_REMOTE") == "1" {
		// Guarded: without the explicit return, a failed config read (e.g.
		// rejected sync.dest) would fall through into remoteCI and
		// ssh/rsync with the bad value anyway.
		if err := readRemoteConfig(cy, remoteDeps{providerName: d.deps.ProviderName}, stderr); err != nil {
			return err
		}
		if getenv("LOK8S_REMOTE_MODE") == "ci" && getenv("LOK8S_REMOTE_IP") != "" {
			// ErrFullLifecycle (bash rc 100) is the "remote CI handled the
			// FULL lifecycle" success sentinel (the dispatch maps it to
			// success). A failed remoteCI (ssh/rsync/remote provision) must
			// surface as a FAILURE, not be relabeled as that sentinel.
			if err := d.remoteCI(ctx, domain, cy, stdout, stderr); err != nil {
				return err
			}
			return driver.ErrFullLifecycle
		}
	}

	// Read all config from the cluster spec.
	if err := readConfig(cy, stderr); err != nil {
		return err
	}
	// Guarded: validateIPs prints "N IP validation error(s). Aborting." and
	// fails — in the bash, without the guard the run continued past its own
	// abort message and built the networks, registry TLS and containerd
	// config for a REJECTED config.
	if err := validateIPs(getenv("LOK8S_NETWORK_SUBNET"), getenv("LOK8S_LB_POOL"), stderr); err != nil {
		return err
	}

	network := getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK")

	// Docker networks.
	// Every step in this branch is guarded: the bash branch used to END on
	// lo::registries_tls_nudge — an advisory check about the host trust
	// store that returns 0 on every path. Its status became the function's,
	// and every failure below was reported as a provisioned cluster.
	if err := d.network(ctx, stderr); err != nil {
		return err
	}
	if d.isShared() {
		if err := d.registryNetwork(ctx, stderr); err != nil {
			return err
		}
	}

	// Registry TLS cert — mint it (via the Secret plugin) before the
	// registry containers start (they mount it) and before certs.d is
	// written (it references the dev CA). No-op unless spec.registries.tls.
	if err := d.registriesTLSCert(ctx, stderr); err != nil {
		return err
	}

	// Registries + containerd config (must exist before kind create).
	// kapply.Run collapses the per-registry "registry/<name> <verb>" lines
	// into the same named progress block the cluster-service phases get.
	if err := kapply.Run("registries", stdout, stderr, func(out, errOut io.Writer) error {
		return d.registries(ctx, out, errOut, domain, cy)
	}); err != nil {
		return err
	}
	if err := d.writeCertsD(stderr); err != nil {
		return err
	}

	// apiserver StructuredAuthenticationConfiguration (spec.oidc) — write
	// the host file before `kind create` so node 0's extraMount has a
	// target. No-op unless OIDC is enabled (no spec.oidc ⇒ kind config
	// byte-identical to today).
	if err := d.writeOIDCAuthConfig(domain, stderr); err != nil {
		return err
	}

	// Kind cluster.
	// Guarded AND checked for emptiness: this is issue #91's own shape — an
	// unguarded assignment whose failure leaves the variable empty, and the
	// empty value then flows into `kind create --config <(echo "")`.
	renderedConfig := d.renderKindConfig(clusterName, k8sVersion, network, cy)
	if strings.TrimSpace(renderedConfig) == "" {
		ui.Errorf(stderr, "the kind config for %s rendered EMPTY — refusing to create a cluster from it", clusterName)
		return fmt.Errorf("kind config rendered empty for %s", clusterName)
	}
	os.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", network)
	if !d.kindClusterExists(ctx, clusterName) {
		// Bash fed the config via <(echo …) — a /dev/fd file; the Go
		// equivalent is a temp file (kind reads it once at create).
		tmp, err := os.CreateTemp("", "lok8s-kind-config-*.yaml")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		if _, err := tmp.WriteString(renderedConfig); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := d.runOut(ctx, stdout, stderr, "kind", "create", "cluster",
			"--name", clusterName, "--config", tmp.Name()); err != nil {
			return err
		}
	}

	if d.isShared() {
		if err := d.connectNodesToRegistryNetwork(ctx, clusterName); err != nil {
			return err
		}
	}

	// Cluster services — each wrapped in a named, collapsing progress
	// phase. (Called for its side effect — writing the kubeconfig file; the
	// returned path is the return value for Kubeconfig, discarded here.)
	nodeKubeconfig, err := d.extractKubeconfig(ctx, domain, stderr)
	if err != nil {
		return err
	}

	// Repair any node kubelet pinned to the wrong docker network's address.
	// The registry NIC (attached above) is the only thing that dual-homes a
	// node, so single-network clusters cannot drift — but gate on NIC
	// MEMBERSHIP, not just the spec: after the shared→per-project default
	// flip, an already-drifted cluster whose spec omits shared.enabled
	// reads "not shared" while its nodes still carry the registry NIC. The
	// heal must run AFTER the attach (it is what perturbs the address
	// resolution) and after the kubeconfig exists (the CNI nudge + latch-up
	// check need it). No-op when every node already agrees. Advisory: a
	// node we cannot reach into is not a reason to fail the provision.
	if d.isShared() || d.nodesOnRegistryNetwork(ctx, clusterName) {
		_ = d.healNodeIPs(ctx, clusterName, nodeKubeconfig, stderr)
	}

	if err := kapply.Run("registries", stdout, stderr, func(out, errOut io.Writer) error {
		return d.applyLocalRegistryHosting(ctx, out, errOut, domain)
	}); err != nil {
		return err
	}
	if err := kapply.Run("registries", stdout, stderr, func(out, errOut io.Writer) error {
		return d.registryConfigmap(ctx, out, errOut, domain, cy)
	}); err != nil {
		return err
	}
	if err := kapply.Run("coredns", stdout, stderr, func(out, errOut io.Writer) error {
		return d.coredns(ctx, out, errOut, domain)
	}); err != nil {
		return err
	}

	if getenv("LOK8S_REMOTE_EXPOSE") == "true" {
		if err := d.expose(ctx, clusterName, cy, stdout, stderr); err != nil {
			return err
		}
	}

	// If registry TLS is on (default) but the dev CA isn't trusted yet,
	// nudge the user to `lo trust` — host `docker push` validates against
	// it. Advisory only — deliberately NOT able to fail the provision: not
	// trusting the dev CA is a host-setup nag, not a failed provision. The
	// explicit `return nil` below is the Go spelling of the bash `return 0`
	// that stops the nudge's status from becoming this function's verdict.
	d.registriesTLSNudge(ctx, stderr)
	return nil
}

// isShared reads the shared bit from the live .registries.json (bash:
// registry::is_shared — a per-call jq, never a cached global).
func (d *Driver) isShared() bool {
	rf, err := regFile()
	return err == nil && rf.Shared
}

func (d *Driver) kindClusterExists(ctx context.Context, clusterName string) bool {
	clusters, _ := d.output(ctx, "kind", "get", "clusters")
	for _, line := range strings.Split(clusters, "\n") {
		if line == clusterName {
			return true
		}
	}
	return false
}

// Export is driver::export — spec-derived env consumed by spec.bootstrap
// addons (LOK8S_SPEC_*). Idempotent (safe to re-run; readConfig also
// (re)writes the derived registry map — benign). Called by the dispatch on
// BOTH the full provision and `--bootstrap` paths, so a re-applied
// bootstrap graph renders with the same env a fresh provision would set.
func (d *Driver) Export(ctx context.Context, domain string) error {
	cy := d.clusterYAML(domain)
	// readConfig first: exportSpecEnvs derives LOK8S_SPEC_LOADBALANCER_POOL
	// from LOK8S_LB_POOL (set by readLBConfig), so on --bootstrap (where
	// Provision — which normally calls readConfig — is skipped) the metallb
	// pool would otherwise render empty.
	if err := readConfig(cy, d.stderr()); err != nil {
		return err
	}
	return exportSpecEnvs(cy, d.stderr())
}

// Destroy tears down a Lo cluster (bash: driver::destroy).
func (d *Driver) Destroy(ctx context.Context, domain string) error {
	cy := d.clusterYAML(domain)
	clusterName := yqRaw(loadYAML(cy), "metadata", "name")
	if clusterName == "null" {
		clusterName = ""
	}

	// Remote mode: reconnect DOCKER_HOST.
	if d.deps.ProviderName != "" && d.deps.Provider != nil {
		if providerOutput, err := d.deps.Provider.Output(ctx, d.deps.ProviderConfigFile); err == nil {
			if remoteIP, remoteUser := providerNode0(providerOutput); remoteIP != "" {
				os.Setenv("DOCKER_HOST", "ssh://"+remoteUser+"@"+remoteIP)
			}
		}
	}

	// Read config so we know which containers to clean up (best-effort —
	// bash: 2>/dev/null || true).
	_ = readNetworkConfig(cy, io.Discard)

	// bash: `kind delete cluster … 2>/dev/null || true` — stdout (kind's
	// own "Deleting cluster …" line) passes through, stderr is dropped.
	_ = d.runOut(ctx, d.out(), io.Discard, "kind", "delete", "cluster", "--name", clusterName)
	d.cleanupRegistries(ctx, clusterName)
	_ = d.runQuiet(ctx, "docker", "rm", "-f", clusterName+"-proxy")

	// Destroy the cloud VM after the cluster is gone.
	if d.deps.ProviderName != "" && d.deps.Provider != nil {
		workDir := filepath.Join(d.deps.Paths.Clusters, domain, ".provider")
		os.Unsetenv("DOCKER_HOST")
		if err := d.deps.Provider.Destroy(ctx, d.deps.ProviderConfigFile, workDir); err != nil {
			return err
		}
	}
	return nil
}

// Status reports "Running"/"NotFound" (bash: driver::status).
func (d *Driver) Status(ctx context.Context, domain string) (string, error) {
	clusterName := yqRaw(loadYAML(d.clusterYAML(domain)), "metadata", "name")
	if clusterName == "null" {
		clusterName = ""
	}
	if d.kindClusterExists(ctx, clusterName) {
		return "Running", nil
	}
	return "NotFound", nil
}

// extractKubeconfig writes .kubeconfig/<name>.yaml from kind and optionally
// tunnels it (bash: _lo_extract_kubeconfig). Returns the path.
func (d *Driver) extractKubeconfig(ctx context.Context, domain string, errOut io.Writer) (string, error) {
	clusterName := yqRaw(loadYAML(d.clusterYAML(domain)), "metadata", "name")
	if clusterName == "null" {
		clusterName = ""
	}
	kubeconfigPath := filepath.Join(d.deps.Paths.Base, ".kubeconfig", clusterName+".yaml")

	if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
		return "", err
	}
	kubeconfig, err := d.output(ctx, "kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig+"\n"), 0o600); err != nil {
		return "", err
	}

	if remoteIP := getenv("LOK8S_REMOTE_IP"); remoteIP != "" {
		if err := d.kubeconfigTunnel(ctx, kubeconfigPath, getenv("LOK8S_REMOTE_USER"), remoteIP, errOut); err != nil {
			return "", err
		}
	}
	return kubeconfigPath, nil
}

// Kubeconfig extracts the kubeconfig (bash: driver::kubeconfig — prints the
// path; the Go contract returns it).
func (d *Driver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	return d.extractKubeconfig(ctx, domain, d.stderr())
}

// Ensure interface compliance.
var (
	_ driver.Driver   = (*Driver)(nil)
	_ driver.Exporter = (*Driver)(nil)
)
