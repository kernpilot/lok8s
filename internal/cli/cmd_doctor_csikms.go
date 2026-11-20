package cli

// cmd_doctor_csikms.go is the rook-ceph-csi-kms check of `lo doctor`. Bash
// twin: doctor::_kms_check in .lok8s/libs/doctor. The piped lines are the
// same in both.
//
// The rook-ceph-csi-kms addon renders the roles and bindings of the options
// in values.kms, and each object carries the label
// lok8s.dev/name=rook-ceph-csi-kms. The bootstrap applies without prune, so
// after a change of the options (or of tenantNamespaces) the objects of the
// earlier apply stay in the cluster, and their grants with them.
//
// When spec.bootstrap has the addon as a bare entry name, the check renders
// each such entry as the bootstrap renders it (addons.Render, with the
// directory, the values and the env of the entry, the driver kind and the
// provider of the spec). The chart of the addon is local, so the render
// downloads nothing, and a render never reaches the cluster. The check then
// reads the ClusterRoles, ClusterRoleBindings, Roles and RoleBindings with
// the label in all namespaces. Each object that no render holds (the same
// kind, namespace and name) is a candidate leftover: a label is not proof
// that the addon made the object, so a line says "if no other tool manages
// it" before a delete command. When spec.bootstrap has rook-ceph but not the
// addon, the check reads too (no render): each object with the label is a
// candidate. Without either addon the check does nothing, so it does not
// run the credential plugin of the kubeconfig for a cluster that cannot
// have such objects.
//
// A candidate Role or ClusterRole gets a delete command only when no
// binding outside the candidates refers to it and, for a ClusterRole, when
// it has no aggregate-to label and no other ClusterRole aggregates it. For
// that, the check reads all RoleBindings and (with a ClusterRole candidate)
// all ClusterRoleBindings without the label selector (kmsRefs), and then
// all ClusterRoles (kmsAggs). An aggregation selector can select any label,
// not only an aggregate-to label: the check evaluates the selectors of each
// ClusterRole with an aggregationRule on the labels of each candidate
// ClusterRole (kmsMatch). A selector that the check cannot evaluate, or a
// failed read, gives no delete command. A candidate binding always gets its
// command: a delete removes its grant and cannot break an object of the
// render.
//
// The check does not render a path entry of spec.bootstrap: its kustomize
// plugins can read the secret store. A path entry can render the objects of
// the addon too, so with a path entry the check prints no candidate: when
// it finds one, it says that it did not check. An entry that does not parse
// stops the check before the reads.
//
// The check uses the machinery of the RBAC migration check
// (cmd_doctor_rbac.go): the domain's kubeconfig, rbacRead (the bound of
// each read, its process group, the fixed reasons, and a list that must be
// complete: kubectl's output is never printed) and rbacDeleteCommandIn. Of
// each object it keeps only the fields of kmsItem, kmsRef or kmsAggRole. A
// name that is not a DNS-1123 subdomain, or a namespace that is not a
// DNS-1123 label, never goes into a line: such objects are counted. The
// option label is printed only when it is a DNS-1123 label, and no line
// prints another label value. A failed render or read is one warning, and
// the check stops. An equal set prints nothing. The check never changes
// the doctor exit code. lo doctor sends only GET requests; a kubeconfig
// exec credential plugin runs as kubectl runs it, and lo does not limit
// what it does.

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// The addon, the addon that it needs, and its two labels.
const (
	kmsAddon       = "rook-ceph-csi-kms"
	kmsParentAddon = "rook-ceph"
	kmsNameLabel   = "lok8s.dev/name"
	kmsOptionLabel = "csi-kms.lok8s.dev/option"
	kmsSelector    = kmsNameLabel + "=" + kmsAddon
	// kmsAggregatePrefix starts the labels that put the rules of a
	// ClusterRole into an aggregated ClusterRole.
	kmsAggregatePrefix = "rbac.authorization.k8s.io/aggregate-to-"
)

