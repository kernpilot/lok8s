// Package kubeone is the Go port of the KubeOne driver
// (.lok8s/drivers/kubeone/{main,config}): standalone production clusters
// via the kubeone CLI, which stays an exec (the binary owns the node walk).
// Infrastructure is provisioned by the provider contract; KubeOne installs
// Kubernetes on the provisioned nodes.
//
// Work directory: clusters/<domain>/.kubeone/ (kubeone.yaml + the legacy
// output.json slot).
//
// The hosted-kubehz branches (kubehz::provision_hosted /
// kubehz::destroy_hosted), the manifest inventory append
// (_append_inventory) and the pre-apply steps (_clean_reinstalled_workers,
// _name_robot_workers, render_addons) are injectable Hooks — nil means
// "not wired yet", exactly like the dispatch tail hooks.
package kubeone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Name is the driver's registry name (spec `kind: KubeOne`).
const Name = "kubeone"

func init() {
	driver.Register(Name, func(deps *driver.Deps) (driver.Driver, error) {
		return New(deps), nil
	})
}

// Hooks are the driver's injectable seams. nil = "not wired yet".
type Hooks struct {
	// ReadKubehzConfig is kubehz::read_config: reads spec.kubehz and
	// returns the hosting mode ("hosted", "self-hosted", ""). nil = the
	// kubehz lib is not wired → the hosted branch never fires.
	ReadKubehzConfig func(clusterYAML string) (hosting string, err error)
	// ProvisionHosted is kubehz::provision_hosted (delegate to the kubehz
	// API). Required once ReadKubehzConfig reports "hosted".
	ProvisionHosted func(ctx context.Context, domain, clusterYAML string) error
	// DestroyHosted is kubehz::destroy_hosted — same rules. Without it a
	// HOSTED cluster must never get the self-hosted teardown (the wrong
	// destroy entirely), so a hosted hosting mode with a nil hook errors.
	DestroyHosted func(ctx context.Context, domain, clusterYAML string) error
	// AppendInventory is _append_inventory: appends apiEndpoint +
	// controlPlane.hosts + staticWorkers.hosts to the rendered
	// kubeone.yaml from the provider's descriptor-anchored inventory
	// (2026-07-10 incident: hosts anchor to server[].name, never label
	// discovery). Ports with the hetzner provider in a later task.
	AppendInventory func(ctx context.Context, configFile, manifest string) error
	// PrepareApply covers the bash pre-apply steps run inside
	// kubeone::apply: _clean_reinstalled_workers (fail-ignored there),
	// _name_robot_workers and render_addons (both abort the apply).
	PrepareApply func(ctx context.Context, workDir, clusterYAML string) error
}

// Driver is the KubeOne driver. Implements driver.Driver and
// driver.Exporter.
type Driver struct {
	deps  *driver.Deps
	Hooks Hooks
}

// New builds the driver over its dispatch-provided dependencies.
func New(deps *driver.Deps) *Driver { return &Driver{deps: deps} }

func (d *Driver) stderr() io.Writer {
	if d.deps.Stderr != nil {
		return d.deps.Stderr
	}
	return os.Stderr
}

func (d *Driver) clusterYAML(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, "cluster.lok8s.yaml")
}

func (d *Driver) workDir(domain string) string {
	return filepath.Join(d.deps.Paths.Clusters, domain, ".kubeone")
}

// hosted evaluates the kubehz hosting branch (bash: kubehz::read_config →
// LOK8S_KUBEHZ_HOSTING == "hosted"; the read is `|| return 1` guarded).
func (d *Driver) hosted(clusterYAML string) (bool, error) {
	if d.Hooks.ReadKubehzConfig == nil {
		return false, nil
	}
	hosting, err := d.Hooks.ReadKubehzConfig(clusterYAML)
	if err != nil {
		return false, err
	}
	return hosting == "hosted", nil
}

