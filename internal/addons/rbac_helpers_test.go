package addons

// rbac_helpers_test.go — the static RBAC evaluation that the addon gates
// share (suc_rbac_test.go, redis_operator_rbac_test.go): one inventory entry
// per Kubernetes API request (rbacCall), a role as one binding applies it
// (rbacGrant), and the checks in both directions (every call allowed, every
// rule atom needed at its scope, no namespaced duplicate of a cluster-wide
// grant). Also the small readers the gates and rookceph_rbac_test.go share.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Scope and name values of an rbacCall.
const (
	// rbacAtCluster: a cluster-scoped resource, or a request across all
	// namespaces.
	rbacAtCluster = ""
	// rbacAtAnyNS: one namespace that the request does not know in advance.
	rbacAtAnyNS = "*"
	// rbacAtCtrlNS: the controller's namespace (read from the fixture).
	rbacAtCtrlNS = "@namespace"
	// rbacAnyName: the request names no object (list, watch, create), or an
	// object that RBAC cannot know in advance.
	rbacAnyName = ""
	// rbacLease: the controller's Lease (read from the fixture).
	rbacLease = "@lease"
)

// rbacCall is one Kubernetes API request.
type rbacCall struct {
	by       string
	group    string
	resource string // with the subresource: plans/status
	verb     string
	ns       string
	name     string
	source   string
}

// rbacSubject is the controller's ServiceAccount, its namespace and the name
// of its leader-election Lease, as a gate reads them from its fixture.
type rbacSubject struct {
	namespace, serviceAccount, lease string
}

// resolve replaces the fixture placeholders of a call.
func (n rbacSubject) resolve(c rbacCall) rbacCall {
	if c.ns == rbacAtCtrlNS {
		c.ns = n.namespace
	}
	if c.name == rbacLease {
		c.name = n.lease
	}
	return c
}

// rbacRuleAllows reports whether one rule allows a call (no wildcards: the
// gates reject them before they evaluate).
func rbacRuleAllows(r map[string]any, c rbacCall) bool {
	if !slices.Contains(stringsOf(r["apiGroups"]), c.group) ||
		!slices.Contains(stringsOf(r["resources"]), c.resource) ||
		!slices.Contains(stringsOf(r["verbs"]), c.verb) {
		return false
	}
	names := stringsOf(r["resourceNames"])
	return len(names) == 0 || (c.name != rbacAnyName && slices.Contains(names, c.name))
}

// rbacGrant is a role as one binding applies it: in every namespace and at
// the cluster scope (ns == ""), or in one namespace.
type rbacGrant struct {
	role object
	ns   string
}

func (g rbacGrant) allows(c rbacCall) bool {
	if g.ns != "" && c.ns != g.ns {
		return false
	}
	for _, r := range g.role.Rules() {
		if rbacRuleAllows(r, c) {
			return true
		}
	}
	return false
}