// The fixed texts of the check.
const (
	kmsNotChecked          = kmsAddon + " RBAC leftovers not checked: "
	kmsSkipped             = kmsAddon + " RBAC check skipped: "
	kmsReasonForbidden     = "the kubeconfig user cannot list ClusterRoles, ClusterRoleBindings, Roles and RoleBindings in all namespaces (forbidden or unauthorized)"
	kmsReasonRefsForbidden = "the kubeconfig user cannot list RoleBindings and ClusterRoleBindings in all namespaces (forbidden or unauthorized)"
	kmsReasonAggForbidden  = "the kubeconfig user cannot list ClusterRoles (forbidden or unauthorized)"
	kmsReasonAggUnknown    = "a ClusterRole has an aggregation selector that doctor cannot evaluate"
	kmsReasonRender        = "the addon does not render with the values of its spec.bootstrap entry (see: lo bootstrap --only " + kmsAddon + " --dry-run)"
	kmsReasonMalformed     = "the render of the addon is malformed (a document that is not an object, an object without a kind, or an RBAC object without a name or, for a Role or RoleBinding, a namespace)"
	kmsReasonResolve       = "spec.bootstrap: doctor cannot read the list of entries (see: lo bootstrap --dry-run)"
)

// kmsReadArgs come after the resource of each read: all namespaces, and
// only the objects with the label of the addon. kmsRefArgs are the
// arguments of the reference reads: all namespaces, no selector.
var (
	kmsReadArgs = []string{"--all-namespaces", "--selector", kmsSelector}
	kmsRefArgs  = []string{"--all-namespaces"}
)

// kmsKind is a kind that the check reads: its name, the resource of the
// read, the resource of the delete command, and its scope.
type kmsKind struct {
	kind, resource, singular string
	namespaced               bool
}

// kmsKinds are in the order of the reads and of the lines.
var kmsKinds = []kmsKind{
	{"ClusterRole", "clusterroles", "clusterrole", false},
	{"ClusterRoleBinding", "clusterrolebindings", "clusterrolebinding", false},
	{"Role", "roles", "role", true},
	{"RoleBinding", "rolebindings", "rolebinding", true},
}

// binding reports a kind that grants a role.
func (k kmsKind) binding() bool { return strings.HasSuffix(k.kind, "Binding") }

// kmsID is an object by kind, namespace ("" for a cluster-scoped kind) and
// name.
type kmsID struct{ kind, namespace, name string }

// kmsItem is what the check keeps of a listed object: its name, its
// namespace, the two labels of the addon, whether a label starts with
// kmsAggregatePrefix, and all its labels for the aggregation selectors
// (kmsMatch). No line prints a label value.
type kmsItem struct {
	name, namespace, addon, option string
	aggregated                     bool
	labels                         map[string]string
}

// kmsRef is what the check keeps of a binding of the reference reads: the
// binding, and the kind and the name of its role.
type kmsRef struct {
	id                 kmsID
	roleKind, roleName string
}

// kmsExtra is an object that no render holds. labels are the labels of a
// ClusterRole (nil for the other kinds), for kmsAggs only.
type kmsExtra struct {
	kind       kmsKind
	id         kmsID
	option     string
	aggregated bool
	labels     map[string]string
}

// kmsAggRole is what the check keeps of a ClusterRole of the aggregation
// read: its name, whether it has an aggregationRule, and the selectors of
// that rule.
type kmsAggRole struct {
	name      string
	rule      bool
	selectors []kmsLabelSelector
}

// kmsLabelSelector is one clusterRoleSelector of an aggregationRule: its
// matchLabels and its matchExpressions.
type kmsLabelSelector struct {
	matchLabels map[string]string
	exprs       []kmsExpr
}

// kmsExpr is one matchExpressions item: the key, the operator and the
// values.
type kmsExpr struct {
	key, op string
	values  []string
}

// kmsAggResult is the result of the aggregation read: for each candidate
// ClusterRole, the ClusterRoles that aggregate it (into), or whether a
// selector that the check cannot evaluate can select it (unknown); or the
// reason why the read failed.
type kmsAggResult struct {
	into    map[kmsID][]kmsID
	unknown map[kmsID]bool
	reason  string
}

// kmsRefResult is the result of the reference reads: the bindings outside
// the candidates per candidate role, or the reason why the reads failed.
type kmsRefResult struct {
	bound  map[kmsID][]kmsID
	reason string
}

