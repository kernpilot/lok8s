package addons

// redis_operator_rbac_test.go — the gate for the redis-operator addon: the
// RBAC that replaces the chart's ClusterRole `redis-operator`. That
// ClusterRole grants create and delete on namespaces, Secrets, pods/exec and
// more in every namespace, and its label aggregate-to-admin adds those rules
// to the built-in ClusterRole `admin` (B446).
//
// The RBAC check is static, like suc_rbac_test.go, and it uses the same
// evaluation helpers (sucCall, sucGrant, sucCheckRulesNeeded, ...).
// redisCalls is an inventory of the Kubernetes API requests of the operator
// (OT-CONTAINER-KIT/redis-operator v0.26.0 and the libraries it calls),
// written by hand from that source, each entry with its file:line. It is
// not a runtime proof: a request that the inventory misses is missed here
// too. A bump of the operator starts with a new read of the source. The
// test pins the image of the inventory and the arguments and environment
// that the inventory assumes, so a chart bump or a values change that moves
// them fails here first.
//
// The inventory leaves out API discovery (the RESTMapper of
// controller-runtime). The addon grants no rule for it: the default
// ClusterRoleBinding system:discovery gives it to every authenticated user.
//
// The test evaluates the roles in rbac.yaml against the inventory in both
// directions: every request must be allowed, and every rule (each group,
// resource, verb and resource name) must serve a request at the scope that
// the rule's binding grants. It also fails when any object of the addon
// carries an aggregate-to-* label or an aggregationRule, and when any role
// grants a verb on namespaces.
//
// TestRedisOperatorRBAC is offline: it reads the names, the image, the
// arguments and the chart's RBAC from a committed fixture of the chart
// render, and renders the addon's patches over that fixture through
// internal/render. TestRedisOperatorFixtureMatchesChart renders the chart
// itself (khelm, online) and diffs the fixture against it; it runs on CI
// and with LOK8S_ONLINE_TESTS=1.

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	redisAddon          = "internal/assets/lok8s/addons/redis-operator"
	redisChartRBACAddon = "internal/assets/lok8s/addons/redis-operator-chart-rbac"
	redisFixture        = "testdata/redis-operator/chart-render.yaml"

	// The names rbac.yaml defines; each binding has the name of its role.
	redisRole      = "lok8s-redis-operator"
	redisExecRole  = "lok8s-redis-operator-exec"
	redisLERole    = "lok8s-redis-operator-leader-election"
	redisChartRBAC = "lok8s-redis-operator-chart-rbac"
	// redisNamespace is the operator's namespace: the ChartRenderer
	// namespace and the kustomization's namespace transformer.
	redisNamespace = "redis-operator"
	// redisChartName is the name of the chart's ClusterRole and
	// ClusterRoleBinding, and of the Deployment.
	redisChartName = "redis-operator"
	// redisImage is the operator version that redisCalls was read from.
	redisImage = "quay.io/opstree/redis-operator:v0.26.0"
	// redisLease is the leader-election ID (redis-operator
	// internal/cmd/manager/cmd.go:206), the name of the Lease.
	redisLease = "6cab913b.redis.opstreelabs.in"
	// aggregateLabelPrefix is the label prefix that makes Kubernetes copy a
	// ClusterRole's rules into a built-in role (admin, edit, view).
	aggregateLabelPrefix = "rbac.authorization.k8s.io/aggregate-to-"
)

// The callers in redisCalls: every controller of the operator, and the
// redis-cli exec that only a RedisCluster needs.
const (
	redisByOperator    = "operator"
	redisByClusterExec = "RedisCluster exec"
)