// Provision provisions a KubeOne cluster (bash: driver::provision).
func (d *Driver) Provision(ctx context.Context, domain string) error {
	cy := d.clusterYAML(domain)

	// Hosted path: delegate to the kubehz API.
	hosted, err := d.hosted(cy)
	if err != nil {
		return err
	}
	if hosted {
		if d.Hooks.ProvisionHosted == nil {
			return errors.New("kubeone: hosted provisioning is not wired (Hooks.ProvisionHosted)")
		}
		return d.Hooks.ProvisionHosted(ctx, domain, cy)
	}

	workDir := d.workDir(domain)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	// 1. The provider is loaded + validated by the dispatch.
	if d.deps.Provider == nil {
		ui.Errorf(d.stderr(), "KubeOne driver requires spec.provider (no provider loaded)")
		return errors.New("kubeone: no provider loaded")
	}

	// 2. Provision infrastructure via the provider contract. Guarded:
	// without it a failed INFRASTRUCTURE reconcile would fall through to
	// the manifest render and `kubeone apply` against hosts that were
	// never created (issue #91's family).
	if err := d.deps.Provider.Provision(ctx, d.deps.ProviderConfigFile, workDir); err != nil {
		return err
	}

	// 3. Generate kubeone.yaml.
	if err := d.GenerateConfig(ctx, cy, d.deps.ProviderName, workDir); err != nil {
		return err
	}

	// 4. Append the host inventory to the manifest itself (manifest fields
	// are versioned + strictly validated by kubeone — unlike the legacy
	// tfjson envelope, whose schema drifted under us).
	os.Remove(filepath.Join(workDir, "output.json"))
	if d.Hooks.AppendInventory != nil {
		if err := d.Hooks.AppendInventory(ctx, d.deps.ProviderConfigFile, filepath.Join(workDir, "kubeone.yaml")); err != nil {
			return err
		}
	}

	// 5. Run kubeone apply. Guarded — and this is the one that actually
	// bit: the kubeconfig check below decided the function's exit status,
	// and on a worker re-join the file already existed, so a failed apply
	// was masked by a stale kubeconfig (issue #91).
	if err := d.Apply(ctx, workDir, cy); err != nil {
		return err
	}

	// 6. Copy the kubeconfig to the standard location, named by
	// metadata.name (framework convention — bootstrap reads it).
	clusterName := metadataName(cy)
	src := KubeconfigPath(workDir, clusterName)
	if !fileExists(src) {
		ui.Errorf(d.stderr(), "Kubeconfig not found at %s after kubeone apply", src)
		return fmt.Errorf("kubeone: kubeconfig not found at %s", src)
	}
	dstDir := filepath.Join(d.deps.Paths.Base, ".kubeconfig")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, clusterName+".yaml")
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return err
	}
	// chmod even when the file pre-existed with looser bits (bash: cp then
	// chmod 600).
	return os.Chmod(dst, 0o600)
}

// Export is driver::export: the spec-derived env consumed by spec.bootstrap
// addons (LOK8S_SPEC_*). Reuses ExtractVars (pure spec→env; the extra
// kubeone-config vars it also sets are harmless). Idempotent — the dispatch
// calls it on BOTH the full provision and the --bootstrap path.
func (d *Driver) Export(ctx context.Context, domain string) error {
	return d.ExtractVars(ctx, d.clusterYAML(domain))
}

// Destroy tears down a KubeOne cluster (bash: driver::destroy).
func (d *Driver) Destroy(ctx context.Context, domain string) error {
	cy := d.clusterYAML(domain)

	// Hosted path: delegate to the kubehz API. Without the hosting read a
	// HOSTED cluster would get the self-hosted teardown — the wrong destroy
	// entirely.
	hosted, err := d.hosted(cy)
	if err != nil {
		return err
	}
	if hosted {
		if d.Hooks.DestroyHosted == nil {
			return errors.New("kubeone: hosted destroy is not wired (Hooks.DestroyHosted)")
		}
		return d.Hooks.DestroyHosted(ctx, domain, cy)
	}

	workDir := d.workDir(domain)

	// 1. Reset Kubernetes (kubeone reset). Fail-soft: warn and continue
	// with the infrastructure destroy.
	if fileExists(filepath.Join(workDir, "kubeone.yaml")) {
		if err := d.Reset(ctx, workDir); err != nil {
			ui.Warnf(d.stderr(), "kubeone reset failed — continuing with infrastructure destroy")
		}
	}

	// 2. Destroy infrastructure via the provider contract. A failed
	// INFRASTRUCTURE destroy RETURNS HERE, keeping the kubeconfig: falling
	// through to the cleanup below once returned SUCCESS while the servers
	// stayed up and billing, AND deleted the one handle left to reach them.
	if d.deps.Provider != nil {
		if err := d.deps.Provider.Destroy(ctx, d.deps.ProviderConfigFile, workDir); err != nil {
			return err
		}
	} else {
		ui.Warnf(d.stderr(), "no provider loaded — cannot destroy infrastructure")
	}

	// 3. Clean up the kubeconfig (named by metadata.name).
	os.Remove(filepath.Join(d.deps.Paths.Base, ".kubeconfig", metadataName(cy)+".yaml"))
	return nil
}

// Status reports the cluster status word (bash: driver::status →
// kubeone::status).
func (d *Driver) Status(ctx context.Context, domain string) (string, error) {
	workDir := d.workDir(domain)

	if !fileExists(filepath.Join(workDir, "kubeone.yaml")) {
		// No kubeone config — check via the provider when available.
		if sp, ok := d.deps.Provider.(driver.ProviderStatuser); ok {
			return sp.ProviderStatus(ctx, d.deps.ProviderConfigFile)
		}
		return "NotProvisioned", nil
	}
	return d.kubeoneStatus(ctx, workDir), nil
}