// kmsResolver reads spec.bootstrap for the check (kmsEntries in
// production): the parsed entries, or the fixed reason why it cannot.
type kmsResolver func(spec, kind, d string) ([]*bootstrap.Entry, string)

// kmsEntries is the kmsResolver over the bootstrap parser (bash: the loop
// over bootstrap::_resolve_entries and _parse_entry in doctor::_kms_check).
// Unlike bootstrapEntries, it drops no error: an entry that does not parse
// can be the addon or rook-ceph, so the check cannot know what the
// bootstrap applies. The reason names the position of the entry, not its
// text: an entry holds values, and values can hold secrets.
func kmsEntries(paths *config.Paths) kmsResolver {
	return func(spec, kind, d string) ([]*bootstrap.Entry, string) {
		raw, err := bootstrap.ResolveEntries(spec, kind)
		if err != nil {
			return nil, kmsReasonResolve
		}
		var out []*bootstrap.Entry
		for i, r := range raw {
			if r == "" {
				continue
			}
			e, err := bootstrap.ParseEntry(paths, io.Discard, d, r)
			if err != nil {
				return nil, "spec.bootstrap entry " + strconv.Itoa(i+1) + ": it does not parse (see: lo bootstrap --dry-run)"
			}
			out = append(out, e)
		}
		return out, ""
	}
}

// doctorKMSLeftovers prints the lines of the check under the domain
// section, after the RBAC migration check (which prints why a domain has
// no spec that it can read). resolve reads spec.bootstrap (kmsEntries in
// production).
func doctorKMSLeftovers(ctx context.Context, r execx.Runner, paths *config.Paths, d, path string, resolve kmsResolver, out io.Writer) {
	if d == "" || !domain.NameRe.MatchString(d) {
		return
	}
	spec := filepath.Join(paths.Clusters, d, "cluster.lok8s.yaml")
	if !fsutil.FileExists(spec) {
		return
	}
	kind, err := domain.SpecDriver(spec, "lo")
	if err != nil {
		return
	}
	parsed, reason := resolve(spec, kind, d)
	if reason != "" {
		doctorWarn(out, kmsNotChecked+reason)
		return
	}
	// An addon counts only as a bare entry name (Builtin), as in the RBAC
	// migration check. The check does not render a path entry.
	var entries []*bootstrap.Entry
	var pathKeys []string
	parent := false
	for _, e := range parsed {
		if !e.Builtin {
			pathKeys = append(pathKeys, e.Key)
			continue
		}
		switch filepath.Base(e.Dir) {
		case kmsAddon:
			entries = append(entries, e)
		case kmsParentAddon:
			parent = true
		}
	}
	enabled := len(entries) > 0
	if !enabled && !parent {
		return
	}
	// The skip lines are for a spec with the addon only: the leftover
	// check of a spec without it says nothing unless it finds an object.
	kubeconfig := build.AmbientKubeconfig(paths, d, "")
	if rbacHasControl(kubeconfig) {
		if enabled {
			doctorInfo(out, kmsSkipped+"the kubeconfig path has a control character (the delete commands of the check name that path)")
		}
		return
	}
	if !fsutil.FileExists(kubeconfig) {
		if enabled {
			doctorInfo(out, kmsSkipped+"no kubeconfig at "+config.RelTo(paths.Base, kubeconfig))
		}
		return
	}
	kubectl, ok := toolchain.LookPath(path, "kubectl")
	if !ok {
		if enabled {
			doctorWarn(out, kmsNotChecked+rbacReasonNoKubectl)
		}
		return
	}
	rendered := map[kmsID]bool{}
	if enabled {
		if reason := kmsRender(ctx, r, paths, spec, kind, entries, rendered); reason != "" {
			doctorWarn(out, kmsNotChecked+reason)
			return
		}
	}
	var extras []kmsExtra
	// candidates holds every object that no render holds, also the ones
	// with an unusual name: a binding among them does not count as a
	// reference to a role.
	candidates := map[kmsID]bool{}
	unusual := 0
	for _, k := range kmsKinds {
		var items []kmsItem
		if reason := rbacRead(ctx, r, kubectl, kubeconfig, k.resource, kmsReadArgs, kmsParseFor(k.namespaced), &items); reason != "" {
			if reason == rbacReasonForbidden {
				reason = kmsReasonForbidden
			}
			doctorWarn(out, kmsNotChecked+reason)
			return
		}
		for _, it := range items {
			// The selector's label again: an object without it is never a
			// finding, also from a server that ignores the selector.
			if it.addon != kmsAddon {
				continue
			}
			id := kmsID{kind: k.kind, name: it.name}
			if k.namespaced {
				id.namespace = it.namespace
			}
			if rendered[id] {
				continue
			}
			candidates[id] = true
			if !rbacNameOK(id.name) || (k.namespaced && !rbacLabelOK(id.namespace)) {
				unusual++
				continue
			}
			x := kmsExtra{kind: k, id: id, option: it.option, aggregated: it.aggregated}
			if k.kind == "ClusterRole" {
				x.labels = it.labels
			}
			extras = append(extras, x)
		}
	}
	if len(candidates) == 0 {
		// A path entry can only add objects to the render: no candidate
		// without it is no candidate with it too.
		return
	}
	if len(pathKeys) > 0 {
		doctorWarn(out, kmsNotChecked+"spec.bootstrap has path entries ("+kmsPathList(pathKeys)+"), which can render these objects too")
		return
	}
	refs := kmsRefs(ctx, r, kubectl, kubeconfig, extras, candidates)
	var aggs kmsAggResult
	if refs.reason == "" {
		aggs = kmsAggs(ctx, r, kubectl, kubeconfig, extras)
	}
	for _, x := range extras {
		kmsExtraLine(x, enabled, kubeconfig, refs, aggs, &unusual, out)
	}
	if unusual > 0 {
		kmsUnusualLine(unusual, enabled, kubeconfig, out)
	}
}

