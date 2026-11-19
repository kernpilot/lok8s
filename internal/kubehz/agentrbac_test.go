package kubehz

// agentrbac_test.go: the heartbeat agent's RBAC grants what its script uses,
// and nothing that lets it change its own configuration. The agent reads its
// configuration (kubehz-agent-config: the API URL, the cluster id, the
// heartbeat owner) through envFrom, so it needs no access to it through the
// API. It keeps its markers (the 24h assessment gate, the claim nonce, the
// desired-state validator) as annotations on a separate ConfigMap,
// kubehz-agent-state, and that ConfigMap is the only object it can patch.
// Its bindings grant only its own roles, and only to its own ServiceAccount.
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
	agentConfigName = "kubehz-agent-config"
	agentStateFile  = "agent/state-configmap.yaml"
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

// TestAgentClusterRoleIsTheScriptSet pins the ClusterRole rule by rule: core
// nodes with get and list, and no namespaces or componentstatuses (the
// script reads neither).
func TestAgentClusterRoleIsTheScriptSet(t *testing.T) {
	t.Parallel()
	core := []string{""}
	gl := []string{"get", "list"}
	want := []rbacRule{
		{APIGroups: core, Resources: []string{"nodes"}, Verbs: gl},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: gl},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: gl},
		{APIGroups: core, Resources: []string{"persistentvolumes"}, Verbs: gl},
		{APIGroups: core, Resources: []string{"services"}, Verbs: []string{"list"}},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"}, Verbs: []string{"list"}},
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
//   - create on Secrets in kubehz-system, for the identity Secret. RBAC
//     cannot scope a create by name (an object has no name before it
//     exists), so this rule has no resourceNames.
var agentWriteAllowlist = []agentWriteRule{
	{"Role", agentNamespace, "kubehz-agent-marker", rbacRule{
		APIGroups: []string{""}, Resources: []string{"configmaps"},
		ResourceNames: []string{agentStateName}, Verbs: []string{"get", "patch"},
	}},
	{"Role", agentNamespace, "kubehz-agent-secret", rbacRule{
		APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"},
	}},
}

// TestAgentRBACWriteRulesAreTheAllowlist: each rule with a write verb is a
// rule of agentWriteAllowlist in its exact form (the role, the API groups,
// the resources, the names and the verbs), and each rule of the allowlist
// is found once. No rule names kubehz-agent-config, and every rule on
// ConfigMaps names its objects.
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
	// The agent's roles hold 14 rules. Fewer means the scan reads nothing.
	if rules < 14 {
		t.Fatalf("%d rules found in agent/, want at least 14", rules)
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
// (nine writers), and no kubectl call names kubehz-agent-config. The CronJob
// still gets kubehz-agent-config through envFrom.
func TestAgentScriptMarkersUseTheStateConfigMap(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	reads, writes := 0, 0
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
			writes++
		default:
			t.Errorf("script line %d: kubectl %s: the agent may only get and annotate (patch) %s", c.line, joined, agentStateName)
		}
	}
	if reads != 1 || writes != 9 {
		t.Errorf("%s: %d reads and %d annotate calls, want 1 and 9", agentStateName, reads, writes)
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