// redisCalls: op = OT-CONTAINER-KIT/redis-operator v0.26.0 (ef33adb),
// paths under internal/; cr = sigs.k8s.io/controller-runtime v0.17.2;
// client-go = k8s.io/client-go v0.29.3. Without WATCH_NAMESPACE the caches
// watch all namespaces (op cmd/manager/cmd.go:213-219): a cache makes a
// list and a watch request across namespaces (sucAtCluster). A read from the
// cache makes no request (cr pkg/client/client.go:351-356). Every other
// request goes to the namespace of a Redis CR, which the operator does not
// know in advance (sucAtAnyNS).
var redisCalls = []sucCall{
	// The four CR kinds: the caches, and the finalizer add/remove (an update
	// of the CR, op controller/common/finalizer.go:14).
	{redisByOperator, "redis.redis.opstreelabs.in", "redis", "list", sucAtCluster, sucAnyName, "op controller/redis/redis_controller.go:99 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redis", "watch", sucAtCluster, sucAnyName, "op controller/redis/redis_controller.go:99 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redis", "update", sucAtAnyNS, sucAnyName, "op k8sutils/finalizer.go:31; redis_controller.go:62"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisclusters", "list", sucAtCluster, sucAnyName, "op controller/rediscluster/rediscluster_controller.go:499 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisclusters", "watch", sucAtCluster, sucAnyName, "op controller/rediscluster/rediscluster_controller.go:499 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisclusters", "update", sucAtAnyNS, sucAnyName, "op k8sutils/finalizer.go:50; rediscluster_controller.go:81"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisreplications", "list", sucAtCluster, sucAnyName, "op controller/redisreplication/redisreplication_controller.go:548; redissentinel_controller.go:173 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisreplications", "watch", sucAtCluster, sucAnyName, "op controller/redisreplication/redisreplication_controller.go:548; redissentinel_controller.go:173 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisreplications", "update", sucAtAnyNS, sucAnyName, "op k8sutils/finalizer.go:69; redisreplication_controller.go:172"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redissentinels", "list", sucAtCluster, sucAnyName, "op controller/redissentinel/redissentinel_controller.go:170 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redissentinels", "watch", sucAtCluster, sucAnyName, "op controller/redissentinel/redissentinel_controller.go:170 (cache)"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redissentinels", "update", sucAtAnyNS, sucAnyName, "op k8sutils/finalizer.go:83; redissentinel_controller.go:88"},
	// The status of a RedisCluster and of a RedisReplication (op
	// controller/common/status.go:10). No status write for Redis or
	// RedisSentinel.
	{redisByOperator, "redis.redis.opstreelabs.in", "redisclusters/status", "update", sucAtAnyNS, sucAnyName, "op controller/rediscluster/rediscluster_controller.go:465,478"},
	{redisByOperator, "redis.redis.opstreelabs.in", "redisreplications/status", "update", sucAtAnyNS, sucAnyName, "op controller/redisreplication/redisreplication_controller.go:542"},
	// The owner reference with blockOwnerDeletion on the Sentinel Service
	// and StatefulSet of a RedisReplication (op
	// controller/common/reconciler/reconciler.go:30; cr
	// pkg/controller/controllerutil/controllerutil.go:80). Checked by the
	// admission plugin OwnerReferencesPermissionEnforcement when it is on.
	{redisByOperator, "redis.redis.opstreelabs.in", "redisreplications/finalizers", "update", sucAtAnyNS, sucAnyName, "kube-apiserver plugin/pkg/admission/gc (blockOwnerDeletion)"},
	// Password and TLS Secrets, by name.
	{redisByOperator, "", "secrets", "get", sucAtAnyNS, sucAnyName, "op k8sutils/secrets.go:19,40; controller/common/redis/heal.go:246, check.go:40; redisreplication_controller.go:230,289"},
	// The Redis pods.
	{redisByOperator, "", "pods", "get", sucAtAnyNS, sucAnyName, "op k8sutils/redis.go:50,108,794,834,923, pod.go:18; controller/common/redis/check.go:110; redisreplication_controller.go:207"},
	{redisByOperator, "", "pods", "list", sucAtAnyNS, sucAnyName, "op controller/common/redis/heal.go:53,210, check.go:62; redisreplication_controller.go:274"},
	{redisByOperator, "", "pods", "patch", sucAtAnyNS, sucAnyName, "op controller/common/redis/heal.go:68"},
	// Services.
	{redisByOperator, "", "services", "list", sucAtCluster, sucAnyName, "op controller/redis/redis_controller.go:101 (cache)"},
	{redisByOperator, "", "services", "watch", sucAtCluster, sucAnyName, "op controller/redis/redis_controller.go:101 (cache)"},
	{redisByOperator, "", "services", "get", sucAtAnyNS, sucAnyName, "op k8sutils/services.go:118"},
	{redisByOperator, "", "services", "create", sucAtAnyNS, sucAnyName, "op k8sutils/services.go:93; controller/common/reconciler/reconciler.go:50"},
	{redisByOperator, "", "services", "update", sucAtAnyNS, sucAnyName, "op k8sutils/services.go:104; controller/common/reconciler/reconciler.go:92"},
	// StatefulSets.
	{redisByOperator, "apps", "statefulsets", "list", sucAtCluster, sucAnyName, "op redis_controller.go:102, rediscluster_controller.go:500, redissentinel_controller.go:171 (cache)"},
	{redisByOperator, "apps", "statefulsets", "watch", sucAtCluster, sucAnyName, "op redis_controller.go:102, rediscluster_controller.go:500, redissentinel_controller.go:171 (cache)"},
	{redisByOperator, "apps", "statefulsets", "get", sucAtAnyNS, sucAnyName, "op k8sutils/statefulset.go:49,82,1126; controller/common/redis/heal.go:201, check.go:53"},
	{redisByOperator, "apps", "statefulsets", "create", sucAtAnyNS, sucAnyName, "op k8sutils/statefulset.go:1087; controller/common/reconciler/reconciler.go:50"},
	{redisByOperator, "apps", "statefulsets", "update", sucAtAnyNS, sucAnyName, "op k8sutils/statefulset.go:1098; controller/common/reconciler/reconciler.go:92"},
	{redisByOperator, "apps", "statefulsets", "delete", sucAtAnyNS, sucAnyName, "op k8sutils/statefulset.go:1107"},
	// PVCs: the resize, and the delete in the finalizers.
	{redisByOperator, "", "persistentvolumeclaims", "list", sucAtAnyNS, sucAnyName, "op k8sutils/pvc.go:83"},
	{redisByOperator, "", "persistentvolumeclaims", "update", sucAtAnyNS, sucAnyName, "op k8sutils/pvc.go:105"},
	{redisByOperator, "", "persistentvolumeclaims", "delete", sucAtAnyNS, sucAnyName, "op k8sutils/finalizer.go:107,127,142,164"},
	// PodDisruptionBudgets.
	{redisByOperator, "policy", "poddisruptionbudgets", "get", sucAtAnyNS, sucAnyName, "op k8sutils/poddisruption.go:263"},
	{redisByOperator, "policy", "poddisruptionbudgets", "create", sucAtAnyNS, sucAnyName, "op k8sutils/poddisruption.go:227"},
	{redisByOperator, "policy", "poddisruptionbudgets", "update", sucAtAnyNS, sucAnyName, "op k8sutils/poddisruption.go:238"},
	{redisByOperator, "policy", "poddisruptionbudgets", "delete", sucAtAnyNS, sucAnyName, "op k8sutils/poddisruption.go:249"},
	// Events: the RedisCluster recorder in the CR's namespace, the leader
	// election on its Lease (client-go tools/record/event.go:321 patch, 327
	// create).
	{redisByOperator, "", "events", "create", sucAtAnyNS, sucAnyName, "op cmd/manager/cmd.go:257, rediscluster_controller.go:91"},
	{redisByOperator, "", "events", "patch", sucAtAnyNS, sucAnyName, "op cmd/manager/cmd.go:257, rediscluster_controller.go:91"},
	{redisByOperator, "", "events", "create", sucAtCtrlNS, sucAnyName, "cr pkg/leaderelection/leader_election.go:108; client-go leaselock.go:94"},
	{redisByOperator, "", "events", "patch", sucAtCtrlNS, sucAnyName, "cr pkg/leaderelection/leader_election.go:108; client-go leaselock.go:94"},
	// Leader election (the chart passes --leader-elect): a Lease in the
	// operator's namespace (cr pkg/leaderelection/leader_election.go:65,74-76).
	{redisByOperator, "coordination.k8s.io", "leases", "create", sucAtCtrlNS, sucAnyName, "client-go tools/leaderelection/resourcelock/leaselock.go:58"},
	{redisByOperator, "coordination.k8s.io", "leases", "get", sucAtCtrlNS, sucLease, "client-go tools/leaderelection/resourcelock/leaselock.go:42"},
	{redisByOperator, "coordination.k8s.io", "leases", "update", sucAtCtrlNS, sucLease, "client-go tools/leaderelection/resourcelock/leaselock.go:75"},

	// redis-cli in a RedisCluster pod (a POST to pods/exec; every caller in
	// redis.go and cluster-scaling.go takes a RedisCluster).
	{redisByClusterExec, "", "pods/exec", "create", sucAtAnyNS, sucAnyName, "op k8sutils/redis.go:762-769"},
}

