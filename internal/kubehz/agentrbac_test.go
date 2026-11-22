package kubehz

// agentrbac_test.go: the heartbeat agent's RBAC grants what its script uses,
// and nothing that lets it change its own configuration. The agent reads its
// configuration (kubehz-agent-config: the API URL, the cluster id, the
// heartbeat owner) through envFrom, so it needs no access to it through the
// API. It keeps its markers (the 24h assessment gate, the claim nonce, the
// desired-state validator) as annotations on a separate ConfigMap,
// kubehz-agent-state. That ConfigMap and its identity Secret, kubehz-agent,
// are the only objects it can patch, and it can create nothing: the deploy
// creates both empty. Its bindings grant only its own roles, and only to its
// own ServiceAccount.
//
// The tests read the embedded manifests (internal/kubehz/manifests/agent).
// TestEmbeddedManifestsMatchBashTree pins them to .lok8s/libs/kubehz/manifests
// byte for byte, and TestEmbeddedMirrorMatchesLegacyTree pins the asset
// mirror, so all three copies are checked.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	agentConfigName    = "kubehz-agent-config"
	agentStateFile     = "agent/state-configmap.yaml"
	agentIdentityFile  = "agent/identity-secret.yaml"
	agentIdentitySName = "kubehz-agent"
)

type rbacRule struct {
	APIGroups       []string `yaml:"apiGroups"`
	Resources       []string `yaml:"resources"`
	ResourceNames   []string `yaml:"resourceNames"`
	Verbs           []string `yaml:"verbs"`
	NonResourceURLs []string `yaml:"nonResourceURLs"`
}

