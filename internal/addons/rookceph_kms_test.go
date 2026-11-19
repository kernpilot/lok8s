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
// option alone and sets of options, and checks each render:
//
//   - the objects are exactly the roles and bindings of the options (kind,
//     namespace, name);
//   - each binding refers to a role of the render with the same kind and
//     namespace, and its subjects are exactly the plugins that make a call
//     the role allows;
//   - each call of the options is allowed for its plugin, and each rule
//     (group, resource, verb, resource name) serves a call of each plugin
//     it is bound to, at the scope of its binding (rbacCheckRulesNeeded).
//     A rule of another option fails, and so does a Role turned into a
//     ClusterRole: a call in one known namespace never justifies a
//     cluster-wide rule.
//
// `all` must render the objects of kmsAllFixture (the addon's rbac.yaml
// before the split) exactly. No option, an unknown option and a `kms:`
// that is not a list must stop the render with the named error.

import (
	"context"
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
	// kmsConfigFeature names the calls that every KMS type makes.
	kmsConfigFeature = "kms-config"
	// kmsConfigRole is the Role (and RoleBinding) of kmsConfigFeature.
	kmsConfigRole = "rook-ceph-csi-kms-config"
)

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

// kmsPair is a role and its binding of the same name: a Role in the Rook
// namespace, or a ClusterRole.
func kmsPair(kind, name string) []string {
	if kind == "Role" {
		return []string{objKey("Role", rookCephNS, name), objKey("RoleBinding", rookCephNS, name)}
	}
	return []string{objKey("ClusterRole", "", name), objKey("ClusterRoleBinding", "", name)}
}

// kmsObjects are the objects each option renders, besides the KMS
// ConfigMap Role that the KMS types share (kmsConfigObjects).
var kmsObjects = map[string][]string{
	"all": {
		objKey("ClusterRole", "", kmsRole),
		objKey("ClusterRoleBinding", "", "rook-ceph-csi-nodeplugin-kms-crb"),
	},
	"metadata":           kmsPair("ClusterRole", "rook-ceph-csi-kms-metadata"),
	"vault":              nil,
	"vaulttokens":        kmsPair("ClusterRole", "rook-ceph-csi-kms-vaulttokens"),
	"vaulttenantsa":      kmsPair("ClusterRole", "rook-ceph-csi-kms-vaulttenantsa"),
	"aws-metadata":       kmsPair("Role", "rook-ceph-csi-kms-aws-metadata"),
	"aws-sts-metadata":   kmsPair("ClusterRole", "rook-ceph-csi-kms-aws-sts-metadata"),
	"azure-kv":           kmsPair("Role", "rook-ceph-csi-kms-azure-kv"),
	"kmip":               kmsPair("Role", "rook-ceph-csi-kms-kmip"),
	"ibmkeyprotect":      kmsPair("Role", "rook-ceph-csi-kms-ibmkeyprotect"),
	"vault-tokenreview":  kmsPair("ClusterRole", "rook-ceph-csi-kms-vault-tokenreview"),
	"rbd-nbd":            kmsPair("Role", "rook-ceph-csi-kms-rbd-nbd"),
	"block-volume-stats": kmsPair("Role", "rook-ceph-csi-kms-block-volume-stats"),
}

var kmsConfigObjects = kmsPair("Role", kmsConfigRole)

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
// for both).
func kmsCallsOf(opts []string, plugin string) []rbacCall {
	features := kmsFeatures(opts)
	var out []rbacCall
	for _, c := range kmsCalls {
		if slices.Contains(features, c.feature) && (plugin == "" || c.by == plugin) {
			out = append(out, c.rbacCall)
		}
	}
	return out
}

// kmsWantKeys are the objects a set of options must render.
func kmsWantKeys(opts []string) []string {
	var want []string
	for _, o := range opts {
		want = append(want, kmsObjects[o]...)
	}
	if slices.ContainsFunc(opts, func(o string) bool { return slices.Contains(kmsTypes, o) }) {
		want = append(want, kmsConfigObjects...)
	}
	sort.Strings(want)
	return slices.Compact(want)
}

// kmsInline is the `values:` of a spec.bootstrap entry with these options.
func kmsInline(t *testing.T, opts []string) string {
	t.Helper()
	out, err := yaml.Marshal(map[string]any{"kms": opts})
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

// checkKMSRender checks one render of a set of options (not `all`) against
// kmsObjects and kmsCalls.
func checkKMSRender(t *testing.T, opts []string, objs map[string]object) {
	t.Helper()
	if got, want := sortedKeys(objs), kmsWantKeys(opts); !reflect.DeepEqual(got, want) {
		t.Errorf("kms %v renders\n%v\nwant\n%v", opts, got, want)
	}
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
		ref, _ := b.Doc["roleRef"].(map[string]any)
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		ns := b.Namespace
		if kind == "ClusterRole" {
			ns = ""
		}
		role, ok := objs[objKey(kind, ns, name)]
		if !ok || b.Kind != kind+"Binding" {
			t.Errorf("%s binds %s/%s, which is not a role of the same scope in the render", k, kind, name)
			continue
		}
		g := rbacGrant{role, ns}
		plugins := kmsSubjectPlugins(t, b)
		var want []string
		for _, p := range []string{kmsCtrl, kmsNode} {
			if slices.ContainsFunc(kmsCallsOf(opts, p), g.allows) {
				want = append(want, p)
			}
		}
		if !reflect.DeepEqual(plugins, want) {
			t.Errorf("%s binds %v, want the plugins whose calls %s/%s serves: %v", k, plugins, kind, name, want)
		}
		for _, p := range plugins {
			rbacCheckRulesNeeded(t, g, kmsCallsOf(opts, p))
			grantsOf[p] = append(grantsOf[p], g)
		}
	}
	for _, p := range []string{kmsNode, kmsCtrl} {
		for _, c := range kmsCallsOf(opts, p) {
			if !slices.ContainsFunc(grantsOf[p], func(g rbacGrant) bool { return g.allows(c) }) {
				t.Errorf("kms %v: the %s plugin may not %s %q/%s name %q in %q (%s)", opts, p, c.verb, c.group, c.resource, c.name, c.ns, c.source)
			}
		}
	}
}

