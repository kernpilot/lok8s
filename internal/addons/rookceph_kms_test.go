package addons

// rookceph_kms_test.go — the gate for the rook-ceph-csi-kms addon. The
// addon is a local chart with one `kms:` value per ceph-csi feature that
// reads objects through the Kubernetes API. Each value renders only the
// rules of its feature, bound only to the plugin that calls them.
//
// kmsCalls is an inventory of those calls. It is written by hand from the
// source of ceph-csi v3.17.0 (the plugin image of ceph-csi-operator
// v1.0.4, internal/controller/defaults.go:37: the operator the rook-ceph
// addon installs) and of Rook v1.20.6, each entry with its file:line. It
// is not a runtime proof: a call that the inventory misses is missed here
// too. A bump of the plugin image starts with a new read of the source.
//
// Every Secret read is one Get by name (ceph-csi
// internal/util/k8s/secrets.go:157-168). The Secret informer that would
// list and watch is in a blank function (secrets.go:177), so no option
// grants list or watch; only `all` keeps them, as earlier releases did.
//
// TestRookCephCSIKMSOptions renders the addon through Render, the render
// of the bootstrap: the entry's values over values.yaml, then kustomize
// and khelm (the chart is local, nothing is downloaded). It renders each
// option alone, sets of options, and the tenant options with
// tenantNamespaces, and checks each render:
//
//   - the objects are exactly the roles and bindings of the options (kind,
//     namespace, name);
//   - each object carries exactly the addon's labels (kmsBaseLabels) and
//     the option label of the option that renders it, and no annotation;
//     nothing aggregates (rbacCheckMetadata);
//   - each binding refers to a role of the render (a Role in its
//     namespace, or a ClusterRole), and its subjects are exactly the
//     plugins that make a call the role allows;
//   - each call of the options is allowed for its plugin, and each rule
//     (group, resource, verb, resource name) serves a call of each plugin
//     it is bound to, at the scope of its binding (rbacCheckRulesNeeded).
//     A rule of another option fails, and so does a Role turned into a
//     ClusterRole: a call in one known namespace never justifies a
//     cluster-wide rule. With tenantNamespaces, a call in the namespace of
//     the PVC is a call in each listed namespace, so a ClusterRoleBinding
//     fails too.
//
// `all` must render the objects of kmsAllFixture (the addon's rbac.yaml
// before the split) with the same names, rules and subjects, plus exactly
// the addon's labels and the option label. No option, an unknown option,
// a value of the wrong shape, a bad tenantNamespaces and `all` with
// tenantNamespaces must stop the render with the named error.

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	// kmsAllFixture is the addon's rbac.yaml before the per-feature split,
	// byte for byte under a header: what `kms: [all]` must render.
	kmsAllFixture = "testdata/rook-ceph/csi-kms-all.yaml"
	// The plugins, as the RBD ServiceAccount names spell them.
	kmsNode = "node"
	kmsCtrl = "ctrl"
	// kmsConfigFeature names the calls that every KMS type makes, and is
	// the option label of their Role.
	kmsConfigFeature = "kms-config"
	// kmsConfigRole is the Role (and RoleBinding) of kmsConfigFeature.
	kmsConfigRole = "rook-ceph-csi-kms-config"
	// kmsOptionLabel names the option that renders an object.
	kmsOptionLabel = "csi-kms.lok8s.dev/option"
	// kmsAllBinding is the ClusterRoleBinding of `all` (and of earlier
	// releases).
	kmsAllBinding = "rook-ceph-csi-nodeplugin-kms-crb"
)

// kmsBaseLabels are the labels of the addon's kustomization: every
// rendered object carries them. lok8s.dev/name selects the addon's objects
// for the cleanup before a change of the options.
var kmsBaseLabels = map[string]string{
	"lok8s.dev/name":     "rook-ceph-csi-kms",
	"lok8s.dev/type":     "system",
	"lok8s.dev/category": "storage",
}

// kmsTenantNS are the tenantNamespaces of the tenant cases.
var kmsTenantNS = []string{"team-a", "team-b"}

// kmsSA is the ServiceAccount of an RBD plugin that the rook-ceph addon
// creates (csi-drivers.yaml pins the driver name rook-ceph.rbd.csi.ceph.com).
func kmsSA(plugin string) string { return "rook-ceph-rbd-csi-ceph-com-" + plugin + "plugin-sa" }

// kmsOptions are the `kms:` values, in the order of the template's list
// (and so of its error messages).
var kmsOptions = []string{
	"all", "metadata", "vault", "vaulttokens", "vaulttenantsa", "aws-metadata", "aws-sts-metadata",
	"azure-kv", "kmip", "ibmkeyprotect", "vault-tokenreview", "rbd-nbd", "block-volume-stats",
}

// kmsTypes are the options that are ceph-csi KMS types (the
// encryptionKMSType of a KMS configuration); each one also reads the KMS
// ConfigMap (kmsConfigFeature).
var kmsTypes = []string{
	"metadata", "vault", "vaulttokens", "vaulttenantsa", "aws-metadata", "aws-sts-metadata",
	"azure-kv", "kmip", "ibmkeyprotect",
}

