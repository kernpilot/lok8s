package addons

// suc_rbac_test.go — the gate for the system-upgrade-controller addon: the
// RBAC that replaces the chart's ClusterRoleBinding to cluster-admin, and
// the digest pins of the two images.
//
// The RBAC check is static. sucCalls lists every Kubernetes API request the
// controller makes (system-upgrade-controller v0.20.1 and the libraries it
// calls, each with its file:line) and every request the cordon and drain
// containers of a Plan's Job make (kubectl pkg/drain). The test evaluates
// the roles in rbac.yaml against that list in both directions: every
// request must be allowed, and every rule (each group, resource, verb and
// resource name) must serve a request at the scope that the rule's binding
// grants. A rule that grants more fails, and so does a request that loses
// its grant. On a bump of the controller, update sucCalls from its source
// first.
//
// TestSystemUpgradeControllerRBAC is offline: it reads the ServiceAccount,
// the namespace, the Lease name and the chart's binding from a committed
// fixture of the chart render, and renders the addon's patch over that
// fixture through internal/render. TestSystemUpgradeControllerFixtureMatchesChart
// re-renders the chart itself (khelm, online) and diffs the fixture against
// it; it runs on CI and with LOK8S_ONLINE_TESTS=1.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	sucAddon      = "internal/assets/lok8s/addons/system-upgrade-controller"
	sucAdminAddon = "internal/assets/lok8s/addons/system-upgrade-controller-cluster-admin"
	sucFixture    = "testdata/system-upgrade-controller/chart-render.yaml"

	// The names rbac.yaml defines (upstream SUC's manifests/clusterrole.yaml).
	sucRole           = "system-upgrade-controller"
	sucDrainerRole    = "system-upgrade-controller-drainer"
	sucBinding        = "system-upgrade"
	sucDrainerBinding = "system-upgrade-drainer"
	// sucChartBinding is the chart's ClusterRoleBinding to cluster-admin.
	sucChartBinding = "system-upgrade-controller"
	// sucNameLabel is the pod label the chart turns into
	// SYSTEM_UPGRADE_CONTROLLER_NAME, the name of the Lease.
	sucNameLabel = "upgrade.cattle.io/controller"
)

// Scope and name values of an apiCall.
const (
	// atCluster: a cluster-scoped resource, or a request across all
	// namespaces.
	atCluster = ""
	// atAnyNS: one namespace that the request does not know in advance.
	atAnyNS = "*"
	// atCtrlNS: the controller's namespace (read from the fixture).
	atCtrlNS = "@namespace"
	// anyName: the request names no object (list, watch, create), or an
	// object that RBAC cannot know in advance.
	anyName = ""
	// leaseName: the controller's Lease (read from the fixture).
	leaseName = "@lease"
)

// The callers in sucCalls: the controller, and the Plan features whose Job
// containers call the API as the Plan's ServiceAccount.
const (
	byController   = "controller"
	byCordon       = "spec.cordon"
	byDrain        = "spec.drain"
	byDrainDS      = "spec.drain.ignoreDaemonSets"
	byDrainNoEvict = "spec.drain.disableEviction"
)