// redisCallsBy returns the resolved calls of the given callers.
func redisCallsBy(n sucNames, by ...string) []sucCall {
	var out []sucCall
	for _, c := range redisCalls {
		if slices.Contains(by, c.by) {
			out = append(out, n.resolve(c))
		}
	}
	return out
}

// rawDocs decodes a multi-document stream into generic documents, without
// the metadata reduction of decodeObjects.
func rawDocs(t *testing.T, stream []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(string(stream)))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding the object stream: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// redisAggregation returns, per object (objKey), the aggregate-to-* labels
// it carries, plus "aggregationRule" for a ClusterRole that aggregates
// others.
func redisAggregation(t *testing.T, stream []byte) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, doc := range rawDocs(t, stream) {
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		key := objKey(kind, ns, name)
		labels, _ := meta["labels"].(map[string]any)
		for _, k := range sortedKeys(labels) {
			if strings.HasPrefix(k, aggregateLabelPrefix) {
				out[key] = append(out[key], k)
			}
		}
		if _, ok := doc["aggregationRule"]; ok {
			out[key] = append(out[key], "aggregationRule")
		}
	}
	return out
}

// readRedisFixture decodes the fixture and reads the names the RBAC
// depends on. It stops when the Deployment does not run the operator the
// inventory was read from, with the arguments and environment it assumes.
func readRedisFixture(t *testing.T) (map[string]object, sucNames) {
	t.Helper()
	objs := readObjects(t, redisFixture)
	var n sucNames
	for _, o := range objs {
		if o.Kind == "ServiceAccount" {
			if n.serviceAccount != "" {
				t.Fatalf("%s: more than one ServiceAccount", redisFixture)
			}
			n.serviceAccount, n.namespace = o.Name, o.Namespace
		}
	}
	if n.namespace != redisNamespace {
		t.Fatalf("%s: the ServiceAccount %q is in namespace %q, want %q", redisFixture, n.serviceAccount, n.namespace, redisNamespace)
	}
	n.lease = redisLease
	dep, ok := objs[objKey("Deployment", n.namespace, redisChartName)]
	if !ok {
		t.Fatalf("%s: no Deployment %s/%s: %v", redisFixture, n.namespace, redisChartName, sortedKeys(objs))
	}
	podSpec := sucNested(dep.Doc, "spec", "template", "spec")
	if sa, _ := podSpec["serviceAccountName"].(string); sa != n.serviceAccount {
		t.Fatalf("the Deployment runs as %s, the fixture's ServiceAccount is %s", sa, n.serviceAccount)
	}
	containers, _ := podSpec["containers"].([]any)
	if inits, _ := podSpec["initContainers"].([]any); len(containers) != 1 || len(inits) != 0 {
		t.Fatalf("the Deployment has %d containers and %d init containers, want the one operator container", len(containers), len(inits))
	}
	c, _ := containers[0].(map[string]any)
	if img, _ := c["image"].(string); img != redisImage {
		t.Fatalf("the Deployment runs %q, but redisCalls is the inventory of %s: read the source of the new version, then change rbac.yaml, redisCalls and redisImage", img, redisImage)
	}
	args := stringsOf(c["args"])
	if !slices.Contains(args, "--leader-elect") {
		t.Errorf("the operator runs without --leader-elect (%v): the leader-election Role serves no call", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--metrics-secure") {
			t.Errorf("the operator runs with %s: the metrics filter calls tokenreviews and subjectaccessreviews, which redisCalls does not hold", a)
		}
	}
	env := map[string]string{}
	vars, _ := c["env"].([]any)
	for _, e := range vars {
		em, _ := e.(map[string]any)
		name, _ := em["name"].(string)
		value, _ := em["value"].(string)
		env[name] = value
	}
	if ns, ok := env["WATCH_NAMESPACE"]; ok {
		t.Errorf("the operator runs with WATCH_NAMESPACE=%q: redisCalls assumes caches in all namespaces", ns)
	}
	if env["ENABLE_WEBHOOKS"] != "false" {
		t.Errorf("ENABLE_WEBHOOKS=%q, want \"false\": redisCalls holds no webhook calls", env["ENABLE_WEBHOOKS"])
	}
	return objs, n
}

