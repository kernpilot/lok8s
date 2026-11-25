package bootstrap

// clientside.go: the check of the field managers of the live objects of an
// entry before its server-side apply (bash: bootstrap::_csa_read and the
// helpers next to it).
//
// kubectl apply --server-side moves the fields of a client-side writer to
// its own field manager when a managedFields entry of the live object has
// the operation Update and owns the last-applied annotation (the move of
// kubectl from a client-side to a server-side apply). In the same apply,
// kubectl then drops each of these fields that the target does not set,
// unless another manager owns the field too. kubectl diff --server-side
// does not do the move, so a diff does not show the drop. kubectl keeps the
// last-applied annotation under a manager of its own (kubectl-last-applied)
// when the apply uses the default manager, as lo does.
//
// The check reads the field managers of the live objects, with the stdin
// of the diff or of the apply, and gives a [warn] line for each object
// whose fields a client-side writer owns: the writers, and the labels and
// annotations that the apply drops. A dry run counts such an entry as
// "differs". A real run reads again after the apply and names each label
// and annotation that the apply dropped. --skip-client-side-owned leaves
// such objects out of the apply (a dry run: out of the diff). The check
// never removes the last-applied annotation.
//
// kubectl moves only the Update entry of a writer that has the apiVersion
// of its apply entry (the apiVersion of the target). It drops each other
// Update entry of the writer with no subresource: the fields of such an
// entry stay on the object with no manager, so the apply does not drop
// them.
//
// The read gets no value of an object: only its apiVersion, kind,
// namespace and name, and for each managedFields entry the manager, the
// operation, the subresource, the apiVersion and the key names of its
// labels and annotations. lo escapes each name before it prints it
// (kapply.EscapeText): a client can write any manager name and any key
// name into managedFields.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/ui"
	"gopkg.in/yaml.v3"
)

const (
	// lastAppliedKey is the annotation of a client-side apply.
	lastAppliedKey = "kubectl.kubernetes.io/last-applied-configuration"
	// applyManager is the field manager of the server-side apply of lo:
	// the default of kubectl (lo passes no --field-manager).
	applyManager = "kubectl"
)

// csaObjectTemplate prints one object: a line "O<TAB>apiVersion<TAB>kind
// <TAB>namespace<TAB>name", then for each managedFields entry a line
// "M<TAB>manager<TAB>operation<TAB>subresource<TAB>apiVersion<TAB>labels
// <TAB>annotations". labels and annotations are the JSON of the f:labels
// and f:annotations sets of the entry: key names, no values.
const csaObjectTemplate = `{"O\t"}{.apiVersion}{"\t"}{.kind}{"\t"}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}` +
	`{range .metadata.managedFields[*]}{"M\t"}{.manager}{"\t"}{.operation}{"\t"}{.subresource}{"\t"}{.apiVersion}{"\t"}` +
	`{.fieldsV1.f:metadata.f:labels}{"\t"}{.fieldsV1.f:metadata.f:annotations}{"\n"}{end}`

// csaTemplate is the jsonpath of the read (bash: _BOOTSTRAP_CSA_TEMPLATE).
// kubectl prints a List for the objects of a stdin, also for one object:
// the first part reads its items. The second part reads one object that is
// not in a List. For a List it prints an O line with no name, which
// parseFieldManagers skips.
const csaTemplate = `{range .items[*]}` + csaObjectTemplate + `{end}` + csaObjectTemplate

// fieldsEntry is one managedFields entry of a live object.
type fieldsEntry struct {
	manager, operation, subresource, apiVersion string
	// labels and annotations are the key names the entry owns, sorted.
	labels, annotations []string
}

// liveObject is one object of the read.
type liveObject struct {
	group, kind, namespace, name string
	entries                      []fieldsEntry
}

// csaRead is one read of the field managers.
type csaRead struct {
	objects []liveObject
	// failed is the exit code of kubectl when the read failed for another
	// reason than a missing CRD; 0 when it worked. An object whose CRD is
	// not there cannot be live, and kubectl reads the other objects, so
	// that failure loses nothing.
	failed int
}

// readFieldManagers runs the read: KUBERC=off kubectl --kubeconfig <kc>
// get -f - --ignore-not-found --show-managed-fields -o jsonpath=<template>,
// with the manifest on stdin. Its stderr goes to a buffer that only the
// classification reads: lo never prints it.
func (e *Engine) readFieldManagers(ctx context.Context, kubeconfig, manifest string) csaRead {
	var out, errOut strings.Builder
	err := e.Runner.Run(ctx, execx.Cmd{
		Name:   "kubectl",
		Args:   []string{"--kubeconfig", kubeconfig, "get", "-f", "-", "--ignore-not-found", "--show-managed-fields", "-o", "jsonpath=" + csaTemplate},
		Env:    []string{execx.KubeRCOff},
		Stdin:  strings.NewReader(manifest),
		Stdout: &out, Stderr: &errOut,
	})
	r := csaRead{objects: parseFieldManagers(out.String())}
	switch {
	case err == nil:
	case execx.LaunchFailed(err):
		// bash: the shell's 127 of a command that is not found.
		r.failed = 127
	case !raceRe.MatchString(errOut.String()):
		r.failed = execx.ExitCode(err)
	}
	return r
}