// apiCall is one Kubernetes API request.
type apiCall struct {
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
var sucCalls = []apiCall{
	// The Plan CRD, on every start (SUC pkg/upgrade/controller.go:147,194).
	{byController, "apiextensions.k8s.io", "customresourcedefinitions", "list", atCluster, anyName, "wrangler pkg/crd/crd.go:64"},
	{byController, "apiextensions.k8s.io", "customresourcedefinitions", "create", atCluster, anyName, "wrangler pkg/crd/crd.go:83"},
	{byController, "apiextensions.k8s.io", "customresourcedefinitions", "get", atCluster, "plans.upgrade.cattle.io", "wrangler pkg/crd/crd.go:91,114"},
	{byController, "apiextensions.k8s.io", "customresourcedefinitions", "update", atCluster, "plans.upgrade.cattle.io", "wrangler pkg/crd/crd.go:101"},
	// The cluster ID.
	{byController, "", "namespaces", "get", atCluster, "kube-system", "SUC pkg/upgrade/controller.go:141"},
	// The node cache, and the Plan label written after a completed Job.
	{byController, "", "nodes", "list", atCluster, anyName, "SUC pkg/upgrade/handle_core.go:16"},
	{byController, "", "nodes", "watch", atCluster, anyName, "SUC pkg/upgrade/handle_core.go:16"},
	{byController, "", "nodes", "update", atCluster, anyName, "SUC pkg/upgrade/handle_batch.go:139"},
	// The Plan's Jobs. wrangler apply lists them in all namespaces: an
	// owner and no lister namespace (SUC handle_upgrade.go:114).
	{byController, "batch", "jobs", "list", atCluster, anyName, "wrangler pkg/apply/desiredset_process.go:362,461"},
	{byController, "batch", "jobs", "list", atCtrlNS, anyName, "SUC pkg/upgrade/handle_batch.go:30 (cache, controller.go:103)"},
	{byController, "batch", "jobs", "watch", atCtrlNS, anyName, "SUC pkg/upgrade/handle_batch.go:30 (cache, controller.go:103)"},
	{byController, "batch", "jobs", "create", atCtrlNS, anyName, "wrangler pkg/apply/desiredset_crud.go:43"},
	{byController, "batch", "jobs", "get", atCtrlNS, anyName, "wrangler pkg/apply/desiredset_crud.go:50"},
	{byController, "batch", "jobs", "patch", atCtrlNS, anyName, "wrangler pkg/apply/desiredset_process.go:189"},
	{byController, "batch", "jobs", "delete", atCtrlNS, anyName, "SUC pkg/upgrade/handle_batch.go:180; wrangler pkg/apply/desiredset_crud.go:68"},
	// The Plans and their status.
	{byController, "upgrade.cattle.io", "plans", "list", atCtrlNS, anyName, "SUC pkg/upgrade/controller.go:110 (cache)"},
	{byController, "upgrade.cattle.io", "plans", "watch", atCtrlNS, anyName, "SUC pkg/upgrade/controller.go:110 (cache)"},
	{byController, "upgrade.cattle.io", "plans", "get", atCtrlNS, anyName, "SUC pkg/upgrade/handle_batch.go:54"},
	{byController, "upgrade.cattle.io", "plans/status", "update", atCtrlNS, anyName, "SUC pkg/upgrade/handle_batch.go:98; generated controllers/upgrade.cattle.io/v1/plan.go:121"},
	// The Secret cache. Reads go to the cache only (SUC pkg/upgrade/plan/plan.go:62,256).
	{byController, "", "secrets", "list", atCtrlNS, anyName, "SUC pkg/upgrade/handle_core.go:42, handle_upgrade.go:25"},
	{byController, "", "secrets", "watch", atCtrlNS, anyName, "SUC pkg/upgrade/handle_core.go:42, handle_upgrade.go:25"},
	// Events: on Plans in the controller's namespace, on the node in default.
	{byController, "", "events", "create", atCtrlNS, anyName, "SUC controller.go:124; client-go tools/record/event.go:340"},
	{byController, "", "events", "patch", atCtrlNS, anyName, "SUC controller.go:124; client-go tools/record/event.go:334"},
	{byController, "", "events", "create", "default", anyName, "SUC controller.go:171; client-go tools/record/event.go:340,491"},
	{byController, "", "events", "patch", "default", anyName, "SUC controller.go:171; client-go tools/record/event.go:334,491"},
	// Leader election (SUC controller.go:176; wrangler pkg/leader/leader.go:47).
	{byController, "coordination.k8s.io", "leases", "create", atCtrlNS, anyName, "client-go tools/leaderelection/resourcelock/leaselock.go:68"},
	{byController, "coordination.k8s.io", "leases", "get", atCtrlNS, leaseName, "client-go tools/leaderelection/resourcelock/leaselock.go:43"},
	{byController, "coordination.k8s.io", "leases", "update", atCtrlNS, leaseName, "client-go tools/leaderelection/resourcelock/leaselock.go:89"},

	// kubectl cordon (SUC pkg/upgrade/job/job.go:423).
	{byCordon, "", "nodes", "get", atCluster, anyName, "kubectl pkg/cmd/drain/drain.go:292-305"},
	{byCordon, "", "nodes", "patch", atCluster, anyName, "kubectl pkg/drain/cordon.go:104"},
	// kubectl drain (SUC pkg/upgrade/job/job.go:367).
	{byDrain, "", "nodes", "get", atCluster, anyName, "kubectl pkg/cmd/drain/drain.go:292-305"},
	{byDrain, "", "nodes", "patch", atCluster, anyName, "kubectl pkg/drain/cordon.go:104"},
	{byDrain, "", "pods", "list", atCluster, anyName, "kubectl pkg/drain/drain.go:198"},
	{byDrain, "", "pods", "get", atAnyNS, anyName, "kubectl pkg/drain/drain.go:254"},
	{byDrain, "", "pods/eviction", "create", atAnyNS, anyName, "kubectl pkg/drain/drain.go:165"},
	// --ignore-daemonsets, on unless the Plan turns it off (job.go:368).
	{byDrainDS, "apps", "daemonsets", "get", atAnyNS, anyName, "kubectl pkg/drain/filters.go:195"},
	// --disable-eviction (job.go:391).
	{byDrainNoEvict, "", "pods", "delete", atAnyNS, anyName, "kubectl pkg/drain/drain.go:148,257"},
}

// sucNames holds what the test reads from the fixture.
type sucNames struct {
	namespace, serviceAccount, lease string
	chartBinding                     object
}

// resolve replaces the fixture placeholders of a call.
func (n sucNames) resolve(c apiCall) apiCall {
	if c.ns == atCtrlNS {
		c.ns = n.namespace
	}
	if c.name == leaseName {
		c.name = n.lease
	}
	return c
}

// strs returns a rule field as strings.
func strs(v any) []string {
	seq, _ := v.([]any)
	out := make([]string, 0, len(seq))
	for _, s := range seq {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

// ruleAllows reports whether one rule allows a call (no wildcards: the test
// rejects them before it evaluates).
func ruleAllows(r map[string]any, c apiCall) bool {
	if !slices.Contains(strs(r["apiGroups"]), c.group) ||
		!slices.Contains(strs(r["resources"]), c.resource) ||
		!slices.Contains(strs(r["verbs"]), c.verb) {
		return false
	}
	names := strs(r["resourceNames"])
	return len(names) == 0 || (c.name != anyName && slices.Contains(names, c.name))
}

// grant is a role as one binding applies it: in every namespace and at the
// cluster scope (ns == ""), or in one namespace.
type grant struct {
	role object
	ns   string
}

func (g grant) allows(c apiCall) bool {
	if g.ns != "" && c.ns != g.ns {
		return false
	}
	for _, r := range g.role.Rules() {
		if ruleAllows(r, c) {
			return true
		}
	}
	return false
}

// checkNoWildcards rejects `*` in any rule field and nonResourceURLs.
func checkNoWildcards(t *testing.T, o object) {
	t.Helper()
	for _, r := range o.Rules() {
		if _, ok := r["nonResourceURLs"]; ok {
			t.Errorf("%s/%s: nonResourceURLs in %s", o.Kind, o.Name, canon(t, r))
		}
		for _, f := range []string{"apiGroups", "resources", "verbs", "resourceNames"} {
			if slices.Contains(strs(r[f]), "*") {
				t.Errorf("%s/%s: wildcard in %s: %s", o.Kind, o.Name, f, canon(t, r))
			}
		}
	}
}

// checkRulesNeeded asserts that every atom of every rule of g (group,
// resource, verb, and each resource name or none) serves a call of `by` at
// the scope g grants: a cluster-wide grant must serve a call outside the
// controller's namespace, a namespaced one a call in its namespace. A rule
// without resourceNames must serve a call that RBAC cannot restrict by name.
func checkRulesNeeded(t *testing.T, g grant, calls []apiCall, ctrlNS string) {
	t.Helper()
	for _, r := range g.role.Rules() {
		names := strs(r["resourceNames"])
		if len(names) == 0 {
			names = []string{anyName}
		}
		for _, grp := range strs(r["apiGroups"]) {
			for _, res := range strs(r["resources"]) {
				for _, verb := range strs(r["verbs"]) {
					for _, name := range names {
						needed := slices.ContainsFunc(calls, func(c apiCall) bool {
							if c.group != grp || c.resource != res || c.verb != verb || c.name != name {
								return false
							}
							if g.ns == "" {
								return c.ns != ctrlNS
							}
							return c.ns == g.ns
						})
						if !needed {
							scope := "cluster-wide"
							if g.ns != "" {
								scope = "in " + g.ns
							}
							t.Errorf("%s/%s grants %s %q/%s name %q %s, and no call in sucCalls needs it", g.role.Kind, g.role.Name, verb, grp, res, name, scope)
						}
					}
				}
			}
		}
	}
}

// callsBy returns the resolved calls of the given callers.
func callsBy(n sucNames, by ...string) []apiCall {
	var out []apiCall
	for _, c := range sucCalls {
		if slices.Contains(by, c.by) {
			out = append(out, n.resolve(c))
		}
	}
	return out
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
	podSpec := dig(dep.Doc, "spec", "template", "spec")
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
			path, _ := dig(em, "valueFrom", "fieldRef")["fieldPath"].(string)
			env[name] = path
		}
	}
	if got, want := env["SYSTEM_UPGRADE_CONTROLLER_NAME"], "metadata.labels['"+sucNameLabel+"']"; got != want {
		t.Fatalf("SYSTEM_UPGRADE_CONTROLLER_NAME comes from %q, want %q", got, want)
	}
	if got := env["SYSTEM_UPGRADE_CONTROLLER_NAMESPACE"]; got != "metadata.namespace" {
		t.Fatalf("SYSTEM_UPGRADE_CONTROLLER_NAMESPACE comes from %q, want metadata.namespace", got)
	}
	n.lease, _ = dig(dep.Doc, "spec", "template", "metadata", "labels")[sucNameLabel].(string)
	if n.lease == "" {
		t.Fatalf("the Deployment's pod template has no %s label", sucNameLabel)
	}
	n.chartBinding, ok = objs["ClusterRoleBinding/"+sucChartBinding]
	if !ok {
		t.Fatalf("%s: no ClusterRoleBinding/%s", sucFixture, sucChartBinding)
	}
	return objs, n
}

// dig walks nested maps; a missing key gives an empty map.
func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		m = next
	}
	return m
}