// redisChartRBACObjects returns the chart's ClusterRole and ClusterRoleBinding
// from the fixture, and asserts that the binding binds the operator to that
// ClusterRole.
func redisChartRBACObjects(t *testing.T, objs map[string]object, n sucNames) (role, binding object) {
	t.Helper()
	role, ok1 := objs[objKey("ClusterRole", "", redisChartName)]
	binding, ok2 := objs[objKey("ClusterRoleBinding", "", redisChartName)]
	if !ok1 || !ok2 {
		t.Fatalf("%s: no ClusterRole or ClusterRoleBinding %s: %v", redisFixture, redisChartName, sortedKeys(objs))
	}
	if ref, _ := binding.Doc["roleRef"].(map[string]any); ref["kind"] != "ClusterRole" || ref["name"] != redisChartName || !sucBindsSA(binding, n) {
		t.Fatalf("%s: ClusterRoleBinding/%s does not bind %s/%s to ClusterRole/%s", redisFixture, redisChartName, n.namespace, n.serviceAccount, redisChartName)
	}
	return role, binding
}

// redisGrantsNamespaces reports the first rule of a role that names the
// resource namespaces.
func redisGrantsNamespaces(o object) (map[string]any, bool) {
	for _, r := range o.Rules() {
		if slices.Contains(stringsOf(r["resources"]), "namespaces") {
			return r, true
		}
	}
	return nil, false
}

