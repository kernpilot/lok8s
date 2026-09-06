package operator

// capi_test.go — the capi-reconcile cases of tests/operator/hooks_test.bats
// ported one-to-one, plus the template render the bats could not cover
// (the templates live only in the image — here a synthetic tree pins the
// glob order, the separators, the worker-pool loop and the unrestricted
// envsubst; a differential check against the real GNU envsubst runs when
// one is on PATH).

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

const capiCR = `{"metadata":{"name":"prod","namespace":"default","finalizers":[]},"spec":{"cluster":{"domain":"prod.lok8s.dev","namespace":"clusters"},"hcloud":{"region":"fsn1"}}}`

const capiDeletingCR = `{"metadata":{"name":"prod","namespace":"default","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["lok8s.dev/capi-teardown"]},"spec":{"cluster":{"domain":"prod.lok8s.dev","namespace":"clusters"}}}`

// minimal templates so the provision path reaches `kubectl apply -f -`
// (bats stubbed capi::generate_from_spec to `kind: Cluster`).
func (f *capiFixture) stubTemplates(t *testing.T) {
	f.writeTemplates(t, map[string]string{"core/cluster.yaml": "kind: Cluster\n"})
}

// bats: "capi-reconcile hook::config: events, synchronization, drift
// schedule, deletion" (+ the older "returns valid JSON" partials)
func TestCapiConfigPins(t *testing.T) {
	cfg := (&CapiHook{}).Config()
	for _, want := range []string{
		"configVersion: v1", "kind: Capi", `"Added", "Modified"`,
		"executeHookOnSynchronization: true", `crontab: "*/3 * * * *"`, "deletionTimestamp",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q", want)
		}
	}
}

