package recover

// recover_test.go ports tests/unit/recover_test.bats: the phase sequence
// resolve → doctor → consent → rebuild → provision → verify with RECORDING
// fakes for the reused primitives, the timing summary, --skip-rebuild /
// --dry-run, the destructive consent guard (and its LOK8S_NONINTERACTIVE
// = CONSENT asymmetry), the resolve guards, and the bash-bridge argv.

import (
	"bytes"
	"context"
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
)

// fakeProvider records the primitives recover reuses.
type fakeProvider struct {
	record      *[]string
	noRebuild   bool
	noDoctor    bool
	rebuildFail bool
	output      string
	outputErr   error
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); err != nil {
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); err != nil {
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
	h.r.verify(context.Background())
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
	h.r.verify(context.Background())
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
	if _, ok := h.r.inventoryCount(context.Background()); ok {
		t.Error("failed output must be the sentinel")
	}
	h.prov.outputErr = nil
	if n, ok := h.r.inventoryCount(context.Background()); !ok || n != 3 {
		t.Errorf("got %d %v", n, ok)
	}
	h.prov.output = "not json"
	if _, ok := h.r.inventoryCount(context.Background()); ok {
		t.Error("non-JSON must be the sentinel")
	}
	h.prov.output = `{"api":{}}`
	if n, ok := h.r.inventoryCount(context.Background()); !ok || n != 0 {
		t.Errorf("missing nodes = genuine 0, got %d %v", n, ok)
	}
}

