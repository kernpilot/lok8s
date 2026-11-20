package recover

// recover_test.go ports tests/unit/recover_test.bats: the phase sequence
// resolve → doctor → consent → rebuild → provision → verify with RECORDING
// fakes for the reused primitives, the timing summary, --skip-rebuild /
// --dry-run, the destructive consent guard (and its LOK8S_NONINTERACTIVE
// = CONSENT asymmetry), the resolve guards, and the bash-bridge argv.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// fakeProvider records the primitives recover reuses.
type fakeProvider struct {
	record      *[]string
	noRebuild   bool
	noDoctor    bool
	rebuildFail bool
	output      string
	outputErr   error
	// expand is the envsubst of Expand; nil gives the config as it is.
	expand func(cfg string) ([]byte, error)
}

func (f *fakeProvider) HasRebuild() bool { return !f.noRebuild }
func (f *fakeProvider) HasDoctor() bool  { return !f.noDoctor }

func (f *fakeProvider) Doctor(ctx context.Context, cfg string) string {
	*f.record = append(*f.record, "doctor")
	return "ok\thcloud API reachable\nwarn\tRobot creds unset\nsummary\t1 ok, 1 warn\n"
}

func (f *fakeProvider) Rebuild(ctx context.Context, cfg, wd string) error {
	*f.record = append(*f.record, fmt.Sprintf("rebuild cfg=%s wd=%s dry=%s", cfg, wd, os.Getenv("CLOUD_DRY_RUN")))
	if f.rebuildFail {
		return errors.New("rebuild failed")
	}
	return nil
}

func (f *fakeProvider) Expand(ctx context.Context, cfg string) ([]byte, error) {
	if f.expand != nil {
		return f.expand(cfg)
	}
	return os.ReadFile(cfg)
}

func (f *fakeProvider) Output(ctx context.Context, cfg string) ([]byte, error) {
	if f.outputErr != nil {
		return nil, f.outputErr
	}
	if f.output == "" {
		return []byte(`{"nodes":[{},{},{}]}`), nil
	}
	return []byte(f.output), nil
}

type harness struct {
	r      *Runner
	out    *bytes.Buffer
	errBuf *bytes.Buffer
	record []string
	prov   *fakeProvider
	cfg    string
	spec   string
}

// fakeKubectl answers `kubectl get nodes` with a canned listing.
type fakeKubectl struct {
	nodes string
	calls []string
	envs  [][]string
}

func (f *fakeKubectl) Run(ctx context.Context, c execx.Cmd) error {
	f.calls = append(f.calls, c.Name+" "+strings.Join(c.Args, " "))
	f.envs = append(f.envs, c.Env)
	if c.Name == "kubectl" && c.Stdout != nil {
		fmt.Fprint(c.Stdout, f.nodes)
	}
	return nil
}

// newHarness mirrors the bats setup: a cluster-domain resolve, a stubbed
// provider load ("mock", a non-existent config path), a recording
// provision, and an all-Ready 3-node verify (via a fake kubectl).
func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("LOK8S_NONINTERACTIVE", "")
	os.Unsetenv("LOK8S_NONINTERACTIVE")
	t.Setenv("CLOUD_DRY_RUN", "")
	os.Unsetenv("CLOUD_DRY_RUN")
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	// The project holds the bash tree, so the children's PATH_LOK8S is
	// the project's own (a checkout wins over the cache).
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
	h := &harness{out: &bytes.Buffer{}, errBuf: &bytes.Buffer{}}
	h.spec = filepath.Join(base, "cluster.lok8s.yaml") // non-existent → names fall back
	h.cfg = filepath.Join(base, "hetzner.json")
	h.prov = &fakeProvider{record: &h.record}
	kc := &fakeKubectl{nodes: "cp1  Ready  control-plane  10d  v1.30.0\ncp2  Ready  control-plane  10d  v1.30.0\nw1  Ready  <none>  10d  v1.30.0\n"}
	h.r = &Runner{
		Paths:  p,
		Exec:   kc,
		Stdout: h.out,
		Stderr: h.errBuf,
		In:     strings.NewReader(""),
		ResolveSpec: func(d string) (*provision.Spec, error) {
			h.record = append(h.record, "resolve")
			return &provision.Spec{Domain: d, File: h.spec, Kind: provision.SpecKindCluster}, nil
		},
		LoadProvider: func(ctx context.Context, spec string) (string, string, func(), Provider, error) {
			return "mock", h.cfg, nil, h.prov, nil
		},
		Provision: func(ctx context.Context, d string) error {
			h.record = append(h.record, "provision "+d)
			return nil
		},
		DriverKubeconfig: func(ctx context.Context, d string) (string, error) { return "", errors.New("no driver") },
		KubectlAvailable: func() bool { return true },
		Now:              func() time.Time { return time.Unix(1700000000, 0) },
	}
	// The recovered cluster's kubeconfig the driver would have written.
	if err := os.MkdirAll(filepath.Join(base, ".kubeconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".kubeconfig", "test.dom.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) all() string { return h.out.String() + h.errBuf.String() }

func (h *harness) recorded() string { return strings.Join(h.record, "\n") }

// bats: "recover runs the phases in order and prints the timing summary"
func TestRunPhaseOrderAndSummary(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatal(err)
	}
	all := h.all()
	for _, want := range []string{"⏱ resolve took", "⏱ verify took", "phases: resolve=0m0s doctor=0m0s rebuild=0m0s provision=0m0s verify=0m0s"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	want := []string{
		"resolve",
		"doctor",
		"rebuild cfg=" + h.cfg + " wd=" + filepath.Join(h.r.Paths.Clusters, "test.dom", ".provider") + " dry=",
		"provision test.dom",
	}
	if h.recorded() != strings.Join(want, "\n") {
		t.Errorf("record:\n%s\nwant:\n%s", h.recorded(), strings.Join(want, "\n"))
	}
	if h.errBuf.String()[:len("\n\033[1;36m━━━ recover test.dom ━━━\033[0m\n")] != "\n\033[1;36m━━━ recover test.dom ━━━\033[0m\n" {
		t.Errorf("header missing: %q", h.errBuf.String())
	}
}

// bats: "recover verify compares Ready nodes to inventory count"
func TestRunVerify(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- verify ---", "  nodes Ready: 3/3\n", "  \033[32m✓\033[0m all 3 node(s) Ready — cluster back from bare metal\n"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in stdout:\n%s", want, h.out.String())
		}
	}
}