// checkRedisNamespaces asserts that every namespaced object of a render is
// in the operator's namespace, and that a cluster-scoped object has none.
func checkRedisNamespaces(t *testing.T, objs map[string]object) {
	t.Helper()
	seen := map[string]bool{}
	for _, k := range sortedKeys(objs) {
		o := objs[k]
		if slices.Contains(sucClusterScoped, o.Kind) {
			if o.Namespace != "" {
				t.Errorf("%s is cluster-scoped but carries namespace %q", k, o.Namespace)
			}
			continue
		}
		seen[o.Kind] = true
		if o.Namespace != redisNamespace {
			t.Errorf("%s is in namespace %q, want %q", k, o.Namespace, redisNamespace)
		}
	}
	for _, kind := range []string{"ServiceAccount", "Deployment", "Role", "RoleBinding"} {
		if !seen[kind] {
			t.Errorf("the render has no %s: the namespace check did not see it", kind)
		}
	}
}

// checkRedisBindings asserts that exactly the three bindings of rbac.yaml
// bind the operator's ServiceAccount, each with that one subject, and that
// the chart's ClusterRole and ClusterRoleBinding are gone.
func checkRedisBindings(t *testing.T, objs map[string]object, n sucNames) {
	t.Helper()
	want := map[string]string{
		objKey("ClusterRoleBinding", "", redisRole):     "ClusterRole/" + redisRole,
		objKey("ClusterRoleBinding", "", redisExecRole): "ClusterRole/" + redisExecRole,
		objKey("RoleBinding", n.namespace, redisLERole): "Role/" + redisLERole,
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
		got[k] = kind + "/" + name
		if subjects, _ := o.Doc["subjects"].([]any); len(subjects) != 1 {
			t.Errorf("%s has %d subjects, want only %s/%s", k, len(subjects), n.namespace, n.serviceAccount)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bindings of %s/%s (binding: roleRef):\n got: %v\nwant: %v", n.namespace, n.serviceAccount, got, want)
	}
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding"} {
		if _, ok := objs[objKey(kind, "", redisChartName)]; ok {
			t.Errorf("%s/%s of the chart is still in the render", kind, redisChartName)
		}
	}
}

// checkRedisPatch asserts the kustomization lists rbac.yaml and deletes the
// chart's ClusterRole and ClusterRoleBinding with anchored, targeted
// `$patch: delete` entries, and nothing else.
func checkRedisPatch(t *testing.T, addon string, entries []patchEntry) {
	t.Helper()
	if res := sucResources(t, addon); !slices.Contains(res, "rbac.yaml") {
		t.Errorf("kustomization.yaml does not list rbac.yaml: %v", res)
	}
	var deleted []string
	for _, e := range entries {
		var body struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(e.Patch), &body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.Patch, "$patch: delete") || body.Metadata.Name != redisChartName ||
			e.Target.Kind != body.Kind || e.Target.Name != "^"+redisChartName+"$" {
			t.Errorf("a patch is not an anchored, targeted delete of a %s object: target %+v\n%s", redisChartName, e.Target, e.Patch)
			continue
		}
		deleted = append(deleted, body.Kind)
	}
	slices.Sort(deleted)
	if want := []string{"ClusterRole", "ClusterRoleBinding"}; !reflect.DeepEqual(deleted, want) {
		t.Errorf("the patches delete %v of %s, want %v", deleted, redisChartName, want)
	}
}

// redisChartVersion reads `version:` from the addon's chart.yaml.
func redisChartVersion(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, redisAddon, "chart.yaml"))
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
	if spec.Chart != "redis-operator" || spec.Version == "" {
		t.Fatalf("chart.yaml: chart=%q version=%q", spec.Chart, spec.Version)
	}
	return spec.Version
}