// kmsTenantOptions are the options whose calls are in the namespace of the
// PVC: tenantNamespaces binds their ClusterRole per namespace. The order is
// the template's (its error message lists them).
var kmsTenantOptions = []string{"metadata", "vaulttokens", "vaulttenantsa", "aws-sts-metadata"}

// kmsRoleSpec is the role of an option: a Role in the Rook namespace, or a
// ClusterRole.
type kmsRoleSpec struct{ kind, name string }

// kmsRoles are the roles of the options. `all` and `vault` have none of
// their own (kmsObjectsOf); every KMS type also renders the KMS ConfigMap
// Role (kmsConfigObjects).
var kmsRoles = map[string]kmsRoleSpec{
	"metadata":           {"ClusterRole", "rook-ceph-csi-kms-metadata"},
	"vaulttokens":        {"ClusterRole", "rook-ceph-csi-kms-vaulttokens"},
	"vaulttenantsa":      {"ClusterRole", "rook-ceph-csi-kms-vaulttenantsa"},
	"aws-metadata":       {"Role", "rook-ceph-csi-kms-aws-metadata"},
	"aws-sts-metadata":   {"ClusterRole", "rook-ceph-csi-kms-aws-sts-metadata"},
	"azure-kv":           {"Role", "rook-ceph-csi-kms-azure-kv"},
	"kmip":               {"Role", "rook-ceph-csi-kms-kmip"},
	"ibmkeyprotect":      {"Role", "rook-ceph-csi-kms-ibmkeyprotect"},
	"vault-tokenreview":  {"ClusterRole", "rook-ceph-csi-kms-vault-tokenreview"},
	"rbd-nbd":            {"Role", "rook-ceph-csi-kms-rbd-nbd"},
	"block-volume-stats": {"Role", "rook-ceph-csi-kms-block-volume-stats"},
}

// kmsPair is a role and its binding of the same name: a Role in the Rook
// namespace, or a ClusterRole.
func kmsPair(kind, name string) []string {
	if kind == "Role" {
		return []string{objKey("Role", rookCephNS, name), objKey("RoleBinding", rookCephNS, name)}
	}
	return []string{objKey("ClusterRole", "", name), objKey("ClusterRoleBinding", "", name)}
}

var kmsConfigObjects = kmsPair("Role", kmsConfigRole)

// kmsObjectsOf are the objects that one option renders, besides the KMS
// ConfigMap Role. A tenant option with tenantNamespaces renders its
// ClusterRole and one RoleBinding in each namespace, no ClusterRoleBinding.
func kmsObjectsOf(option string, tenantNS []string) []string {
	switch option {
	case "all":
		return []string{objKey("ClusterRole", "", kmsRole), objKey("ClusterRoleBinding", "", kmsAllBinding)}
	case "vault":
		return nil
	}
	r := kmsRoles[option]
	if r.kind == "Role" || !slices.Contains(kmsTenantOptions, option) || len(tenantNS) == 0 {
		return kmsPair(r.kind, r.name)
	}
	out := []string{objKey("ClusterRole", "", r.name)}
	for _, ns := range tenantNS {
		out = append(out, objKey("RoleBinding", ns, r.name))
	}
	return out
}

// kmsOwners maps each object that a set of options renders to the value
// of its option label.
func kmsOwners(opts, tenantNS []string) map[string]string {
	owners := map[string]string{}
	for _, o := range opts {
		for _, k := range kmsObjectsOf(o, tenantNS) {
			owners[k] = o
		}
	}
	if slices.ContainsFunc(opts, func(o string) bool { return slices.Contains(kmsTypes, o) }) {
		for _, k := range kmsConfigObjects {
			owners[k] = kmsConfigFeature
		}
	}
	return owners
}

// kmsWantKeys are the objects a set of options must render.
func kmsWantKeys(opts, tenantNS []string) []string {
	return sortedKeys(kmsOwners(opts, tenantNS))
}

// kmsCall is one Kubernetes API request of a feature; rbacCall.by is the
// plugin that makes it.
type kmsCall struct {
	feature string
	rbacCall
}

// kmsBoth is a call that both plugins make: a KMS runs on both sides, the
// controller plugin sets up the key at CreateVolume (ceph-csi
// internal/rbd/controllerserver.go:205), the node plugin opens it at
// NodeStageVolume (internal/rbd/nodeserver.go:228).
func kmsBoth(feature, group, resource, verb, ns, name, source string) []kmsCall {
	return []kmsCall{
		{feature, rbacCall{kmsNode, group, resource, verb, ns, name, source}},
		{feature, rbacCall{kmsCtrl, group, resource, verb, ns, name, source}},
	}
}

// kmsNodeOnly is a call that only the node plugin makes.
func kmsNodeOnly(feature, group, resource, verb, ns, name, source string) []kmsCall {
	return []kmsCall{{feature, rbacCall{kmsNode, group, resource, verb, ns, name, source}}}
}