// parseFieldManagers reads the output of csaTemplate. A line of another
// shape is ignored; an M line before the first O line, or after an O line
// with no name, too.
func parseFieldManagers(out string) []liveObject {
	var objs []liveObject
	cur := -1
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "O" && len(f) == 5:
			cur = -1
			if f[4] != "" {
				objs = append(objs, liveObject{group: apiGroup(f[1]), kind: f[2], namespace: f[3], name: f[4]})
				cur = len(objs) - 1
			}
		case f[0] == "M" && len(f) == 7 && cur >= 0:
			objs[cur].entries = append(objs[cur].entries, fieldsEntry{
				manager: f[1], operation: f[2], subresource: f[3], apiVersion: f[4],
				labels: fieldKeys(f[5]), annotations: fieldKeys(f[6]),
			})
		}
	}
	return objs
}

// fieldKeys are the key names of a fieldsV1 set in JSON ({".":{},
// "f:team":{}} gives [team]), sorted. Empty text, text that is not a JSON
// object and a key without the f: prefix give nothing.
func fieldKeys(s string) []string {
	if s == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	var keys []string
	for k := range m {
		if name, ok := strings.CutPrefix(k, "f:"); ok {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}

// apiGroup is the group of an apiVersion: the part before the first "/",
// or "" for the core group.
func apiGroup(apiVersion string) string {
	if group, _, ok := strings.Cut(apiVersion, "/"); ok {
		return group
	}
	return ""
}

// manifestObject is one object of the manifest of the read: its label
// (objectLabel, what withoutObjects drops), its identity, its apiVersion
// (the apiVersion of the apply entry of kubectl), and the key names of the
// labels and annotations that it sets.
type manifestObject struct {
	label, apiVersion, group, kind, name, namespace string
	labels, annotations                             []string
}

// manifestObjects reads the objects of a manifest, in order. A document
// that is not a map is not an object. A manifest that does not parse gives
// no objects: the check then finds no object to name (bash: yq fails on
// the stream).
func manifestObjects(stream string) []manifestObject {
	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	keys := func(v any) []string {
		m, _ := v.(map[string]any)
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var objs []manifestObject
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				return objs
			}
			return nil
		}
		var m map[string]any
		if node.Decode(&m) != nil || m == nil {
			continue
		}
		meta, _ := m["metadata"].(map[string]any)
		objs = append(objs, manifestObject{
			label: objectLabel(m), apiVersion: str(m["apiVersion"]), group: apiGroup(str(m["apiVersion"])), kind: str(m["kind"]),
			name: str(meta["name"]), namespace: str(meta["namespace"]),
			labels: keys(meta["labels"]), annotations: keys(meta["annotations"]),
		})
	}
}

// matchManifest finds the manifest object of a live object: the same
// group, kind and name, and the first one with the same namespace, else
// the first one with no namespace (the namespace of the kubeconfig), else
// the first one.
func matchManifest(docs []manifestObject, o liveObject) (manifestObject, bool) {
	var cand []manifestObject
	for _, d := range docs {
		if d.group == o.group && d.kind == o.kind && d.name == o.name {
			cand = append(cand, d)
		}
	}
	for _, d := range cand {
		if d.namespace == o.namespace {
			return d, true
		}
	}
	for _, d := range cand {
		if d.namespace == "" {
			return d, true
		}
	}
	if len(cand) > 0 {
		return cand[0], true
	}
	return manifestObject{}, false
}

// clientSideObject is one live object whose fields a client-side writer
// owns.
type clientSideObject struct {
	// object names it: "<Kind> <namespace>/<name>", or "<Kind> <name>",
	// escaped.
	object string
	// label is the label of its manifest object (withoutObjects).
	label string
	// group, kind, namespace and name find it in the read after the apply.
	group, kind, namespace, name string
	// writers are the managers of the Update entries that own the
	// last-applied annotation, sorted.
	writers []string
	// unset: the labels and annotations that the writers own and the
	// target does not set (the last-applied annotation is not one of them).
	unsetLabels, unsetAnnotations []string
	// drop: the unset ones that no other manager owns. The apply drops them.
	dropLabels, dropAnnotations []string
}

