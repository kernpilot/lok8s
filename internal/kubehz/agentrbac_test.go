package kubehz

// agentrbac_test.go: the heartbeat agent's RBAC grants what its script uses,
// and nothing that lets it change its own configuration. The agent reads its
// configuration (kubehz-agent-config: the API URL, the cluster id, the
// heartbeat owner) through envFrom, so it needs no access to it through the
// API. It keeps its markers (the 24h assessment gate, the claim nonce, the
// desired-state validator) as annotations on a separate ConfigMap,
// kubehz-agent-state, and that ConfigMap is the only object it can patch.
//
// The tests read the embedded manifests. TestEmbeddedManifestsMatchBashTree
// pins them to .lok8s/libs/kubehz/manifests byte for byte, and
// TestEmbeddedMirrorMatchesLegacyTree pins the asset mirror, so all three
// copies are checked.

import (
	"errors"
	"io"
	"io/fs"
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

type rbacDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules     []rbacRule        `yaml:"rules"`
	Data      map[string]string `yaml:"data"`
	Resources []string          `yaml:"resources"`
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

// writeVerbs are the verbs that change an object (or grant that).
var writeVerbs = []string{"patch", "update", "delete", "deletecollection", "*", "escalate", "bind", "impersonate"}

// TestAgentRBACPatchesOnlyTheStateConfigMap: the one rule with a write verb
// on an existing object is get and patch on the ConfigMap kubehz-agent-state
// in kubehz-system. No rule names kubehz-agent-config, and every rule on
// ConfigMaps names its objects.
func TestAgentRBACPatchesOnlyTheStateConfigMap(t *testing.T) {
	t.Parallel()
	writers := 0
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
			writers++
			ok := r.Kind == "Role" && r.Metadata.Namespace == agentNamespace &&
				reflect.DeepEqual(rule.APIGroups, []string{""}) &&
				reflect.DeepEqual(rule.Resources, []string{"configmaps"}) &&
				reflect.DeepEqual(rule.ResourceNames, []string{agentStateName}) &&
				reflect.DeepEqual(rule.Verbs, []string{"get", "patch"})
			if !ok {
				t.Errorf("%s %s/%s: a write rule other than get/patch on configmap %s/%s: %+v", r.Kind, r.Metadata.Namespace, r.Metadata.Name, agentNamespace, agentStateName, rule)
			}
		}
	}
	if writers != 1 {
		t.Errorf("%d rules with a write verb, want 1 (get/patch on configmap %s)", writers, agentStateName)
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
// the annotations that the agent wrote.
func TestAgentStateConfigMapIsDeployed(t *testing.T) {
	t.Parallel()
	docs := agentDocs(t)
	kust := docs["agent/kustomization.yaml"]
	if len(kust) != 1 || !slices.Contains(kust[0].Resources, "state-configmap.yaml") {
		t.Errorf("agent/kustomization.yaml must list state-configmap.yaml: %+v", kust)
	}
	state := docs[agentStateFile]
	if len(state) != 1 {
		t.Fatalf("%s: %d documents, want 1", agentStateFile, len(state))
	}
	d := state[0]
	if d.Kind != "ConfigMap" || d.Metadata.Name != agentStateName || d.Metadata.Namespace != agentNamespace || len(d.Data) != 0 {
		t.Errorf("%s: want an empty ConfigMap %s/%s, got %+v", agentStateFile, agentNamespace, agentStateName, d)
	}
}