// checkRedisOptIn asserts that the opt-in addon binds exactly the chart's
// rules, under its own name and without aggregation.
func checkRedisOptIn(t *testing.T, root string, chartRole, chartBinding object, n sucNames) {
	t.Helper()
	path := filepath.Join(root, redisChartRBACAddon, "rbac.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if agg := redisAggregation(t, raw); len(agg) != 0 {
		t.Errorf("%s aggregates into built-in roles: %v", path, agg)
	}
	objs := decodeObjects(t, raw)
	role, ok1 := objs[objKey("ClusterRole", "", redisChartRBAC)]
	binding, ok2 := objs[objKey("ClusterRoleBinding", "", redisChartRBAC)]
	if !ok1 || !ok2 || len(objs) != 2 {
		t.Fatalf("%s: want ClusterRole and ClusterRoleBinding %s only, have %v", path, redisChartRBAC, sortedKeys(objs))
	}
	if !reflect.DeepEqual(role.Doc["rules"], chartRole.Doc["rules"]) {
		t.Errorf("%s: the rules differ from the chart's ClusterRole/%s\n got: %s\nwant: %s", path, redisChartName, canon(t, role.Doc["rules"]), canon(t, chartRole.Doc["rules"]))
	}
	ref, _ := binding.Doc["roleRef"].(map[string]any)
	if ref["kind"] != "ClusterRole" || ref["name"] != redisChartRBAC || !reflect.DeepEqual(binding.Doc["subjects"], chartBinding.Doc["subjects"]) {
		t.Errorf("%s: ClusterRoleBinding/%s is not the chart's subjects bound to ClusterRole/%s: %s", path, redisChartRBAC, redisChartRBAC, canon(t, binding.Doc))
	}
	if !sucBindsSA(binding, n) {
		t.Errorf("%s: ClusterRoleBinding/%s does not bind %s/%s", path, redisChartRBAC, n.namespace, n.serviceAccount)
	}
	if res := sucResources(t, filepath.Join(root, redisChartRBACAddon)); !reflect.DeepEqual(res, []string{"rbac.yaml"}) {
		t.Errorf("%s/kustomization.yaml resources %v, want [rbac.yaml]", redisChartRBACAddon, res)
	}
}