// bats: "recover verify shows 'unknown' (not 0) when the inventory could not be resolved"
func TestVerifyUnknownInventory(t *testing.T) {
	h := newHarness(t)
	h.prov.outputErr = errors.New("api error")
	h.r.domain, h.r.clusterName, h.r.spec, h.r.prov, h.r.config = "test.dom", "test.dom", h.spec, h.prov, h.cfg
	h.r.verify(context.Background())
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
	if got := h.r.kubeconfigPath(context.Background()); got != "/from/driver/test.dom.yaml" {
		t.Errorf("got %q", got)
	}
	h.r.DriverKubeconfig = func(ctx context.Context, d string) (string, error) { return "", errors.New("none") }
	if got, want := h.r.kubeconfigPath(context.Background()), filepath.Join(h.r.Paths.Base, ".kubeconfig", "test.dom.yaml"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// bats: "recover --skip-rebuild skips rebuild but still provisions + verifies"
func TestRunSkipRebuild(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	if err := h.r.Run(context.Background(), "test.dom", true, false); err != nil {
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
	if err := h.r.Run(context.Background(), "test.dom", false, true); err != nil {
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
		err := h.r.Run(context.Background(), "test.dom", false, false)
		if !errors.Is(err, ErrHandled) {
			t.Errorf("%q: err = %v", in, err)
		}
		if !strings.Contains(h.errBuf.String(), "\033[0;33m[warn]\033[0m recover: aborted by operator — nothing was changed\n") {
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); err != nil {
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); err != nil {
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); err != nil {
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
	if h.r.confirm(context.Background(), false) {
		t.Fatal("no must decline")
	}
	if !strings.Contains(h.errBuf.String(), "unknown number of nodes") || strings.Contains(h.errBuf.String(), "reset 0 node(s)") {
		t.Errorf("prompt = %q", h.errBuf.String())
	}

	h = newHarness(t)
	h.r.clusterName, h.r.prov = "test.dom", h.prov
	h.r.In = strings.NewReader("no\n")
	h.r.confirm(context.Background(), true)
	if want := "\033[31m!\033[0m recover: this will re-provision 3 node(s) of cluster test.dom (reinstalling Kubernetes, may wipe data disks) — the bare-metal node reset is SKIPPED — continue? [type yes to continue] "; h.errBuf.String() != want {
		t.Errorf("prompt = %q", h.errBuf.String())
	}

	h = newHarness(t)
	h.r.clusterName, h.r.prov = "test.dom", h.prov
	h.r.In = strings.NewReader("no\n")
	h.r.confirm(context.Background(), false)
	if want := "\033[31m!\033[0m recover: this will reset 3 node(s) of cluster test.dom from bare metal and reinstall them — continue? [type yes to continue] "; h.errBuf.String() != want {
		t.Errorf("prompt = %q", h.errBuf.String())
	}
}

// bats: "recover aborts before provision when rebuild fails"
func TestRunRebuildFailureStops(t *testing.T) {
	h := newHarness(t)
	h.r.Force = true
	h.prov.rebuildFail = true
	if err := h.r.Run(context.Background(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "\033[0;31m[error]\033[0m recover: rebuild failed — NOT provisioning on a half-reset cluster\n") {
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
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
	if err := h.r.loadProvider(context.Background()); !errors.Is(err, ErrHandled) {
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
	if err := h.r.loadProvider(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.r.provider != "mock" || h.r.config != filepath.Join(dir, "provider.yaml") {
		t.Errorf("provider=%q config=%q", h.r.provider, h.r.config)
	}
	if got := h.r.resolveClusterName(); got != "mocky" {
		t.Errorf("cluster name = %q", got)
	}
	h.r.NewProvider = func(ctx context.Context, name string) (Provider, error) { return nil, errors.New("not found") }
	if err := h.r.loadProvider(context.Background()); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errBuf.String(), "\033[0;31m[error]\033[0m recover: provider 'mock' failed to load\n") {
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
	err = h.r.Run(context.Background(), "../bad", false, false)
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
	if err := h.r.Run(context.Background(), "test.dom", false, false); !errors.Is(err, ErrHandled) {
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
	if err := h.r.Run(context.Background(), "nope.dom", false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if h.recorded() != "resolve" {
		t.Errorf("record = %q", h.recorded())
	}
}

// bats: the three _pick_domain cases.
func TestPickDomain(t *testing.T) {
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
	if want := "\033[0;31m[error]\033[0m too many arguments: stray-token\n"; errBuf.String() != want {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// Doctor rendering: the status → glyph table, incl. the pass-through of an
// unknown status and a message-less line.
func TestDoctorRendering(t *testing.T) {
	h := newHarness(t)
	h.r.provider, h.r.prov, h.r.config = "mock", h.prov, h.cfg
	h.r.doctor(context.Background())
	want := "\n--- provider / infrastructure (mock) ---\n  \033[32m✓\033[0m hcloud API reachable\n  \033[33m!\033[0m Robot creds unset\n    1 ok, 1 warn\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out.String(), want)
	}
	h.out.Reset()
	h.prov.noDoctor = true
	h.r.doctor(context.Background())
	if !strings.Contains(h.out.String(), "  \033[33m!\033[0m provider has no doctor hook — infrastructure diagnosis unavailable\n") {
		t.Errorf("stdout = %q", h.out.String())
	}
}

// ── the bash bridge: exact argv/env of every child ─────────

type argvRecorder struct {
	cmds  []execx.Cmd
	probe string
	fail  bool
}

func (a *argvRecorder) Run(ctx context.Context, c execx.Cmd) error {
	a.cmds = append(a.cmds, c)
	if a.fail {
		return errors.New("exit status 1")
	}
	if c.Stdout != nil && a.probe != "" {
		fmt.Fprint(c.Stdout, a.probe)
	}
	return nil
}

func TestBashBridgeArgv(t *testing.T) {
	h := newHarness(t)
	rec := &argvRecorder{probe: "rebuild\ndoctor\n"}
	h.r.Exec = rec
	prov, err := h.r.bashProvider(context.Background(), "hetzner")
	if err != nil {
		t.Fatal(err)
	}
	if !prov.HasRebuild() || !prov.HasDoctor() {
		t.Error("probe not parsed")
	}
	_ = prov.Doctor(context.Background(), "/cfg")
	_ = prov.Rebuild(context.Background(), "/cfg", "/wd")
	_, _ = prov.Output(context.Background(), "/cfg")
	_ = h.r.bashProvision(context.Background(), "test.dom")

	wantArgs := [][]string{
		{"-c", providerProbeScript, "lo-recover-provider", "hetzner"},
		{"-c", providerCallScript, "lo-recover-provider", "hetzner", "provider::doctor", "/cfg"},
		{"-c", providerCallScript, "lo-recover-provider", "hetzner", "provider::rebuild", "/cfg", "/wd"},
		{"-c", providerCallScript, "lo-recover-provider", "hetzner", "provider::output", "/cfg"},
		{"-c", provisionScript, "lo-recover-provision", "test.dom"},
	}
	if len(rec.cmds) != len(wantArgs) {
		t.Fatalf("%d children, want %d", len(rec.cmds), len(wantArgs))
	}
	p := h.r.Paths
	for i, c := range rec.cmds {
		if c.Name != "bash" || strings.Join(c.Args, "\x00") != strings.Join(wantArgs[i], "\x00") {
			t.Errorf("child %d: %s %q", i, c.Name, c.Args)
		}
		if c.Dir != p.Base {
			t.Errorf("child %d: dir = %q", i, c.Dir)
		}
		env := strings.Join(c.Env, "\n")
		for _, want := range []string{"PATH_BASE=" + p.Base, "PATH_BIN=" + p.Bin, "PATH_LOK8S=" + p.Lok8s, "PATH_CLUSTERS=" + p.Clusters, "PATH_SCRIPTS=" + p.Lok8s, "PATH_SECRETS=" + filepath.Join(p.Base, ".secrets")} {
			if !strings.Contains(env, want) {
				t.Errorf("child %d: env missing %q", i, want)
			}
		}
		// shimEnv order: .bin ends up first, then .lok8s, then the inherited PATH.
		if !strings.HasPrefix(c.Env[0], "PATH="+p.Bin+string(os.PathListSeparator)+p.Lok8s+string(os.PathListSeparator)) {
			t.Errorf("child %d: PATH = %q", i, c.Env[0])
		}
	}
	// The scripts hold the load-bearing pieces verbatim.
	for _, want := range []string{`provider::load "${1}" >/dev/null`, "declare -F provider::rebuild", "declare -F provider::doctor"} {
		if !strings.Contains(providerProbeScript, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	for _, want := range []string{"import ^libs/provision", "import ^libs/bootstrap", "import ^libs/kubehz/main", "import ^libs/inventory/main", "import ^libs/gitops", "force=1\nprovision::dispatch \"${1}\""} {
		if !strings.Contains(provisionScript, want) {
			t.Errorf("provision script missing %q", want)
		}
	}
	// A failing probe is a failed load.
	h.r.Exec = &argvRecorder{fail: true}
	if _, err := h.r.bashProvider(context.Background(), "nosuch"); err == nil {
		t.Error("failed probe must fail the load")
	}
}