// bats: "recover verify counts a Ready,SchedulingDisabled node as Ready (real parse)"
func TestVerifyCordonedCountsReady(t *testing.T) {
	h := newHarness(t)
	h.r.Exec = &fakeKubectl{nodes: "cp1  Ready                     control-plane  10d  v1.30.0\ncp2  Ready,SchedulingDisabled  control-plane  10d  v1.30.0\nw1   Ready                     <none>         10d  v1.30.0\nw2   NotReady                  <none>         10d  v1.30.0\n"}
	h.r.domain, h.r.clusterName, h.r.spec, h.r.prov, h.r.config = "test.dom", "test.dom", h.spec, h.prov, h.cfg
	h.r.verify(t.Context())
	for _, want := range []string{"nodes Ready: 3/3", "all 3 node(s) Ready"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q:\n%s", want, h.out.String())
		}
	}
	for _, want := range []string{"cp2 Ready,SchedulingDisabled", "w2 NotReady"} {
		if !strings.Contains(h.errBuf.String(), want) {
			t.Errorf("missing %q:\n%s", want, h.errBuf.String())
		}
	}
	// The kubectl ran against the PINNED kubeconfig, never the ambient one.
	kc := h.r.Exec.(*fakeKubectl)
	if len(kc.envs) != 1 || kc.envs[0][0] != "KUBECONFIG="+filepath.Join(h.r.Paths.Base, ".kubeconfig", "test.dom.yaml") {
		t.Errorf("kubectl env = %v", kc.envs)
	}
}

// bats: "recover verify reports not-ready when the recovered kubeconfig is missing"
func TestVerifyMissingKubeconfigNeverFallsBack(t *testing.T) {
	h := newHarness(t)
	os.Remove(filepath.Join(h.r.Paths.Base, ".kubeconfig", "test.dom.yaml"))
	kc := &fakeKubectl{nodes: "x Ready a b c\ny Ready a b c\nz Ready a b c\n"}
	h.r.Exec = kc
	h.r.domain, h.r.clusterName, h.r.spec, h.r.prov, h.r.config = "test.dom", "test.dom", h.spec, h.prov, h.cfg
	h.r.verify(t.Context())
	if !strings.Contains(h.errBuf.String(), "recovered kubeconfig not found") {
		t.Errorf("stderr = %q", h.errBuf.String())
	}
	if !strings.Contains(h.out.String(), "nodes Ready: 0/3") || strings.Contains(h.out.String(), "all 3 node(s) Ready") {
		t.Errorf("stdout = %q", h.out.String())
	}
	if len(kc.calls) != 0 {
		t.Errorf("kubectl must not be consulted: %v", kc.calls)
	}
}

// bats: the three inventory_count cases — failed output / real count /
// non-JSON → the sentinel is distinguishable from a genuine 0.
func TestInventoryCountSentinel(t *testing.T) {
	h := newHarness(t)
	h.r.prov = h.prov
	h.prov.outputErr = errors.New("no creds")
	if _, ok := h.r.inventoryCount(t.Context()); ok {
		t.Error("failed output must be the sentinel")
	}
	h.prov.outputErr = nil
	if n, ok := h.r.inventoryCount(t.Context()); !ok || n != 3 {
		t.Errorf("got %d %v", n, ok)
	}
	h.prov.output = "not json"
	if _, ok := h.r.inventoryCount(t.Context()); ok {
		t.Error("non-JSON must be the sentinel")
	}
	h.prov.output = `{"api":{}}`
	if n, ok := h.r.inventoryCount(t.Context()); !ok || n != 0 {
		t.Errorf("missing nodes = genuine 0, got %d %v", n, ok)
	}
}

// bats: "recover verify shows 'unknown' (not 0) when the inventory could not be resolved"
func TestVerifyUnknownInventory(t *testing.T) {
	h := newHarness(t)
	h.prov.outputErr = errors.New("api error")
	h.r.domain, h.r.clusterName, h.r.spec, h.r.prov, h.r.config = "test.dom", "test.dom", h.spec, h.prov, h.cfg
	h.r.verify(t.Context())
	if !strings.Contains(h.out.String(), "nodes Ready: 3/unknown") || strings.Contains(h.out.String(), "back from bare metal") {
		t.Errorf("stdout = %q", h.out.String())
	}
}