func TestRedisOperatorRBAC(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, redisAddon)
	fixtureRaw, err := os.ReadFile(redisFixture)
	if err != nil {
		t.Fatal(err)
	}
	fixture, n := readRedisFixture(t)
	if pin, fix := redisChartVersion(t, root), fixtureChartVersion(t, redisFixture, "redis-operator"); pin != fix {
		t.Fatalf("chart.yaml pins redis-operator %s but %s was rendered from %s: refresh it (see its header)", pin, redisFixture, fix)
	}

	// The fixture proves the defect: the chart's ClusterRole aggregates into
	// admin and grants namespaces, and the chart binds the operator to it.
	chartRole, chartBinding := redisChartRBACObjects(t, fixture, n)
	if agg := redisAggregation(t, fixtureRaw)[objKey("ClusterRole", "", redisChartName)]; !slices.Contains(agg, aggregateLabelPrefix+"admin") {
		t.Fatalf("%s: ClusterRole/%s does not carry %sadmin (%v): the fixture is not the chart this addon fixes", redisFixture, redisChartName, aggregateLabelPrefix, agg)
	}
	if _, ok := redisGrantsNamespaces(chartRole); !ok {
		t.Fatalf("%s: ClusterRole/%s grants no namespaces rule: the fixture is not the chart this addon fixes", redisFixture, redisChartName)
	}

	rbacPath := filepath.Join(addon, "rbac.yaml")
	rbacRaw, err := os.ReadFile(rbacPath)
	if err != nil {
		t.Fatal(err)
	}
	if agg := redisAggregation(t, rbacRaw); len(agg) != 0 {
		t.Errorf("rbac.yaml aggregates into built-in roles: %v", agg)
	}
	rbac := decodeObjects(t, rbacRaw)
	both := maps.Clone(fixture)
	for k, o := range rbac {
		if _, dup := both[k]; dup {
			t.Errorf("rbac.yaml redefines %s of the chart", k)
		}
		both[k] = o
		if !strings.HasPrefix(o.Name, "lok8s-") {
			t.Errorf("%s: the name has no prefix lok8s-", k)
		}
		if o.Name == redisChartName {
			t.Errorf("%s uses the chart's RBAC name %s: a binding's roleRef cannot change", k, redisChartName)
		}
	}
	checkRedisNamespaces(t, both)
	clusterRole, ok1 := rbac[objKey("ClusterRole", "", redisRole)]
	execRole, ok2 := rbac[objKey("ClusterRole", "", redisExecRole)]
	leRole, ok3 := rbac[objKey("Role", n.namespace, redisLERole)]
	if !ok1 || !ok2 || !ok3 || len(rbac) != 6 {
		t.Fatalf("rbac.yaml: want ClusterRole/%s, ClusterRole/%s, Role %s/%s and their three bindings, have %v",
			redisRole, redisExecRole, n.namespace, redisLERole, sortedKeys(rbac))
	}
	for _, o := range []object{clusterRole, execRole, leRole} {
		sucCheckNoWildcards(t, o)
		if r, ok := redisGrantsNamespaces(o); ok {
			t.Errorf("%s/%s grants namespaces: %s", o.Kind, o.Name, canon(t, r))
		}
	}
	checkRedisBindings(t, rbac, n)

	// Every call is allowed: the operator's calls by its ClusterRole and the
	// leader-election Role alone, the RedisCluster exec by the exec role
	// alone.
	opGrants := []sucGrant{{clusterRole, ""}, {leRole, n.namespace}}
	for _, c := range redisCallsBy(n, redisByOperator) {
		if !slices.ContainsFunc(opGrants, func(g sucGrant) bool { return g.allows(c) }) {
			t.Errorf("the operator's roles do not allow %s %q/%s name %q in %q (%s)", c.verb, c.group, c.resource, c.name, c.ns, c.source)
		}
	}
	for _, c := range redisCallsBy(n, redisByClusterExec) {
		if !(sucGrant{execRole, ""}).allows(c) {
			t.Errorf("ClusterRole/%s does not allow %s %q/%s in %q (%s)", redisExecRole, c.verb, c.group, c.resource, c.ns, c.source)
		}
	}
	// No rule grants more than the calls need, at the scope it is granted.
	for _, g := range opGrants {
		sucCheckRulesNeeded(t, g, redisCallsBy(n, redisByOperator))
	}
	sucCheckRulesNeeded(t, sucGrant{execRole, ""}, redisCallsBy(n, redisByClusterExec))
	// No namespaced rule repeats what the token holds cluster-wide.
	sucCheckNoDuplicates(t, sucGrant{leRole, n.namespace}, []sucGrant{{clusterRole, ""}, {execRole, ""}})

	// The opt-in addon binds exactly the chart's rules, without aggregation.
	checkRedisOptIn(t, root, chartRole, chartBinding, n)

	patches, entries := patchesNode(t, addon)
	checkRedisPatch(t, addon, entries)
	if t.Failed() {
		t.FailNow()
	}

	// The addon's real patches over the chart render plus rbac.yaml.
	requireRenderToolchain(t, root, "the redis-operator RBAC render gate")
	stream := renderPatchedStream(t, root, [][]byte{fixtureRaw, rbacRaw}, patches)
	if agg := redisAggregation(t, stream); len(agg) != 0 {
		t.Errorf("the patched render still aggregates into built-in roles: %v", agg)
	}
	rendered := decodeObjects(t, stream)
	checkRedisBindings(t, rendered, n)
	checkRedisNamespaces(t, rendered)
	for k, o := range rbac {
		if !reflect.DeepEqual(rendered[k].Doc, o.Doc) {
			t.Errorf("%s changed in the render:\n got: %s\nwant: %s", k, canon(t, rendered[k].Doc), canon(t, o.Doc))
		}
	}
}

// redisReduce drops the Namespace and the CRDs of a chart render and keeps,
// per object, its aggregation (the fixture keeps those labels).
func redisReduce(t *testing.T, stream []byte) (map[string]object, map[string][]string) {
	t.Helper()
	objs := decodeObjects(t, stream)
	for k, o := range objs {
		if o.Kind == "Namespace" || o.Kind == "CustomResourceDefinition" {
			delete(objs, k)
		}
	}
	return objs, redisAggregation(t, stream)
}