// kmsCalls: ceph-csi v3.17.0 unless the source names Rook (v1.20.6). A
// name that the KMS configuration can change is rbacAnyName; a call in the
// namespace of the PVC is rbacAtAnyNS. The `vault` type makes no call of
// its own: it authenticates with the plugin's token file
// (internal/kms/vault.go:414) and takes the CA from the StorageClass secret
// (vault.go:309).
var kmsCalls = slices.Concat(
	// The KMS configuration, in the plugin's namespace (POD_NAMESPACE,
	// rook-ceph), when the Driver does not mount it as a file.
	kmsBoth(kmsConfigFeature, "", "configmaps", "get", rookCephNS, "csi-kms-connection-details",
		"internal/kms/kms.go:100,153-160 (POD_NAMESPACE kms.go:129; KMS_CONFIGMAP_NAME, unset by the operator, kms.go:141)"),
	kmsBoth("metadata", "", "secrets", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/secretskms.go:129,263 (secretNamespace, else the PVC namespace, :256-261)"),
	kmsBoth("vaulttokens", "", "secrets", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/vault_tokens.go:263,496 (the token Secret); :415,441,468 (the TLS Secrets)"),
	kmsBoth("vaulttokens", "", "configmaps", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/vault_tokens.go:347,552 (the tenant ConfigMap)"),
	kmsBoth("vaulttenantsa", "", "serviceaccounts", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/vault_sa.go:265"),
	kmsBoth("vaulttenantsa", "", "serviceaccounts/token", "create", rbacAtAnyNS, rbacAnyName,
		"internal/kms/vault_sa.go:318; internal/util/k8s/serviceaccounts.go:37"),
	kmsBoth("vaulttenantsa", "", "secrets", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/vault_sa.go:280 (when the TokenRequest fails); vault_sa.go:138, vault_tokens.go:415,441,468 (the TLS Secrets)"),
	kmsBoth("vaulttenantsa", "", "configmaps", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/vault_sa.go:174; vault_tokens.go:552 (the tenant ConfigMap)"),
	kmsBoth("aws-metadata", "", "secrets", "get", rookCephNS, "ceph-csi-aws-credentials",
		"internal/kms/aws_metadata.go:82,90,187"),
	kmsBoth("aws-sts-metadata", "", "secrets", "get", rbacAtAnyNS, rbacAnyName,
		"internal/kms/aws_sts_metadata.go:80,162"),
	kmsBoth("azure-kv", "", "secrets", "get", rookCephNS, "ceph-csi-azure-credentials",
		"internal/kms/azure_vault.go:77,85,186"),
	kmsBoth("kmip", "", "secrets", "get", rookCephNS, "ceph-csi-kmip-credentials",
		"internal/kms/kmip.go:109,117,392"),
	kmsBoth("ibmkeyprotect", "", "secrets", "get", rookCephNS, "ceph-csi-kp-credentials",
		"internal/kms/keyprotect.go:83,90,202"),
	// Vault, not ceph-csi, makes this request, with the node plugin's own
	// token when the Vault role has no token_reviewer_jwt. The controller
	// plugin keeps the rule in its chart role.
	kmsNodeOnly("vault-tokenreview", "authentication.k8s.io", "tokenreviews", "create", rbacAtCluster, rbacAnyName,
		"Vault Kubernetes auth (ceph-csi docs/rbd/deploy.md:376-381)"),
	kmsNodeOnly("rbd-nbd", "", "secrets", "get", rookCephNS, "rook-csi-rbd-node",
		"internal/rbd/rbd_healer.go:91-93,172; Rook pkg/operator/ceph/csi/secrets.go:44"),
	kmsNodeOnly("block-volume-stats", "", "secrets", "get", rookCephNS, "rook-csi-rbd-provisioner",
		"internal/rbd/nodeserver.go:1574,1587; Rook pkg/operator/ceph/csi/config.go:66-72, secrets.go:45"),
)

// kmsFeatures are the features a set of options turns on.
func kmsFeatures(opts []string) []string {
	out := slices.Clone(opts)
	if slices.ContainsFunc(opts, func(o string) bool { return slices.Contains(kmsTypes, o) }) {
		out = append(out, kmsConfigFeature)
	}
	return out
}

// kmsCallsOf returns the calls of the features of opts, by one plugin (""
// for both). With tenantNamespaces, a call in the namespace of the PVC is
// a call in each listed namespace.
func kmsCallsOf(opts []string, plugin string, tenantNS []string) []rbacCall {
	features := kmsFeatures(opts)
	var out []rbacCall
	for _, c := range kmsCalls {
		if !slices.Contains(features, c.feature) || (plugin != "" && c.by != plugin) {
			continue
		}
		if c.ns == rbacAtAnyNS && len(tenantNS) > 0 {
			for _, ns := range tenantNS {
				in := c.rbacCall
				in.ns = ns
				out = append(out, in)
			}
			continue
		}
		out = append(out, c.rbacCall)
	}
	return out
}

// kmsValues is the `values:` of a spec.bootstrap entry with these options
// and, when it is not empty, tenantNamespaces.
func kmsValues(t *testing.T, opts, tenantNS []string) string {
	t.Helper()
	v := map[string]any{"kms": opts}
	if len(tenantNS) > 0 {
		v["tenantNamespaces"] = tenantNS
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// requireKMSRender skips (fails on CI) when the core build would exec a
// kustomize or a khelm plugin that is not installed.
func requireKMSRender(t *testing.T, root string) {
	t.Helper()
	requireRenderToolchain(t, root, "the rook-ceph-csi-kms render gate")
	if render.InProcessAvailable() {
		return
	}
	plugin := filepath.Join(config.KustomizePluginHome(projectPaths(root)), "khelm.mgoltzsche.github.com", "v2", "chartrenderer", "ChartRenderer")
	if _, err := os.Stat(plugin); err != nil {
		msg := "khelm exec plugin not installed at " + plugin + " (b install): the rook-ceph-csi-kms render gate skipped"
		if os.Getenv("CI") != "" {
			t.Fatalf("%s; on CI the toolchain must be installed before the Go tests", msg)
		}
		t.Skip(msg)
	}
}

// renderKMS renders the addon with these entry values (inline "" = none),
// the way the bootstrap renders it. On a failed render it returns the
// error text and the render's stderr.
func renderKMS(t *testing.T, root, inline string) (map[string]object, string, error) {
	t.Helper()
	var stderr strings.Builder
	out, err := Render(context.Background(), projectPaths(root), nil, &stderr, filepath.Join(root, rookCephKMSAddon), "", "", inline, nil)
	if err != nil {
		return nil, err.Error() + "\n" + stderr.String(), err
	}
	return decodeObjects(t, []byte(out)), "", nil
}

// kmsSubjectPlugins returns the plugins a binding names; any other subject
// fails the test.
func kmsSubjectPlugins(t *testing.T, b object) []string {
	t.Helper()
	var plugins []string
	for _, s := range subjectNames(t, b) {
		switch s {
		case kmsSA(kmsNode):
			plugins = append(plugins, kmsNode)
		case kmsSA(kmsCtrl):
			plugins = append(plugins, kmsCtrl)
		default:
			t.Errorf("%s/%s: subject %s is not an RBD plugin ServiceAccount", b.Kind, b.Name, s)
		}
	}
	sort.Strings(plugins)
	return plugins
}

// kmsBindingGrant resolves a binding of the render to its role, as the
// binding applies it: a ClusterRoleBinding of a ClusterRole in every
// namespace, a RoleBinding of a Role or of a ClusterRole in its own
// namespace. It reports false for any other pair, or a role that is not
// in the render.
func kmsBindingGrant(objs map[string]object, b object) (rbacGrant, bool) {
	ref, _ := b.Doc["roleRef"].(map[string]any)
	kind, _ := ref["kind"].(string)
	name, _ := ref["name"].(string)
	var key, ns string
	switch {
	case b.Kind == "ClusterRoleBinding" && kind == "ClusterRole":
		key = objKey("ClusterRole", "", name)
	case b.Kind == "RoleBinding" && kind == "Role":
		key, ns = objKey("Role", b.Namespace, name), b.Namespace
	case b.Kind == "RoleBinding" && kind == "ClusterRole":
		key, ns = objKey("ClusterRole", "", name), b.Namespace
	default:
		return rbacGrant{}, false
	}
	role, ok := objs[key]
	return rbacGrant{role, ns}, ok
}

// checkKMSMetadata asserts that each object carries exactly the addon's
// labels and the option label of its owner (extra holds labels that the
// object may carry on top: the fixture's), no annotation, and nothing that
// aggregates.
func checkKMSMetadata(t *testing.T, owners map[string]string, objs map[string]object, extra map[string]map[string]string) {
	t.Helper()
	for _, k := range sortedKeys(objs) {
		o := objs[k]
		want := maps.Clone(extra[k])
		if want == nil {
			want = map[string]string{}
		}
		maps.Copy(want, kmsBaseLabels)
		want[kmsOptionLabel] = owners[k]
		if !maps.Equal(o.Labels, want) {
			t.Errorf("%s: labels %v, want %v", k, o.Labels, want)
		}
		if len(o.Annotations) > 0 {
			t.Errorf("%s: annotations %v, want none", k, o.Annotations)
		}
		rbacCheckMetadata(t, o)
	}
}

// checkKMSRender checks one render of a set of options (not `all`) with
// these tenantNamespaces against kmsOwners and kmsCalls.
func checkKMSRender(t *testing.T, opts, tenantNS []string, objs map[string]object) {
	t.Helper()
	owners := kmsOwners(opts, tenantNS)
	if got, want := sortedKeys(objs), sortedKeys(owners); !reflect.DeepEqual(got, want) {
		t.Errorf("kms %v tenantNamespaces %v renders\n%v\nwant\n%v", opts, tenantNS, got, want)
	}
	checkKMSMetadata(t, owners, objs, nil)
	grantsOf := map[string][]rbacGrant{}
	for _, k := range sortedKeys(objs) {
		b := objs[k]
		switch b.Kind {
		case "Role", "ClusterRole":
			rbacCheckNoWildcards(t, b)
			continue
		case "RoleBinding", "ClusterRoleBinding":
		default:
			t.Errorf("%s: not an RBAC object", k)
			continue
		}
		g, ok := kmsBindingGrant(objs, b)
		if !ok {
			t.Errorf("%s binds %v, which is not a role of the render that this kind of binding can bind", k, b.Doc["roleRef"])
			continue
		}
		plugins := kmsSubjectPlugins(t, b)
		var want []string
		for _, p := range []string{kmsCtrl, kmsNode} {
			if slices.ContainsFunc(kmsCallsOf(opts, p, tenantNS), g.allows) {
				want = append(want, p)
			}
		}
		if !reflect.DeepEqual(plugins, want) {
			t.Errorf("%s binds %v, want the plugins whose calls %s/%s serves: %v", k, plugins, g.role.Kind, g.role.Name, want)
		}
		for _, p := range plugins {
			rbacCheckRulesNeeded(t, g, kmsCallsOf(opts, p, tenantNS))
			grantsOf[p] = append(grantsOf[p], g)
		}
	}
	for _, p := range []string{kmsNode, kmsCtrl} {
		for _, c := range kmsCallsOf(opts, p, tenantNS) {
			if !slices.ContainsFunc(grantsOf[p], func(g rbacGrant) bool { return g.allows(c) }) {
				t.Errorf("kms %v: the %s plugin may not %s %q/%s name %q in %q (%s)", opts, p, c.verb, c.group, c.resource, c.name, c.ns, c.source)
			}
		}
	}
}

// checkKMSAllUnion asserts that a render of `all` and other options grants
// what `all` grants and nothing more: the objects of `all` are there as
// `all` renders them, and every rule atom of every other binding, at the
// scope of that binding, is one that the ClusterRole of `all` allows to
// the same plugins.
func checkKMSAllUnion(t *testing.T, objs, all map[string]object) {
	t.Helper()
	for k, o := range all {
		if got, ok := objs[k]; !ok || !reflect.DeepEqual(got.Doc, o.Doc) {
			t.Errorf("%s: not as `all` renders it alone", k)
		}
	}
	allGrant := rbacGrant{all[objKey("ClusterRole", "", kmsRole)], ""}
	allPlugins := kmsSubjectPlugins(t, all[objKey("ClusterRoleBinding", "", kmsAllBinding)])
	for _, k := range sortedKeys(objs) {
		b := objs[k]
		if b.Kind != "RoleBinding" && b.Kind != "ClusterRoleBinding" {
			continue
		}
		g, ok := kmsBindingGrant(objs, b)
		if !ok {
			t.Errorf("%s binds %v, which is not a role of the render", k, b.Doc["roleRef"])
			continue
		}
		for _, p := range kmsSubjectPlugins(t, b) {
			if !slices.Contains(allPlugins, p) {
				t.Errorf("%s binds the %s plugin, which `all` does not bind", k, p)
			}
		}
		ns := g.ns
		if ns == "" {
			ns = rbacAtCluster
		}
		for _, r := range g.role.Rules() {
			names := stringsOf(r["resourceNames"])
			if len(names) == 0 {
				names = []string{rbacAnyName}
			}
			for _, grp := range stringsOf(r["apiGroups"]) {
				for _, res := range stringsOf(r["resources"]) {
					for _, verb := range stringsOf(r["verbs"]) {
						for _, name := range names {
							c := rbacCall{group: grp, resource: res, verb: verb, ns: ns, name: name}
							if !allGrant.allows(c) {
								t.Errorf("%s grants %s %q/%s name %q in %q, which `all` does not grant", k, verb, grp, res, name, ns)
							}
						}
					}
				}
			}
		}
	}
}

// kmsTemplateList reads a `{{- $<name> := list ... }}` line of the chart
// template.
func kmsTemplateList(t *testing.T, root, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rookCephKMSAddon, "chart", "templates", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\{\{- \$` + regexp.QuoteMeta(name) + ` := list ((?:"[a-z-]+" ?)+)\}\}`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("chart/templates/rbac.yaml: no `{{- $%s := list ... }}` line", name)
	}
	return strings.Fields(strings.ReplaceAll(string(m[1]), `"`, ""))
}

// TestRookCephCSIKMSAddonShape runs without the render toolchain: the
// addon is the local chart with an empty `kms:` default and no
// tenantNamespaces default, its kustomization carries kmsBaseLabels, the
// template knows exactly the options and tenant options of this test, and
// every option has its role and calls here.
func TestRookCephCSIKMSAddonShape(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, rookCephKMSAddon)

	if got := kmsTemplateList(t, root, "options"); !reflect.DeepEqual(got, kmsOptions) {
		t.Errorf("the template's options %v, the test's %v", got, kmsOptions)
	}
	if got := kmsTemplateList(t, root, "types"); !reflect.DeepEqual(got, kmsTypes) {
		t.Errorf("the template's KMS types %v, the test's %v", got, kmsTypes)
	}
	if got := kmsTemplateList(t, root, "tenantOptions"); !reflect.DeepEqual(got, kmsTenantOptions) {
		t.Errorf("the template's tenant options %v, the test's %v", got, kmsTenantOptions)
	}
	for _, o := range kmsOptions {
		if o == "all" || o == "vault" {
			continue
		}
		if _, ok := kmsRoles[o]; !ok {
			t.Errorf("option %s has no entry in kmsRoles", o)
		}
		if !slices.ContainsFunc(kmsCalls, func(c kmsCall) bool { return c.feature == o }) {
			t.Errorf("option %s has no call in kmsCalls", o)
		}
	}
	// A tenant option is exactly an option with a call in the namespace of
	// the PVC, and its role is a ClusterRole.
	for _, o := range kmsOptions {
		tenant := slices.ContainsFunc(kmsCalls, func(c kmsCall) bool { return c.feature == o && c.ns == rbacAtAnyNS })
		if tenant != slices.Contains(kmsTenantOptions, o) {
			t.Errorf("option %s: a call in the PVC namespace %v, a tenant option %v", o, tenant, !tenant)
		}
		if tenant && kmsRoles[o].kind != "ClusterRole" {
			t.Errorf("tenant option %s renders a %s", o, kmsRoles[o].kind)
		}
	}

	raw, err := os.ReadFile(filepath.Join(addon, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kz struct {
		Generators []string `yaml:"generators"`
		Resources  []string `yaml:"resources"`
		Labels     []struct {
			IncludeSelectors bool              `yaml:"includeSelectors"`
			Pairs            map[string]string `yaml:"pairs"`
		} `yaml:"labels"`
	}
	if err := yaml.Unmarshal(raw, &kz); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kz.Generators, []string{"chart.yaml"}) || len(kz.Resources) != 0 {
		t.Errorf("kustomization.yaml: generators %v resources %v, want the chart generator alone", kz.Generators, kz.Resources)
	}
	if len(kz.Labels) != 1 || kz.Labels[0].IncludeSelectors || !maps.Equal(kz.Labels[0].Pairs, kmsBaseLabels) {
		t.Errorf("kustomization.yaml: labels %+v, want one entry %v without selectors", kz.Labels, kmsBaseLabels)
	}
	raw, err = os.ReadFile(filepath.Join(addon, "chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Kind       string   `yaml:"kind"`
		Chart      string   `yaml:"chart"`
		Repository string   `yaml:"repository"`
		ValueFiles []string `yaml:"valueFiles"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "ChartRenderer" || spec.Chart != "./chart" || spec.Repository != "" || !reflect.DeepEqual(spec.ValueFiles, []string{"values.yaml"}) {
		t.Errorf("chart.yaml = %+v, want the local ./chart with values.yaml", spec)
	}
	for _, f := range []string{"values.yaml", "chart/values.yaml"} {
		raw, err := os.ReadFile(filepath.Join(addon, f))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if kms, ok := v["kms"].([]any); !ok || len(kms) != 0 || len(v) != 1 {
			t.Errorf("%s = %v, want only `kms: []` (no tenantNamespaces default)", f, v)
		}
	}
	if _, err := os.Stat(filepath.Join(addon, "rbac.yaml")); err == nil {
		t.Errorf("rbac.yaml is back: the chart renders the roles; `all` keeps the old set")
	}
}

func TestRookCephCSIKMSOptions(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	requireKMSRender(t, root)

	// Each option alone.
	singles := map[string]map[string]object{}
	for _, o := range kmsOptions {
		t.Run("option="+o, func(t *testing.T) {
			objs, msg, err := renderKMS(t, root, kmsValues(t, []string{o}, nil))
			if err != nil {
				t.Fatalf("kms [%s]: %s", o, msg)
			}
			singles[o] = objs
			if o != "all" {
				checkKMSRender(t, []string{o}, nil, objs)
				return
			}
			// `all`: the objects of the earlier rbac.yaml, with the same
			// names, rules and subjects; on top of its (empty) labels
			// exactly the addon's labels and the option label.
			fixture := readObjects(t, kmsAllFixture)
			if got, want := sortedKeys(objs), sortedKeys(fixture); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, kmsWantKeys([]string{o}, nil)) {
				t.Fatalf("kms [all] renders %v, %s has %v", got, kmsAllFixture, want)
			}
			extra := map[string]map[string]string{}
			for k, f := range fixture {
				if !reflect.DeepEqual(objs[k].Doc, f.Doc) {
					t.Errorf("kms [all]: %s\n got: %s\nwant (%s): %s", k, canon(t, objs[k].Doc), kmsAllFixture, canon(t, f.Doc))
				}
				if len(f.Annotations) > 0 {
					t.Errorf("%s: %s has annotations %v", kmsAllFixture, k, f.Annotations)
				}
				extra[k] = f.Labels
			}
			checkKMSMetadata(t, kmsOwners([]string{o}, nil), objs, extra)
		})
	}

	// Sets of options render the union of their objects, each as the
	// option renders it alone.
	for _, opts := range [][]string{
		{"metadata", "rbd-nbd"},
		{"vaulttenantsa", "aws-metadata", "block-volume-stats", "vault-tokenreview"},
		{"vault", "vaulttokens", "kmip"},
	} {
		t.Run("options="+strings.Join(opts, ","), func(t *testing.T) {
			objs, msg, err := renderKMS(t, root, kmsValues(t, opts, nil))
			if err != nil {
				t.Fatalf("kms %v: %s", opts, msg)
			}
			checkKMSRender(t, opts, nil, objs)
			for k, o := range objs {
				owner := slices.IndexFunc(opts, func(opt string) bool { return slices.Contains(kmsObjectsOf(opt, nil), k) })
				var alone object
				if owner >= 0 {
					alone = singles[opts[owner]][k]
				} else {
					alone = singles["vault"][k] // the KMS ConfigMap Role
				}
				if !reflect.DeepEqual(o.Doc, alone.Doc) || !maps.Equal(o.Labels, alone.Labels) {
					t.Errorf("kms %v: %s\n got: %s %v\nalone: %s %v", opts, k, canon(t, o.Doc), o.Labels, canon(t, alone.Doc), alone.Labels)
				}
			}
		})
	}

	// tenantNamespaces: each tenant option binds its ClusterRole with one
	// RoleBinding in each listed namespace, and with no ClusterRoleBinding.
	// The ClusterRole is the one of the option alone.
	for _, o := range kmsTenantOptions {
		t.Run("tenant="+o, func(t *testing.T) {
			objs, msg, err := renderKMS(t, root, kmsValues(t, []string{o}, kmsTenantNS))
			if err != nil {
				t.Fatalf("kms [%s] tenantNamespaces %v: %s", o, kmsTenantNS, msg)
			}
			checkKMSRender(t, []string{o}, kmsTenantNS, objs)
			name := kmsRoles[o].name
			var bindingNS []string
			for _, k := range sortedKeys(objs) {
				b := objs[k]
				switch b.Kind {
				case "ClusterRoleBinding":
					t.Errorf("%s: a ClusterRoleBinding with tenantNamespaces", k)
				case "RoleBinding":
					if b.Name != name {
						continue
					}
					bindingNS = append(bindingNS, b.Namespace)
					if ref, _ := b.Doc["roleRef"].(map[string]any); ref["kind"] != "ClusterRole" || ref["name"] != name {
						t.Errorf("%s: roleRef %v, want ClusterRole %s", k, ref, name)
					}
					if got, want := subjectNames(t, b), []string{kmsSA(kmsCtrl), kmsSA(kmsNode)}; !reflect.DeepEqual(got, want) {
						t.Errorf("%s: subjects %v, want %v", k, got, want)
					}
				}
			}
			if !reflect.DeepEqual(bindingNS, kmsTenantNS) {
				t.Errorf("RoleBindings of %s in %v, want %v", name, bindingNS, kmsTenantNS)
			}
			role := objKey("ClusterRole", "", name)
			if !reflect.DeepEqual(objs[role].Doc, singles[o][role].Doc) || !maps.Equal(objs[role].Labels, singles[o][role].Labels) {
				t.Errorf("%s with tenantNamespaces: %s, alone: %s", role, canon(t, objs[role].Doc), canon(t, singles[o][role].Doc))
			}
		})
	}
	t.Run("tenant=metadata,vaulttenantsa,rbd-nbd,vault-tokenreview", func(t *testing.T) {
		opts := []string{"metadata", "vaulttenantsa", "rbd-nbd", "vault-tokenreview"}
		objs, msg, err := renderKMS(t, root, kmsValues(t, opts, kmsTenantNS))
		if err != nil {
			t.Fatalf("kms %v tenantNamespaces %v: %s", opts, kmsTenantNS, msg)
		}
		checkKMSRender(t, opts, kmsTenantNS, objs)
	})
	// A namespace that comes twice is bound once.
	t.Run("tenant=duplicates", func(t *testing.T) {
		once, msg, err := renderKMS(t, root, kmsValues(t, []string{"metadata"}, kmsTenantNS))
		if err != nil {
			t.Fatalf("%s", msg)
		}
		twice, msg, err := renderKMS(t, root, "kms: [metadata]\ntenantNamespaces: [team-a, team-b, team-a]")
		if err != nil {
			t.Fatalf("%s", msg)
		}
		if !reflect.DeepEqual(sortedKeys(twice), sortedKeys(once)) {
			t.Errorf("tenantNamespaces [team-a, team-b, team-a] renders %v, [team-a, team-b] renders %v", sortedKeys(twice), sortedKeys(once))
		}
		checkKMSRender(t, []string{"metadata"}, kmsTenantNS, twice)
	})

	// Shapes of kms that render: an option that comes twice renders as
	// once; `all` with another option renders the objects of both (the
	// KMS ConfigMap Role too), with no authority beyond `all`.
	t.Run("value=duplicates", func(t *testing.T) {
		objs, msg, err := renderKMS(t, root, "kms: [metadata, metadata]")
		if err != nil {
			t.Fatalf("%s", msg)
		}
		checkKMSRender(t, []string{"metadata"}, nil, objs)
		for k, o := range singles["metadata"] {
			if !reflect.DeepEqual(objs[k].Doc, o.Doc) || !maps.Equal(objs[k].Labels, o.Labels) {
				t.Errorf("kms [metadata, metadata]: %s differs from kms [metadata]", k)
			}
		}
	})
	t.Run("value=all_and_metadata", func(t *testing.T) {
		opts := []string{"all", "metadata"}
		objs, msg, err := renderKMS(t, root, kmsValues(t, opts, nil))
		if err != nil {
			t.Fatalf("%s", msg)
		}
		owners := kmsOwners(opts, nil)
		if got, want := sortedKeys(objs), sortedKeys(owners); !reflect.DeepEqual(got, want) {
			t.Errorf("kms %v renders\n%v\nwant\n%v", opts, got, want)
		}
		checkKMSMetadata(t, owners, objs, nil)
		checkKMSAllUnion(t, objs, singles["all"])
	})

	// No option stops the render; so do an unknown option, a value of the
	// wrong shape, a bad tenantNamespaces, `all` with tenantNamespaces, and
	// tenantNamespaces without a tenant option. A bad namespace is named by
	// its position, never by its value (absent).
	noOption := "rook-ceph-csi-kms: no kms option is set. Set `values: {kms: [...]}` on its spec.bootstrap entry"
	listed := "The options: " + strings.Join(kmsOptions, ", ") + ". `all` renders the rule set of earlier releases"
	options := "The options: " + strings.Join(kmsOptions, ", ")
	notList := "rook-ceph-csi-kms: kms must be a list, got "
	tnKind := "rook-ceph-csi-kms: tenantNamespaces must be a list of namespace names; the value is of kind "
	tnName := "] is not a namespace name (a DNS-1123 label: at most 63 lowercase letters, digits or '-', with a letter or digit at each end)"
	long := strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		inline string
		want   []string
		absent string
	}{
		"no values":                  {"", []string{noOption, listed}, ""},
		"empty list":                 {"kms: []", []string{noOption, listed}, ""},
		"null":                       {"kms: null", []string{noOption, listed}, ""},
		"unknown":                    {"kms: [metadata, vault-tenant-sa]", []string{`unknown kms option "vault-tenant-sa". ` + options}, ""},
		"not a list":                 {"kms: metadata", []string{notList + "string metadata. " + options}, ""},
		"map":                        {"kms: {metadata: true}", []string{notList + "map map[metadata:true]. " + options}, ""},
		"string with commas":         {`kms: "metadata,rbd-nbd"`, []string{notList + "string metadata,rbd-nbd. " + options}, ""},
		"nested list":                {"kms: [[metadata]]", []string{`unknown kms option "[metadata]". ` + options}, ""},
		"number":                     {"kms: [metadata, 7]", []string{`unknown kms option "7". ` + options}, ""},
		"tenant scalar":              {"kms: [metadata]\ntenantNamespaces: team-a", []string{tnKind + "string. Remove the key"}, ""},
		"tenant map":                 {"kms: [metadata]\ntenantNamespaces: {team-a: true}", []string{tnKind + "map. Remove the key"}, ""},
		"tenant null":                {"kms: [metadata]\ntenantNamespaces: null", []string{tnKind + "null. Remove the key"}, ""},
		"tenant empty list":          {"kms: [metadata]\ntenantNamespaces: []", []string{"rook-ceph-csi-kms: tenantNamespaces is an empty list. List the namespaces of the PVCs and of the KMS Secrets, or remove the key"}, ""},
		"tenant bad name":            {"kms: [metadata]\ntenantNamespaces: [team-a, Bad_Tenant]", []string{"rook-ceph-csi-kms: tenantNamespaces[1" + tnName}, "Bad_Tenant"},
		"tenant leading dash":        {"kms: [metadata]\ntenantNamespaces: [-team]", []string{"rook-ceph-csi-kms: tenantNamespaces[0" + tnName}, "-team"},
		"tenant too long":            {"kms: [metadata]\ntenantNamespaces: [" + long + "]", []string{"rook-ceph-csi-kms: tenantNamespaces[0" + tnName}, long},
		"tenant number":              {"kms: [metadata]\ntenantNamespaces: [team-a, 7]", []string{"rook-ceph-csi-kms: tenantNamespaces[1" + tnName}, ""},
		"tenant nested list":         {"kms: [metadata]\ntenantNamespaces: [[team-a]]", []string{"rook-ceph-csi-kms: tenantNamespaces[0" + tnName}, ""},
		"all with tenant":            {"kms: [all]\ntenantNamespaces: [team-a]", []string{"rook-ceph-csi-kms: `all` renders the rule set of earlier releases, bound in every namespace, and takes no tenantNamespaces"}, ""},
		"all and option with tenant": {"kms: [metadata, all]\ntenantNamespaces: [team-a]", []string{"takes no tenantNamespaces"}, ""},
		"tenant without option":      {"kms: [rbd-nbd, kmip]\ntenantNamespaces: [team-a]", []string{"rook-ceph-csi-kms: tenantNamespaces is set, but no kms option grants in the namespace of a PVC (" + strings.Join(kmsTenantOptions, ", ") + "). Remove tenantNamespaces"}, ""},
	} {
		t.Run("error="+strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			objs, msg, err := renderKMS(t, root, tc.inline)
			if err == nil {
				t.Fatalf("%q rendered %v, want an error", tc.inline, sortedKeys(objs))
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("%q: the error lacks %q:\n%s", tc.inline, w, msg)
				}
			}
			if tc.absent != "" && strings.Contains(msg, tc.absent) {
				t.Errorf("%q: the error shows the value %q:\n%s", tc.inline, tc.absent, msg)
			}
		})
	}
}
