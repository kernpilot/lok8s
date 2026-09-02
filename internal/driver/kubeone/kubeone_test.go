package kubeone

// kubeone_test.go pins the exec-wrapper behavior of the KubeOne driver
// against the bash source of truth (.lok8s/drivers/kubeone/main): the
// apply-in-workdir contract, the status word mapping (negative patterns
// first), the destroy ordering (reset warn-and-continue, provider-destroy
// KEEPS the kubeconfig on failure — the billing incident), and the
// kubeconfig copy + chmod 600.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

const testSpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: test-prod
spec:
  kubernetes:
    version: "v1.35.5"
  provider:
    name: hetzner
`

type fakeRunner struct {
	calls   []execx.Cmd
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, c)
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

type fakeProvider struct {
	log          []string
	provisionErr error
	destroyErr   error
	validateErr  error
	output       []byte
	status       string
}

func (p *fakeProvider) Validate(ctx context.Context, configFile string) error {
	p.log = append(p.log, "validate")
	return p.validateErr
}

func (p *fakeProvider) CredentialData(ctx context.Context, configFile string) (map[string]string, error) {
	return nil, nil
}

func (p *fakeProvider) Provision(ctx context.Context, configFile, workDir string) error {
	p.log = append(p.log, "provision:"+workDir)
	return p.provisionErr
}

func (p *fakeProvider) Destroy(ctx context.Context, configFile, workDir string) error {
	p.log = append(p.log, "destroy:"+workDir)
	return p.destroyErr
}

func (p *fakeProvider) Output(ctx context.Context, configFile string) ([]byte, error) {
	if p.output == nil {
		return nil, errors.New("no output")
	}
	return p.output, nil
}

type statusFakeProvider struct{ fakeProvider }

func (p *statusFakeProvider) ProviderStatus(ctx context.Context, configFile string) (string, error) {
	if p.status == "" {
		return "Running", nil
	}
	return p.status, nil
}

func testDriver(t *testing.T, prov driver.Provider) (*Driver, *fakeRunner, *bytes.Buffer, *config.Paths) {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), testSpecYAML)
	runner := &fakeRunner{}
	var errBuf bytes.Buffer
	d := New(&driver.Deps{
		Paths:              p,
		Runner:             runner,
		Stderr:             &errBuf,
		Provider:           prov,
		ProviderName:       "hetzner",
		ProviderConfigFile: "/dev/null",
	})
	return d, runner, &errBuf, p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clearVarEnv registers restores for (and unsets) every env var the driver
// exports, so tests are hermetic against each other and the caller.
func clearVarEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CLUSTER_NAME", "K8S_VERSION", "POD_SUBNET", "SERVICE_SUBNET",
		"CP_REPLICAS", "CNI_PLUGIN", "CSI_PLUGIN", "CLOUD_PROVIDER",
		"KUBE_PROXY_SKIP", "SSH_USER", "SSH_PORT", "SSH_PRIVATE_KEY",
		"SSH_PUBLIC_KEY", "ADDONS_ENABLED", "ADDONS_PATH",
		"LOK8S_SPEC_CLUSTER_DOMAIN", "LOK8S_SPEC_DNS_DOMAINFILTER",
		"LOK8S_SPEC_OIDC_ISSUER", "LOK8S_SPEC_OIDC_CLIENTID",
		"LOK8S_SPEC_OIDC_USERNAMECLAIM", "LOK8S_SPEC_OIDC_USERNAMEPREFIX",
		"LOK8S_SPEC_OIDC_GROUPSCLAIM", "LOK8S_SPEC_OIDC_GROUPSPREFIX",
		"LOK8S_SPEC_OIDC_CABUNDLE", "DOMAIN_NAME",
		"HROBOT_USER", "HROBOT_PASSWORD", "HCLOUD_TOKEN",
		"ROBOT_USER", "ROBOT_PASSWORD", "HETZNER_ROBOT_USER", "HETZNER_ROBOT_PASSWORD",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// ── Registry ──────────────────────────────────────────────

func TestDriverRegistered(t *testing.T) {
	if _, ok := driver.Get(Name); !ok {
		t.Fatal("kubeone driver not registered")
	}
}

// ── kubeone::apply — CWD + relative manifest contract ─────

func TestApplyRunsInWorkDirWithRelativeManifest(t *testing.T) {
	clearVarEnv(t)
	d, runner, _, p := testDriver(t, nil)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	writeFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")

	if err := d.Apply(context.Background(), wd, ""); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 exec, got %d", len(runner.calls))
	}
	c := runner.calls[0]
	if c.Name != "kubeone" {
		t.Errorf("tool = %s", c.Name)
	}
	// kubeone writes <name>-kubeconfig to CWD — the run MUST be CD'd into
	// the work dir with the RELATIVE manifest path.
	if c.Dir != wd {
		t.Errorf("Dir = %q, want %q", c.Dir, wd)
	}
	want := []string{"apply", "--manifest", "kubeone.yaml", "--auto-approve"}
	if strings.Join(c.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", c.Args, want)
	}
}

func TestApplyAddsTfjsonWhenPresent(t *testing.T) {
	clearVarEnv(t)
	d, runner, _, p := testDriver(t, nil)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	writeFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
	writeFile(t, filepath.Join(wd, "output.json"), "{}")

	if err := d.Apply(context.Background(), wd, ""); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.calls[0].Args, " ")
	if !strings.Contains(args, "--tfjson output.json") {
		t.Errorf("args missing relative --tfjson: %s", args)
	}
}

func TestApplyCarriesRobotEnvAliases(t *testing.T) {
	clearVarEnv(t)
	t.Setenv("HROBOT_USER", "rob")
	t.Setenv("HROBOT_PASSWORD", "pw")
	d, runner, _, p := testDriver(t, nil)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	writeFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")

	if err := d.Apply(context.Background(), wd, ""); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(runner.calls[0].Env, ",")
	for _, want := range []string{"ROBOT_USER=rob", "HETZNER_ROBOT_USER=rob", "ROBOT_PASSWORD=pw", "HETZNER_ROBOT_PASSWORD=pw"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %s (got %s)", want, env)
		}
	}
}

func TestApplyMissingManifestFails(t *testing.T) {
	clearVarEnv(t)
	d, runner, errBuf, p := testDriver(t, nil)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	if err := d.Apply(context.Background(), wd, ""); err == nil {
		t.Fatal("expected failure")
	}
	if len(runner.calls) != 0 {
		t.Fatal("kubeone must not run without a manifest")
	}
	if !strings.Contains(errBuf.String(), "kubeone.yaml not found in") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// ── kubeone::reset — absolute paths ───────────────────────

func TestResetUsesAbsolutePaths(t *testing.T) {
	clearVarEnv(t)
	d, runner, _, p := testDriver(t, nil)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	writeFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
	writeFile(t, filepath.Join(wd, "output.json"), "{}")

	if err := d.Reset(context.Background(), wd); err != nil {
		t.Fatal(err)
	}
	c := runner.calls[0]
	if c.Dir != "" {
		t.Errorf("reset must not change CWD, got Dir=%q", c.Dir)
	}
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "--manifest "+filepath.Join(wd, "kubeone.yaml")) {
		t.Errorf("reset manifest not absolute: %s", args)
	}
	if !strings.Contains(args, "--tfjson "+filepath.Join(wd, "output.json")) {
		t.Errorf("reset tfjson not absolute: %s", args)
	}
}

// ── Status word mapping ───────────────────────────────────

func TestStatusWordMapping(t *testing.T) {
	cases := []struct {
		name   string
		output string
		err    error
		want   string
	}{
		// NEGATIVE patterns first: "not healthy" contains "healthy" — the
		// original bug reported a degraded cluster as Healthy.
		{"not healthy is Degraded", "cluster is not healthy", nil, "Degraded"},
		{"unhealthy is Degraded", "node x UNHEALTHY", nil, "Degraded"},
		{"degraded", "status: degraded", nil, "Degraded"},
		{"healthy", "all components healthy", nil, "Healthy"},
		{"unknown words", "something else entirely", nil, "Unknown"},
		{"failure connection refused", "dial: connection refused", errors.New("exit 1"), "NotFound"},
		{"failure not found", "manifest not found", errors.New("exit 1"), "NotFound"},
		{"failure no such file", "no such file or directory", errors.New("exit 1"), "NotFound"},
		{"failure other", "boom", errors.New("exit 1"), "Unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearVarEnv(t)
			d, runner, _, p := testDriver(t, nil)
			wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
			writeFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
			runner.handler = func(c execx.Cmd) error {
				fmt.Fprint(c.Stdout, tc.output)
				return tc.err
			}
			got, err := d.Status(context.Background(), "test.lok8s.dev")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusNotProvisionedWithoutManifest(t *testing.T) {
	clearVarEnv(t)
	d, _, _, _ := testDriver(t, nil)
	got, err := d.Status(context.Background(), "test.lok8s.dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NotProvisioned" {
		t.Fatalf("status = %q, want NotProvisioned", got)
	}
}

func TestStatusDelegatesToProviderWithoutManifest(t *testing.T) {
	clearVarEnv(t)
	prov := &statusFakeProvider{}
	prov.status = "Partial"
	d, _, _, _ := testDriver(t, prov)
	got, err := d.Status(context.Background(), "test.lok8s.dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Partial" {
		t.Fatalf("status = %q, want Partial", got)
	}
}

// ── Destroy ordering — the billing incident pins ──────────

func TestDestroyResetFailureWarnsAndContinues(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{}
	d, runner, errBuf, p := testDriver(t, prov)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	writeFile(t, filepath.Join(wd, "kubeone.yaml"), "name: test-prod\n")
	runner.handler = func(c execx.Cmd) error { return errors.New("reset boom") }

	if err := d.Destroy(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatalf("reset failure must not fail the destroy, got %v", err)
	}
	if !strings.Contains(errBuf.String(), "kubeone reset failed — continuing with infrastructure destroy") {
		t.Errorf("missing warn, stderr = %q", errBuf.String())
	}
	if len(prov.log) != 1 || !strings.HasPrefix(prov.log[0], "destroy:") {
		t.Fatalf("provider destroy must still run, log = %v", prov.log)
	}
}

func TestDestroyProviderFailureKeepsKubeconfig(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{destroyErr: errors.New("cloud api down")}
	d, _, _, p := testDriver(t, prov)
	// The one handle left to reach the still-billing servers:
	kc := filepath.Join(p.Base, ".kubeconfig", "test-prod.yaml")
	writeFile(t, kc, "apiVersion: v1\n")

	err := d.Destroy(context.Background(), "test.lok8s.dev")
	if err == nil {
		t.Fatal("a failed infrastructure destroy must RETURN AN ERROR")
	}
	if _, statErr := os.Stat(kc); statErr != nil {
		t.Fatal("kubeconfig must be KEPT when the provider destroy fails (servers stay up and billing)")
	}
}

func TestDestroySuccessRemovesKubeconfig(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{}
	d, _, _, p := testDriver(t, prov)
	kc := filepath.Join(p.Base, ".kubeconfig", "test-prod.yaml")
	writeFile(t, kc, "apiVersion: v1\n")

	if err := d.Destroy(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(kc); statErr == nil {
		t.Fatal("kubeconfig must be removed after a successful destroy")
	}
}

func TestDestroyWithoutProviderWarns(t *testing.T) {
	clearVarEnv(t)
	d, _, errBuf, p := testDriver(t, nil)
	kc := filepath.Join(p.Base, ".kubeconfig", "test-prod.yaml")
	writeFile(t, kc, "apiVersion: v1\n")

	if err := d.Destroy(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBuf.String(), "no provider loaded — cannot destroy infrastructure") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// ── Hosted hooks ──────────────────────────────────────────

func TestHostedProvisionDelegates(t *testing.T) {
	clearVarEnv(t)
	d, runner, _, _ := testDriver(t, nil)
	called := ""
	d.Hooks.ReadKubehzConfig = func(clusterYAML string) (string, error) { return "hosted", nil }
	d.Hooks.ProvisionHosted = func(ctx context.Context, domain, clusterYAML string) error {
		called = domain
		return nil
	}
	if err := d.Provision(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if called != "test.lok8s.dev" {
		t.Fatal("hosted hook not called")
	}
	if len(runner.calls) != 0 {
		t.Fatal("hosted path must not exec kubeone")
	}
}

func TestHostedDestroyRequiresHook(t *testing.T) {
	clearVarEnv(t)
	d, _, _, _ := testDriver(t, nil)
	d.Hooks.ReadKubehzConfig = func(clusterYAML string) (string, error) { return "hosted", nil }
	// A HOSTED cluster must never get the self-hosted teardown.
	if err := d.Destroy(context.Background(), "test.lok8s.dev"); err == nil {
		t.Fatal("hosted destroy without a wired hook must error, not fall through")
	}
}

// ── Provision flow ────────────────────────────────────────

func realCoreTemplate(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".lok8s", "drivers", "kubeone", "cluster", "core", "kubeone.yaml"))
	if err != nil {
		t.Fatalf("real core template unreadable: %v", err)
	}
	return string(raw)
}

func TestProvisionFullFlow(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{}
	d, runner, _, p := testDriver(t, prov)
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml"), realCoreTemplate(t))
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	// A stale legacy tfjson that MUST be removed before the apply.
	writeFile(t, filepath.Join(wd, "output.json"), "{}")

	inventoried := false
	d.Hooks.AppendInventory = func(ctx context.Context, configFile, manifest string) error {
		inventoried = true
		if manifest != filepath.Join(wd, "kubeone.yaml") {
			t.Errorf("inventory manifest = %s", manifest)
		}
		return nil
	}
	runner.handler = func(c execx.Cmd) error {
		// kubeone apply writes <name>-kubeconfig into its CWD.
		writeFile(t, filepath.Join(c.Dir, "test-prod-kubeconfig"), "apiVersion: v1\nkind: Config\n")
		return nil
	}

	if err := d.Provision(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if len(prov.log) == 0 || prov.log[0] != "provision:"+wd {
		t.Fatalf("provider provision log = %v", prov.log)
	}
	if !inventoried {
		t.Fatal("AppendInventory hook not called")
	}
	// The stale output.json was removed → no --tfjson on the apply.
	if args := strings.Join(runner.calls[0].Args, " "); strings.Contains(args, "--tfjson") {
		t.Errorf("stale output.json must be removed before apply: %s", args)
	}
	// Kubeconfig copied to the standard location with mode 600.
	dst := filepath.Join(p.Base, ".kubeconfig", "test-prod.yaml")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("kubeconfig not copied: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
	}
}

func TestProvisionRequiresProvider(t *testing.T) {
	clearVarEnv(t)
	d, _, errBuf, _ := testDriver(t, nil)
	if err := d.Provision(context.Background(), "test.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "KubeOne driver requires spec.provider (no provider loaded)") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestProvisionFailsWhenKubeconfigMissingAfterApply(t *testing.T) {
	clearVarEnv(t)
	prov := &fakeProvider{}
	d, _, errBuf, p := testDriver(t, prov)
	writeFile(t, filepath.Join(p.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml"), realCoreTemplate(t))
	// Runner default: succeeds but writes nothing — the stale-kubeconfig
	// masking scenario of issue #91 inverted (fresh provision, no file).
	if err := d.Provision(context.Background(), "test.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "after kubeone apply") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// ── Kubeconfig path ───────────────────────────────────────

func TestKubeconfigPath(t *testing.T) {
	clearVarEnv(t)
	d, _, _, p := testDriver(t, nil)
	got, err := d.Kubeconfig(context.Background(), "test.lok8s.dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(p.Base, ".kubeconfig", "test-prod.yaml") {
		t.Fatalf("kubeconfig = %s", got)
	}
}