// TestRedisOperatorFixtureMatchesChart renders the pinned chart through the
// addon's own generator (khelm, so it downloads the chart): the addon
// without its RBAC changes must equal the fixture, and the addon as shipped
// must carry the narrowed bindings and no aggregation. It also renders the
// opt-in addon.
func TestRedisOperatorFixtureMatchesChart(t *testing.T) {
	t.Parallel()
	if os.Getenv("CI") == "" && os.Getenv(onlineTestsEnv) == "" {
		t.Skipf("downloads the pinned chart: runs on CI, or with %s=1", onlineTestsEnv)
	}
	root := testutil.RepoRoot(t)
	requireRenderToolchain(t, root, "the redis-operator fixture gate")
	if !render.InProcessAvailable() {
		plugin := filepath.Join(config.KustomizePluginHome(projectPaths(root)), "khelm.mgoltzsche.github.com", "v2", "chartrenderer", "ChartRenderer")
		if _, err := os.Stat(plugin); err != nil {
			t.Fatalf("khelm exec plugin missing at %s (b install): the fixture gate cannot render the chart", plugin)
		}
	}
	addon := filepath.Join(root, redisAddon)
	fixtureRaw, err := os.ReadFile(redisFixture)
	if err != nil {
		t.Fatal(err)
	}
	fixture, n := readRedisFixture(t)

	renderDir := func(src string, mutate func(dir string)) []byte {
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
		return out
	}

	// The fixture recipe: no patches, no rbac.yaml.
	chart, chartAgg := redisReduce(t, renderDir(addon, func(dir string) {
		stripPatches(t, filepath.Join(dir, "kustomization.yaml"))
		dropResource(t, filepath.Join(dir, "kustomization.yaml"), "rbac.yaml")
		if err := os.Remove(filepath.Join(dir, "rbac.yaml")); err != nil {
			t.Fatal(err)
		}
	}))
	if !reflect.DeepEqual(sortedKeys(chart), sortedKeys(fixture)) {
		t.Fatalf("the chart renders %v, the fixture has %v: refresh %s", sortedKeys(chart), sortedKeys(fixture), redisFixture)
	}
	for k, o := range chart {
		if !reflect.DeepEqual(o.Doc, fixture[k].Doc) {
			t.Errorf("%s: the chart renders\n%s\nbut the fixture has\n%s\nrefresh %s", k, canon(t, o.Doc), canon(t, fixture[k].Doc), redisFixture)
		}
	}
	if fixAgg := redisAggregation(t, fixtureRaw); !reflect.DeepEqual(chartAgg, fixAgg) {
		t.Errorf("aggregation: the chart renders %v, the fixture has %v: refresh %s", chartAgg, fixAgg, redisFixture)
	}

	// The addon as shipped.
	shippedRaw := renderDir(addon, func(string) {})
	if agg := redisAggregation(t, shippedRaw); len(agg) != 0 {
		t.Errorf("the shipped render aggregates into built-in roles: %v", agg)
	}
	shipped := decodeObjects(t, shippedRaw)
	checkRedisBindings(t, shipped, n)
	checkRedisNamespaces(t, shipped)
	for _, k := range sortedKeys(shipped) {
		if o := shipped[k]; o.Kind == "ClusterRole" || o.Kind == "Role" {
			if r, ok := redisGrantsNamespaces(o); ok {
				t.Errorf("%s in the shipped render grants namespaces: %s", k, canon(t, r))
			}
		}
	}
	rbac := readObjects(t, filepath.Join(addon, "rbac.yaml"))
	for k, o := range rbac {
		if !reflect.DeepEqual(shipped[k].Doc, o.Doc) {
			t.Errorf("%s in the shipped render:\n got: %s\nwant: %s", k, canon(t, shipped[k].Doc), canon(t, o.Doc))
		}
	}

	// The opt-in addon renders its rbac.yaml, without aggregation.
	optRaw := renderDir(filepath.Join(root, redisChartRBACAddon), func(string) {})
	if agg := redisAggregation(t, optRaw); len(agg) != 0 {
		t.Errorf("the opt-in render aggregates into built-in roles: %v", agg)
	}
	opt := decodeObjects(t, optRaw)
	want := readObjects(t, filepath.Join(root, redisChartRBACAddon, "rbac.yaml"))
	if !reflect.DeepEqual(sortedKeys(opt), sortedKeys(want)) {
		t.Fatalf("the opt-in addon renders %v, want %v", sortedKeys(opt), sortedKeys(want))
	}
	for k, o := range want {
		if !reflect.DeepEqual(opt[k].Doc, o.Doc) {
			t.Errorf("%s in the opt-in render:\n got: %s\nwant: %s", k, canon(t, opt[k].Doc), canon(t, o.Doc))
		}
	}
}
