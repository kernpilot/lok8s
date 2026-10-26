package addons

// suc_rbac_test.go — the gate for the system-upgrade-controller addon: the
// RBAC that replaces the chart's ClusterRoleBinding to cluster-admin, and
// the digest pins of the two images.
//
// The RBAC check is static. sucCalls is an inventory of the Kubernetes API
// requests of the controller (system-upgrade-controller v0.20.1 and the
// libraries it calls) and of the cordon and drain containers of a Plan's
// Job (kubectl pkg/drain). It is written by hand from that source, each
// entry with its file:line. It is not a runtime proof: a request that the
// inventory misses is missed here too. A bump of the controller starts
// with a new read of the source.
//
// The inventory leaves out API discovery (wrangler apply.go:166,
// ServerResourcesForGroupVersion). The addon grants no rule for it: the
// default ClusterRoleBinding system:discovery gives it to every
// authenticated user. A cluster that removes that binding breaks the
// controller.
//
// The test evaluates the roles in rbac.yaml against the inventory in both
// directions: every request must be allowed, and every rule (each group,
// resource, verb and resource name) must serve a request at the scope that
// the rule's binding grants. A ClusterRole rule must serve a request on a
// cluster-scoped resource or across namespaces; a request in one known
// namespace goes through a Role there. A rule that grants more fails, and
// so does a request that loses its grant.
//
// TestSystemUpgradeControllerRBAC is offline: it reads the ServiceAccount,
// the namespace, the Lease name and the chart's binding from a committed
// fixture of the chart render, and renders the addon's patch over that
// fixture through internal/render. TestSystemUpgradeControllerFixtureMatchesChart
// renders the chart itself (khelm) and reads the upstream manifests (both
// online) and diffs the fixtures against them; it runs on CI and with
// LOK8S_ONLINE_TESTS=1.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	sucAddon         = "internal/assets/lok8s/addons/system-upgrade-controller"
	sucAdminAddon    = "internal/assets/lok8s/addons/system-upgrade-controller-cluster-admin"
	sucFixture       = "testdata/system-upgrade-controller/chart-render.yaml"
	sucUpstreamNames = "testdata/system-upgrade-controller/upstream-rbac-names.yaml"

	// The names rbac.yaml defines; each binding has the name of its role.
	sucRole        = "lok8s-system-upgrade-controller"
	sucEventsRole  = "lok8s-system-upgrade-controller-events"
	sucDrainerRole = "lok8s-system-upgrade-controller-drainer"
	// sucEventsNS is where the recorder writes the events on a Node.
	sucEventsNS = "default"
	// sucChartBinding is the chart's ClusterRoleBinding to cluster-admin.
	sucChartBinding = "system-upgrade-controller"
	// sucNameLabel is the pod label the chart turns into
	// SYSTEM_UPGRADE_CONTROLLER_NAME, the name of the Lease.
	sucNameLabel = "upgrade.cattle.io/controller"
)

// Scope and name values of a sucCall.
const (
	// sucAtCluster: a cluster-scoped resource, or a request across all
	// namespaces.
	sucAtCluster = ""
	// sucAtAnyNS: one namespace that the request does not know in advance.
	sucAtAnyNS = "*"
	// sucAtCtrlNS: the controller's namespace (read from the fixture).
	sucAtCtrlNS = "@namespace"
	// sucAnyName: the request names no object (list, watch, create), or an
	// object that RBAC cannot know in advance.
	sucAnyName = ""
	// sucLease: the controller's Lease (read from the fixture).
	sucLease = "@lease"
)

// The callers in sucCalls: the controller, and the Plan features whose Job
// containers call the API as the Plan's ServiceAccount.
const (
	sucByController   = "controller"
	sucByCordon       = "spec.cordon"
	sucByDrain        = "spec.drain"
	sucByDrainDS      = "spec.drain.ignoreDaemonSets"
	sucByDrainNoEvict = "spec.drain.disableEviction"
)

// sucCall is one Kubernetes API request.
type sucCall struct {
	by       string
	group    string
	resource string // with the subresource: plans/status
	verb     string
	ns       string
	name     string
	source   string
}