// bindsSA reports whether a binding names the controller's ServiceAccount.
func bindsSA(b object, n sucNames) bool {
	subjects, _ := b.Doc["subjects"].([]any)
	for _, s := range subjects {
		m, _ := s.(map[string]any)
		if m["kind"] == "ServiceAccount" && m["name"] == n.serviceAccount && m["namespace"] == n.namespace {
			return true
		}
	}
	return false
}

// checkSUCBindings asserts that exactly the three bindings of rbac.yaml
// bind the controller's ServiceAccount, each with that one subject, and
// that the chart's binding to cluster-admin is gone.
func checkSUCBindings(t *testing.T, objs map[string]object, n sucNames) {
	t.Helper()
	want := map[string]string{
		"ClusterRoleBinding/" + sucBinding:        "ClusterRole/" + sucRole,
		"RoleBinding/" + sucBinding:               "Role/" + sucRole,
		"ClusterRoleBinding/" + sucDrainerBinding: "ClusterRole/" + sucDrainerRole,
	}
	got := map[string]string{}
	for _, k := range sortedKeys(objs) {
		o := objs[k]
		if o.Kind != "ClusterRoleBinding" && o.Kind != "RoleBinding" || !bindsSA(o, n) {
			continue
		}
		ref, _ := o.Doc["roleRef"].(map[string]any)
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		got[k] = kind + "/" + name
		if subjects, _ := o.Doc["subjects"].([]any); len(subjects) != 1 {
			t.Errorf("%s has %d subjects, want only %s/%s", k, len(subjects), n.namespace, n.serviceAccount)
		}
		if o.Kind == "RoleBinding" && o.Namespace != n.namespace {
			t.Errorf("%s is in namespace %q, want %q", k, o.Namespace, n.namespace)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bindings of %s/%s:\n got: %v\nwant: %v", n.namespace, n.serviceAccount, got, want)
	}
	if _, ok := objs["ClusterRoleBinding/"+sucChartBinding]; ok {
		t.Errorf("ClusterRoleBinding/%s (the chart's binding to cluster-admin) is still in the render", sucChartBinding)
	}
}

// sucImage is one image pin in values.yaml.
type sucImage struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
}

var digestTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$`)

// checkSUCImages asserts that values.yaml pins both images by digest and
// that the chart render (objs) uses exactly those references.
func checkSUCImages(t *testing.T, root string, objs map[string]object) {
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
	for _, img := range []sucImage{values.Controller.Image, values.Kubectl.Image} {
		if img.Repository == "" || !digestTag.MatchString(img.Tag) {
			t.Errorf("values.yaml: image %q tag %q is not `<version>@sha256:<64 hex>`", img.Repository, img.Tag)
		}
	}
	ctrl := values.Controller.Image.Repository + ":" + values.Controller.Image.Tag
	kubectl := values.Kubectl.Image.Repository + ":" + values.Kubectl.Image.Tag

	dep := objs["Deployment/system-upgrade-controller"]
	var images []string
	for _, kind := range []string{"containers", "initContainers"} {
		seq, _ := dig(dep.Doc, "spec", "template", "spec")[kind].([]any)
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

// checkSUCPatch asserts the kustomization lists rbac.yaml and deletes the
// chart's binding with one anchored, targeted `$patch: delete`.
func checkSUCPatch(t *testing.T, addon string, entries []patchEntry) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(addon, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kz struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kz); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(kz.Resources, "rbac.yaml") {
		t.Errorf("kustomization.yaml does not list rbac.yaml: %v", kz.Resources)
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
	if ref, _ := n.chartBinding.Doc["roleRef"].(map[string]any); ref["name"] != "cluster-admin" || !bindsSA(n.chartBinding, n) {
		t.Fatalf("%s: ClusterRoleBinding/%s is not the chart's binding of %s/%s to cluster-admin", sucFixture, sucChartBinding, n.namespace, n.serviceAccount)
	}

	rbac := readObjects(t, filepath.Join(addon, "rbac.yaml"))
	clusterRole, ok1 := rbac["ClusterRole/"+sucRole]
	role, ok2 := rbac["Role/"+sucRole]
	drainer, ok3 := rbac["ClusterRole/"+sucDrainerRole]
	if !ok1 || !ok2 || !ok3 || role.Namespace != n.namespace {
		t.Fatalf("rbac.yaml: want ClusterRole/%s, Role %s/%s and ClusterRole/%s, have %v", sucRole, n.namespace, sucRole, sucDrainerRole, sortedKeys(rbac))
	}
	for _, o := range []object{clusterRole, role, drainer} {
		checkNoWildcards(t, o)
	}
	checkSUCBindings(t, rbac, n)

	// Every call is allowed. The controller's calls by its two roles alone;
	// each Plan feature's calls by the drainer role alone, so that a Plan
	// ServiceAccount bound to it can cordon and drain.
	ctrlGrants := []grant{{clusterRole, ""}, {role, n.namespace}}
	for _, c := range callsBy(n, byController) {
		if !slices.ContainsFunc(ctrlGrants, func(g grant) bool { return g.allows(c) }) {
			t.Errorf("the controller's roles do not allow %s %q/%s name %q in %q (%s)", c.verb, c.group, c.resource, c.name, c.ns, c.source)
		}
	}
	features := []string{byCordon, byDrain, byDrainDS, byDrainNoEvict}
	for _, c := range callsBy(n, features...) {
		if !(grant{drainer, ""}).allows(c) {
			t.Errorf("ClusterRole/%s does not allow %s %q/%s in %q, which %s needs (%s)", sucDrainerRole, c.verb, c.group, c.resource, c.ns, c.by, c.source)
		}
	}
	// No rule grants more than the calls need, at the scope it is granted.
	checkRulesNeeded(t, grant{clusterRole, ""}, callsBy(n, byController), n.namespace)
	checkRulesNeeded(t, grant{role, n.namespace}, callsBy(n, byController), n.namespace)
	checkRulesNeeded(t, grant{drainer, ""}, callsBy(n, features...), n.namespace)

	// The image pins, and the chart render that uses them.
	checkSUCImages(t, root, fixture)

	// The opt-in addon restores exactly the chart's binding.
	admin := readObjects(t, filepath.Join(root, sucAdminAddon, "clusterrolebinding.yaml"))
	if got, ok := admin["ClusterRoleBinding/"+sucChartBinding]; !ok || len(admin) != 1 || !reflect.DeepEqual(got.Doc, n.chartBinding.Doc) {
		t.Errorf("%s/clusterrolebinding.yaml\n got: %s\nwant (the chart's binding): %s", sucAdminAddon, canon(t, admin), canon(t, n.chartBinding.Doc))
	}
	if raw, err := os.ReadFile(filepath.Join(root, sucAdminAddon, "kustomization.yaml")); err != nil || !strings.Contains(string(raw), "- clusterrolebinding.yaml") {
		t.Errorf("%s/kustomization.yaml does not list clusterrolebinding.yaml", sucAdminAddon)
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

// TestSystemUpgradeControllerFixtureMatchesChart renders the pinned chart
// through the addon's own generator (khelm, so it downloads the chart): the
// addon without its RBAC changes must equal the fixture, and the addon as
// shipped must carry the narrowed bindings and the pinned images. It also
// renders the opt-in addon.
func TestSystemUpgradeControllerFixtureMatchesChart(t *testing.T) {
	t.Parallel()
	if os.Getenv("CI") == "" && os.Getenv(onlineTestsEnv) == "" {
		t.Skipf("downloads the pinned chart: runs on CI, or with %s=1", onlineTestsEnv)
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
}

// dropResource removes one entry from the `resources:` list of a
// kustomization.yaml.
func dropResource(t *testing.T, path, name string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	doc := root.Content[0]
	found := false
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "resources" {
			continue
		}
		seq := doc.Content[i+1]
		seq.Content = slices.DeleteFunc(seq.Content, func(n *yaml.Node) bool {
			if n.Value == name {
				found = true
				return true
			}
			return false
		})
	}
	if !found {
		t.Fatalf("%s: no resource %q", path, name)
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}
