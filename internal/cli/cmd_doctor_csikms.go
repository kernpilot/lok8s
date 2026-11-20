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
// the label in all namespaces, and warns once for each object that no
// render holds (the same kind, namespace and name), with its delete
// command. When spec.bootstrap has rook-ceph but not the addon, the check
// reads too (no render): each object with the label is a leftover of the
// addon. Without either addon the check does nothing, so it does not run
// the credential plugin of the kubeconfig for a cluster that cannot have
// such objects.
//
// The check uses the machinery of the RBAC migration check
// (cmd_doctor_rbac.go): the domain's kubeconfig, rbacRead (the bound of
// each read, its process group and the fixed reasons: kubectl's output is
// never printed) and rbacDeleteCommandIn. Of each object it reads only the
// name, the namespace and the labels, and it keeps two labels. A name that
// is not a DNS-1123 subdomain, or a namespace that is not a DNS-1123 label,
// never goes into a line: such objects are counted. The option label is
// printed only when it is a DNS-1123 label. A failed render or read is one
// warning, and the check stops. An equal set prints nothing. The check
// never changes the cluster or the doctor exit code.

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// The addon, the addon that it needs, and its two labels.
const (
	kmsAddon       = "rook-ceph-csi-kms"
	kmsParentAddon = "rook-ceph"
	kmsNameLabel   = "lok8s.dev/name"
	kmsOptionLabel = "csi-kms.lok8s.dev/option"
	kmsSelector    = kmsNameLabel + "=" + kmsAddon
)

// The fixed texts of the check.
const (
	kmsNotChecked      = kmsAddon + " RBAC leftovers not checked: "
	kmsSkipped         = kmsAddon + " RBAC check skipped: "
	kmsReasonForbidden = "the kubeconfig user cannot list ClusterRoles, ClusterRoleBindings, Roles and RoleBindings in all namespaces (forbidden or unauthorized)"
	kmsReasonRender    = "the addon does not render with the values of its spec.bootstrap entry (see: lo bootstrap --only " + kmsAddon + " --dry-run)"
)

// kmsReadArgs come after the resource of each read: all namespaces, and
// only the objects with the label of the addon.
var kmsReadArgs = []string{"--all-namespaces", "--selector", kmsSelector}

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

// kmsID is an object by kind, namespace ("" for a cluster-scoped kind) and
// name.
type kmsID struct{ kind, namespace, name string }

// kmsItem is what the check keeps of a listed object.
type kmsItem struct{ name, namespace, addon, option string }

// kmsExtra is an object that no render holds.
type kmsExtra struct {
	kind   kmsKind
	id     kmsID
	option string
}

// doctorKMSLeftovers prints the lines of the check under the domain
// section, after the RBAC migration check (which prints why a domain has
// no spec that it can read). resolve reads spec.bootstrap
// (bootstrapEntries in production).
func doctorKMSLeftovers(ctx context.Context, r execx.Runner, paths *config.Paths, d, path string, resolve addons.EntryResolver, out io.Writer) {
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
	// An addon counts only as a bare entry name (Builtin), as in the RBAC
	// migration check.
	var entries []addons.Resolved
	parent := false
	for _, e := range resolve(spec, kind, d) {
		if !e.Builtin {
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
	if enabled && !kmsRender(ctx, r, paths, spec, kind, entries, rendered) {
		doctorWarn(out, kmsNotChecked+kmsReasonRender)
		return
	}
	var extras []kmsExtra
	unusual := 0
	for _, k := range kmsKinds {
		var items []kmsItem
		if reason := rbacRead(ctx, r, kubectl, kubeconfig, k.resource, kmsReadArgs, kmsParse, &items); reason != "" {
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
			if !rbacNameOK(id.name) || (k.namespaced && !rbacLabelOK(id.namespace)) {
				unusual++
				continue
			}
			extras = append(extras, kmsExtra{kind: k, id: id, option: it.option})
		}
	}
	for _, x := range extras {
		kmsExtraLine(x, enabled, kubeconfig, &unusual, out)
	}
	if unusual > 0 {
		kmsUnusualLine(unusual, enabled, kubeconfig, out)
	}
}

// kmsRender adds the RBAC objects of each entry's render to set, and
// reports false when a render fails. The render's messages are not
// printed: the line of the failure names the command that shows them.
func kmsRender(ctx context.Context, r execx.Runner, paths *config.Paths, spec, kind string, entries []addons.Resolved, set map[kmsID]bool) bool {
	provider := specProviderName(spec)
	for _, e := range entries {
		stream, err := addons.Render(ctx, paths, r, io.Discard, e.Dir, kind, provider, e.Inline, e.Env)
		if err != nil || !kmsRenderedIDs(stream, set) {
			return false
		}
	}
	return true
}

// kmsRenderedIDs adds the ClusterRoles, ClusterRoleBindings, Roles and
// RoleBindings of a rendered stream to set (bash: the yq of
// doctor::_kms_check). A cluster-scoped kind has no namespace.
func kmsRenderedIDs(stream string, set map[kmsID]bool) bool {
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); err != nil {
			return errors.Is(err, io.EOF)
		}
		for _, k := range kmsKinds {
			if doc.Kind != k.kind {
				continue
			}
			id := kmsID{kind: k.kind, name: doc.Metadata.Name}
			if k.namespaced {
				id.namespace = doc.Metadata.Namespace
			}
			set[id] = true
		}
	}
}

// kmsParse reads metadata.name, metadata.namespace and metadata.labels
// (string values) of a listed object (bash: the "labelled" shape of
// doctor::_rbac_shape), and keeps the two labels of the addon.
func kmsParse(m map[string]any) (kmsItem, bool) {
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
	if !ok1 || !ok2 || !ok3 {
		return it, false
	}
	it.addon, it.option = labels[kmsNameLabel], labels[kmsOptionLabel]
	return it, true
}

// kmsExtraLine is the warning of one object that no render holds, with its
// delete command as the rest of the line. rbacDeleteCommandIn checks the
// name and the namespace again: an object that fails is counted instead.
func kmsExtraLine(x kmsExtra, enabled bool, kubeconfig string, unusual *int, out io.Writer) {
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
	line := kmsAddon + ": " + what
	if enabled {
		line += " is not in the render of the current values"
	} else {
		line += " is a leftover of the addon, which spec.bootstrap does not have"
	}
	doctorWarn(out, line+". It keeps its grant. Delete it: "+cmd)
}

// kmsUnusualLine is the warning of the objects with an unusual name or
// namespace: their count, and the command that lists the objects of the
// addon (it prints no name).
func kmsUnusualLine(n int, enabled bool, kubeconfig string, out io.Writer) {
	what := "1 object with the label " + kmsSelector + " and an unusual name or namespace (not DNS-1123)"
	verb, leftover := " is", " a leftover"
	if n > 1 {
		what = strconv.Itoa(n) + " objects with the label " + kmsSelector + " and an unusual name or namespace (not DNS-1123)"
		verb, leftover = " are", " leftovers"
	}
	line := kmsAddon + ": " + what + verb
	if enabled {
		line += " not in the render of the current values"
	} else {
		line += leftover + " of the addon, which spec.bootstrap does not have"
	}
	doctorWarn(out, line+". Doctor does not print those names. List the objects of the addon: kubectl --kubeconfig "+rbacShQuote(kubeconfig)+" get clusterroles,clusterrolebindings,roles,rolebindings --all-namespaces --selector "+kmsSelector)
}