// kmsRender adds the RBAC objects of each entry's render to set, and
// returns "" or the reason of a failed or malformed render. The render's
// messages are not printed: the line of the failure names the command that
// shows them. addons.Render refuses an env name that the bootstrap refuses
// (addons.ProtectedEnvOverride, before any build), so such an entry is a
// failed render.
func kmsRender(ctx context.Context, r execx.Runner, paths *config.Paths, spec, kind string, entries []*bootstrap.Entry, set map[kmsID]bool) string {
	provider := specProviderName(spec)
	for _, e := range entries {
		stream, err := addons.Render(ctx, paths, r, io.Discard, e.Dir, kind, provider, e.Inline, bootstrap.EnvMap(e.EnvLines))
		if err != nil {
			return kmsReasonRender
		}
		if !kmsRenderedIDs(stream, set) {
			return kmsReasonMalformed
		}
	}
	return ""
}

// kmsRenderedIDs adds the ClusterRoles, ClusterRoleBindings, Roles and
// RoleBindings of a rendered stream to set (bash: doctor::_kms_ids). A
// cluster-scoped kind has no namespace. It reports false for a stream
// that does not parse, a document that is not an object (an empty
// document is skipped), an object without a string kind, and an RBAC
// object without a string metadata.name or, for a Role or a RoleBinding, a
// string metadata.namespace: the check cannot know what such a render
// applies.
func kmsRenderedIDs(stream string, set map[kmsID]bool) bool {
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			return errors.Is(err, io.EOF)
		}
		n := yqsem.Deref(&doc)
		if n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null") {
			continue
		}
		if n.Kind != yaml.MappingNode {
			return false
		}
		kind, ok := kmsYAMLString(yqsem.MapGet(n, "kind"))
		if !ok || kind == "" {
			return false
		}
		for _, k := range kmsKinds {
			if kind != k.kind {
				continue
			}
			// MapGet of a metadata that is not a mapping is nil: no name.
			md := yqsem.MapGet(n, "metadata")
			name, ok := kmsYAMLString(yqsem.MapGet(md, "name"))
			if !ok || name == "" {
				return false
			}
			id := kmsID{kind: k.kind, name: name}
			if k.namespaced {
				ns, ok := kmsYAMLString(yqsem.MapGet(md, "namespace"))
				if !ok || ns == "" {
					return false
				}
				id.namespace = ns
			}
			set[id] = true
		}
	}
}