// bats: "recover kubeconfig path prefers driver::kubeconfig when it is loaded"
// bats: "recover kubeconfig path falls back to local resolution without a driver"
func TestKubeconfigPath(t *testing.T) {
	h := newHarness(t)
	h.r.domain, h.r.clusterName, h.r.spec = "test.dom", "test.dom", h.spec
	h.r.DriverKubeconfig = func(ctx context.Context, d string) (string, error) { return "/from/driver/" + d + ".yaml", nil }
	if got := h.r.kubeconfigPath(t.Context()); got != "/from/driver/test.dom.yaml" {
		t.Errorf("got %q", got)
	}
	h.r.DriverKubeconfig = func(ctx context.Context, d string) (string, error) { return "", errors.New("none") }
	if got, want := h.r.kubeconfigPath(t.Context()), filepath.Join(h.r.Paths.Base, ".kubeconfig", "test.dom.yaml"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// bats: "recover --skip-rebuild skips rebuild but still provisions + verifies"
func TestRunSkipRebuild(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	if err := h.r.Run(t.Context(), "test.dom", true, false); err != nil {
		t.Fatal(err)
	}
	all := h.all()
	for _, want := range []string{"skipping the node rebuild", "rebuild=skipped", "nodes Ready: 3/3"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(h.recorded(), "rebuild cfg=") || !strings.Contains(h.recorded(), "provision test.dom") {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: "recover --dry-run sets CLOUD_DRY_RUN, calls rebuild, skips provision/verify, prints DRY RUN"
// bats: "recover --dry-run does not prompt even without --force"
func TestRunDryRun(t *testing.T) {
	h := newHarness(t)
	h.r.Force = false
	h.r.In = strings.NewReader("") // a prompt would hit EOF → abort; it must not prompt
	if err := h.r.Run(t.Context(), "test.dom", false, true); err != nil {
		t.Fatal(err)
	}
	all := h.all()
	for _, want := range []string{"DRY RUN — nothing changed", "lo provision WOULD run next"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	rec := h.recorded()
	if !strings.Contains(rec, "rebuild cfg="+h.cfg) || !strings.Contains(rec, "dry=1") || strings.Contains(rec, "provision test.dom") {
		t.Errorf("record = %q", rec)
	}
	if os.Getenv("CLOUD_DRY_RUN") != "" {
		t.Error("CLOUD_DRY_RUN must be unset after the run")
	}
}

// bats: "recover confirm blocks without --force: a 'no' aborts and rebuild is NOT called"
// + the empty / EOF / whitespace-only answers all decline.
func TestConfirmDeclines(t *testing.T) {
	for _, in := range []string{"no\n", "\n", "", "   \n"} {
		h := newHarness(t)
		h.r.In = strings.NewReader(in)
		err := h.r.Run(t.Context(), "test.dom", false, false)
		if !errors.Is(err, ErrHandled) {
			t.Errorf("%q: err = %v", in, err)
		}
		if !strings.Contains(h.errBuf.String(), "[warn] recover: aborted by operator — nothing was changed\n") {
			t.Errorf("%q: stderr = %q", in, h.errBuf.String())
		}
		if strings.Contains(h.recorded(), "rebuild cfg=") || strings.Contains(h.recorded(), "provision") {
			t.Errorf("%q: destructive phase ran: %q", in, h.recorded())
		}
	}
}

// A padded "  yes  " is accepted (bash read -r strips IFS whitespace).
func TestConfirmAcceptsPaddedYes(t *testing.T) {
	h := newHarness(t)
	h.r.In = strings.NewReader("  yes  \n")
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.recorded(), "provision test.dom") {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: "recover proceeds when --force is set (no prompt)"
func TestRunForceNoPrompt(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	h.r.In = strings.NewReader("") // must not be read
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.errBuf.String(), "continue? [type yes") {
		t.Error("prompted under --force")
	}
	if !strings.Contains(h.recorded(), "rebuild cfg=") || !strings.Contains(h.recorded(), "provision test.dom") {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: "recover proceeds non-interactively with LOK8S_NONINTERACTIVE" —
// THE consent-asymmetry pin: LOK8S_NONINTERACTIVE is CONSENT here (the
// provision gate treats it as REFUSE).
func TestRunNonInteractiveIsConsent(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	h.r.Force = false
	h.r.In = strings.NewReader("no\n") // would decline if it were read
	if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
		t.Fatalf("LOK8S_NONINTERACTIVE must consent, got %v", err)
	}
	if !strings.Contains(h.recorded(), "rebuild cfg=") {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: consent wording — unknown inventory, --skip-rebuild, bare-metal reset.
func TestConfirmWording(t *testing.T) {
	h := newHarness(t)
	h.r.clusterName = "test.dom"
	h.r.prov = h.prov
	h.prov.outputErr = errors.New("api error")
	h.r.In = strings.NewReader("no\n")
	if h.r.confirm(t.Context(), false) {
		t.Fatal("no must decline")
	}
	if !strings.Contains(h.errBuf.String(), "unknown number of nodes") || strings.Contains(h.errBuf.String(), "reset 0 node(s)") {
		t.Errorf("prompt = %q", h.errBuf.String())
	}

	h = newHarness(t)
	h.r.clusterName, h.r.prov = "test.dom", h.prov
	h.r.In = strings.NewReader("no\n")
	h.r.confirm(t.Context(), true)
	if want := "\033[31m!\033[0m recover: this will re-provision 3 node(s) of cluster test.dom (reinstalling Kubernetes, may wipe data disks) — the bare-metal node reset is SKIPPED — continue? [type yes to continue] "; h.errBuf.String() != want {
		t.Errorf("prompt = %q", h.errBuf.String())
	}

	h = newHarness(t)
	h.r.clusterName, h.r.prov = "test.dom", h.prov
	h.r.In = strings.NewReader("no\n")
	h.r.confirm(t.Context(), false)
	if want := "\033[31m!\033[0m recover: this will reset 3 node(s) of cluster test.dom from bare metal and reinstall them — continue? [type yes to continue] "; h.errBuf.String() != want {
		t.Errorf("prompt = %q", h.errBuf.String())
	}
}

// bats: "recover aborts before provision when rebuild fails"
func TestRunRebuildFailureStops(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	h.prov.rebuildFail = true
	if err := h.r.Run(t.Context(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "[error] recover: rebuild failed — NOT provisioning on a half-reset cluster\n") {
		t.Errorf("stderr = %q", h.errBuf.String())
	}
	if !strings.Contains(h.recorded(), "rebuild cfg=") || strings.Contains(h.recorded(), "provision") {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: "recover declines cleanly when the provider has no provider::rebuild"
func TestResolveNoRebuildHook(t *testing.T) {
	h := newHarness(t)
	h.prov.noRebuild = true
	h.r.LoadProvider = func(ctx context.Context, spec string) (string, string, func(), Provider, error) {
		return "norebuild", "", nil, h.prov, nil
	}
	if err := h.r.Run(t.Context(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "provider 'norebuild' does not support recover (no provider::rebuild)") {
		t.Errorf("stderr = %q", h.errBuf.String())
	}
	if h.recorded() != "resolve" {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: "recover _load_provider surfaces read_name's diagnostic on an invalid provider name"
func TestLoadProviderInvalidNameSurfacesDiagnostic(t *testing.T) {
	h := newHarness(t)
	h.r.LoadProvider = nil
	spec := filepath.Join(h.r.Paths.Base, "bad.yaml")
	if err := os.WriteFile(spec, []byte("kind: KubeOne\nspec:\n  provider:\n    name: \"bad name!\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.r.domain, h.r.spec = "test.dom", spec
	if err := h.r.loadProvider(t.Context()); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	got := h.errBuf.String()
	if !strings.Contains(got, "provider name 'bad name!' is invalid") || !strings.Contains(got, "no usable spec.provider") || strings.Contains(got, "has no spec.provider") {
		t.Errorf("stderr = %q", got)
	}
}

// The real load path over a fake constructor: configRef resolution + the
// "failed to load" message.
func TestLoadProviderRealPath(t *testing.T) {
	h := newHarness(t)
	h.r.LoadProvider = nil
	dir := filepath.Join(h.r.Paths.Clusters, "mock.cloud")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	_ = os.WriteFile(spec, []byte("kind: KubeOne\nmetadata: { name: mock }\nspec:\n  provider:\n    name: mock\n    configRef: provider.yaml\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "provider.yaml"), []byte("cluster_name: mocky\n"), 0o644)
	h.r.domain, h.r.spec = "mock.cloud", spec
	h.r.NewProvider = func(ctx context.Context, name string) (Provider, error) { return h.prov, nil }
	if err := h.r.loadProvider(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.r.provider != "mock" || h.r.config != filepath.Join(dir, "provider.yaml") {
		t.Errorf("provider=%q config=%q", h.r.provider, h.r.config)
	}
	if got := h.r.resolveClusterName(); got != "mocky" {
		t.Errorf("cluster name = %q", got)
	}
	h.r.NewProvider = func(ctx context.Context, name string) (Provider, error) { return nil, errors.New("not found") }
	if err := h.r.loadProvider(t.Context()); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "[error] recover: provider 'mock' failed to load\n") {
		t.Errorf("stderr = %q", h.errBuf.String())
	}
}

// bats: "recover _workdir returns non-zero and never echoes /tmp when mktemp fails"
// bats: "recover aborts (nothing destructive) when the work dir cannot be created"
func TestWorkdirFallbackAndFailure(t *testing.T) {
	h := newHarness(t)
	wd, err := h.r.workdir("../bad")
	if err != nil || !strings.HasPrefix(filepath.Base(wd), "tmp.") {
		t.Errorf("invalid domain must fall back to a private temp dir: %q %v", wd, err)
	}
	os.RemoveAll(wd)
	// Make the temp fallback fail (TMPDIR unwritable) — no shared /tmp last resort.
	t.Setenv("TMPDIR", filepath.Join(h.r.Paths.Base, "nope", "deeper"))
	h.r.Force = true
	err = h.r.Run(t.Context(), "../bad", false, false)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "could not create a work directory") || strings.Contains(h.errBuf.String(), "/tmp") {
		t.Errorf("stderr = %q", h.errBuf.String())
	}
	if len(h.record) != 0 {
		t.Errorf("no phase may run: %v", h.record)
	}
}

// bats: "recover rejects a non-cluster (deploy) domain"
func TestRunRejectsDeployDomain(t *testing.T) {
	h := newHarness(t)
	h.r.ResolveSpec = func(d string) (*provision.Spec, error) {
		h.record = append(h.record, "resolve")
		return &provision.Spec{Domain: d, File: h.spec, Kind: provision.SpecKindDeploy}, nil
	}
	loaded := false
	h.r.LoadProvider = func(ctx context.Context, spec string) (string, string, func(), Provider, error) {
		loaded = true
		return "", "", nil, nil, nil
	}
	if err := h.r.Run(t.Context(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "recover: 'test.dom' is not a cluster domain (kind=deploy) — recover rebuilds a cluster from bare metal; a deploy domain has nothing to reset") {
		t.Errorf("stderr = %q", h.errBuf.String())
	}
	if loaded || h.recorded() != "resolve" {
		t.Errorf("loaded=%v record=%q", loaded, h.recorded())
	}
}

// bats: "recover aborts when resolve_spec itself fails (bad/missing domain)"
func TestRunResolveFailure(t *testing.T) {
	h := newHarness(t)
	h.r.ResolveSpec = func(d string) (*provision.Spec, error) {
		h.record = append(h.record, "resolve")
		return nil, errors.New("no spec")
	}
	if err := h.r.Run(t.Context(), "nope.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if h.recorded() != "resolve" {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: the three _pick_domain cases.
func TestPickDomain(t *testing.T) {
	t.Parallel()
	if d, err := PickDomain(io.Discard, "active.dom", []string{"explicit.dom"}); err != nil || d != "explicit.dom" {
		t.Errorf("got %q %v", d, err)
	}
	if d, err := PickDomain(io.Discard, "active.dom", []string{""}); err != nil || d != "active.dom" {
		t.Errorf("got %q %v", d, err)
	}
	if d, err := PickDomain(io.Discard, "active.dom", nil); err != nil || d != "active.dom" {
		t.Errorf("got %q %v", d, err)
	}
	var errBuf bytes.Buffer
	if _, err := PickDomain(&errBuf, "active.dom", []string{"explicit.dom", "stray-token"}); !errors.Is(err, ErrHandled) {
		t.Errorf("err = %v", err)
	}
	if want := "[error] too many arguments: stray-token\n"; errBuf.String() != want {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// Doctor rendering: the status → glyph table, incl. the pass-through of an
// unknown status and a message-less line.
func TestDoctorRendering(t *testing.T) {
	h := newHarness(t)
	h.r.provider, h.r.prov, h.r.config = "mock", h.prov, h.cfg
	h.r.doctor(t.Context())
	want := "\n--- provider / infrastructure (mock) ---\n  \033[32m✓\033[0m hcloud API reachable\n  \033[33m!\033[0m Robot creds unset\n    1 ok, 1 warn\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out.String(), want)
	}
	h.out.Reset()
	h.prov.noDoctor = true
	h.r.doctor(t.Context())
	if !strings.Contains(h.out.String(), "  \033[33m!\033[0m provider has no doctor hook — infrastructure diagnosis unavailable\n") {
		t.Errorf("stdout = %q", h.out.String())
	}
}

// ── the bash bridge: exact argv/env of every child ─────────

type argvRecorder struct {
	cmds  []execx.Cmd
	probe string
	fail  bool
	// stdins holds what each child read on its stdin ("" for none).
	stdins []string
}

func (a *argvRecorder) Run(ctx context.Context, c execx.Cmd) error {
	a.cmds = append(a.cmds, c)
	in := ""
	if c.Stdin != nil {
		raw, err := io.ReadAll(c.Stdin)
		if err != nil {
			return err
		}
		in = string(raw)
	}
	a.stdins = append(a.stdins, in)
	if a.fail {
		return errors.New("exit status 1")
	}
	if c.Stdout != nil && a.probe != "" {
		fmt.Fprint(c.Stdout, a.probe)
	}
	return nil
}

// sharedNameForms are the three places where the spec of another domain can
// carry the cluster_name "shared": hetzner.json, the file of
// spec.provider.configRef, and an inline spec.provider.config.
var sharedNameForms = []struct {
	name  string
	write func(t *testing.T, dir string)
}{
	{"hetzner.json", func(t *testing.T, dir string) {
		testutil.WriteFile(t, filepath.Join(dir, "hetzner.json"), `{"cluster_name": "shared"}`+"\n")
	}},
	{"configRef", func(t *testing.T, dir string) {
		testutil.WriteFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: copy\nspec:\n  provider:\n    name: mock\n    configRef: infra/desc.yaml\n")
		testutil.WriteFile(t, filepath.Join(dir, "infra", "desc.yaml"), "cluster_name: shared\n")
	}},
	{"inline", func(t *testing.T, dir string) {
		testutil.WriteFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: copy\nspec:\n  provider:\n    name: mock\n    config:\n      cluster_name: shared\n")
	}},
}

// bats: "recover refuses a cluster_name that another domain has: ..."
// Another domain has the cluster_name of this domain's provider config. Run
// refuses at resolve, before the doctor, the dry run and the consent: the
// fake provider records no doctor and no rebuild, and nothing is
// provisioned. No mode skips the check (the destroy guard has no bypass
// either): not a "yes" at the prompt, not --force, not
// LOK8S_NONINTERACTIVE, not --dry-run, not --skip-rebuild.
func TestRunRefusesAClusterNameOfAnotherDomain(t *testing.T) {
	modes := []struct {
		name                                       string
		force, nonInteractive, skipRebuild, dryRun bool
	}{
		{name: "yes at the prompt"},
		{name: "force", force: true},
		{name: "noninteractive", nonInteractive: true},
		{name: "dry-run", dryRun: true},
		{name: "force dry-run", force: true, dryRun: true},
		{name: "force skip-rebuild", force: true, skipRebuild: true},
	}
	for _, form := range sharedNameForms {
		for _, m := range modes {
			t.Run(form.name+"/"+m.name, func(t *testing.T) {
				h := newHarness(t)
				testutil.WriteFile(t, h.cfg, `{"cluster_name": "shared"}`+"\n")
				form.write(t, filepath.Join(h.r.Paths.Clusters, "copy.dom"))
				h.r.Force = m.force
				h.r.In = strings.NewReader("yes\n")
				if m.nonInteractive {
					t.Setenv("LOK8S_NONINTERACTIVE", "1")
				}
				err := h.r.Run(t.Context(), "test.dom", m.skipRebuild, m.dryRun)
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("err = %v, want ErrHandled", err)
				}
				for _, want := range []string{
					"[error] recover: cluster_name 'shared' of 'test.dom' is also the cluster_name of: copy.dom. The rebuild resets only the servers with this cluster_name and the owner mark of 'test.dom'.",
					" Nothing was changed. Give each domain its own cluster_name, then run lo recover again.\n",
				} {
					if !strings.Contains(h.errBuf.String(), want) {
						t.Errorf("missing %q in stderr:\n%s", want, h.errBuf.String())
					}
				}
				if got := h.recorded(); got != "resolve" {
					t.Errorf("record = %q, want only the resolve (no doctor, rebuild or provision)", got)
				}
				for _, not := range []string{"continue? [type yes", "DRY RUN"} {
					if strings.Contains(h.all(), not) {
						t.Errorf("%q after the refusal:\n%s", not, h.all())
					}
				}
			})
		}
	}
}

// Another domain whose provider config has a merge key (<<) on the path to
// its cluster_name: the scan cannot tell the name that its provider uses,
// so recover is refused at resolve (fail closed), before the doctor, the
// rebuild and the provision; a plain user does not hide it.
func TestRunClusterNameGuardRefusesAMergeKeyInAnotherDomain(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	testutil.WriteFile(t, h.cfg, `{"cluster_name": "shared"}`+"\n")
	sharedNameForms[0].write(t, filepath.Join(h.r.Paths.Clusters, "copy.dom"))
	testutil.WriteFile(t, filepath.Join(h.r.Paths.Clusters, "merge.dom", "cluster.lok8s.yaml"),
		"kind: KubeOne\nmetadata:\n  name: m\nspec:\n  provider:\n    name: mock\n    config:\n      <<: {cluster_name: shared}\n")
	if err := h.r.Run(t.Context(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	if want := "[error] recover: a domain has a merge key (<<) on the path to the cluster_name of its provider config: merge.dom. The provider can take the cluster_name from the merged mapping, so recover cannot compare it with the cluster_name 'shared' of 'test.dom'. Nothing was changed. Write cluster_name as a literal key, without a merge key, in the provider config of that domain, then run lo recover again.\n"; !strings.Contains(h.errBuf.String(), want) {
		t.Errorf("missing %q in stderr:\n%s", want, h.errBuf.String())
	}
	if strings.Contains(h.errBuf.String(), "is also the cluster_name of") {
		t.Errorf("the guard compared the name:\n%s", h.errBuf.String())
	}
	if got := h.recorded(); got != "resolve" {
		t.Errorf("record = %q, want only the resolve (no doctor, rebuild or provision)", got)
	}
}

// A clusters dir that the scan cannot list (no read permission): recover
// cannot tell the other domains, so it is refused at resolve (fail closed).
func TestRunClusterNameGuardRefusesWhenTheClustersDirCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a dir without the read permission")
	}
	h := newHarness(t)
	h.r.Force = true
	testutil.WriteFile(t, h.cfg, `{"cluster_name": "shared"}`+"\n")
	testutil.WriteFile(t, filepath.Join(h.r.Paths.Clusters, "copy.dom", "hetzner.json"), `{"cluster_name": "other"}`+"\n")
	if err := os.Chmod(h.r.Paths.Clusters, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.r.Paths.Clusters, 0o755) })
	if err := h.r.Run(t.Context(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	if want := "[error] recover: could not check if another domain has the cluster_name 'shared' of 'test.dom': lo cannot read the directory " + h.r.Paths.Clusters +
		". Nothing was changed. Make " + h.r.Paths.Clusters + " a directory that you can read and search, then run lo recover again.\n"; !strings.Contains(h.errBuf.String(), want) {
		t.Errorf("missing %q in stderr:\n%s", want, h.errBuf.String())
	}
	if got := h.recorded(); got != "resolve" {
		t.Errorf("record = %q, want only the resolve (no doctor, rebuild or provision)", got)
	}
}

// The refusal names every other domain with the cluster_name, sorted.
func TestRunClusterNameGuardNamesEveryDomain(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	testutil.WriteFile(t, h.cfg, `{"cluster_name": "shared"}`+"\n")
	sharedNameForms[0].write(t, filepath.Join(h.r.Paths.Clusters, "copy.dom"))
	sharedNameForms[2].write(t, filepath.Join(h.r.Paths.Clusters, "b.dom"))
	if err := h.r.Run(t.Context(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	if want := "is also the cluster_name of: b.dom, copy.dom. "; !strings.Contains(h.errBuf.String(), want) {
		t.Errorf("missing %q in stderr:\n%s", want, h.errBuf.String())
	}
}

// The check counts the other domains only, and reads the cluster_name of
// this domain's provider config only. This domain's own files, a dot dir
// and a domain with another cluster_name refuse nothing. A config without
// a cluster_name is not checked: a domain whose cluster_name is the name
// that the prompt shows for this domain (the domain itself) refuses
// nothing. The recover runs to the end.
func TestRunClusterNameGuardPasses(t *testing.T) {
	cases := []struct {
		name   string
		config string
		files  map[string]string
	}{
		{"own files, a dot dir, another name", `{"cluster_name": "shared"}`, map[string]string{
			"test.dom/hetzner.json":  `{"cluster_name": "shared"}`,
			".old/hetzner.json":      `{"cluster_name": "shared"}`,
			"other.dom/hetzner.json": `{"cluster_name": "other"}`,
		}},
		{"no cluster_name in the config", `{}`, map[string]string{
			"other.dom/hetzner.json": `{"cluster_name": "test.dom"}`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.r.Force = true
			testutil.WriteFile(t, h.cfg, c.config+"\n")
			for rel, body := range c.files {
				testutil.WriteFile(t, filepath.Join(h.r.Paths.Clusters, filepath.FromSlash(rel)), body+"\n")
			}
			if err := h.r.Run(t.Context(), "test.dom", false, false); err != nil {
				t.Fatalf("err = %v\n%s", err, h.all())
			}
			if rec := h.recorded(); !strings.Contains(rec, "rebuild cfg=") || !strings.Contains(rec, "provision test.dom") {
				t.Errorf("record = %q, want the rebuild and the provision", rec)
			}
			if strings.Contains(h.errBuf.String(), "is also the cluster_name of") {
				t.Errorf("refused:\n%s", h.errBuf.String())
			}
		})
	}
}

// The refusal texts of the cluster_name guard for test.dom (the bats file
// has the same texts).
const (
	msgConfig   = "[error] recover: the provider config of 'test.dom' is not one YAML or JSON document whose top is a mapping with a plain cluster_name (not an alias, a mapping or a list), so recover cannot compare its cluster_name with the other domains. Nothing was changed. Correct the provider config, then run lo recover again.\n"
	msgMerge    = "[error] recover: the provider config of 'test.dom' has a merge key (<<) at the top. The rebuild can take the cluster_name from the merged mapping, so recover cannot compare it with the cluster_name of the other domains. Nothing was changed. Write cluster_name as a literal key, without a merge key, then run lo recover again.\n"
	msgQuote    = "[error] recover: the cluster_name of the provider config of 'test.dom' is not a text: YAML reads it as a number, as true or as another type. The rebuild writes it as JSON, which can change how it is written, so recover cannot compare it with the cluster_name of the other domains. Nothing was changed. Write the cluster_name in quotes, then run lo recover again.\n"
	msgUnshown  = "[error] recover: the provider config of 'test.dom' names a cluster_name with a character that a message cannot show as it is (a control, separator, bidi or invisible character, a backslash, U+FFFD, or a byte that is not UTF-8). Nothing was changed. Correct the cluster_name in the provider config, then run lo recover again.\n"
	msgExpand   = "[error] recover: could not expand the variables of the provider config of 'test.dom' as the rebuild does (the provider shell or envsubst failed). Nothing was changed. Check the provider config, that the provider loads and that envsubst runs, then run lo recover again.\n"
	msgUnquoted = "[error] recover: the provider config of 'test.dom' parses only after its variables are expanded. recover reads the cluster_name before and after the expansion, so it cannot compare it with the other domains. A variable outside quotes can cause this. Nothing was changed. Write each variable of the provider config in quotes, then run lo recover again.\n"
	msgChanged  = "[error] recover: the provider config of 'test.dom' gives another cluster_name when the rebuild expands its variables (a variable adds or changes it), so recover cannot compare the cluster_name that the rebuild uses with the other domains. Nothing was changed. Write the provider config so that no variable sets or changes the cluster_name, then run lo recover again.\n"
)

func msgDollar(n string) string {
	return "[error] recover: the cluster_name '" + n + "' of 'test.dom' has a '$'. The rebuild expands the variables in the provider config, so the cluster_name that it uses can differ from this text, and recover cannot compare it with the cluster_name of the other domains. Nothing was changed. Write the cluster_name without a variable, then run lo recover again.\n"
}

// configShapes are provider configs of this domain in the shapes that the
// bash twin reads with yq (tests/unit/recover_test.bats has the same table).
// want is one of:
//   - "none:<text>": no cluster_name, so no check. Another domain has the
//     cluster_name <text>, the text that a wrong read gives (yq without
//     `// ""` prints "null" and "false");
//   - "shared:<name>": the guard reads <name>, and another domain with that
//     name is refused;
//   - "config", "merge", "quote" or "unshown": that refusal.
var configShapes = []struct{ name, config, want string }{
	{"empty file", "", "none:null"},
	{"only a comment", "# a comment\n", "none:null"},
	{"null document", "null\n", "none:null"},
	{"empty mapping", "{}\n", "none:null"},
	{"a mapping without the key", "x: 1\n", "none:null"},
	{"null", "cluster_name: null\n", "none:null"},
	{"false", "cluster_name: false\n", "none:false"},
	{"False", "cluster_name: False\n", "none:False"},
	{"json", `{"cluster_name": "a"}`, "shared:a"},
	{"json with tabs", "{\n\t\"cluster_name\": \"a\"\n}\n", "shared:a"},
	{"yaml with a leading document marker", "---\ncluster_name: a\n", "shared:a"},
	{"a quoted number is a text", "cluster_name: \"0123\"\n", "shared:0123"},
	{"a quoted false is a text", "cluster_name: \"false\"\n", "shared:false"},
	{"yes is a text", "cluster_name: yes\n", "shared:yes"},
	{"two keys: the last one", "cluster_name: a\ncluster_name: b\n", "shared:b"},
	{"does not parse", `{"cluster_name": "a", "token": [}`, "config"},
	{"top is a list", "- a\n- b\n", "config"},
	{"top is a text", "hello\n", "config"},
	{"top has a custom tag", "!foo {cluster_name: a}\n", "config"},
	{"two documents", "cluster_name: a\n---\ncluster_name: b\n", "config"},
	{"an empty second document", "cluster_name: a\n---\n", "config"},
	{"name is a mapping", "cluster_name:\n  x: 1\n", "config"},
	{"name is a list", "cluster_name: [a]\n", "config"},
	{"name is an alias", "n: &x a\ncluster_name: *x\n", "config"},
	{"a merge key gives the name", "base: &b {cluster_name: merged}\n<<: *b\n", "merge"},
	{"a merge key after the name", "base: &b {cluster_name: merged}\ncluster_name: a\n<<: *b\n", "merge"},
	{"a merge key with a list", "base: &b {x: 1}\n<<: [*b]\ncluster_name: a\n", "merge"},
	{"a quoted << key", "\"<<\": x\ncluster_name: a\n", "merge"},
	{"a key with the explicit tag !!merge", "x: 1\n!!merge base: {cluster_name: merged}\n", "merge"},
	{"an alias key that names a plain <<", "x: &k <<\n*k : {cluster_name: merged}\ncluster_name: a\n", "merge"},
	{"an alias key that names a quoted <<", "x: &k '<<'\n*k : {cluster_name: merged}\ncluster_name: a\n", "merge"},
	{"int", "cluster_name: 0123\n", "quote"},
	{"hex int", "cluster_name: 0x1F\n", "quote"},
	{"float", "cluster_name: 1.5\n", "quote"},
	{"true", "cluster_name: true\n", "quote"},
	{"True", "cluster_name: True\n", "quote"},
	{"name has a custom tag", "cluster_name: !foo a\n", "quote"},
	{"name is a timestamp", "cluster_name: 2001-12-14\n", "quote"},
	{"name ends with a newline", "cluster_name: \"a\\n\"\n", "unshown"},
	{"name has a NUL", "cluster_name: \"a\\0b\"\n", "unshown"},
}

// The guard reads this domain's provider config as the bash twin does: the
// same name, the same refusals (configShapes).
func TestGuardClusterNameReadsTheConfigAsYq(t *testing.T) {
	refusal := map[string]string{"config": msgConfig, "merge": msgMerge, "quote": msgQuote, "unshown": msgUnshown}
	for _, c := range configShapes {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			testutil.WriteFile(t, h.cfg, c.config)
			other := "a-name-no-row-reads"
			kind, n, found := strings.Cut(c.want, ":")
			if found {
				other = n
			}
			raw, err := json.Marshal(map[string]string{"cluster_name": other})
			if err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(h.r.Paths.Clusters, "copy.dom", "hetzner.json"), string(raw)+"\n")
			h.r.domain, h.r.config, h.r.prov = "test.dom", h.cfg, h.prov
			err = h.r.guardClusterName(t.Context())
			stderr := h.errBuf.String()
			switch kind {
			case "none":
				if err != nil || stderr != "" {
					t.Errorf("err = %v, stderr = %q; want no refusal", err, stderr)
				}
			case "shared":
				if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "cluster_name '"+other+"' of 'test.dom' is also the cluster_name of: copy.dom. ") {
					t.Errorf("err = %v, stderr = %q; want the shared-name refusal for %q", err, stderr, other)
				}
			default:
				if want := refusal[kind]; !errors.Is(err, ErrHandled) || stderr != want {
					t.Errorf("err = %v, stderr = %q; want %q", err, stderr, want)
				}
			}
		})
	}
}

// A cluster_name that a message cannot show is refused before a message
// shows it (the rule of the destroy guard), and a cluster_name with a '$'
// is refused: the rebuild expands it, so the guard cannot compare it.
func TestGuardClusterNameRefusesNamesItCannotCompare(t *testing.T) {
	unshown, config, dollar := msgUnshown, msgConfig, msgDollar
	cases := []struct{ name, value, want string }{
		{"escape", "a\x1b[2Jb", unshown},
		{"backslash", `a\\b`, unshown},
		{"bidi", "a\u202eb", unshown},
		{"zero width", "a\u200bb", unshown},
		{"U+FFFD", "a\ufffdb", unshown},
		{"NUL", "a\x00b", unshown},
		// yaml (both twins) does not parse a file that is not UTF-8.
		{"not UTF-8", "a\xffb", config},
		{"braced variable", "${NAME}-a", dollar("${NAME}-a")},
		{"bare variable", "prod-$ENV", dollar("prod-$ENV")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			raw, err := json.Marshal(map[string]string{"cluster_name": c.value})
			if err != nil {
				t.Fatal(err)
			}
			if c.name == "not UTF-8" {
				raw = []byte("cluster_name: " + c.value + "\n")
			}
			testutil.WriteFile(t, h.cfg, string(raw)+"\n")
			// Another domain with the same text: the guard must refuse
			// before it compares.
			testutil.WriteFile(t, filepath.Join(h.r.Paths.Clusters, "copy.dom", "hetzner.json"), string(raw)+"\n")
			h.r.Force = true
			err = h.r.Run(t.Context(), "test.dom", false, false)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v, want ErrHandled", err)
			}
			if !strings.Contains(h.errBuf.String(), c.want) || strings.Contains(h.errBuf.String(), "is also the cluster_name of") {
				t.Errorf("stderr = %q, want %q", h.errBuf.String(), c.want)
			}
			// The header of recover has colour codes; the name must not
			// bring its own escape sequence.
			if strings.Contains(h.errBuf.String(), "\x1b[2J") {
				t.Errorf("the escape sequence of the name reached stderr: %q", h.errBuf.String())
			}
			if got := h.recorded(); got != "resolve" {
				t.Errorf("record = %q, want only the resolve", got)
			}
		})
	}
}

// The guard reads the config again after the expansion of the rebuild
// (Provider.Expand): a variable that adds or changes the cluster_name is
// refused, an expansion that keeps it passes, and a failed expansion stops
// the recover.
func TestGuardClusterNameExpandsAsTheRebuild(t *testing.T) {
	cases := []struct{ name, config, expanded, want string }{
		{"a variable adds a cluster_name", `{"x": 1, "note": "${NOTE}"}`, `{"x": 1, "note": "a", "cluster_name": "shared"}`, msgChanged},
		{"a variable adds a cluster_name in yaml", "x: 1\nnote: ${NOTE}\n", "x: 1\nnote: a\ncluster_name: shared\n", msgChanged},
		{"a variable changes the cluster_name", `{"cluster_name": "a", "note": "${NOTE}"}`, `{"cluster_name": "a", "note": "b", "cluster_name": "shared"}`, msgChanged},
		{"a variable takes the cluster_name away", `{"cluster_name": "a", "note": "${NOTE}"}`, `{"cluster_name": "a", "note": "x", "cluster_name": null}`, msgChanged},
		{"a variable adds a merge key", "cluster_name: a\nnote: ${NOTE}\n", "cluster_name: a\nnote: x\n<<: {cluster_name: shared}\n", msgChanged},
		{"a variable breaks the config", "cluster_name: a\nnote: ${NOTE}\n", "cluster_name: a\nnote: [\n", msgChanged},
		// No cluster_name before the expansion: only the verdict of the read
		// changes, not the name ("" before and after).
		{"a variable adds a merge key with a cluster_name, no cluster_name before", "x: 1\nnote: ${NOTE}\n", "x: 1\nnote: x\n<<: {cluster_name: shared}\n", msgChanged},
		{"a variable adds a cluster_name that is not a text", `{"x": 1, "note": "${NOTE}"}`, `{"x": 1, "note": "a", "cluster_name": 123, "y": ""}`, msgChanged},
		{"a variable breaks a config without a cluster_name", "x: 1\nnote: ${NOTE}\n", "x: 1\nnote: [\n", msgChanged},
		// A config that parses only after the expansion names the cause.
		{"a variable outside quotes", `{"cluster_name": "a", "count": ${NOTE}}`, `{"cluster_name": "a", "count": 3}`, msgUnquoted},
		{"a config that does not parse after the expansion either", `{"cluster_name": "a", "count": ${NOTE}`, `{"cluster_name": "a", "count": 3`, msgConfig},
		{"an expansion that keeps the cluster_name", `{"cluster_name": "a", "token": "${TOKEN}"}`, `{"cluster_name": "a", "token": "xyz"}` + "\n\n", ""},
		{"an expansion without a cluster_name", `{"token": "${TOKEN}"}`, `{"token": "xyz"}`, ""},
		{"envsubst fails", `{"cluster_name": "a"}`, "", msgExpand},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			testutil.WriteFile(t, h.cfg, c.config)
			testutil.WriteFile(t, filepath.Join(h.r.Paths.Clusters, "copy.dom", "hetzner.json"), `{"cluster_name": "shared"}`+"\n")
			h.prov.expand = func(cfg string) ([]byte, error) {
				if cfg != h.cfg {
					t.Errorf("Expand(%q), want the config %q", cfg, h.cfg)
				}
				if c.want == msgExpand {
					return nil, errors.New("envsubst failed")
				}
				return []byte(c.expanded), nil
			}
			h.r.Force = true
			err := h.r.Run(t.Context(), "test.dom", false, false)
			if c.want == "" {
				if err != nil || strings.Contains(h.errBuf.String(), "[error]") {
					t.Fatalf("err = %v, want no refusal\n%s", err, h.all())
				}
				return
			}
			if !errors.Is(err, ErrHandled) || !strings.Contains(h.errBuf.String(), c.want) {
				t.Errorf("err = %v, stderr = %q, want %q", err, h.errBuf.String(), c.want)
			}
			if got := h.recorded(); got != "resolve" {
				t.Errorf("record = %q, want only the resolve", got)
			}
		})
	}
}

// A config that does not parse, and whose expansion fails: the general
// text, not the text about a variable outside quotes.
func TestGuardClusterNameUnparsedConfigWithAFailedExpansion(t *testing.T) {
	h := newHarness(t)
	testutil.WriteFile(t, h.cfg, `{"cluster_name": "a", "count": ${NOTE}}`)
	h.prov.expand = func(string) ([]byte, error) { return nil, errors.New("the provider shell failed") }
	h.r.domain, h.r.config, h.r.prov = "test.dom", h.cfg, h.prov
	if err := h.r.guardClusterName(t.Context()); !errors.Is(err, ErrHandled) || h.errBuf.String() != msgConfig {
		t.Errorf("err = %v, stderr = %q; want %q", err, h.errBuf.String(), msgConfig)
	}
}

// Under DEBUG, resolve prints the resolved cluster name. The line comes after
// the guard: a name that the guard refuses never reaches it.
func TestResolveDebugLineComesAfterTheGuard(t *testing.T) {
	t.Setenv("DEBUG", "1")
	cases := []struct {
		name, value string
		refused     bool
	}{
		{"a refused name", "a\x1b[2Jb", true},
		{"a name that passes", "solo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			raw, err := json.Marshal(map[string]string{"cluster_name": c.value})
			if err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, h.cfg, string(raw)+"\n")
			h.r.Force = true
			err = h.r.Run(t.Context(), "test.dom", false, false)
			stderr := h.errBuf.String()
			if !c.refused {
				if err != nil || !strings.Contains(stderr, "recover: resolved test.dom → provider=") {
					t.Fatalf("err = %v, want the debug line after the guard\n%s", err, h.all())
				}
				return
			}
			if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, msgUnshown) {
				t.Fatalf("err = %v, stderr = %q; want the refusal", err, stderr)
			}
			if strings.Contains(stderr, "recover: resolved") || strings.Contains(stderr, "\x1b[2J") {
				t.Errorf("the debug line showed the refused name: %q", stderr)
			}
		})
	}
}

// The guard with this domain's own spec: a configRef file and an inline
// config, through the real loadProvider (provision.WriteProviderConfig).
func TestRunGuardWithTheRealProviderConfig(t *testing.T) {
	forms := []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{"configRef", func(t *testing.T, dir string) {
			testutil.WriteFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: test\nspec:\n  provider:\n    name: mock\n    configRef: infra/desc.yaml\n")
			testutil.WriteFile(t, filepath.Join(dir, "infra", "desc.yaml"), "cluster_name: shared\n")
		}},
		{"inline", func(t *testing.T, dir string) {
			testutil.WriteFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: test\nspec:\n  provider:\n    name: mock\n    config:\n      cluster_name: shared\n")
		}},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			// WriteProviderConfig exports these; t.Setenv restores them.
			t.Setenv("PROVIDER_DOMAIN", "")
			t.Setenv("PROVIDER_CONFIG_FILE", "")
			h := newHarness(t)
			dir := filepath.Join(h.r.Paths.Clusters, "test.dom")
			f.write(t, dir)
			sharedNameForms[0].write(t, filepath.Join(h.r.Paths.Clusters, "copy.dom"))
			spec := filepath.Join(dir, "cluster.lok8s.yaml")
			h.r.ResolveSpec = func(d string) (*provision.Spec, error) {
				h.record = append(h.record, "resolve")
				return &provision.Spec{Domain: d, File: spec, Kind: provision.SpecKindCluster}, nil
			}
			h.r.LoadProvider = nil
			h.r.NewProvider = func(ctx context.Context, name string) (Provider, error) { return h.prov, nil }
			h.r.Force = true
			err := h.r.Run(t.Context(), "test.dom", false, false)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v, want ErrHandled\n%s", err, h.all())
			}
			if want := "cluster_name 'shared' of 'test.dom' is also the cluster_name of: copy.dom. "; !strings.Contains(h.errBuf.String(), want) {
				t.Errorf("missing %q in stderr:\n%s", want, h.errBuf.String())
			}
			if got := h.recorded(); got != "resolve" {
				t.Errorf("record = %q, want only the resolve", got)
			}
		})
	}
}