// bats: "capi-reconcile detects hetzner/aws provider from spec", "fails for
// unknown provider"
func TestCapiDetectProvider(t *testing.T) {
	f := newCapiFixture(t)
	spec := func(s string) any {
		v, err := decode([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if p, ok := f.hook.detectProvider(spec(`{"hcloud":{"region":"fsn1","sshKeyName":"test-key"},"cluster":{"domain":"prod.lok8s.dev"}}`)); !ok || p != "hetzner" {
		t.Errorf("hetzner: got %q %v", p, ok)
	}
	if p, ok := f.hook.detectProvider(spec(`{"aws":{"region":"eu-central-1"},"cluster":{"domain":"aws.lok8s.dev"}}`)); !ok || p != "aws" {
		t.Errorf("aws: got %q %v", p, ok)
	}
	if _, ok := f.hook.detectProvider(spec(`{"cluster":{"domain":"gcp.lok8s.dev"}}`)); ok {
		t.Error("unknown provider must fail")
	}
	assertStderr(t, f.stderr, "error: no known CAPI provider found in Capi spec\n")
	// `jq -e '.hcloud'`: null and false are absent.
	if _, ok := f.hook.detectProvider(spec(`{"hcloud":null,"aws":false}`)); ok {
		t.Error("null/false provider keys must not detect")
	}
}

// bats: "capi-reconcile: fresh CR gets a finalizer, then provisions"
func TestCapiFreshCRProvisions(t *testing.T) {
	f := newCapiFixture(t)
	f.stubTemplates(t)
	f.hook.Reconcile(context.Background(), []byte(capiCR))
	assertHas(t, f.log, "/metadata/finalizers", `"phase":"Provisioning"`, "apply -f -")
	assertStderr(t, f.stderr, "info: reconciling Capi default/prod\n", "info: CAPI resources applied for prod\n")

	wantSeq := []string{
		`kubectl patch capi prod -n default --type json -p [{"op":"add","path":"/metadata/finalizers/-","value":"lok8s.dev/capi-teardown"}]`,
		`kubectl patch capi prod -n default --type merge --subresource status -p {"status":{"phase":"Provisioning","ready":false}}`,
		`kubectl patch capi prod -n default --type merge --subresource status -p {"status":{"provider":"hetzner"}}`,
		"kubectl apply -f -",
	}
	if !reflect.DeepEqual(f.log.lines, wantSeq) {
		t.Errorf("call sequence:\n%s\nwant:\n%s", f.log.text(), strings.Join(wantSeq, "\n"))
	}
	if got := f.log.stdins; len(got) != 1 || got[0] != "kind: Cluster\n" {
		t.Errorf("apply stdin = %q, want the rendered stream + one newline", got)
	}
}

// Provider detection failure → UnknownProvider, nothing rendered/applied.
func TestCapiUnknownProviderFails(t *testing.T) {
	f := newCapiFixture(t)
	f.stubTemplates(t)
	f.hook.Reconcile(context.Background(), []byte(`{"metadata":{"name":"gcp"},"spec":{"cluster":{"domain":"gcp.lok8s.dev"}}}`))
	assertHas(t, f.log, "UnknownProvider")
	refuteHas(t, f.log, "apply -f -", `"provider":`)
}

// Missing template tree → GenerationFailed (the repo layout has no
// capi-templates; only the image does).
func TestCapiMissingTemplatesFails(t *testing.T) {
	f := newCapiFixture(t)
	f.hook.Reconcile(context.Background(), []byte(capiCR))
	assertHas(t, f.log, `"provider":"hetzner"`, "GenerationFailed")
	refuteHas(t, f.log, "apply -f -")
	assertStderr(t, f.stderr, "error: CAPI template directory not found: "+f.tmpl+"\n")
}

// A rejected apply → ApplyFailed + the error line.
func TestCapiApplyFailure(t *testing.T) {
	f := newCapiFixture(t)
	f.stubTemplates(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.HasPrefix(argv(c), "kubectl apply") {
			return errors.New("webhook denied")
		}
		return capiKubectl(c)
	}
	f.hook.Reconcile(context.Background(), []byte(capiCR))
	assertHas(t, f.log, "ApplyFailed")
	assertStderr(t, f.stderr, "error: failed to apply CAPI resources for prod\n")
}

// bats: "capi-reconcile: deletion deletes the CAPI Cluster and removes the
// finalizer"
func TestCapiDeletionTearsDown(t *testing.T) {
	f := newCapiFixture(t)
	f.stubTemplates(t)
	f.hook.Reconcile(context.Background(), []byte(capiDeletingCR))
	assertHas(t, f.log,
		`"phase":"Terminating"`,
		// the anti-leak action: delete the CAPI Cluster in spec.cluster.namespace
		"delete cluster.cluster.x-k8s.io prod -n clusters",
		`{"metadata":{"finalizers":[]}}`,
	)
	// must NOT try to (re)provision a cluster being deleted
	refuteHas(t, f.log, "apply -f -")
	assertStderr(t, f.stderr, "info: Capi default/prod torn down (deleted CAPI Cluster clusters/prod)\n")
	wantDelete := "kubectl delete cluster.cluster.x-k8s.io prod -n clusters --wait=false --ignore-not-found"
	if got := f.log.matching("delete cluster"); len(got) != 1 || got[0] != wantDelete {
		t.Errorf("delete = %q, want [%q]", got, wantDelete)
	}
}

// bats: "capi-reconcile: failed Cluster delete keeps the finalizer for retry"
func TestCapiFailedDeleteKeepsFinalizer(t *testing.T) {
	f := newCapiFixture(t)
	f.runner.handler = func(c execx.Cmd) error {
		if strings.HasPrefix(argv(c), "kubectl delete cluster.cluster.x-k8s.io") {
			return errors.New("api down")
		}
		return capiKubectl(c)
	}
	f.hook.Reconcile(context.Background(), []byte(capiDeletingCR))
	assertHas(t, f.log, "DestroyFailed")
	refuteHas(t, f.log, `{"metadata":{"finalizers":[]}}`)
	assertStderr(t, f.stderr, "error: Capi default/prod teardown failed (will retry)\n")
}

// bats: "capi-reconcile: schedule event re-lists all Capi resources"
func TestCapiScheduleRelists(t *testing.T) {
	f := newCapiFixture(t)
	if err := f.hook.Trigger(context.Background(), mustEvents(t, `[{"type": "Schedule", "binding": "capi-drift"}]`)); err != nil {
		t.Fatal(err)
	}
	assertHas(t, f.log, "get capi -A -o json")
}

// The hook's own render: core/*.yaml in glob order joined by `---`, every
// providers/<p>/*.yaml prefixed by `---`, one machine-deployment per pool
// (keys SORTED, as jq's keys), exported values substituted, unset
// references blanked, and no export leak across CRs.
func TestCapiGenerateRender(t *testing.T) {
	f := newCapiFixture(t)
	f.writeTemplates(t, map[string]string{
		"core/cluster.yaml":               "name: ${CLUSTER_NAME}\nns: ${CLUSTER_NAMESPACE}\ndomain: ${CLUSTER_DOMAIN}\nk8s: ${K8S_VERSION}\ncp: ${CP_REPLICAS}\ncred: ${CREDENTIAL_SECRET_NAME}\n",
		"core/machine-deployment.yaml":    "pool: ${POOL_NAME}\nreplicas: ${POOL_REPLICAS}\ntype: ${POOL_TYPE}\narch: $ARCH\n",
		"core/notes.txt":                  "not a yaml, never rendered\n",
		"providers/hetzner/b-second.yaml": "kind: ${INFRA_MACHINE_TEMPLATE_KIND}\nkey: ${HCLOUD_SSH_KEY_NAME}\n",
		"providers/hetzner/a-first.yaml":  "kind: ${INFRA_CLUSTER_KIND}\napi: ${INFRA_API_VERSION}\nregion: ${HCLOUD_REGION}\n",
	})
	t.Setenv("ARCH", "")
	os.Unsetenv("ARCH")

	spec, _ := decode([]byte(`{"cluster":{"domain":"prod.example.com","namespace":"clusters"},"kubernetes":{"version":"v1.31.10"},"controlPlane":{"replicas":3},"hcloud":{"region":"fsn1","sshKeyName":"my-key"},"workers":{"zeta":{"replicas":2,"type":"cax21"},"alpha":{"type":"cax11"}}}`))
	got, ok := f.hook.generate(spec, "hetzner", "prod")
	if !ok {
		t.Fatalf("generate failed: %s", f.stderr.String())
	}
	want := strings.Join([]string{
		"name: prod\nns: clusters\ndomain: prod.example.com\nk8s: v1.31.10\ncp: 3\ncred: prod-credentials\n",
		"---\n",
		// core/machine-deployment.yaml rendered ONCE here with no POOL_* yet
		"pool: \nreplicas: \ntype: \narch: \n",
		"---\n",
		"kind: HetznerCluster\napi: infrastructure.cluster.x-k8s.io/v1beta1\nregion: fsn1\n",
		"---\n",
		"kind: HCloudMachineTemplate\nkey: my-key\n",
		"---\n",
		"pool: alpha\nreplicas: 1\ntype: cax11\narch: \n",
		"---\n",
		"pool: zeta\nreplicas: 2\ntype: cax21\narch: \n",
	}, "")
	if got != want {
		t.Errorf("render:\n%s\nwant:\n%s", got, want)
	}

	// Second CR in the same run, no workers: the exports were scoped to the
	// bash `$(…)` subshell, so nothing of the first render leaks — the core
	// machine-deployment renders blank again (hack/parity-operator.sh
	// measures this against the frozen hook).
	spec2, _ := decode([]byte(`{"cluster":{"domain":"two.example.com"},"aws":{"region":"eu-central-1"}}`))
	got, ok = f.hook.generate(spec2, "aws", "two")
	if !ok {
		t.Fatalf("generate failed: %s", f.stderr.String())
	}
	if strings.Contains(got, "zeta") || !strings.Contains(got, "pool: \nreplicas: \ntype: \n") {
		t.Errorf("POOL_* must not leak across CRs:\n%s", got)
	}
	if strings.Contains(got, "---\nkind: HetznerCluster") {
		t.Error("aws must not render the hetzner provider dir")
	}
	if !strings.Contains(got, "name: two\nns: default\ndomain: two.example.com\nk8s: v1.31.10\ncp: 1\ncred: two-credentials\n") {
		t.Errorf("defaults (namespace/version/replicas/secret) wrong:\n%s", got)
	}
}

// Unsupported provider name (unreachable from detect, pinned anyway) and a
// missing `.workers` entry field (`.type` → "null").
func TestCapiGenerateEdges(t *testing.T) {
	f := newCapiFixture(t)
	f.writeTemplates(t, map[string]string{"core/machine-deployment.yaml": "${POOL_NAME}/${POOL_REPLICAS}/${POOL_TYPE}\n"})
	spec, _ := decode([]byte(`{"workers":{"w":{}}}`))
	if _, ok := f.hook.generate(spec, "gcp", "x"); ok {
		t.Error("gcp must fail")
	}
	assertStderr(t, f.stderr, "error: unsupported CAPI provider: gcp\n")

	got, _ := f.hook.generate(spec, "hetzner", "x")
	if !strings.HasSuffix(got, "---\nw/1/null\n") {
		t.Errorf("pool defaults:\n%s", got)
	}
	// `.workers // empty` on an empty object still iterates zero pools.
	spec, _ = decode([]byte(`{"workers":{}}`))
	got, _ = f.hook.generate(spec, "hetzner", "x")
	if strings.Count(got, "---") != 0 {
		t.Errorf("empty workers must add no pool documents:\n%s", got)
	}
}

// envsubstAll is GNU envsubst's default (no SHELL-FORMAT) mode. When the
// real binary is around, diff against it on the repo's actual CAPI
// templates and on a stress line.
func TestEnvsubstAllMatchesGNU(t *testing.T) {
	bin, err := exec.LookPath("envsubst")
	if err != nil {
		t.Skip("no envsubst on PATH")
	}
	exported := map[string]string{"CLUSTER_NAME": "c1", "POOL_NAME": "p", "K8S_VERSION": "v1.31.10"}
	env := append(os.Environ(), "CLUSTER_NAME=c1", "POOL_NAME=p", "K8S_VERSION=v1.31.10")
	inputs := [][]byte{[]byte("a=$CLUSTER_NAME b=${POOL_NAME} c=$UNSET_X d=${K8S_VERSION}x e=$CLUSTER_NAMEX f=$( g=${POOL_NAME}\n")}
	files, _ := filepath.Glob(filepath.Join("..", "..", ".lok8s", "drivers", "capi", "cluster", "*", "*.yaml"))
	more, _ := filepath.Glob(filepath.Join("..", "..", ".lok8s", "drivers", "capi", "cluster", "providers", "*", "*.yaml"))
	for _, path := range append(files, more...) {
		raw, err := os.ReadFile(path)
		if err == nil {
			inputs = append(inputs, raw)
		}
	}
	if len(inputs) < 2 {
		t.Fatal("no CAPI templates found — the differential check measured nothing")
	}
	for i, in := range inputs {
		cmd := exec.Command(bin)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(string(in))
		want, err := cmd.Output()
		if err != nil {
			t.Fatalf("envsubst: %v", err)
		}
		if got := envsubstAll(in, exported); string(got) != string(want) {
			t.Errorf("input %d: envsubstAll diverges from GNU envsubst:\n got: %q\nwant: %q", i, got, want)
		}
	}
}