// Kubeconfig returns the standard kubeconfig path (bash:
// driver::kubeconfig).
func (d *Driver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	return filepath.Join(d.deps.Paths.Base, ".kubeconfig", metadataName(d.clusterYAML(domain))+".yaml"), nil
}

// ── kubeone CLI operations ─────────────────────────────────

// Apply runs kubeone apply (bash: kubeone::apply). The process runs WITH
// ITS CWD SET INTO the work dir — kubeone writes <cluster>-kubeconfig into
// the CWD (no output flag) — using the RELATIVE --manifest kubeone.yaml,
// --auto-approve, and --tfjson output.json when that file exists. Hetzner
// Robot credentials ride along in the environment for the CCM.
func (d *Driver) Apply(ctx context.Context, workDir, clusterYAML string) error {
	if !fileExists(filepath.Join(workDir, "kubeone.yaml")) {
		ui.Errorf(d.stderr(), "kubeone.yaml not found in %s", workDir)
		return fmt.Errorf("kubeone: kubeone.yaml not found in %s", workDir)
	}
	args := []string{"apply", "--manifest", "kubeone.yaml", "--auto-approve"}
	if fileExists(filepath.Join(workDir, "output.json")) {
		args = append(args, "--tfjson", "output.json")
	}

	// Pre-apply steps (stale-worker cleanup, Robot naming, addon render) —
	// injectable, ports with the addon engine.
	if d.Hooks.PrepareApply != nil {
		if err := d.Hooks.PrepareApply(ctx, workDir, clusterYAML); err != nil {
			return err
		}
	}

	ui.Debugf(d.stderr(), "Running (in %s): kubeone %s", workDir, strings.Join(args, " "))
	return d.deps.Runner.Run(ctx, execx.Cmd{
		Name: "kubeone",
		Args: args,
		Dir:  workDir,
		Env:  robotEnv(),
	})
}

// Reset runs kubeone reset (bash: kubeone::reset) — ABSOLUTE paths, no CWD
// change (reset writes nothing the work dir needs to catch).
func (d *Driver) Reset(ctx context.Context, workDir string) error {
	manifest := filepath.Join(workDir, "kubeone.yaml")
	if !fileExists(manifest) {
		ui.Errorf(d.stderr(), "kubeone.yaml not found in %s", workDir)
		return fmt.Errorf("kubeone: kubeone.yaml not found in %s", workDir)
	}
	args := []string{"reset", "--manifest", manifest, "--auto-approve"}
	if tf := filepath.Join(workDir, "output.json"); fileExists(tf) {
		args = append(args, "--tfjson", tf)
	}
	ui.Debugf(d.stderr(), "Running: kubeone %s", strings.Join(args, " "))
	return d.deps.Runner.Run(ctx, execx.Cmd{Name: "kubeone", Args: args, Env: robotEnv()})
}

// kubeoneStatus parses `kubeone status` output into a single word (bash:
// kubeone::status): Healthy | Degraded | NotFound | Unknown. NEGATIVE
// patterns are tested FIRST — "not healthy" contains "healthy", so testing
// "healthy" first reported a degraded cluster as Healthy.
func (d *Driver) kubeoneStatus(ctx context.Context, workDir string) string {
	args := []string{"status", "--manifest", filepath.Join(workDir, "kubeone.yaml")}
	if tf := filepath.Join(workDir, "output.json"); fileExists(tf) {
		args = append(args, "--tfjson", tf)
	}
	var buf strings.Builder
	err := d.deps.Runner.Run(ctx, execx.Cmd{Name: "kubeone", Args: args, Stdout: &buf, Stderr: &buf})
	out := strings.ToLower(buf.String())
	if err == nil {
		switch {
		case containsAny(out, "degraded", "not healthy", "unhealthy"):
			return "Degraded"
		case strings.Contains(out, "healthy"):
			return "Healthy"
		default:
			return "Unknown"
		}
	}
	if containsAny(out, "no such file", "not found", "connection refused") {
		return "NotFound"
	}
	return "Unknown"
}

// KubeconfigPath is where kubeone apply writes the kubeconfig (bash:
// kubeone::kubeconfig_path): <work_dir>/<cluster_name>-kubeconfig.
func KubeconfigPath(workDir, clusterName string) string {
	return filepath.Join(workDir, clusterName+"-kubeconfig")
}

// robotEnv mirrors the env exports of the bash apply subshell: the Hetzner
// Robot credentials ride along under every alias the CCM tooling reads.
func robotEnv() []string {
	var env []string
	if u := os.Getenv("HROBOT_USER"); u != "" {
		env = append(env, "ROBOT_USER="+u, "HETZNER_ROBOT_USER="+u)
	}
	if p := os.Getenv("HROBOT_PASSWORD"); p != "" {
		env = append(env, "ROBOT_PASSWORD="+p, "HETZNER_ROBOT_PASSWORD="+p)
	}
	return env
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