// clientSideObjects finds the live objects whose fields a client-side
// writer owns, in the order of the read, the way kubectl finds them: a
// managedFields entry with the operation Update that owns the last-applied
// annotation. kubectl moves the Update entry of each of these managers with
// no subresource and with the apiVersion of the target to its apply
// manager, and drops each other Update entry of the manager with no
// subresource: those fields stay on the object with no manager, and they
// keep no field of the moved entry. An object with no entry to move is not
// named: the apply drops nothing of it. An entry of another manager, or of
// the same manager with another operation or a subresource, keeps its
// fields: the apply does not drop a field that it owns too. lo's own Apply
// entry does not keep a field. A label that the API server sets
// (serverSetLabels) is not named. A live object with no manifest object is
// not named.
func clientSideObjects(docs []manifestObject, live []liveObject) []clientSideObject {
	var out []clientSideObject
	for _, o := range live {
		writers := map[string]bool{}
		for _, en := range o.entries {
			if en.operation == "Update" && slices.Contains(en.annotations, lastAppliedKey) {
				writers[en.manager] = true
			}
		}
		if len(writers) == 0 {
			continue
		}
		doc, ok := matchManifest(docs, o)
		if !ok {
			continue
		}
		owned, other := keySet{}, keySet{}
		moved := false
		for _, en := range o.entries {
			switch {
			case writers[en.manager] && en.operation == "Update" && en.subresource == "":
				if en.apiVersion == doc.apiVersion {
					owned.add(en)
					moved = true
				}
			case en.manager == applyManager && en.operation == "Apply" && en.subresource == "":
			default:
				other.add(en)
			}
		}
		if !moved {
			continue
		}
		c := clientSideObject{
			object: objectName(o), label: doc.label,
			group: o.group, kind: o.kind, namespace: o.namespace, name: o.name,
			writers:          sortedKeys(writers),
			unsetLabels:      minus(sortedKeys(owned.labels), append(serverSetLabels(o), doc.labels...)),
			unsetAnnotations: minus(sortedKeys(owned.annotations), append([]string{lastAppliedKey}, doc.annotations...)),
		}
		c.dropLabels = minus(c.unsetLabels, sortedKeys(other.labels))
		c.dropAnnotations = minus(c.unsetAnnotations, sortedKeys(other.annotations))
		out = append(out, c)
	}
	return out
}

// serverSetLabels are the labels that the API server sets on each write of
// the object, so an apply cannot drop them: the label
// kubernetes.io/metadata.name (the name) of a Namespace. The manager of the
// write that created the Namespace owns it (a client-side apply too).
func serverSetLabels(o liveObject) []string {
	if o.group == "" && o.kind == "Namespace" {
		return []string{"kubernetes.io/metadata.name"}
	}
	return nil
}

// keySet is the union of the label and annotation keys of entries.
type keySet struct{ labels, annotations map[string]bool }