// rbacCheckNoWildcards rejects `*` in any rule field and nonResourceURLs.
func rbacCheckNoWildcards(t *testing.T, o object) {
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

// rbacCheckRulesNeeded asserts that every atom of every rule of g (group,
// resource, verb, and each resource name or none) serves a call at the
// scope g grants. A cluster-wide grant must serve a call on a
// cluster-scoped resource or across namespaces (rbacAtCluster, rbacAtAnyNS):
// a call in a known namespace belongs to a Role there. A namespaced grant
// must serve a call in its namespace. A rule without resourceNames must
// serve a call that RBAC cannot restrict by name.
func rbacCheckRulesNeeded(t *testing.T, g rbacGrant, calls []rbacCall) {
	t.Helper()
	for _, r := range g.role.Rules() {
		names := stringsOf(r["resourceNames"])
		if len(names) == 0 {
			names = []string{rbacAnyName}
		}
		for _, grp := range stringsOf(r["apiGroups"]) {
			for _, res := range stringsOf(r["resources"]) {
				for _, verb := range stringsOf(r["verbs"]) {
					for _, name := range names {
						needed := slices.ContainsFunc(calls, func(c rbacCall) bool {
							if c.group != grp || c.resource != res || c.verb != verb || c.name != name {
								return false
							}
							if g.ns == "" {
								return c.ns == rbacAtCluster || c.ns == rbacAtAnyNS
							}
							return c.ns == g.ns
						})
						if !needed {
							scope := "cluster-wide"
							if g.ns != "" {
								scope = "in " + g.ns
							}
							t.Errorf("%s/%s grants %s %q/%s name %q %s, and no call in the inventory needs it at that scope", g.role.Kind, g.role.Name, verb, grp, res, name, scope)
						}
					}
				}
			}
		}
	}
}

// rbacClusterScoped are the cluster-scoped kinds a render of an addon can
// carry; every other kind is namespaced.
var rbacClusterScoped = []string{"Namespace", "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition"}

// rbacCheckNoDuplicates asserts that no atom of a namespaced grant (group,
// resource, verb, resource name) is already allowed in that namespace by a
// cluster-wide grant of the same ServiceAccount: such a rule adds nothing
// and hides what the ClusterRole grants.
func rbacCheckNoDuplicates(t *testing.T, g rbacGrant, cluster []rbacGrant) {
	t.Helper()
	for _, r := range g.role.Rules() {
		names := stringsOf(r["resourceNames"])
		if len(names) == 0 {
			names = []string{rbacAnyName}
		}
		for _, grp := range stringsOf(r["apiGroups"]) {
			for _, res := range stringsOf(r["resources"]) {
				for _, verb := range stringsOf(r["verbs"]) {
					for _, name := range names {
						c := rbacCall{group: grp, resource: res, verb: verb, ns: g.ns, name: name}
						for _, cg := range cluster {
							if cg.allows(c) {
								t.Errorf("%s %s/%s grants %s %q/%s name %q, which %s/%s already grants in every namespace", g.role.Kind, g.ns, g.role.Name, verb, grp, res, name, cg.role.Kind, cg.role.Name)
							}
						}
					}
				}
			}
		}
	}
}

// nestedMap walks nested maps; a missing key gives an empty map.
func nestedMap(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		m = next
	}
	return m
}

// rbacBindsSA reports whether a binding names the controller's ServiceAccount.
func rbacBindsSA(b object, n rbacSubject) bool {
	subjects, _ := b.Doc["subjects"].([]any)
	for _, s := range subjects {
		m, _ := s.(map[string]any)
		if m["kind"] == "ServiceAccount" && m["name"] == n.serviceAccount && m["namespace"] == n.namespace {
			return true
		}
	}
	return false
}

// chartPinVersion reads `version:` from the chart.yaml in an addon
// directory, and fails when the file pins another chart.
func chartPinVersion(t *testing.T, addonDir, chart string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(addonDir, "chart.yaml"))
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
	if spec.Chart != chart || spec.Version == "" {
		t.Fatalf("%s/chart.yaml: chart=%q version=%q, want chart %q with a version", addonDir, spec.Chart, spec.Version, chart)
	}
	return spec.Version
}

// kustomizationResources reads the `resources:` list of a kustomization.yaml.
func kustomizationResources(t *testing.T, dir string) []string {
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

// rawDoc is one document of a stream as it is, and its key (objKey).
type rawDoc struct {
	Key  string
	Kind string
	Doc  map[string]any
}

// decodeDocs returns every document of a multi-document stream with its
// kind and name, unchanged (labels and all). Any decode error other than
// EOF fails the test: a corrupt stream must never read as "nothing there".
// So does a key that comes twice: the second object must never hide the
// first. A document without a kind or a name is skipped.
func decodeDocs(t *testing.T, stream []byte) []rawDoc {
	t.Helper()
	var out []rawDoc
	seen := map[string]bool{}
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
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if kind == "" || name == "" {
			continue
		}
		key := objKey(kind, ns, name)
		if seen[key] {
			t.Fatalf("decoding the object stream: %s comes twice", key)
		}
		seen[key] = true
		out = append(out, rawDoc{Key: key, Kind: kind, Doc: doc})
	}
	return out
}