// sucCalls: SUC = rancher/system-upgrade-controller v0.20.1 (f1362a3),
// wrangler = rancher/wrangler v3.7.0-rc.1, client-go = k8s.io/client-go
// v0.36.0, kubectl = k8s.io/kubectl v0.36.2. A cache (informer) makes a
// list and a watch request. Against an apiserver that serves watch-list
// (1.35 does) it makes one watch with sendInitialEvents and falls back to
// list and watch when that fails, so the rules keep both verbs. A read from
// the cache makes no request.
var sucCalls = []sucCall{
	// The Plan CRD, on every start (SUC pkg/upgrade/controller.go:147,194).
	{sucByController, "apiextensions.k8s.io", "customresourcedefinitions", "list", sucAtCluster, sucAnyName, "wrangler pkg/crd/crd.go:64"},
	{sucByController, "apiextensions.k8s.io", "customresourcedefinitions", "create", sucAtCluster, sucAnyName, "wrangler pkg/crd/crd.go:83"},
	{sucByController, "apiextensions.k8s.io", "customresourcedefinitions", "get", sucAtCluster, "plans.upgrade.cattle.io", "wrangler pkg/crd/crd.go:91,114"},
	{sucByController, "apiextensions.k8s.io", "customresourcedefinitions", "update", sucAtCluster, "plans.upgrade.cattle.io", "wrangler pkg/crd/crd.go:101"},
	// The cluster ID.
	{sucByController, "", "namespaces", "get", sucAtCluster, "kube-system", "SUC pkg/upgrade/controller.go:141"},
	// The node cache, and the Plan label written after a completed Job.
	{sucByController, "", "nodes", "list", sucAtCluster, sucAnyName, "SUC pkg/upgrade/handle_core.go:16"},
	{sucByController, "", "nodes", "watch", sucAtCluster, sucAnyName, "SUC pkg/upgrade/handle_core.go:16"},
	{sucByController, "", "nodes", "update", sucAtCluster, sucAnyName, "SUC pkg/upgrade/handle_batch.go:139"},
	// The Plan's Jobs. wrangler apply lists them in all namespaces: an
	// owner and no lister namespace (SUC handle_upgrade.go:114).
	{sucByController, "batch", "jobs", "list", sucAtCluster, sucAnyName, "wrangler pkg/apply/desiredset_process.go:362,461"},
	{sucByController, "batch", "jobs", "list", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_batch.go:30 (cache, controller.go:103)"},
	{sucByController, "batch", "jobs", "watch", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_batch.go:30 (cache, controller.go:103)"},
	{sucByController, "batch", "jobs", "create", sucAtCtrlNS, sucAnyName, "wrangler pkg/apply/desiredset_crud.go:43"},
	{sucByController, "batch", "jobs", "get", sucAtCtrlNS, sucAnyName, "wrangler pkg/apply/desiredset_crud.go:50"},
	{sucByController, "batch", "jobs", "patch", sucAtCtrlNS, sucAnyName, "wrangler pkg/apply/desiredset_process.go:189"},
	{sucByController, "batch", "jobs", "delete", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_batch.go:180; wrangler pkg/apply/desiredset_crud.go:68"},
	// The Plans and their status.
	{sucByController, "upgrade.cattle.io", "plans", "list", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/controller.go:110 (cache)"},
	{sucByController, "upgrade.cattle.io", "plans", "watch", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/controller.go:110 (cache)"},
	{sucByController, "upgrade.cattle.io", "plans", "get", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_batch.go:54"},
	{sucByController, "upgrade.cattle.io", "plans/status", "update", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_batch.go:98; generated controllers/upgrade.cattle.io/v1/plan.go:121"},
	// The Secret cache. Reads go to the cache only (SUC pkg/upgrade/plan/plan.go:62,256).
	{sucByController, "", "secrets", "list", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_core.go:42, handle_upgrade.go:25"},
	{sucByController, "", "secrets", "watch", sucAtCtrlNS, sucAnyName, "SUC pkg/upgrade/handle_core.go:42, handle_upgrade.go:25"},
	// Events: on Plans in the controller's namespace, on the node in default.
	{sucByController, "", "events", "create", sucAtCtrlNS, sucAnyName, "SUC controller.go:124; client-go tools/record/event.go:340"},
	{sucByController, "", "events", "patch", sucAtCtrlNS, sucAnyName, "SUC controller.go:124; client-go tools/record/event.go:334"},
	{sucByController, "", "events", "create", sucEventsNS, sucAnyName, "SUC controller.go:171; client-go tools/record/event.go:340,491"},
	{sucByController, "", "events", "patch", sucEventsNS, sucAnyName, "SUC controller.go:171; client-go tools/record/event.go:334,491"},
	// Leader election (SUC controller.go:176; wrangler pkg/leader/leader.go:47).
	{sucByController, "coordination.k8s.io", "leases", "create", sucAtCtrlNS, sucAnyName, "client-go tools/leaderelection/resourcelock/leaselock.go:68"},
	{sucByController, "coordination.k8s.io", "leases", "get", sucAtCtrlNS, sucLease, "client-go tools/leaderelection/resourcelock/leaselock.go:43"},
	{sucByController, "coordination.k8s.io", "leases", "update", sucAtCtrlNS, sucLease, "client-go tools/leaderelection/resourcelock/leaselock.go:89"},

	// kubectl cordon (SUC pkg/upgrade/job/job.go:423).
	{sucByCordon, "", "nodes", "get", sucAtCluster, sucAnyName, "kubectl pkg/cmd/drain/drain.go:292-305"},
	{sucByCordon, "", "nodes", "patch", sucAtCluster, sucAnyName, "kubectl pkg/drain/cordon.go:104"},
	// kubectl drain (SUC pkg/upgrade/job/job.go:367).
	{sucByDrain, "", "nodes", "get", sucAtCluster, sucAnyName, "kubectl pkg/cmd/drain/drain.go:292-305"},
	{sucByDrain, "", "nodes", "patch", sucAtCluster, sucAnyName, "kubectl pkg/drain/cordon.go:104"},
	{sucByDrain, "", "pods", "list", sucAtCluster, sucAnyName, "kubectl pkg/drain/drain.go:198"},
	{sucByDrain, "", "pods", "get", sucAtAnyNS, sucAnyName, "kubectl pkg/drain/drain.go:254"},
	{sucByDrain, "", "pods/eviction", "create", sucAtAnyNS, sucAnyName, "kubectl pkg/drain/drain.go:165"},
	// --ignore-daemonsets, on unless the Plan turns it off (job.go:368).
	{sucByDrainDS, "apps", "daemonsets", "get", sucAtAnyNS, sucAnyName, "kubectl pkg/drain/filters.go:195"},
	// --disable-eviction (job.go:391).
	{sucByDrainNoEvict, "", "pods", "delete", sucAtAnyNS, sucAnyName, "kubectl pkg/drain/drain.go:148,257"},
}

// sucNames holds what the test reads from the fixture.
type sucNames struct {
	namespace, serviceAccount, lease string
	chartBinding                     object
}

// resolve replaces the fixture placeholders of a call.
func (n sucNames) resolve(c sucCall) sucCall {
	if c.ns == sucAtCtrlNS {
		c.ns = n.namespace
	}
	if c.name == sucLease {
		c.name = n.lease
	}
	return c
}

// sucRuleAllows reports whether one rule allows a call (no wildcards: the
// test rejects them before it evaluates).
func sucRuleAllows(r map[string]any, c sucCall) bool {
	if !slices.Contains(stringsOf(r["apiGroups"]), c.group) ||
		!slices.Contains(stringsOf(r["resources"]), c.resource) ||
		!slices.Contains(stringsOf(r["verbs"]), c.verb) {
		return false
	}
	names := stringsOf(r["resourceNames"])
	return len(names) == 0 || (c.name != sucAnyName && slices.Contains(names, c.name))
}

// sucGrant is a role as one binding applies it: in every namespace and at
// the cluster scope (ns == ""), or in one namespace.
type sucGrant struct {
	role object
	ns   string
}

func (g sucGrant) allows(c sucCall) bool {
	if g.ns != "" && c.ns != g.ns {
		return false
	}
	for _, r := range g.role.Rules() {
		if sucRuleAllows(r, c) {
			return true
		}
	}
	return false
}

// sucCheckNoWildcards rejects `*` in any rule field and nonResourceURLs.
func sucCheckNoWildcards(t *testing.T, o object) {
	t.Helper()
	for _, r := range o.Rules() {
		if _, ok := r["nonResourceURLs"]; ok {
			t.Errorf("%s/%s: nonResourceURLs in %s", o.Kind, o.Name, canon(t, r))
		}
		for _, f := range []string{"apiGroups", "resources", "verbs", "resourceNames"} {
			if slices.Contains(stringsOf(r[f]), "*") {
				t.Errorf("%s/%s: wildcard in %s: %s", o.Kind, o.Name, f, canon(t, r))
			}
		}
	}
}

// sucCheckRulesNeeded asserts that every atom of every rule of g (group,
// resource, verb, and each resource name or none) serves a call of `by` at
// the scope g grants. A cluster-wide grant must serve a call on a
// cluster-scoped resource or across namespaces (sucAtCluster, sucAtAnyNS):
// a call in a known namespace belongs to a Role there. A namespaced grant
// must serve a call in its namespace. A rule without resourceNames must
// serve a call that RBAC cannot restrict by name.
func sucCheckRulesNeeded(t *testing.T, g sucGrant, calls []sucCall) {
	t.Helper()
	for _, r := range g.role.Rules() {
		names := stringsOf(r["resourceNames"])
		if len(names) == 0 {
			names = []string{sucAnyName}
		}
		for _, grp := range stringsOf(r["apiGroups"]) {
			for _, res := range stringsOf(r["resources"]) {
				for _, verb := range stringsOf(r["verbs"]) {
					for _, name := range names {
						needed := slices.ContainsFunc(calls, func(c sucCall) bool {
							if c.group != grp || c.resource != res || c.verb != verb || c.name != name {
								return false
							}
							if g.ns == "" {
								return c.ns == sucAtCluster || c.ns == sucAtAnyNS
							}
							return c.ns == g.ns
						})
						if !needed {
							scope := "cluster-wide"
							if g.ns != "" {
								scope = "in " + g.ns
							}
							t.Errorf("%s/%s grants %s %q/%s name %q %s, and no call in sucCalls needs it at that scope", g.role.Kind, g.role.Name, verb, grp, res, name, scope)
						}
					}
				}
			}
		}
	}
}

// sucCallsBy returns the resolved calls of the given callers.
func sucCallsBy(n sucNames, by ...string) []sucCall {
	var out []sucCall
	for _, c := range sucCalls {
		if slices.Contains(by, c.by) {
			out = append(out, n.resolve(c))
		}
	}
	return out
}

// sucNested walks nested maps; a missing key gives an empty map.
func sucNested(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		m = next
	}
	return m
}

// readSUCFixture decodes the fixture and reads the names the RBAC depends on.
func readSUCFixture(t *testing.T) (map[string]object, sucNames) {
	t.Helper()
	objs := readObjects(t, sucFixture)
	var n sucNames
	for _, o := range objs {
		if o.Kind == "ServiceAccount" {
			if n.serviceAccount != "" {
				t.Fatalf("%s: more than one ServiceAccount", sucFixture)
			}
			n.serviceAccount, n.namespace = o.Name, o.Namespace
		}
	}
	dep, ok := objs["Deployment/system-upgrade-controller"]
	if !ok || n.serviceAccount == "" || n.namespace == "" {
		t.Fatalf("%s: no Deployment/system-upgrade-controller or no ServiceAccount: %v", sucFixture, sortedKeys(objs))
	}
	podSpec := sucNested(dep.Doc, "spec", "template", "spec")
	if sa, _ := podSpec["serviceAccountName"].(string); sa != n.serviceAccount || dep.Namespace != n.namespace {
		t.Fatalf("the Deployment runs as %s/%s, the fixture's ServiceAccount is %s/%s", dep.Namespace, sa, n.namespace, n.serviceAccount)
	}
	// The controller names its Lease after SYSTEM_UPGRADE_CONTROLLER_NAME
	// (SUC main.go:59-63, controller.go:176) in its own namespace
	// (SYSTEM_UPGRADE_CONTROLLER_NAMESPACE); the chart fills both from the pod.
	env := map[string]string{}
	containers, _ := podSpec["containers"].([]any)
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		vars, _ := cm["env"].([]any)
		for _, e := range vars {
			em, _ := e.(map[string]any)
			name, _ := em["name"].(string)
			path, _ := sucNested(em, "valueFrom", "fieldRef")["fieldPath"].(string)
			env[name] = path
		}
	}
	if got, want := env["SYSTEM_UPGRADE_CONTROLLER_NAME"], "metadata.labels['"+sucNameLabel+"']"; got != want {
		t.Fatalf("SYSTEM_UPGRADE_CONTROLLER_NAME comes from %q, want %q", got, want)
	}
	if got := env["SYSTEM_UPGRADE_CONTROLLER_NAMESPACE"]; got != "metadata.namespace" {
		t.Fatalf("SYSTEM_UPGRADE_CONTROLLER_NAMESPACE comes from %q, want metadata.namespace", got)
	}
	n.lease, _ = sucNested(dep.Doc, "spec", "template", "metadata", "labels")[sucNameLabel].(string)
	if n.lease == "" {
		t.Fatalf("the Deployment's pod template has no %s label", sucNameLabel)
	}
	n.chartBinding, ok = objs["ClusterRoleBinding/"+sucChartBinding]
	if !ok {
		t.Fatalf("%s: no ClusterRoleBinding/%s", sucFixture, sucChartBinding)
	}
	return objs, n
}

// sucBindsSA reports whether a binding names the controller's ServiceAccount.
func sucBindsSA(b object, n sucNames) bool {
	subjects, _ := b.Doc["subjects"].([]any)
	for _, s := range subjects {
		m, _ := s.(map[string]any)
		if m["kind"] == "ServiceAccount" && m["name"] == n.serviceAccount && m["namespace"] == n.namespace {
			return true
		}
	}
	return false
}

// checkSUCBindings asserts that exactly the four bindings of rbac.yaml
// bind the controller's ServiceAccount, each with that one subject and in
// its namespace, and that the chart's binding to cluster-admin is gone.
func checkSUCBindings(t *testing.T, objs map[string]object, n sucNames) {
	t.Helper()
	// binding -> namespace + " " + roleRef
	want := map[string]string{
		"ClusterRoleBinding/" + sucRole:        " ClusterRole/" + sucRole,
		"RoleBinding/" + sucRole:               n.namespace + " Role/" + sucRole,
		"RoleBinding/" + sucEventsRole:         sucEventsNS + " Role/" + sucEventsRole,
		"ClusterRoleBinding/" + sucDrainerRole: " ClusterRole/" + sucDrainerRole,
	}
	got := map[string]string{}
	for _, k := range sortedKeys(objs) {
		o := objs[k]
		if o.Kind != "ClusterRoleBinding" && o.Kind != "RoleBinding" || !sucBindsSA(o, n) {
			continue
		}
		ref, _ := o.Doc["roleRef"].(map[string]any)
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		got[k] = o.Namespace + " " + kind + "/" + name
		if subjects, _ := o.Doc["subjects"].([]any); len(subjects) != 1 {
			t.Errorf("%s has %d subjects, want only %s/%s", k, len(subjects), n.namespace, n.serviceAccount)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bindings of %s/%s (namespace roleRef):\n got: %v\nwant: %v", n.namespace, n.serviceAccount, got, want)
	}
	if _, ok := objs["ClusterRoleBinding/"+sucChartBinding]; ok {
		t.Errorf("ClusterRoleBinding/%s (the chart's binding to cluster-admin) is still in the render", sucChartBinding)
	}
}

// sucUpstreamName is one entry of upstream-rbac-names.yaml.
type sucUpstreamName struct {
	Kind   string `yaml:"kind"`
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
}

func readSUCUpstreamNames(t *testing.T) []sucUpstreamName {
	t.Helper()
	raw, err := os.ReadFile(sucUpstreamNames)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Names []sucUpstreamName `yaml:"names"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Names) == 0 {
		t.Fatalf("%s: no names", sucUpstreamNames)
	}
	return f.Names
}

// checkSUCNameCollisions asserts that no RBAC object of rbac.yaml uses a
// name that an upstream install uses for an RBAC object of any kind.
func checkSUCNameCollisions(t *testing.T, rbac map[string]object, upstream []sucUpstreamName) {
	t.Helper()
	for _, k := range sortedKeys(rbac) {
		o := rbac[k]
		for _, u := range upstream {
			if o.Name == u.Name {
				t.Errorf("%s uses the name of the upstream %s/%s (%s): pick a name no upstream install uses", k, u.Kind, u.Name, u.Source)
			}
		}
	}
}

// sucImage is one image pin in values.yaml.
type sucImage struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
}

var sucDigestTag = regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}$`)

// readSUCImages reads the two image pins from values.yaml.
func readSUCImages(t *testing.T, root string) (ctrl, kubectl sucImage) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, sucAddon, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Controller struct {
			Image sucImage `yaml:"image"`
		} `yaml:"systemUpgradeController"`
		Kubectl struct {
			Image sucImage `yaml:"image"`
		} `yaml:"kubectl"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	return values.Controller.Image, values.Kubectl.Image
}

// checkSUCImages asserts that values.yaml pins both images by digest and
// that the chart render (objs) uses exactly those references.
func checkSUCImages(t *testing.T, root string, objs map[string]object) {
	t.Helper()
	ctrlImg, kubectlImg := readSUCImages(t, root)
	for _, img := range []sucImage{ctrlImg, kubectlImg} {
		if img.Repository == "" || !sucDigestTag.MatchString(img.Tag) {
			t.Errorf("values.yaml: image %q tag %q is not `<version>@sha256:<64 hex>`", img.Repository, img.Tag)
		}
	}
	ctrl := ctrlImg.Repository + ":" + ctrlImg.Tag
	kubectl := kubectlImg.Repository + ":" + kubectlImg.Tag

	dep := objs["Deployment/system-upgrade-controller"]
	var images []string
	for _, kind := range []string{"containers", "initContainers"} {
		seq, _ := sucNested(dep.Doc, "spec", "template", "spec")[kind].([]any)
		for _, c := range seq {
			cm, _ := c.(map[string]any)
			img, _ := cm["image"].(string)
			images = append(images, img)
		}
	}
	if !reflect.DeepEqual(images, []string{ctrl}) {
		t.Errorf("Deployment images %v, want [%s]", images, ctrl)
	}
	data, _ := objs["ConfigMap/system-upgrade-controller-config"].Doc["data"].(map[string]any)
	for _, key := range []string{"SYSTEM_UPGRADE_JOB_KUBECTL_IMAGE", "SYSTEM_UPGRADE_JOB_KUBECTL_IMAGE_WINDOWS"} {
		if data[key] != kubectl {
			t.Errorf("ConfigMap %s = %v, want %s", key, data[key], kubectl)
		}
	}
}

// sucChartVersion reads `version:` from the addon's chart.yaml.
func sucChartVersion(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, sucAddon, "chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Chart   string `yaml:"chart"`
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Chart != "system-upgrade-controller" || spec.Version == "" {
		t.Fatalf("chart.yaml: chart=%q version=%q", spec.Chart, spec.Version)
	}
	return spec.Version
}

// sucResources reads the `resources:` list of a kustomization.yaml.
func sucResources(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kz struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kz); err != nil {
		t.Fatal(err)
	}
	return kz.Resources
}

// checkSUCPatch asserts the kustomization lists rbac.yaml and deletes the
// chart's binding with one anchored, targeted `$patch: delete`.
func checkSUCPatch(t *testing.T, addon string, entries []patchEntry) {
	t.Helper()
	if res := sucResources(t, addon); !slices.Contains(res, "rbac.yaml") {
		t.Errorf("kustomization.yaml does not list rbac.yaml: %v", res)
	}
	if len(entries) != 1 {
		t.Fatalf("kustomization.yaml has %d patches, want the one delete of ClusterRoleBinding/%s", len(entries), sucChartBinding)
	}
	e := entries[0]
	var body struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(e.Patch), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Patch, "$patch: delete") || body.Kind != "ClusterRoleBinding" || body.Metadata.Name != sucChartBinding ||
		e.Target.Kind != "ClusterRoleBinding" || e.Target.Name != "^"+sucChartBinding+"$" {
		t.Errorf("the patch is not an anchored, targeted delete of ClusterRoleBinding/%s: target %+v\n%s", sucChartBinding, e.Target, e.Patch)
	}
}

func TestSystemUpgradeControllerRBAC(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, sucAddon)
	fixture, n := readSUCFixture(t)
	if pin, fix := sucChartVersion(t, root), fixtureChartVersion(t, sucFixture, "system-upgrade-controller"); pin != fix {
		t.Fatalf("chart.yaml pins system-upgrade-controller %s but %s was rendered from %s: refresh it (see its header)", pin, sucFixture, fix)
	}
	// The fixture proves the unpatched chart binds the controller to
	// cluster-admin; the delete patch must remove exactly that object.
	if ref, _ := n.chartBinding.Doc["roleRef"].(map[string]any); ref["name"] != "cluster-admin" || !sucBindsSA(n.chartBinding, n) {
		t.Fatalf("%s: ClusterRoleBinding/%s is not the chart's binding of %s/%s to cluster-admin", sucFixture, sucChartBinding, n.namespace, n.serviceAccount)
	}

	rbac := readObjects(t, filepath.Join(addon, "rbac.yaml"))
	clusterRole, ok1 := rbac["ClusterRole/"+sucRole]
	role, ok2 := rbac["Role/"+sucRole]
	eventsRole, ok3 := rbac["Role/"+sucEventsRole]
	drainer, ok4 := rbac["ClusterRole/"+sucDrainerRole]
	if !ok1 || !ok2 || !ok3 || !ok4 || role.Namespace != n.namespace || eventsRole.Namespace != sucEventsNS {
		t.Fatalf("rbac.yaml: want ClusterRole/%s, Role %s/%s, Role %s/%s and ClusterRole/%s, have %v",
			sucRole, n.namespace, sucRole, sucEventsNS, sucEventsRole, sucDrainerRole, sortedKeys(rbac))
	}
	for _, o := range []object{clusterRole, role, eventsRole, drainer} {
		sucCheckNoWildcards(t, o)
	}
	checkSUCBindings(t, rbac, n)

	// The fixture of upstream names must know the chart's own binding, and
	// rbac.yaml must reuse none of them.
	upstream := readSUCUpstreamNames(t)
	if !slices.ContainsFunc(upstream, func(u sucUpstreamName) bool {
		return u.Kind == "ClusterRoleBinding" && u.Name == n.chartBinding.Name
	}) {
		t.Errorf("%s lacks the chart's ClusterRoleBinding/%s", sucUpstreamNames, n.chartBinding.Name)
	}
	checkSUCNameCollisions(t, rbac, upstream)

	// Every call is allowed. The controller's calls by its three roles
	// alone; each Plan feature's calls by the drainer role alone, so that a
	// Plan ServiceAccount bound to it can cordon and drain.
	ctrlGrants := []sucGrant{{clusterRole, ""}, {role, n.namespace}, {eventsRole, sucEventsNS}}
	for _, c := range sucCallsBy(n, sucByController) {
		if !slices.ContainsFunc(ctrlGrants, func(g sucGrant) bool { return g.allows(c) }) {
			t.Errorf("the controller's roles do not allow %s %q/%s name %q in %q (%s)", c.verb, c.group, c.resource, c.name, c.ns, c.source)
		}
	}
	features := []string{sucByCordon, sucByDrain, sucByDrainDS, sucByDrainNoEvict}
	for _, c := range sucCallsBy(n, features...) {
		if !(sucGrant{drainer, ""}).allows(c) {
			t.Errorf("ClusterRole/%s does not allow %s %q/%s in %q, which %s needs (%s)", sucDrainerRole, c.verb, c.group, c.resource, c.ns, c.by, c.source)
		}
	}
	// No rule grants more than the calls need, at the scope it is granted.
	for _, g := range ctrlGrants {
		sucCheckRulesNeeded(t, g, sucCallsBy(n, sucByController))
	}
	sucCheckRulesNeeded(t, sucGrant{drainer, ""}, sucCallsBy(n, features...))

	// The image pins, and the chart render that uses them.
	checkSUCImages(t, root, fixture)

	// The opt-in addon restores exactly the chart's binding.
	admin := readObjects(t, filepath.Join(root, sucAdminAddon, "clusterrolebinding.yaml"))
	if got, ok := admin["ClusterRoleBinding/"+sucChartBinding]; !ok || len(admin) != 1 || !reflect.DeepEqual(got.Doc, n.chartBinding.Doc) {
		t.Errorf("%s/clusterrolebinding.yaml\n got: %s\nwant (the chart's binding): %s", sucAdminAddon, canon(t, admin), canon(t, n.chartBinding.Doc))
	}
	if res := sucResources(t, filepath.Join(root, sucAdminAddon)); !reflect.DeepEqual(res, []string{"clusterrolebinding.yaml"}) {
		t.Errorf("%s/kustomization.yaml resources %v, want [clusterrolebinding.yaml]", sucAdminAddon, res)
	}

	patches, entries := patchesNode(t, addon)
	checkSUCPatch(t, addon, entries)
	if t.Failed() {
		t.FailNow()
	}

	// The addon's real patch over the chart render plus rbac.yaml.
	requireRenderToolchain(t, root, "the system-upgrade-controller RBAC render gate")
	fx, err := os.ReadFile(sucFixture)
	if err != nil {
		t.Fatal(err)
	}
	rbacRaw, err := os.ReadFile(filepath.Join(addon, "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderPatched(t, root, [][]byte{fx, rbacRaw}, patches)
	checkSUCBindings(t, rendered, n)
	for k, o := range rbac {
		if !reflect.DeepEqual(rendered[k].Doc, o.Doc) {
			t.Errorf("%s changed in the render:\n got: %s\nwant: %s", k, canon(t, rendered[k].Doc), canon(t, o.Doc))
		}
	}
}

// sucFetchUpstreamRBAC reads the RBAC objects of upstream SUC's
// manifests/ at a release tag.
func sucFetchUpstreamRBAC(t *testing.T, tag string) map[string]object {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	out := map[string]object{}
	for _, f := range []string{"clusterrole.yaml", "clusterrolebinding.yaml", "system-upgrade-controller.yaml"} {
		url := "https://raw.githubusercontent.com/rancher/system-upgrade-controller/" + tag + "/manifests/" + f
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, %v", url, resp.StatusCode, err)
		}
		for k, o := range decodeObjects(t, body) {
			if strings.HasSuffix(o.Kind, "Role") || strings.HasSuffix(o.Kind, "RoleBinding") {
				out[k] = o
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no RBAC objects in the upstream manifests at %s", tag)
	}
	return out
}

// TestSystemUpgradeControllerFixtureMatchesChart renders the pinned chart
// through the addon's own generator (khelm, so it downloads the chart): the
// addon without its RBAC changes must equal the fixture, and the addon as
// shipped must carry the narrowed bindings and the pinned images. It also
// renders the opt-in addon, and checks that every RBAC name in the upstream
// manifests of the pinned controller version is in upstream-rbac-names.yaml.
func TestSystemUpgradeControllerFixtureMatchesChart(t *testing.T) {
	t.Parallel()
	if os.Getenv("CI") == "" && os.Getenv(onlineTestsEnv) == "" {
		t.Skipf("downloads the pinned chart and the upstream manifests: runs on CI, or with %s=1", onlineTestsEnv)
	}
	root := testutil.RepoRoot(t)
	requireRenderToolchain(t, root, "the system-upgrade-controller fixture gate")
	if !render.InProcessAvailable() {
		plugin := filepath.Join(config.KustomizePluginHome(projectPaths(root)), "khelm.mgoltzsche.github.com", "v2", "chartrenderer", "ChartRenderer")
		if _, err := os.Stat(plugin); err != nil {
			t.Fatalf("khelm exec plugin missing at %s (b install): the fixture gate cannot render the chart", plugin)
		}
	}
	addon := filepath.Join(root, sucAddon)
	fixture, n := readSUCFixture(t)

	renderDir := func(src string, mutate func(dir string)) map[string]object {
		dir := t.TempDir()
		if err := copyTree(src, dir); err != nil {
			t.Fatal(err)
		}
		mutate(dir)
		var stderr strings.Builder
		out, err := render.Build(context.Background(), dir, render.Options{
			Paths:      projectPaths(root),
			EnableExec: true,
			Env:        []string{"KHELM_TRUST_ANY_REPO=true"},
			Stderr:     &stderr,
		})
		if err != nil {
			t.Fatalf("render of %s failed: %v\n%s", src, err, stderr.String())
		}
		return decodeObjects(t, out)
	}

	// The fixture recipe: no patches, no rbac.yaml.
	chart := renderDir(addon, func(dir string) {
		stripPatches(t, filepath.Join(dir, "kustomization.yaml"))
		dropResource(t, filepath.Join(dir, "kustomization.yaml"), "rbac.yaml")
		if err := os.Remove(filepath.Join(dir, "rbac.yaml")); err != nil {
			t.Fatal(err)
		}
	})
	delete(chart, "Namespace/"+n.namespace)
	if !reflect.DeepEqual(sortedKeys(chart), sortedKeys(fixture)) {
		t.Fatalf("the chart renders %v, the fixture has %v: refresh %s", sortedKeys(chart), sortedKeys(fixture), sucFixture)
	}
	for k, o := range chart {
		if !reflect.DeepEqual(o.Doc, fixture[k].Doc) {
			t.Errorf("%s: the chart renders\n%s\nbut the fixture has\n%s\nrefresh %s", k, canon(t, o.Doc), canon(t, fixture[k].Doc), sucFixture)
		}
	}

	// The addon as shipped.
	shipped := renderDir(addon, func(string) {})
	checkSUCBindings(t, shipped, n)
	checkSUCImages(t, root, shipped)
	rbac := readObjects(t, filepath.Join(addon, "rbac.yaml"))
	for k, o := range rbac {
		if !reflect.DeepEqual(shipped[k].Doc, o.Doc) {
			t.Errorf("%s in the shipped render:\n got: %s\nwant: %s", k, canon(t, shipped[k].Doc), canon(t, o.Doc))
		}
	}

	// The opt-in addon renders the chart's binding, unchanged.
	admin := renderDir(filepath.Join(root, sucAdminAddon), func(string) {})
	if got := admin["ClusterRoleBinding/"+sucChartBinding]; len(admin) != 1 || !reflect.DeepEqual(got.Doc, chart["ClusterRoleBinding/"+sucChartBinding].Doc) {
		t.Errorf("the opt-in addon renders %s, want only the chart's ClusterRoleBinding/%s", canon(t, admin), sucChartBinding)
	}

	// The upstream manifests of the pinned controller version use no RBAC
	// name that upstream-rbac-names.yaml does not know.
	ctrlImg, _ := readSUCImages(t, root)
	m := sucDigestTag.FindStringSubmatch(ctrlImg.Tag)
	if m == nil {
		t.Fatalf("values.yaml: controller tag %q has no version", ctrlImg.Tag)
	}
	upstream := readSUCUpstreamNames(t)
	for k, o := range sucFetchUpstreamRBAC(t, m[1]) {
		if !slices.ContainsFunc(upstream, func(u sucUpstreamName) bool { return u.Kind == o.Kind && u.Name == o.Name }) {
			t.Errorf("upstream manifests %s carry %s, which %s does not list", m[1], k, sucUpstreamNames)
		}
	}
}