// kmsTemplateOptions reads the `$options` list of the chart template.
var kmsTemplateOptionsRE = regexp.MustCompile(`\{\{- \$options := list ((?:"[a-z-]+" ?)+)\}\}`)

func kmsTemplateOptions(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rookCephKMSAddon, "chart", "templates", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := kmsTemplateOptionsRE.FindSubmatch(raw)
	if m == nil {
		t.Fatal("chart/templates/rbac.yaml: no `{{- $options := list ... }}` line")
	}
	return strings.Fields(strings.ReplaceAll(string(m[1]), `"`, ""))
}

// TestRookCephCSIKMSAddonShape runs without the render toolchain: the
// addon is the local chart with an empty `kms:` default, the template
// knows exactly the options of this test, and every option has its
// objects and calls here.
func TestRookCephCSIKMSAddonShape(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, rookCephKMSAddon)

	if got := kmsTemplateOptions(t, root); !reflect.DeepEqual(got, kmsOptions) {
		t.Errorf("the template's options %v, the test's %v", got, kmsOptions)
	}
	for _, o := range kmsOptions {
		if _, ok := kmsObjects[o]; !ok {
			t.Errorf("option %s has no entry in kmsObjects", o)
		}
		if o != "all" && o != "vault" && !slices.ContainsFunc(kmsCalls, func(c kmsCall) bool { return c.feature == o }) {
			t.Errorf("option %s has no call in kmsCalls", o)
		}
	}

	raw, err := os.ReadFile(filepath.Join(addon, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kz struct {
		Generators []string `yaml:"generators"`
		Resources  []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kz); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kz.Generators, []string{"chart.yaml"}) || len(kz.Resources) != 0 {
		t.Errorf("kustomization.yaml: generators %v resources %v, want the chart generator alone", kz.Generators, kz.Resources)
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
			t.Errorf("%s = %v, want only `kms: []`", f, v)
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
			objs, msg, err := renderKMS(t, root, kmsInline(t, []string{o}))
			if err != nil {
				t.Fatalf("kms [%s]: %s", o, msg)
			}
			singles[o] = objs
			if o == "all" {
				fixture := readObjects(t, kmsAllFixture)
				if got, want := sortedKeys(objs), sortedKeys(fixture); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, kmsWantKeys([]string{o})) {
					t.Fatalf("kms [all] renders %v, %s has %v", got, kmsAllFixture, want)
				}
				for k, f := range fixture {
					if !reflect.DeepEqual(objs[k].Doc, f.Doc) {
						t.Errorf("kms [all]: %s\n got: %s\nwant (%s): %s", k, canon(t, objs[k].Doc), kmsAllFixture, canon(t, f.Doc))
					}
				}
				return
			}
			checkKMSRender(t, []string{o}, objs)
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
			objs, msg, err := renderKMS(t, root, kmsInline(t, opts))
			if err != nil {
				t.Fatalf("kms %v: %s", opts, msg)
			}
			checkKMSRender(t, opts, objs)
			for k, o := range objs {
				owner := slices.IndexFunc(opts, func(opt string) bool { return slices.Contains(kmsObjects[opt], k) })
				var alone object
				if owner >= 0 {
					alone = singles[opts[owner]][k]
				} else {
					alone = singles["vault"][k] // the KMS ConfigMap Role
				}
				if !reflect.DeepEqual(o.Doc, alone.Doc) {
					t.Errorf("kms %v: %s\n got: %s\nalone: %s", opts, k, canon(t, o.Doc), canon(t, alone.Doc))
				}
			}
		})
	}

	// No option stops the render; so do an unknown option and a value
	// that is not a list.
	noOption := "rook-ceph-csi-kms: no kms option is set. Set `values: {kms: [...]}` on its spec.bootstrap entry"
	listed := "The options: " + strings.Join(kmsOptions, ", ") + ". `all` renders the rule set of earlier releases"
	for name, tc := range map[string]struct {
		inline string
		want   []string
	}{
		"no values":  {"", []string{noOption, listed}},
		"empty list": {"kms: []", []string{noOption, listed}},
		"null":       {"kms: null", []string{noOption, listed}},
		"unknown":    {"kms: [metadata, vault-tenant-sa]", []string{`unknown kms option "vault-tenant-sa". The options: ` + strings.Join(kmsOptions, ", ")}},
		"not a list": {"kms: metadata", []string{"rook-ceph-csi-kms: kms must be a list, got string metadata. The options: " + strings.Join(kmsOptions, ", ")}},
	} {
		t.Run("error="+name, func(t *testing.T) {
			objs, msg, err := renderKMS(t, root, tc.inline)
			if err == nil {
				t.Fatalf("%q rendered %v, want an error", tc.inline, sortedKeys(objs))
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("%q: the error lacks %q:\n%s", tc.inline, w, msg)
				}
			}
		})
	}
}