type rbacSubject struct {
	Kind      string `yaml:"kind"`
	APIGroup  string `yaml:"apiGroup"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type rbacMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type rbacRoleRef struct {
	APIGroup string `yaml:"apiGroup"`
	Kind     string `yaml:"kind"`
	Name     string `yaml:"name"`
}

type rbacDoc struct {
	Kind      string        `yaml:"kind"`
	Metadata  rbacMeta      `yaml:"metadata"`
	Rules     []rbacRule    `yaml:"rules"`
	RoleRef   rbacRoleRef   `yaml:"roleRef"`
	Subjects  []rbacSubject `yaml:"subjects"`
	Resources []string      `yaml:"resources"`
	// AggregationRule keeps the node of an aggregationRule key. Its Kind is
	// 0 only when the document has no such key: a null value is a node too.
	AggregationRule yaml.Node `yaml:"aggregationRule"`
}

// agentDocs decodes every document of every YAML file in the embedded
// agent/ directory, keyed by file.
func agentDocs(t *testing.T) map[string][]rbacDoc {
	t.Helper()
	out := map[string][]rbacDoc{}
	err := fs.WalkDir(Manifests(), "agent", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		raw, err := fs.ReadFile(Manifests(), path)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var doc rbacDoc
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("%s: %v", path, err)
			}
			out[path] = append(out[path], doc)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// agentDocList returns every document of the agent manifests, file by file
// in name order.
func agentDocList(t *testing.T) []rbacDoc {
	t.Helper()
	byFile := agentDocs(t)
	var out []rbacDoc
	for _, f := range slices.Sorted(maps.Keys(byFile)) {
		out = append(out, byFile[f]...)
	}
	return out
}

// agentRoles returns every Role and ClusterRole of the agent manifests.
func agentRoles(t *testing.T) []rbacDoc {
	t.Helper()
	var roles []rbacDoc
	for _, docs := range agentDocs(t) {
		for _, d := range docs {
			if d.Kind == "Role" || d.Kind == "ClusterRole" {
				roles = append(roles, d)
			}
		}
	}
	if len(roles) < 4 {
		t.Fatalf("%d Roles and ClusterRoles found in agent/, want at least 4", len(roles))
	}
	return roles
}

// TestAgentClusterRoleIsTheScriptSet pins the ClusterRole rule by rule:
// list only on nodes, CSRs, storageclasses and persistentvolumes (each call
// of the script on them is a list, and TestHeartbeatKubectlCallsDoNotGrowWithTheCluster
// refuses a call that reads one node), and no namespaces or
// componentstatuses (the script reads neither).
func TestAgentClusterRoleIsTheScriptSet(t *testing.T) {
	t.Parallel()
	core := []string{""}
	list := []string{"list"}
	want := []rbacRule{
		{APIGroups: core, Resources: []string{"nodes"}, Verbs: list},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: list},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: list},
		{APIGroups: core, Resources: []string{"persistentvolumes"}, Verbs: list},
		{APIGroups: core, Resources: []string{"services"}, Verbs: list},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"}, Verbs: list},
		{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"}, ResourceNames: []string{"clusters.cluster.x-k8s.io"}, Verbs: []string{"get"}},
		{NonResourceURLs: []string{"/readyz", "/version"}, Verbs: []string{"get"}},
	}
	found := 0
	for _, r := range agentRoles(t) {
		if r.Kind != "ClusterRole" {
			continue
		}
		found++
		if r.Metadata.Name != "kubehz-agent" {
			t.Errorf("ClusterRole %q: the agent has one ClusterRole, kubehz-agent", r.Metadata.Name)
			continue
		}
		if !reflect.DeepEqual(r.Rules, want) {
			t.Errorf("ClusterRole kubehz-agent rules:\n got %+v\nwant %+v", r.Rules, want)
		}
		for _, rule := range r.Rules {
			for _, res := range rule.Resources {
				if res == "namespaces" || res == "componentstatuses" {
					t.Errorf("ClusterRole kubehz-agent grants %v on %s: the script reads neither", rule.Verbs, res)
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("%d ClusterRoles found, want 1", found)
	}
}

// agentRoleKey names a Role or ClusterRole: its kind, its namespace ("" for
// a ClusterRole) and its name.
type agentRoleKey struct {
	Kind, Namespace, Name string
}

// agentRoleRules is every Role and ClusterRole of the agent manifests with
// its full rule set: each grant has a caller in the heartbeat script (the
// comments in rbac.yaml name them). The ClusterRole is the set of
// TestAgentClusterRoleIsTheScriptSet.
var agentRoleRules = map[agentRoleKey][]rbacRule{
	{"ClusterRole", "", "kubehz-agent"}: {
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"list"}},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: []string{"list"}},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: []string{"list"}},
		{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"list"}},
		{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"list"}},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"}, Verbs: []string{"list"}},
		{APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"}, ResourceNames: []string{"clusters.cluster.x-k8s.io"}, Verbs: []string{"get"}},
		{NonResourceURLs: []string{"/readyz", "/version"}, Verbs: []string{"get"}},
	},
	{"Role", "kube-system", "kubehz-agent"}: {
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
		{APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, Verbs: []string{"list"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"kubeadm-config"}, Verbs: []string{"get"}},
	},
	{"Role", agentNamespace, "kubehz-agent-secret"}: {
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{agentIdentitySName}, Verbs: []string{"get", "patch"}},
	},
	{"Role", agentNamespace, "kubehz-agent-marker"}: {
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{agentStateName}, Verbs: []string{"get", "patch"}},
	},
}

// canonicalRules gives a rule set in a form that does not depend on order:
// RBAC grants the union of the rules, and each list of a rule is a set. So
// each list is sorted, each rule is printed, and the printed rules are
// sorted. Nothing is merged or dropped: a rule or a list item that occurs
// twice stays twice, and two rule sets are the same only when they hold the
// same rules item for item.
func canonicalRules(rules []rbacRule) []string {
	sorted := func(s []string) []string { return slices.Sorted(slices.Values(s)) }
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, fmt.Sprintf("apiGroups=%q resources=%q resourceNames=%q verbs=%q nonResourceURLs=%q",
			sorted(r.APIGroups), sorted(r.Resources), sorted(r.ResourceNames), sorted(r.Verbs), sorted(r.NonResourceURLs)))
	}
	slices.Sort(out)
	return out
}

// agentRoleProblems checks every Role and ClusterRole of docs:
//   - role: the role is a key of agentRoleRules;
//   - rules: its rule set equals the one of agentRoleRules (canonicalRules);
//   - aggregation: it carries no aggregationRule (the controller would fill
//     a ClusterRole with the rules of every ClusterRole it selects);
//   - once: each role of agentRoleRules is defined exactly once.
func agentRoleProblems(docs []rbacDoc) []rbacProblem {
	var out []rbacProblem
	add := func(rule, format string, args ...any) {
		out = append(out, rbacProblem{rule, fmt.Sprintf(format, args...)})
	}
	found := map[agentRoleKey]int{}
	for _, d := range docs {
		if d.Kind != "Role" && d.Kind != "ClusterRole" {
			continue
		}
		key := agentRoleKey{d.Kind, d.Metadata.Namespace, d.Metadata.Name}
		if d.Kind == "ClusterRole" {
			key.Namespace = ""
		}
		where := fmt.Sprintf("%s %s/%s", d.Kind, key.Namespace, key.Name)
		if d.AggregationRule.Kind != 0 {
			add("aggregation", "%s carries an aggregationRule: its rules would come from other ClusterRoles", where)
		}
		want, ok := agentRoleRules[key]
		if !ok {
			add("role", "%s is not one of the agent's roles", where)
			continue
		}
		found[key]++
		if got, w := canonicalRules(d.Rules), canonicalRules(want); !slices.Equal(got, w) {
			add("rules", "%s rules:\n got %q\nwant %q", where, got, w)
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(agentRoleRules), func(a, b agentRoleKey) int {
		return strings.Compare(a.Kind+"/"+a.Namespace+"/"+a.Name, b.Kind+"/"+b.Namespace+"/"+b.Name)
	}) {
		if n := found[key]; n != 1 {
			add("once", "%s %s/%s: defined %d times, want 1", key.Kind, key.Namespace, key.Name, n)
		}
	}
	return out
}

// TestAgentRolesAreTheScriptSet pins the full rule set of every Role and
// ClusterRole of the agent manifests, read and write rules alike: an extra
// rule, verb, resource or name, or one that is missing, fails. The compare
// does not depend on the order of the rules or of the items in a list
// (canonicalRules). No role carries an aggregationRule.
func TestAgentRolesAreTheScriptSet(t *testing.T) {
	t.Parallel()
	docs := agentDocList(t)
	roles := 0
	for _, d := range docs {
		if d.Kind == "Role" || d.Kind == "ClusterRole" {
			roles++
		}
	}
	if roles != len(agentRoleRules) {
		t.Errorf("%d Roles and ClusterRoles in agent/, want %d", roles, len(agentRoleRules))
	}
	for _, p := range agentRoleProblems(docs) {
		t.Errorf("%s: %s", p.rule, p.text)
	}
}

// TestAgentRoleCheckRefusesExtraGrants: the role check finds each change to
// the real manifests by the rule that it breaks, and takes the same rules
// in another order.
func TestAgentRoleCheckRefusesExtraGrants(t *testing.T) {
	t.Parallel()
	base := agentDocList(t)
	if p := agentRoleProblems(base); len(p) != 0 {
		t.Fatalf("the real manifests break the role check: %+v", p)
	}
	// edit returns a copy of base in which fn changed the role key; the
	// rules of that role are copied first, so base stays as it is.
	edit := func(key agentRoleKey, fn func(rules []rbacRule) []rbacRule) []rbacDoc {
		docs := slices.Clone(base)
		n := 0
		for i, d := range docs {
			if d.Kind != key.Kind || d.Metadata.Name != key.Name || (d.Kind == "Role" && d.Metadata.Namespace != key.Namespace) {
				continue
			}
			rules := make([]rbacRule, len(d.Rules))
			for j, r := range d.Rules {
				rules[j] = rbacRule{
					APIGroups: slices.Clone(r.APIGroups), Resources: slices.Clone(r.Resources),
					ResourceNames: slices.Clone(r.ResourceNames), Verbs: slices.Clone(r.Verbs),
					NonResourceURLs: slices.Clone(r.NonResourceURLs),
				}
			}
			docs[i].Rules = fn(rules)
			n++
		}
		if n != 1 {
			t.Fatalf("%+v: %d roles in the manifests, want 1", key, n)
		}
		return docs
	}
	clusterRole := agentRoleKey{"ClusterRole", "", "kubehz-agent"}
	marker := agentRoleKey{"Role", agentNamespace, "kubehz-agent-marker"}
	secret := agentRoleKey{"Role", agentNamespace, "kubehz-agent-secret"}
	// aggregated swaps the ClusterRole for one decoded from YAML with an
	// aggregationRule key (src is its value).
	aggregated := func(src string) []rbacDoc {
		var d rbacDoc
		if err := yaml.Unmarshal([]byte("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: kubehz-agent\naggregationRule:"+src), &d); err != nil {
			t.Fatal(err)
		}
		d.Rules = agentRoleRules[clusterRole]
		docs := slices.DeleteFunc(slices.Clone(base), func(x rbacDoc) bool { return x.Kind == "ClusterRole" })
		return append(docs, d)
	}
	markerDoc := base[slices.IndexFunc(base, func(d rbacDoc) bool { return d.Kind == "Role" && d.Metadata.Name == marker.Name })]
	for _, tc := range []struct {
		name string
		docs []rbacDoc
		want string // "" = no problem
	}{
		{"the same rules in reverse order, each list reversed", edit(secret, func(rules []rbacRule) []rbacRule {
			slices.Reverse(rules)
			for i := range rules {
				slices.Reverse(rules[i].Verbs)
			}
			return rules
		}), ""},
		{"the ClusterRole's lists reversed", edit(clusterRole, func(rules []rbacRule) []rbacRule {
			for i := range rules {
				slices.Reverse(rules[i].Resources)
				slices.Reverse(rules[i].NonResourceURLs)
			}
			return rules
		}), ""},
		{"an extra read rule in the marker Role", edit(marker, func(rules []rbacRule) []rbacRule {
			return append(rules, rbacRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}})
		}), "rules"},
		{"an extra verb in the ClusterRole", edit(clusterRole, func(rules []rbacRule) []rbacRule {
			rules[0].Verbs = append(rules[0].Verbs, "watch")
			return rules
		}), "rules"},
		{"an extra resource in the ClusterRole", edit(clusterRole, func(rules []rbacRule) []rbacRule {
			rules[0].Resources = append(rules[0].Resources, "namespaces")
			return rules
		}), "rules"},
		{"an extra API group in the kube-system Role", edit(agentRoleKey{"Role", "kube-system", "kubehz-agent"}, func(rules []rbacRule) []rbacRule {
			rules[1].APIGroups = append(rules[1].APIGroups, "extensions")
			return rules
		}), "rules"},
		{"the name scope removed from the identity Secret get and patch", edit(secret, func(rules []rbacRule) []rbacRule {
			rules[0].ResourceNames = nil
			return rules
		}), "rules"},
		{"a create on Secrets back in the Secret Role", edit(secret, func(rules []rbacRule) []rbacRule {
			return append(rules, rbacRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}})
		}), "rules"},
		{"a create verb on the identity Secret rule", edit(secret, func(rules []rbacRule) []rbacRule {
			rules[0].Verbs = append(rules[0].Verbs, "create")
			return rules
		}), "rules"},
		{"an extra non-resource URL", edit(clusterRole, func(rules []rbacRule) []rbacRule {
			rules[len(rules)-1].NonResourceURLs = append(rules[len(rules)-1].NonResourceURLs, "/metrics")
			return rules
		}), "rules"},
		{"a rule removed from the ClusterRole", edit(clusterRole, func(rules []rbacRule) []rbacRule {
			return rules[1:]
		}), "rules"},
		{"a rule that occurs twice", edit(marker, func(rules []rbacRule) []rbacRule {
			return append(rules, rules[0])
		}), "rules"},
		{"an aggregationRule on the ClusterRole", aggregated("\n  clusterRoleSelectors:\n    - matchLabels:\n        rbac.authorization.k8s.io/aggregate-to-view: \"true\"\n"), "aggregation"},
		{"an aggregationRule with a null value", aggregated(" null\n"), "aggregation"},
		{"an aggregationRule with no value", aggregated("\n"), "aggregation"},
		{"a Role that is not the agent's", append(slices.Clone(base), rbacDoc{Kind: "Role", Metadata: rbacMeta{Name: "kubehz-agent-extra", Namespace: agentNamespace}}), "role"},
		{"a role defined twice", append(slices.Clone(base), markerDoc), "once"},
	} {
		problems := agentRoleProblems(tc.docs)
		if tc.want == "" {
			if len(problems) != 0 {
				t.Errorf("%s: %+v, want no problem", tc.name, problems)
			}
			continue
		}
		if !slices.ContainsFunc(problems, func(p rbacProblem) bool { return p.rule == tc.want }) {
			t.Errorf("%s: no %q problem, got %+v", tc.name, tc.want, problems)
		}
	}
}

// writeVerbs are the verbs that create or change an object, or that grant
// that (escalate, bind, impersonate, and * for every verb).
var writeVerbs = []string{"create", "patch", "update", "delete", "deletecollection", "*", "escalate", "bind", "impersonate"}

// agentWriteRule is one rule with a write verb, and the role that holds it.
type agentWriteRule struct {
	Kind, Namespace, Name string
	Rule                  rbacRule
}

// agentWriteAllowlist is every rule of the agent manifests with a write
// verb, in its exact form:
//   - get and patch on the one ConfigMap kubehz-agent-state (the markers);
//   - get and patch on the one Secret kubehz-agent (the identity), which
//     the bootstrap fills once. No create: RBAC cannot scope a create by
//     name (an object has no name before it exists), so a create rule would
//     let the token make a Secret of any name in kubehz-system. The deploy
//     creates the Secret empty (agent/identity-secret.yaml).
var agentWriteAllowlist = []agentWriteRule{
	{"Role", agentNamespace, "kubehz-agent-marker", rbacRule{
		APIGroups: []string{""}, Resources: []string{"configmaps"},
		ResourceNames: []string{agentStateName}, Verbs: []string{"get", "patch"},
	}},
	{"Role", agentNamespace, "kubehz-agent-secret", rbacRule{
		APIGroups: []string{""}, Resources: []string{"secrets"},
		ResourceNames: []string{agentIdentitySName}, Verbs: []string{"get", "patch"},
	}},
}

// TestAgentRBACWriteRulesAreTheAllowlist: each rule with a write verb is a
// rule of agentWriteAllowlist in its exact form (the role, the API groups,
// the resources, the names and the verbs), and each rule of the allowlist
// is found once. No rule names kubehz-agent-config, every rule on
// ConfigMaps or Secrets names its objects, and no rule grants create.
func TestAgentRBACWriteRulesAreTheAllowlist(t *testing.T) {
	t.Parallel()
	found := make([]int, len(agentWriteAllowlist))
	for _, r := range agentRoles(t) {
		for _, rule := range r.Rules {
			if slices.Contains(rule.ResourceNames, agentConfigName) {
				t.Errorf("%s %s/%s names %s: the agent gets its configuration through envFrom and needs no access to it", r.Kind, r.Metadata.Namespace, r.Metadata.Name, agentConfigName)
			}
			if slices.Contains(rule.Resources, "configmaps") && len(rule.ResourceNames) == 0 {
				t.Errorf("%s %s/%s grants %v on every ConfigMap", r.Kind, r.Metadata.Namespace, r.Metadata.Name, rule.Verbs)
			}
			if slices.Contains(rule.Resources, "secrets") && len(rule.ResourceNames) == 0 {
				t.Errorf("%s %s/%s grants %v on every Secret in its namespace", r.Kind, r.Metadata.Namespace, r.Metadata.Name, rule.Verbs)
			}
			if slices.Contains(rule.Verbs, "create") {
				t.Errorf("%s %s/%s grants create on %v: the deploy creates the agent's objects", r.Kind, r.Metadata.Namespace, r.Metadata.Name, rule.Resources)
			}
			if !slices.ContainsFunc(rule.Verbs, func(v string) bool { return slices.Contains(writeVerbs, v) }) {
				continue
			}
			i := slices.IndexFunc(agentWriteAllowlist, func(w agentWriteRule) bool {
				return w.Kind == r.Kind && w.Namespace == r.Metadata.Namespace && w.Name == r.Metadata.Name &&
					reflect.DeepEqual(w.Rule, rule)
			})
			if i < 0 {
				t.Errorf("%s %s/%s: a write rule that is not on the allowlist: %+v", r.Kind, r.Metadata.Namespace, r.Metadata.Name, rule)
				continue
			}
			found[i]++
		}
	}
	for i, w := range agentWriteAllowlist {
		if found[i] != 1 {
			t.Errorf("%s %s/%s %+v: found %d times, want 1", w.Kind, w.Namespace, w.Name, w.Rule, found[i])
		}
	}
}

// TestAgentRBACHasNoWildcard: no rule of a Role or ClusterRole of the agent
// holds a `*` in its apiGroups, resources, verbs or nonResourceURLs.
func TestAgentRBACHasNoWildcard(t *testing.T) {
	t.Parallel()
	rules := 0
	for _, r := range agentRoles(t) {
		for _, rule := range r.Rules {
			rules++
			fields := []struct {
				name   string
				values []string
			}{
				{"apiGroups", rule.APIGroups},
				{"resources", rule.Resources},
				{"verbs", rule.Verbs},
				{"nonResourceURLs", rule.NonResourceURLs},
			}
			for _, f := range fields {
				for _, v := range f.values {
					if strings.Contains(v, "*") {
						t.Errorf("%s %s/%s: %s %q holds a wildcard: %+v", r.Kind, r.Metadata.Namespace, r.Metadata.Name, f.name, v, rule)
					}
				}
			}
		}
	}
	// The agent's roles hold 13 rules. Fewer means the scan reads nothing.
	if rules < 13 {
		t.Fatalf("%d rules found in agent/, want at least 13", rules)
	}
}

// agentRoleRef is a binding as the allowlist sees it: the binding kind, its
// namespace ("" for a ClusterRoleBinding) and the role it grants.
type agentRoleRef struct {
	Binding, Namespace, RoleKind, RoleName string
}

// agentBindingAllowlist is every binding of the agent manifests. Each one
// grants one of the agent's own roles, in the namespace of that role. A
// built-in role (edit, admin, cluster-admin, view) is not on it.
var agentBindingAllowlist = []agentRoleRef{
	{"ClusterRoleBinding", "", "ClusterRole", "kubehz-agent"},
	{"RoleBinding", "kube-system", "Role", "kubehz-agent"},
	{"RoleBinding", agentNamespace, "Role", "kubehz-agent-secret"},
	{"RoleBinding", agentNamespace, "Role", "kubehz-agent-marker"},
}

// agentSubject is the one subject of every binding: the agent's
// ServiceAccount.
var agentSubject = rbacSubject{Kind: "ServiceAccount", Name: "kubehz-agent", Namespace: agentNamespace}

// rbacProblem is one finding of the binding check, with the rule it breaks.
type rbacProblem struct {
	rule string // "allowlist", "defined", "subject" or "once"
	text string
}

// agentBindingProblems checks every RoleBinding and ClusterRoleBinding of
// docs:
//   - allowlist: the binding and its roleRef are on agentBindingAllowlist;
//   - defined: docs define the role that the roleRef names, in its scope;
//   - subject: the only subject is the agent's ServiceAccount;
//   - once: each entry of the allowlist is bound exactly once.
func agentBindingProblems(docs []rbacDoc) []rbacProblem {
	type roleKey struct{ kind, namespace, name string }
	defined := map[roleKey]bool{}
	for _, d := range docs {
		switch d.Kind {
		case "ClusterRole":
			defined[roleKey{d.Kind, "", d.Metadata.Name}] = true
		case "Role":
			defined[roleKey{d.Kind, d.Metadata.Namespace, d.Metadata.Name}] = true
		}
	}
	var out []rbacProblem
	add := func(rule, format string, args ...any) {
		out = append(out, rbacProblem{rule, fmt.Sprintf(format, args...)})
	}
	bound := map[agentRoleRef]int{}
	for _, d := range docs {
		if d.Kind != "RoleBinding" && d.Kind != "ClusterRoleBinding" {
			continue
		}
		where := fmt.Sprintf("%s %s/%s", d.Kind, d.Metadata.Namespace, d.Metadata.Name)
		ref := agentRoleRef{d.Kind, d.Metadata.Namespace, d.RoleRef.Kind, d.RoleRef.Name}
		if d.RoleRef.APIGroup == "rbac.authorization.k8s.io" && slices.Contains(agentBindingAllowlist, ref) {
			bound[ref]++
		} else {
			add("allowlist", "%s grants %s %s (apiGroup %q): not one of the agent's own roles", where, d.RoleRef.Kind, d.RoleRef.Name, d.RoleRef.APIGroup)
		}
		scope := d.Metadata.Namespace
		if d.RoleRef.Kind == "ClusterRole" {
			scope = ""
		}
		if !defined[roleKey{d.RoleRef.Kind, scope, d.RoleRef.Name}] {
			add("defined", "%s grants %s %s, which the agent manifests do not define", where, d.RoleRef.Kind, d.RoleRef.Name)
		}
		if len(d.Subjects) != 1 || d.Subjects[0] != agentSubject {
			add("subject", "%s binds %+v, want only ServiceAccount %s/%s", where, d.Subjects, agentSubject.Namespace, agentSubject.Name)
		}
	}
	for _, ref := range agentBindingAllowlist {
		if n := bound[ref]; n != 1 {
			add("once", "%s %q to %s %s: %d bindings, want 1", ref.Binding, ref.Namespace, ref.RoleKind, ref.RoleName, n)
		}
	}
	return out
}

// TestAgentBindingsAreTheAllowlist: every RoleBinding and ClusterRoleBinding
// of the agent manifests grants one of the agent's own roles to the agent's
// ServiceAccount only, and each entry of the allowlist is bound once. A
// binding to a built-in role (edit, admin, cluster-admin) fails here.
func TestAgentBindingsAreTheAllowlist(t *testing.T) {
	t.Parallel()
	docs := agentDocList(t)
	n := 0
	for _, d := range docs {
		if d.Kind == "RoleBinding" || d.Kind == "ClusterRoleBinding" {
			n++
		}
	}
	if n != len(agentBindingAllowlist) {
		t.Errorf("%d bindings in agent/, want %d", n, len(agentBindingAllowlist))
	}
	for _, p := range agentBindingProblems(docs) {
		t.Errorf("%s: %s", p.rule, p.text)
	}
}

// TestAgentBindingCheckRefusesForeignBindings: the binding check finds each
// foreign binding, added to the real manifests, by the rule that it breaks.
func TestAgentBindingCheckRefusesForeignBindings(t *testing.T) {
	t.Parallel()
	base := agentDocList(t)
	if p := agentBindingProblems(base); len(p) != 0 {
		t.Fatalf("the real manifests break the binding check: %+v", p)
	}
	binding := func(kind, ns, name, roleKind, roleName string) rbacDoc {
		return rbacDoc{
			Kind:     kind,
			Metadata: rbacMeta{Name: name, Namespace: ns},
			RoleRef:  rbacRoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: roleKind, Name: roleName},
			Subjects: []rbacSubject{agentSubject},
		}
	}
	plus := func(extra rbacDoc) []rbacDoc { return append(slices.Clone(base), extra) }
	// markerSubjects gives the marker RoleBinding these subjects.
	markerSubjects := func(subjects ...rbacSubject) []rbacDoc {
		docs := slices.Clone(base)
		for i, d := range docs {
			if d.Kind == "RoleBinding" && d.Metadata.Name == "kubehz-agent-marker" {
				docs[i].Subjects = subjects
			}
		}
		return docs
	}
	foreignAPIGroup := binding("RoleBinding", agentNamespace, "kubehz-agent-x", "Role", "kubehz-agent-marker")
	foreignAPIGroup.RoleRef.APIGroup = "example.com"
	for _, tc := range []struct {
		name string
		docs []rbacDoc
		want string
	}{
		{"a RoleBinding to the built-in edit ClusterRole", plus(binding("RoleBinding", agentNamespace, "kubehz-agent-edit", "ClusterRole", "edit")), "allowlist"},
		{"a RoleBinding to the built-in admin ClusterRole", plus(binding("RoleBinding", "kube-system", "kubehz-agent-admin", "ClusterRole", "admin")), "allowlist"},
		{"a ClusterRoleBinding to cluster-admin", plus(binding("ClusterRoleBinding", "", "kubehz-agent-admin", "ClusterRole", "cluster-admin")), "allowlist"},
		{"the marker Role bound in another namespace", plus(binding("RoleBinding", "default", "kubehz-agent-marker", "Role", "kubehz-agent-marker")), "allowlist"},
		{"a roleRef outside the RBAC API group", plus(foreignAPIGroup), "allowlist"},
		{"a Group subject next to the agent", markerSubjects(agentSubject, rbacSubject{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "system:authenticated"}), "subject"},
		{"a User subject next to the agent", markerSubjects(agentSubject, rbacSubject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "someone@example.com"}), "subject"},
		{"another ServiceAccount of kubehz-system", markerSubjects(rbacSubject{Kind: "ServiceAccount", Name: "default", Namespace: agentNamespace}), "subject"},
		{"the agent's name in another namespace", markerSubjects(rbacSubject{Kind: "ServiceAccount", Name: "kubehz-agent", Namespace: "default"}), "subject"},
		{"no subject", markerSubjects(), "subject"},
		{"an allowlisted roleRef whose Role the manifests do not define", slices.DeleteFunc(slices.Clone(base), func(d rbacDoc) bool {
			return d.Kind == "Role" && d.Metadata.Name == "kubehz-agent-marker"
		}), "defined"},
		{"a second binding of the marker Role", plus(binding("RoleBinding", agentNamespace, "kubehz-agent-marker-2", "Role", "kubehz-agent-marker")), "once"},
	} {
		problems := agentBindingProblems(tc.docs)
		if !slices.ContainsFunc(problems, func(p rbacProblem) bool { return p.rule == tc.want }) {
			t.Errorf("%s: no %q problem, got %+v", tc.name, tc.want, problems)
		}
	}
}

// configmapTarget returns the ConfigMap name and the namespace of a kubectl
// call, or "" when the call names no ConfigMap.
func configmapTarget(args []string) (name, ns string) {
	for i, a := range args {
		switch {
		case (a == "configmap" || a == "configmaps" || a == "cm") && i+1 < len(args):
			name = args[i+1]
		case strings.HasPrefix(a, "configmap/") || strings.HasPrefix(a, "configmaps/") || strings.HasPrefix(a, "cm/"):
			name = a[strings.Index(a, "/")+1:]
		case (a == "-n" || a == "--namespace") && i+1 < len(args):
			ns = args[i+1]
		case strings.HasPrefix(a, "--namespace="):
			ns = strings.TrimPrefix(a, "--namespace=")
		}
	}
	return name, ns
}

// TestAgentScriptMarkersUseTheStateConfigMap: the heartbeat script reads its
// markers from kubehz-agent-state once and writes them there with annotate
// (nine writers), and no kubectl call names kubehz-agent-config. No write is
// a dry run: a dry run stores nothing, so it cannot hold the 24h gate. The
// send gate is the one write of last-assessment with the time of this tick.
// It removes a pending assess request in the same call (one PATCH), and it
// runs before the assessment is put together, so before the send. The only
// other write of the 24h marker or of the request removal is the write-back
// of assess_write_back, one call that puts back both. The CronJob still gets
// kubehz-agent-config through envFrom.
func TestAgentScriptMarkersUseTheStateConfigMap(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	reads := 0
	var writes []kubectlCall
	for _, c := range kubectlCalls(script) {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, agentConfigName) {
			t.Errorf("script line %d: kubectl %s names %s: the agent has no access to it", c.line, joined, agentConfigName)
		}
		name, ns := configmapTarget(c.args)
		switch {
		case name == "":
			continue
		case name == "kubeadm-config" && ns == "kube-system":
			continue
		case name != agentStateName || ns != agentNamespace:
			t.Errorf("script line %d: kubectl %s: a ConfigMap other than %s/%s or kube-system/kubeadm-config", c.line, joined, agentNamespace, agentStateName)
			continue
		}
		switch c.args[0] {
		case "get":
			reads++
		case "annotate":
			writes = append(writes, c)
			if slices.ContainsFunc(c.args, func(a string) bool { return strings.HasPrefix(a, "--dry-run") }) {
				t.Errorf("script line %d: kubectl %s is a dry run: it stores nothing", c.line, joined)
			}
		default:
			t.Errorf("script line %d: kubectl %s: the agent may only get and annotate (patch) %s", c.line, joined, agentStateName)
		}
	}
	if reads != 1 || len(writes) != 9 {
		t.Errorf("%s: %d reads and %d annotate calls, want 1 and 9", agentStateName, reads, len(writes))
	}

	// The send gate and the write-back, by their arguments.
	const (
		gateTime  = `"kubehz.cloud/last-assessment=${NOW_EPOCH}"`
		gateReq   = `${GATE_REQ:+"${GATE_REQ}"}`
		backLast  = `"${WB_LAST}"`
		backReq   = `${WB_REQ:+"${WB_REQ}"}`
		overwrite = "--overwrite"
	)
	var gates, backs []kubectlCall
	for _, w := range writes {
		switch {
		case slices.Contains(w.args, gateTime):
			gates = append(gates, w)
		case slices.Contains(w.args, backLast):
			backs = append(backs, w)
		default:
			// Any other writer leaves the 24h marker and the request removal alone.
			for _, a := range w.args {
				if strings.Contains(a, "last-assessment") || strings.Contains(a, "assess-requested-") || strings.Contains(a, "GATE_REQ") || strings.Contains(a, "WB_") {
					t.Errorf("script line %d: kubectl %s writes %s: only the send gate and the write-back may", w.line, strings.Join(w.args, " "), a)
				}
			}
		}
	}
	if len(gates) != 1 || len(backs) != 1 {
		t.Fatalf("%d send gates and %d write-backs, want 1 and 1", len(gates), len(backs))
	}
	gate, back := gates[0], backs[0]
	if !slices.Contains(gate.args, overwrite) || !slices.Contains(gate.args, gateReq) ||
		!strings.Contains(script, `[ "${ASSESS_REQ}" != "true" ] || GATE_REQ="kubehz.cloud/assess-requested-"`) {
		t.Errorf("script line %d: the send gate kubectl %s must set the time and remove a pending request in one call", gate.line, strings.Join(gate.args, " "))
	}
	if !slices.Contains(back.args, overwrite) || !slices.Contains(back.args, backReq) ||
		!strings.Contains(script, `WB_LAST="kubehz.cloud/last-assessment-"`) ||
		!strings.Contains(script, `[ -z "${LAST_ASSESS}" ] || WB_LAST="kubehz.cloud/last-assessment=${LAST_ASSESS}"`) ||
		!strings.Contains(script, `[ "${ASSESS_REQ}" != "true" ] || WB_REQ="kubehz.cloud/assess-requested=true"`) {
		t.Errorf("script line %d: the write-back kubectl %s must put back the old time and the request in one call", back.line, strings.Join(back.args, " "))
	}
	if before, _, found := strings.Cut(script, "ASSESSMENT=$(bounded 5 jq "); !found || gate.line >= strings.Count(before, "\n")+1 {
		t.Errorf("script line %d: the send gate must run before the assessment is put together", gate.line)
	}

	raw, err := fs.ReadFile(Manifests(), "agent/cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cj struct {
		Spec struct {
			JobTemplate struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []struct {
								EnvFrom []struct {
									ConfigMapRef struct {
										Name string `yaml:"name"`
									} `yaml:"configMapRef"`
								} `yaml:"envFrom"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			} `yaml:"jobTemplate"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &cj); err != nil {
		t.Fatal(err)
	}
	cs := cj.Spec.JobTemplate.Spec.Template.Spec.Containers
	if len(cs) != 1 || len(cs[0].EnvFrom) != 1 || cs[0].EnvFrom[0].ConfigMapRef.Name != agentConfigName {
		t.Errorf("the CronJob container must get %s through envFrom: %+v", agentConfigName, cs)
	}
}

// TestAgentStateConfigMapIsDeployed: `lo kubehz deploy` applies the agent
// kustomization, which creates kubehz-agent-state empty. A redeploy keeps
// the annotations that the agent wrote. The manifest carries no annotations,
// data or binaryData: a marker in the manifest would be set back to its
// value at every apply.
func TestAgentStateConfigMapIsDeployed(t *testing.T) {
	t.Parallel()
	docs := agentDocs(t)
	kust := docs["agent/kustomization.yaml"]
	if len(kust) != 1 || !slices.Contains(kust[0].Resources, "state-configmap.yaml") {
		t.Errorf("agent/kustomization.yaml must list state-configmap.yaml: %+v", kust)
	}
	raw, err := fs.ReadFile(Manifests(), agentStateFile)
	if err != nil {
		t.Fatal(err)
	}
	var state []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("%s: %v", agentStateFile, err)
		}
		state = append(state, doc)
	}
	if len(state) != 1 {
		t.Fatalf("%s: %d documents, want 1", agentStateFile, len(state))
	}
	d := state[0]
	meta, _ := d["metadata"].(map[string]any)
	if d["kind"] != "ConfigMap" || meta["name"] != agentStateName || meta["namespace"] != agentNamespace {
		t.Errorf("%s: want the ConfigMap %s/%s, got %+v", agentStateFile, agentNamespace, agentStateName, d)
	}
	for _, k := range []string{"data", "binaryData"} {
		if _, ok := d[k]; ok {
			t.Errorf("%s carries %s: the state ConfigMap has no data", agentStateFile, k)
		}
	}
	if _, ok := meta["annotations"]; ok {
		t.Errorf("%s carries annotations: each apply would set those markers back", agentStateFile)
	}
	if keys := slices.Sorted(maps.Keys(d)); !slices.Equal(keys, []string{"apiVersion", "kind", "metadata"}) {
		t.Errorf("%s: top-level keys %v, want apiVersion, kind and metadata", agentStateFile, keys)
	}
	if keys := slices.Sorted(maps.Keys(meta)); !slices.Equal(keys, []string{"labels", "name", "namespace"}) {
		t.Errorf("%s: metadata keys %v, want labels, name and namespace", agentStateFile, keys)
	}
}

// TestAgentIdentitySecretIsDeployed: the agent kustomization creates the
// identity Secret kubehz-agent empty, so that the agent needs no create on
// Secrets. The manifest carries no data, stringData or annotations: a value
// in the manifest would be set back at every apply (a rotated identity),
// and an apply of a manifest without data keeps the values that the agent
// wrote. The type is Opaque, the type of the Secret that older agents
// created (`kubectl create secret generic`): a Secret's type cannot change,
// so another type would fail the apply on an upgrade.
func TestAgentIdentitySecretIsDeployed(t *testing.T) {
	t.Parallel()
	docs := agentDocs(t)
	kust := docs["agent/kustomization.yaml"]
	if len(kust) != 1 || !slices.Contains(kust[0].Resources, "identity-secret.yaml") {
		t.Errorf("agent/kustomization.yaml must list identity-secret.yaml: %+v", kust)
	}
	raw, err := fs.ReadFile(Manifests(), agentIdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	var secret []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("%s: %v", agentIdentityFile, err)
		}
		secret = append(secret, doc)
	}
	if len(secret) != 1 {
		t.Fatalf("%s: %d documents, want 1", agentIdentityFile, len(secret))
	}
	d := secret[0]
	meta, _ := d["metadata"].(map[string]any)
	if d["kind"] != "Secret" || meta["name"] != agentIdentitySName || meta["namespace"] != agentNamespace {
		t.Errorf("%s: want the Secret %s/%s, got %+v", agentIdentityFile, agentNamespace, agentIdentitySName, d)
	}
	if d["type"] != "Opaque" {
		t.Errorf("%s: type %v, want Opaque", agentIdentityFile, d["type"])
	}
	if keys := slices.Sorted(maps.Keys(d)); !slices.Equal(keys, []string{"apiVersion", "kind", "metadata", "type"}) {
		t.Errorf("%s: top-level keys %v, want apiVersion, kind, metadata and type (no data, no stringData)", agentIdentityFile, keys)
	}
	if keys := slices.Sorted(maps.Keys(meta)); !slices.Equal(keys, []string{"labels", "name", "namespace"}) {
		t.Errorf("%s: metadata keys %v, want labels, name and namespace (no annotations)", agentIdentityFile, keys)
	}
}

// TestAgentScriptSecretCallsAreGetAndPatchByName: each kubectl call of the
// heartbeat script on a Secret names kubehz-agent in kubehz-system and is a
// get or a patch, the two verbs of its Role. No call creates, applies,
// replaces or deletes a Secret. The one patch is the fill of the bootstrap:
// a merge patch from stdin (A and C never on argv) whose body carries the
// resourceVersion of the read, so that a second filler gets a 409 Conflict
// and never overwrites the first.
func TestAgentScriptSecretCallsAreGetAndPatchByName(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	gets, patches := 0, 0
	for _, c := range kubectlCalls(script) {
		joined := strings.Join(c.args, " ")
		if !slices.ContainsFunc(c.args, func(a string) bool {
			return a == "secret" || a == "secrets" || strings.HasPrefix(a, "secret/") || strings.HasPrefix(a, "secrets/")
		}) {
			continue
		}
		if !strings.HasPrefix(joined, "get secret "+agentIdentitySName+" -n "+agentNamespace) &&
			!strings.HasPrefix(joined, "patch secret "+agentIdentitySName+" -n "+agentNamespace) {
			t.Errorf("script line %d: kubectl %s: the agent may only get and patch Secret %s/%s", c.line, joined, agentNamespace, agentIdentitySName)
			continue
		}
		switch c.args[0] {
		case "get":
			gets++
		case "patch":
			patches++
			if joined != "patch secret "+agentIdentitySName+" -n "+agentNamespace+" --type=merge --patch-file=/dev/stdin" {
				t.Errorf("script line %d: kubectl %s: the fill is a merge patch from stdin", c.line, joined)
			}
		}
	}
	if gets != 3 || patches != 1 {
		t.Errorf("%d Secret reads and %d patches, want 3 and 1 (the state read, the two value reads, the fill)", gets, patches)
	}
	if !strings.Contains(script, `printf '{"metadata":{"resourceVersion":"%s"},"stringData":{"agent-token":"%s","claim-code":"%s"}}'`) {
		t.Error("the fill patch must carry the resourceVersion of the read (a 409 for a second filler)")
	}
	// The fill runs only for a Secret with no entry at all, whatever the
	// values (never rotate, never write over a key that is there): the
	// read gives no count for such a Secret, so ID_KEYS is empty.
	if !strings.Contains(script, "-o '"+agentIdentityRead+"'") {
		t.Errorf("the identity read must be -o '%s'", agentIdentityRead)
	}
	if !strings.Contains(script, "  case \"${ID_KEYS}\" in\n    '')\n      NEW_A=") ||
		!strings.Contains(script, "    unknown|*' AC') ;;\n    *)\n      echo \"kubehz: Secret kubehz-agent in kubehz-system has data, but not both") {
		t.Error("the fill must run only for a Secret with no entry (ID_KEYS empty), and a Secret with entries but not A and C gets one line and no write")
	}
}