// kmsYAMLString is n as a string: a scalar with the tag !!str, or
// !!timestamp, which yq -o=json prints as the string of its text (bash:
// a JSON string after yq -o=json).
func kmsYAMLString(n *yaml.Node) (string, bool) {
	n = yqsem.Deref(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	if t := n.ShortTag(); t != "!!str" && t != "!!timestamp" {
		return "", false
	}
	return n.Value, true
}

// kmsParseFor is the parse of the labelled reads (bash: the labelled-ns and
// labelled-cluster shapes of doctor::_rbac_shape): a string
// metadata.name, of a namespaced kind a string metadata.namespace, both
// not empty, and string labels. An item without its name or namespace is
// a list that the check cannot compare, so the whole list is invalid. It
// keeps the name, the namespace, the labels, the two labels of the addon
// and whether a label starts with kmsAggregatePrefix.
func kmsParseFor(namespaced bool) func(map[string]any) (kmsItem, bool) {
	return func(m map[string]any) (kmsItem, bool) {
		var it kmsItem
		md, ok := rbacObject(m["metadata"])
		if !ok {
			return it, false
		}
		var ok1, ok2, ok3 bool
		var labels map[string]string
		it.name, ok1 = rbacString(md["name"])
		it.namespace, ok2 = rbacString(md["namespace"])
		labels, ok3 = rbacStringMap(md["labels"])
		if !ok1 || !ok2 || !ok3 || it.name == "" || (namespaced && it.namespace == "") {
			return it, false
		}
		it.labels = labels
		it.addon, it.option = labels[kmsNameLabel], labels[kmsOptionLabel]
		for k := range labels {
			if strings.HasPrefix(k, kmsAggregatePrefix) {
				it.aggregated = true
			}
		}
		return it, true
	}
}

// kmsParseRefFor is the parse of the reference reads (bash: the ref-ns and
// ref-cluster shapes of doctor::_rbac_shape): a string metadata.name, of a
// RoleBinding a string metadata.namespace, and a string roleRef.kind and
// roleRef.name, all not empty. It keeps only these fields: not the
// subjects, the labels or the annotations.
func kmsParseRefFor(k kmsKind) func(map[string]any) (kmsRef, bool) {
	return func(m map[string]any) (kmsRef, bool) {
		ref := kmsRef{id: kmsID{kind: k.kind}}
		md, ok1 := rbacObject(m["metadata"])
		rr, ok2 := rbacObject(m["roleRef"])
		if !ok1 || !ok2 {
			return ref, false
		}
		var ok3, ok4, ok5, ok6 bool
		ref.id.name, ok3 = rbacString(md["name"])
		ref.id.namespace, ok4 = rbacString(md["namespace"])
		ref.roleKind, ok5 = rbacString(rr["kind"])
		ref.roleName, ok6 = rbacString(rr["name"])
		if !ok3 || !ok4 || !ok5 || !ok6 || ref.id.name == "" || ref.roleKind == "" || ref.roleName == "" || (k.namespaced && ref.id.namespace == "") {
			return ref, false
		}
		if !k.namespaced {
			ref.id.namespace = ""
		}
		return ref, true
	}
}

// kmsRefs reads the bindings that can refer to a candidate role, without
// the label selector: all ClusterRoleBindings (only with a ClusterRole
// among the extras) and all RoleBindings (with a Role or a ClusterRole
// among them). A binding that is a candidate itself does not count. A
// RoleBinding refers to a ClusterRole by its name, and to a Role by its
// name in the namespace of the binding. The first failed read stops the
// reads: then no role of the extras gets a delete command.
func kmsRefs(ctx context.Context, r execx.Runner, kubectl, kubeconfig string, extras []kmsExtra, candidates map[kmsID]bool) kmsRefResult {
	clusterRole, role := false, false
	for _, x := range extras {
		switch x.kind.kind {
		case "ClusterRole":
			clusterRole, role = true, true
		case "Role":
			role = true
		}
	}
	res := kmsRefResult{bound: map[kmsID][]kmsID{}}
	if !role {
		return res
	}
	for _, k := range kmsKinds {
		if !k.binding() || (!k.namespaced && !clusterRole) {
			continue
		}
		var refs []kmsRef
		if reason := rbacRead(ctx, r, kubectl, kubeconfig, k.resource, kmsRefArgs, kmsParseRefFor(k), &refs); reason != "" {
			if reason == rbacReasonForbidden {
				reason = kmsReasonRefsForbidden
			}
			return kmsRefResult{reason: reason}
		}
		for _, ref := range refs {
			if candidates[ref.id] {
				continue
			}
			var target kmsID
			switch {
			case ref.roleKind == "ClusterRole":
				target = kmsID{kind: "ClusterRole", name: ref.roleName}
			case ref.roleKind == "Role" && k.namespaced:
				target = kmsID{kind: "Role", namespace: ref.id.namespace, name: ref.roleName}
			default:
				continue
			}
			res.bound[target] = append(res.bound[target], ref.id)
		}
	}
	return res
}

// kmsParseAgg is the parse of the aggregation read (bash: the agg shape of
// doctor::_rbac_shape): a string metadata.name that is not empty, and an
// aggregationRule that is absent, null or an object. Its
// clusterRoleSelectors is absent, null or a list of objects. In each
// selector, matchLabels is an object of strings (absent or null: none) with
// keys that are not empty, and matchExpressions a list of objects (absent
// or null: none). Each expression has a key (a string that is not empty),
// an operator (a string) and values (a list of strings; absent or null:
// none). It keeps the name and the selectors, not the rules or the labels.
func kmsParseAgg(m map[string]any) (kmsAggRole, bool) {
	var role kmsAggRole
	md, ok1 := rbacObject(m["metadata"])
	rule, ok2 := rbacObject(m["aggregationRule"])
	if !ok1 || !ok2 {
		return role, false
	}
	var ok bool
	if role.name, ok = rbacString(md["name"]); !ok || role.name == "" {
		return role, false
	}
	if rule == nil {
		return role, true
	}
	role.rule = true
	selectors, ok := rbacObjects(rule["clusterRoleSelectors"])
	if !ok {
		return role, false
	}
	for _, sm := range selectors {
		var sel kmsLabelSelector
		if sel.matchLabels, ok = rbacStringMap(sm["matchLabels"]); !ok {
			return role, false
		}
		if _, empty := sel.matchLabels[""]; empty {
			return role, false
		}
		exprs, ok := rbacObjects(sm["matchExpressions"])
		if !ok {
			return role, false
		}
		for _, em := range exprs {
			var e kmsExpr
			var ok1, ok2, ok3 bool
			e.key, ok1 = rbacString(em["key"])
			e.op, ok2 = rbacString(em["operator"])
			e.values, ok3 = rbacStrings(em["values"])
			if !ok1 || !ok2 || !ok3 || e.key == "" {
				return role, false
			}
			sel.exprs = append(sel.exprs, e)
		}
		role.selectors = append(role.selectors, sel)
	}
	return role, true
}

// kmsMatch reports whether the selector s selects a ClusterRole with the
// labels, as the aggregation controller of Kubernetes selects
// (metav1.LabelSelectorAsSelector): each matchLabels pair is a label of the
// role, and each expression holds. In: the role has the label, with one of
// the values. NotIn: the role does not have the label, or its value is
// none of the values. Exists: the role has the label. DoesNotExist: the
// role does not have the label. A selector without pairs and expressions
// selects each ClusterRole. ok is false for a form that the check cannot
// evaluate (the API refuses it): another operator, In or NotIn without
// values, or Exists or DoesNotExist with values. It checks every
// expression for that, also after one that does not hold.
func kmsMatch(s kmsLabelSelector, labels map[string]string) (match, ok bool) {
	match = true
	for k, v := range s.matchLabels {
		if got, has := labels[k]; !has || got != v {
			match = false
		}
	}
	for _, e := range s.exprs {
		got, has := labels[e.key]
		switch e.op {
		case "In":
			if len(e.values) == 0 {
				return false, false
			}
			if !has || !slices.Contains(e.values, got) {
				match = false
			}
		case "NotIn":
			if len(e.values) == 0 {
				return false, false
			}
			if has && slices.Contains(e.values, got) {
				match = false
			}
		case "Exists":
			if len(e.values) > 0 {
				return false, false
			}
			if !has {
				match = false
			}
		case "DoesNotExist":
			if len(e.values) > 0 {
				return false, false
			}
			if has {
				match = false
			}
		default:
			return false, false
		}
	}
	return match, true
}

// kmsAggs reads all ClusterRoles (only with a ClusterRole among the
// extras), and evaluates the selectors of each one with an aggregationRule
// on the labels of each candidate ClusterRole of the extras. A ClusterRole
// aggregates a candidate when one of its selectors selects the candidate. A
// ClusterRole does not aggregate itself: a candidate that aggregates other
// candidates keeps its own line. When no ClusterRole aggregates a
// candidate but a selector that the check cannot evaluate can select it
// (kmsMatch), the result says so. A failed read gives the reason, and then
// no ClusterRole of the extras gets a delete command.
func kmsAggs(ctx context.Context, r execx.Runner, kubectl, kubeconfig string, extras []kmsExtra) kmsAggResult {
	res := kmsAggResult{into: map[kmsID][]kmsID{}, unknown: map[kmsID]bool{}}
	var cands []kmsExtra
	for _, x := range extras {
		if x.kind.kind == "ClusterRole" {
			cands = append(cands, x)
		}
	}
	if len(cands) == 0 {
		return res
	}
	var roles []kmsAggRole
	if reason := rbacRead(ctx, r, kubectl, kubeconfig, "clusterroles", kmsRefArgs, kmsParseAgg, &roles); reason != "" {
		if reason == rbacReasonForbidden {
			reason = kmsReasonAggForbidden
		}
		return kmsAggResult{reason: reason}
	}
	for _, x := range cands {
		for _, role := range roles {
			if !role.rule || role.name == x.id.name {
				continue
			}
			match, unknown := false, false
			for _, sel := range role.selectors {
				m, ok := kmsMatch(sel, x.labels)
				match = match || (ok && m)
				unknown = unknown || !ok
			}
			switch {
			case match:
				res.into[x.id] = append(res.into[x.id], kmsID{kind: "ClusterRole", name: role.name})
			case unknown:
				res.unknown[x.id] = true
			}
		}
	}
	return res
}

// kmsExtraLine is the warning of one candidate, with its delete command as
// the rest of the line when the check prints one. rbacDeleteCommandIn
// checks the name and the namespace again: an object that fails is
// counted instead.
func kmsExtraLine(x kmsExtra, enabled bool, kubeconfig string, refs kmsRefResult, aggs kmsAggResult, unusual *int, out io.Writer) {
	cmd, ok := rbacDeleteCommandIn(kubeconfig, x.kind.singular, x.id.namespace, x.id.name)
	if !ok {
		*unusual++
		return
	}
	what := x.kind.kind + " " + x.id.name
	if x.kind.namespaced {
		what += " in namespace " + x.id.namespace
	}
	if rbacLabelOK(x.option) {
		what += " (option " + x.option + ")"
	}
	line := kmsAddon + ": " + what + " is a candidate leftover: it has the label " + kmsSelector + ", and "
	if enabled {
		line += "the current values do not render it"
	} else {
		line += "spec.bootstrap does not have the addon"
	}
	line += ". It keeps its grant."
	// A candidate binding keeps its command, also when its role is in the
	// render: the delete removes a grant, and the render does not hold the
	// binding.
	if x.kind.binding() {
		doctorWarn(out, line+" If no other tool manages it, delete it: "+cmd)
		return
	}
	noCommand := false
	if refs.reason != "" {
		line += " References not checked: " + refs.reason + "."
		noCommand = true
	} else if b := refs.bound[x.id]; len(b) > 0 {
		line += " It is still bound by " + kmsBoundList(b) + "."
		noCommand = true
	}
	if x.kind.kind == "ClusterRole" {
		switch into := aggs.into[x.id]; {
		case aggs.reason != "":
			line += " Aggregation not checked: " + aggs.reason + "."
			noCommand = true
		case len(into) > 0:
			line += " It is aggregated into " + kmsAggList(into) + "."
			noCommand = true
		case aggs.unknown[x.id]:
			line += " Aggregation not checked: " + kmsReasonAggUnknown + "."
			noCommand = true
		}
	}
	if x.kind.kind == "ClusterRole" && x.aggregated {
		line += " It has an aggregate-to label: an aggregated ClusterRole takes its rules."
		noCommand = true
	}
	if noCommand {
		doctorWarn(out, line+" Doctor prints no delete command for it.")
		return
	}
	doctorWarn(out, line+" No binding outside the candidates refers to it. If no other tool manages it, delete it: "+cmd)
}

// kmsListMax is the number of names that a list of the check shows.
const kmsListMax = 3

// kmsBoundList names the bindings of a role: ClusterRoleBinding/<name> or
// RoleBinding/<namespace>/<name>, only for a name that is a DNS-1123
// subdomain and a namespace that is a DNS-1123 label, at most kmsListMax.
// The others are counted.
func kmsBoundList(ids []kmsID) string {
	var shown []string
	more, odd := 0, 0
	for _, id := range ids {
		switch {
		case !rbacNameOK(id.name) || (id.namespace != "" && !rbacLabelOK(id.namespace)):
			odd++
		case len(shown) == kmsListMax:
			more++
		case id.namespace == "":
			shown = append(shown, id.kind+"/"+id.name)
		default:
			shown = append(shown, id.kind+"/"+id.namespace+"/"+id.name)
		}
	}
	return kmsList(shown, more, odd, "binding", "bindings")
}

// kmsAggList names the ClusterRoles that aggregate a candidate:
// ClusterRole/<name>, only for a name that is a DNS-1123 subdomain, at most
// kmsListMax. The others are counted.
func kmsAggList(ids []kmsID) string {
	var shown []string
	more, odd := 0, 0
	for _, id := range ids {
		switch {
		case !rbacNameOK(id.name):
			odd++
		case len(shown) == kmsListMax:
			more++
		default:
			shown = append(shown, id.kind+"/"+id.name)
		}
	}
	return kmsList(shown, more, odd, "ClusterRole", "ClusterRoles")
}

// kmsPathKeyRe is the form of a path entry that the check prints.
var kmsPathKeyRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,253}$`)

// kmsPathList names the path entries of spec.bootstrap as the spec writes
// them, only in the form of kmsPathKeyRe, at most kmsListMax. The others
// are counted.
func kmsPathList(keys []string) string {
	var shown []string
	more, odd := 0, 0
	for _, k := range keys {
		switch {
		case !kmsPathKeyRe.MatchString(k):
			odd++
		case len(shown) == kmsListMax:
			more++
		default:
			shown = append(shown, k)
		}
	}
	return kmsList(shown, more, odd, "entry", "entries")
}

// kmsList joins the shown names, the count of the other names, and the
// count of the names that the check does not print, with ", ".
func kmsList(shown []string, more, odd int, one, many string) string {
	word := func(n int) string {
		if n == 1 {
			return one
		}
		return many
	}
	parts := shown
	if more > 0 {
		parts = append(parts, strconv.Itoa(more)+" more "+word(more))
	}
	if odd > 0 {
		parts = append(parts, strconv.Itoa(odd)+" "+word(odd)+" with an unusual name")
	}
	return strings.Join(parts, ", ")
}

// kmsUnusualLine is the warning of the candidates with an unusual name or
// namespace: their count, and the command that lists the objects of the
// addon (it prints no name).
func kmsUnusualLine(n int, enabled bool, kubeconfig string, out io.Writer) {
	what := "1 object with the label " + kmsSelector + " and an unusual name or namespace (not DNS-1123) is a candidate leftover: "
	it := "it"
	if n > 1 {
		what = strconv.Itoa(n) + " objects with the label " + kmsSelector + " and an unusual name or namespace (not DNS-1123) are candidate leftovers: "
		it = "them"
	}
	line := kmsAddon + ": " + what
	if enabled {
		line += "the current values do not render " + it
	} else {
		line += "spec.bootstrap does not have the addon"
	}
	doctorWarn(out, line+". Doctor does not print those names. List the objects of the addon: kubectl --kubeconfig "+rbacShQuote(kubeconfig)+" get clusterroles,clusterrolebindings,roles,rolebindings --all-namespaces --selector "+kmsSelector)
}