func (s *keySet) add(en fieldsEntry) {
	if s.labels == nil {
		s.labels, s.annotations = map[string]bool{}, map[string]bool{}
	}
	for _, k := range en.labels {
		s.labels[k] = true
	}
	for _, k := range en.annotations {
		s.annotations[k] = true
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// minus is a without the elements of b, in the order of a.
func minus(a, b []string) []string {
	var out []string
	for _, k := range a {
		if !slices.Contains(b, k) {
			out = append(out, k)
		}
	}
	return out
}

func objectName(o liveObject) string {
	ns := ""
	if o.namespace != "" {
		ns = kapply.EscapeText(o.namespace) + "/"
	}
	return kapply.EscapeText(o.kind) + " " + ns + kapply.EscapeText(o.name)
}

// csaWriters names the writers: "a client-side writer (m) owns fields", or
// "client-side writers (m1, m2) own fields".
func csaWriters(writers []string) string {
	if len(writers) == 1 {
		return "a client-side writer (" + kapply.EscapeText(writers[0]) + ") owns fields"
	}
	return "client-side writers (" + escapedList(writers) + ") own fields"
}

// csaKeys names the keys: "label a", "labels a, b", "annotation c", joined
// with "; ". Empty when there are none.
func csaKeys(labels, annotations []string) string {
	var parts []string
	for _, p := range []struct {
		what string
		keys []string
	}{{"label", labels}, {"annotation", annotations}} {
		switch len(p.keys) {
		case 0:
		case 1:
			parts = append(parts, p.what+" "+kapply.EscapeText(p.keys[0]))
		default:
			parts = append(parts, p.what+"s "+escapedList(p.keys))
		}
	}
	return strings.Join(parts, "; ")
}

func escapedList(s []string) string {
	esc := make([]string, len(s))
	for i, v := range s {
		esc[i] = kapply.EscapeText(v)
	}
	return strings.Join(esc, ", ")
}

// warnText is the [warn] line of an object that the apply takes, after
// "bootstrap: <entry>: ".
func (c clientSideObject) warnText() string {
	s := c.object + ": " + csaWriters(c.writers) + "; the apply moves them to lo and drops what the target does not set"
	if k := csaKeys(c.dropLabels, c.dropAnnotations); k != "" {
		s += ": " + k
	}
	return s
}

// skipText is the [warn] line of an object that --skip-client-side-owned
// leaves out, after "bootstrap: <entry>: ".
func (c clientSideObject) skipText() string {
	return c.object + ": left out of the apply (--skip-client-side-owned): " + csaWriters(c.writers)
}

// csaReadFailedText is the [warn] line of a read that failed.
func csaReadFailedText(entry string, rc int) string {
	return fmt.Sprintf("bootstrap: %s: lo cannot read the field managers of the live objects (kubectl get exited %d), so it cannot name the fields that the apply drops", entry, rc)
}

// csaCheck is the outcome of clientSideBefore.
type csaCheck struct {
	// manifest is what the entry diffs or applies: without the objects
	// that --skip-client-side-owned leaves out.
	manifest string
	// objects are the objects of a client-side writer that the apply takes.
	objects []clientSideObject
	// leftOut is the count of the objects left out.
	leftOut int
	// readFailed is the exit code of a read that failed (0: it worked).
	readFailed int
}

// clientSideBefore reads the field managers of the live objects of the
// manifest and prints a [warn] line for each object whose fields a
// client-side writer owns. Under --skip-client-side-owned each line says
// that the object is left out, and the manifest of the check is without
// these objects. ok is false when the entry fails: under
// --skip-client-side-owned when the read failed (lo cannot find the
// objects to leave out) or when the objects cannot be taken out of the
// manifest. The caller prints the line of a read that failed otherwise
// (csaReadFailedText): a dry run only when its diff worked.
func (e *Engine) clientSideBefore(ctx context.Context, stderr io.Writer, entry, kubeconfig, manifest string) (csaCheck, bool) {
	c := csaCheck{manifest: manifest}
	read := e.readFieldManagers(ctx, kubeconfig, manifest)
	skip := e.Options.SkipClientSideOwned
	if read.failed != 0 {
		if skip {
			ui.ErrorTo(stderr, "bootstrap: %s: lo cannot read the field managers of the live objects (kubectl get exited %d), so --skip-client-side-owned cannot find the objects to leave out", entry, read.failed)
			return c, false
		}
		c.readFailed = read.failed
	}
	var drop []string
	for _, o := range clientSideObjects(manifestObjects(manifest), read.objects) {
		if skip {
			ui.WarnTo(stderr, "bootstrap: %s: %s", entry, o.skipText())
			drop = append(drop, o.label)
			continue
		}
		ui.WarnTo(stderr, "bootstrap: %s: %s", entry, o.warnText())
		c.objects = append(c.objects, o)
	}
	if len(drop) > 0 {
		rest, err := withoutObjects(manifest, drop)
		if err != nil {
			// The error of the YAML decoder can quote a value: fixed text.
			ui.ErrorTo(stderr, "bootstrap: %s: cannot take the objects of a client-side writer out of the manifest", entry)
			return c, false
		}
		c.manifest, c.leftOut = rest, len(drop)
	}
	return c, true
}

// clientSideAfter reads the field managers again after the apply, and
// prints a [warn] line for each object of the check that lost labels or
// annotations: the unset keys that no manager owns now. No read when no
// object of the check has an unset key. A read that fails gives a [debug]
// line only.
func (e *Engine) clientSideAfter(ctx context.Context, stderr io.Writer, entry, kubeconfig string, c csaCheck) {
	var check []clientSideObject
	for _, o := range c.objects {
		if len(o.unsetLabels)+len(o.unsetAnnotations) > 0 {
			check = append(check, o)
		}
	}
	if len(check) == 0 {
		return
	}
	read := e.readFieldManagers(ctx, kubeconfig, c.manifest)
	if read.failed != 0 {
		ui.DebugTo(stderr, "bootstrap: %s: the read of the field managers after the apply failed (kubectl get exited %d)", entry, read.failed)
		return
	}
	for _, o := range check {
		i := slices.IndexFunc(read.objects, func(l liveObject) bool {
			return l.group == o.group && l.kind == o.kind && l.namespace == o.namespace && l.name == o.name
		})
		if i < 0 {
			continue
		}
		var owned keySet
		for _, en := range read.objects[i].entries {
			owned.add(en)
		}
		if k := csaKeys(minus(o.unsetLabels, sortedKeys(owned.labels)), minus(o.unsetAnnotations, sortedKeys(owned.annotations))); k != "" {
			ui.WarnTo(stderr, "bootstrap: %s: %s: the apply dropped %s", entry, o.object, k)
		}
	}
}
