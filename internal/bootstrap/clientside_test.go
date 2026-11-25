package bootstrap

// clientside_test.go: the check of the field managers (clientside.go). The
// bats twins are the "client-side writer" cases of
// tests/unit/bootstrap_test.bats.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"k8s.io/client-go/util/jsonpath"
)

// csaCanary is a value in the live objects of the fixtures. The read must
// never carry it: it gets names, never values.
const csaCanary = "CANARY-VALUE-0451"

// liveNamespace is a live Namespace as the API server returns it: a
// client-side apply (kubectl-client-side-apply, an Update entry that owns
// the last-applied annotation) created it with the labels team and app and
// the annotation note. That entry owns the label kubernetes.io/metadata.name
// too: the API server set it in the same write.
const liveNamespace = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"demo",
 "labels":{"team":"x","app":"y","kubernetes.io/metadata.name":"demo"},
 "annotations":{"note":"` + csaCanary + `","kubectl.kubernetes.io/last-applied-configuration":"{\"metadata\":{\"annotations\":{\"note\":\"` + csaCanary + `\"}}}"},
 "managedFields":[
  {"manager":"kubectl-client-side-apply","operation":"Update","apiVersion":"v1","time":"2026-11-25T10:00:00Z","fieldsType":"FieldsV1",
   "fieldsV1":{"f:metadata":{"f:annotations":{".":{},"f:kubectl.kubernetes.io/last-applied-configuration":{},"f:note":{}},"f:labels":{".":{},"f:app":{},"f:kubernetes.io/metadata.name":{},"f:team":{}}}}}]},
 "spec":{"finalizers":["kubernetes"]},"status":{"phase":"Active"}}`

// liveConfigMap is a live ConfigMap with no client-side writer: only a
// server-side apply manager.
const liveConfigMap = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"demo",
 "managedFields":[{"manager":"kubectl","operation":"Apply","apiVersion":"v1","fieldsType":"FieldsV1",
   "fieldsV1":{"f:data":{"f:secret":{}}}}]},"data":{"secret":"` + csaCanary + `"}}`

// evalCSATemplate runs csaTemplate the way kubectl get -o jsonpath does
// (k8s.io/cli-runtime printers.JSONPathPrinter): the object as JSON data,
// missing keys allowed (--allow-missing-template-keys defaults to true).
func evalCSATemplate(t *testing.T, object string) string {
	t.Helper()
	var data any
	if err := json.Unmarshal([]byte(object), &data); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	j := jsonpath.New("out")
	j.AllowMissingKeys(true)
	if err := j.Parse(csaTemplate); err != nil {
		t.Fatalf("the template does not parse: %v", err)
	}
	var buf bytes.Buffer
	if err := j.Execute(&buf, data); err != nil {
		t.Fatalf("the template does not run: %v", err)
	}
	return buf.String()
}

const nsLines = "O\tv1\tNamespace\t\tdemo\n" +
	"M\tkubectl-client-side-apply\tUpdate\t\tv1\t{\".\":{},\"f:app\":{},\"f:kubernetes.io/metadata.name\":{},\"f:team\":{}}\t{\".\":{},\"f:kubectl.kubernetes.io/last-applied-configuration\":{},\"f:note\":{}}\n"

const cmLines = "O\tv1\tConfigMap\tdemo\tcm\nM\tkubectl\tApply\t\tv1\t\t\n"

// The template, run by the jsonpath package of kubectl, reads a List (what
// kubectl get -f - prints for a stdin, also with one object) and one
// object outside a List. Its output holds names only: never a value of the
// object, also not of the last-applied annotation.
func TestCSATemplateReadsBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name, object, want string
		objects            int
	}{
		{"a List of two", `{"apiVersion":"v1","kind":"List","metadata":{"resourceVersion":""},"items":[` + liveNamespace + `,` + liveConfigMap + `]}`,
			nsLines + cmLines + "O\tv1\tList\t\t\n", 2},
		{"a List of one", `{"apiVersion":"v1","kind":"List","metadata":{"resourceVersion":""},"items":[` + liveNamespace + `]}`,
			nsLines + "O\tv1\tList\t\t\n", 1},
		{"one object", liveNamespace, nsLines, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evalCSATemplate(t, tc.object)
			if got != tc.want {
				t.Fatalf("output:\n%q\nwant:\n%q", got, tc.want)
			}
			if strings.Contains(got, csaCanary) {
				t.Fatal("the read carries a value of the object")
			}
			objs := parseFieldManagers(got)
			if len(objs) != tc.objects {
				t.Fatalf("parsed %d objects, want %d: %+v", len(objs), tc.objects, objs)
			}
			ns := objs[0]
			if ns.kind != "Namespace" || ns.name != "demo" || len(ns.entries) != 1 || ns.entries[0].apiVersion != "v1" ||
				!slices.Equal(ns.entries[0].labels, []string{"app", "kubernetes.io/metadata.name", "team"}) ||
				!slices.Equal(ns.entries[0].annotations, []string{lastAppliedKey, "note"}) {
				t.Errorf("parsed %+v", ns)
			}
		})
	}
}

// csaWarnings runs the check over a read and a manifest and returns the
// [warn] texts: the warn text of each object, and its skip text.
func csaWarnings(read, manifest string) (warn, skip []string) {
	docs, _ := manifestObjects(manifest)
	for _, o := range clientSideObjects(docs, parseFieldManagers(read)) {
		warn = append(warn, o.warnText())
		skip = append(skip, o.skipText())
	}
	return warn, skip
}

const nsManifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n  labels:\n    app: y\n"

// Which objects the check names, and the keys it names: the ones that a
// client-side writer owns and the target does not set, without the keys
// of another manager and without the last-applied annotation. Each name
// that lo prints is escaped. bats: "bootstrap::_csa_objects names the
// writers and the keys that the apply drops".
func TestClientSideObjectsNamesTheDroppedKeys(t *testing.T) {
	const writer = "M\tkubectl-client-side-apply\tUpdate\t\tv1\t"
	const la = `"f:kubectl.kubernetes.io/last-applied-configuration":{}`
	const writerApps = "M\tkubectl-client-side-apply\tUpdate\t\tapps/v1\t"
	const depManifest = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n"
	for _, tc := range []struct {
		name, read, manifest string
		warn                 []string
	}{
		{"the writer's labels and annotations that the target does not set", nsLines, nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team; annotation note"}},
		{"no live object", "", nsManifest, nil},
		{"an Apply entry that owns the annotation is no client-side writer",
			"O\tv1\tNamespace\t\tdemo\nM\tkubectl-last-applied\tApply\t\tv1\t\t{" + la + "}\n", nsManifest, nil},
		{"the target sets every key: no key named",
			"O\tv1\tNamespace\t\tdemo\n" + writer + `{"f:app":{}}` + "\t{" + la + "}\n", nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set"}},
		{"a key that another manager owns too stays",
			"O\tv1\tNamespace\t\tdemo\n" + writer + `{"f:team":{},"f:tier":{}}` + "\t{" + la + "}\nM\tkubectl-label\tUpdate\t\tv1\t{\"f:team\":{}}\t\n", nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label tier"}},
		{"lo's own Apply entry keeps no key",
			"O\tv1\tNamespace\t\tdemo\n" + writer + `{"f:team":{}}` + "\t{" + la + "}\nM\tkubectl\tApply\t\tv1\t{\"f:team\":{}}\t\n", nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team"}},
		{"an entry of the writer with a subresource keeps its keys",
			"O\tv1\tNamespace\t\tdemo\n" + writer + `{"f:team":{}}` + "\t{" + la + "}\nM\tkubectl-client-side-apply\tUpdate\tstatus\tv1\t{\"f:team\":{}}\t\n", nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set"}},
		{"two writers, in order",
			"O\tv1\tNamespace\t\tdemo\nM\ttilt\tUpdate\t\tv1\t{\"f:b\":{},\"f:a\":{}}\t{" + la + "}\n" + writer + "\t{" + la + "}\n", nsManifest,
			[]string{"Namespace demo: client-side writers (kubectl-client-side-apply, tilt) own fields; the apply moves them to lo and drops what the target does not set: labels a, b"}},
		{"a namespaced object matches a manifest object with no namespace",
			"O\tv1\tConfigMap\tdemo\tcm\n" + writer + `{"f:team":{}}` + "\t{" + la + "}\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n",
			[]string{"ConfigMap demo/cm: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team"}},
		{"the name label of a Namespace is set by the API server",
			"O\tv1\tNamespace\t\tdemo\n" + writer + `{"f:kubernetes.io/metadata.name":{},"f:team":{}}` + "\t{" + la + "}\n", nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team"}},
		{"the same label of another kind is named",
			"O\tv1\tConfigMap\tdemo\tcm\n" + writer + `{"f:kubernetes.io/metadata.name":{}}` + "\t{" + la + "}\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n",
			[]string{"ConfigMap demo/cm: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label kubernetes.io/metadata.name"}},
		{"an object of another group is not the manifest object",
			"O\tapps/v1\tNamespace\t\tdemo\n" + writer + `{"f:team":{}}` + "\t{" + la + "}\n", nsManifest, nil},
		{"an entry of the writer with another apiVersion keeps its keys on the object",
			"O\tapps/v1\tDeployment\tdemo\tweb\n" + writerApps + `{"f:team":{}}` + "\t{" + la + "}\n" +
				"M\tkubectl-client-side-apply\tUpdate\t\tapps/v1beta2\t{\"f:tier\":{}}\t\n", depManifest,
			[]string{"Deployment demo/web: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team"}},
		{"such an entry keeps no key of the moved entry",
			"O\tapps/v1\tDeployment\tdemo\tweb\n" + writerApps + `{"f:team":{}}` + "\t{" + la + "}\n" +
				"M\tkubectl-client-side-apply\tUpdate\t\tapps/v1beta2\t{\"f:team\":{}}\t\n", depManifest,
			[]string{"Deployment demo/web: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team"}},
		{"a writer with no entry of the apiVersion of the target moves nothing",
			"O\tapps/v1\tDeployment\tdemo\tweb\nM\tkubectl-client-side-apply\tUpdate\t\tapps/v1beta2\t{\"f:team\":{}}\t{" + la + "}\n", depManifest, nil},
		{"control bytes and backslashes are escaped",
			"O\tv1\tNamespace\t\tdemo\nM\tevil\x1b[31m\\\tUpdate\t\tv1\t{\"f:k\\u0007x\":{}}\t{" + la + "}\n", nsManifest,
			[]string{`Namespace demo: a client-side writer (evil\x1b[31m\\) owns fields; the apply moves them to lo and drops what the target does not set: label k\x07x`}},
		{"a line of another shape is ignored",
			"O\tv1\tNamespace\tdemo\nM\tx\tUpdate\t\tv1\t\t{" + la + "}\nO\tv1\tNamespace\t\tdemo\nM\tshort\tUpdate\n" + writer + "not json\t{" + la + "}\n", nsManifest,
			[]string{"Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warn, skip := csaWarnings(tc.read, tc.manifest)
			if !slices.Equal(warn, tc.warn) {
				t.Fatalf("warn:\n%q\nwant:\n%q", warn, tc.warn)
			}
			for i, s := range skip {
				obj, _, _ := strings.Cut(tc.warn[i], ": ")
				writers := strings.SplitN(tc.warn[i], ": ", 2)[1]
				writers, _, _ = strings.Cut(writers, "; the apply")
				if want := obj + ": left out of the apply (--skip-client-side-owned): " + writers; s != want {
					t.Errorf("skip: %q, want %q", s, want)
				}
			}
		})
	}
}

// csaHandler answers kubectl: reads[i] is the output of read i (the read
// of the field managers), each apply and each diff works (rc 0, no
// output). calls gets each verb, and stdins the stdin of each apply and
// diff.
type csaHandler struct {
	reads   []string
	readErr string // stderr of each read; with it, a read exits 1
	n       int
	calls   []string
	stdins  []string
	envs    [][]string
}

func (h *csaHandler) handle(c execx.Cmd) error {
	if isFieldManagerRead(c) {
		h.calls = append(h.calls, "read")
		h.envs = append(h.envs, c.Env)
		out := ""
		if h.n < len(h.reads) {
			out = h.reads[h.n]
		}
		h.n++
		fmt.Fprint(c.Stdout, out)
		if h.readErr != "" {
			fmt.Fprintln(c.Stderr, h.readErr)
			return &rcError{1}
		}
		return nil
	}
	if len(c.Args) > 2 {
		h.calls = append(h.calls, c.Args[2])
		if c.Stdin != nil {
			raw, _ := io.ReadAll(c.Stdin)
			h.stdins = append(h.stdins, string(raw))
		}
	}
	return nil
}

// The manifest of the engine tests: the Namespace that the fixture read
// names, and a ConfigMap with no client-side writer.
const csaManifest = nsManifest + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: demo\n"

const warnNS = "[warn] bootstrap: testcni: Namespace demo: a client-side writer (kubectl-client-side-apply) owns fields; the apply moves them to lo and drops what the target does not set: label team; annotation note\n"

// A dry run names each object of a client-side writer, and the entry
// counts as "differs" although kubectl diff finds no change: the diff does
// not show what the apply drops. The read runs with the kubectl user
// defaults off. bats: "bootstrap::apply --dry-run: a client-side writer
// makes the entry differ".
func TestDryRunNamesTheObjectsOfAClientSideWriter(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.manifest = csaManifest
	h := &csaHandler{reads: []string{nsLines}}
	f.handler = h.handle
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v, want ErrDryRunDiffers\n%s", err, stderrOf(e))
	}
	if want := "bootstrap dry-run: testcni: differs (client-side owned: 1)\n"; !strings.Contains(stdoutOf(e), want) {
		t.Errorf("stdout:\n%s\nwant %q", stdoutOf(e), want)
	}
	if !strings.Contains(stdoutOf(e), " differs=1 ") {
		t.Errorf("the summary does not count the entry: %s", stdoutOf(e))
	}
	if !strings.Contains(stderrOf(e), warnNS) {
		t.Errorf("stderr:\n%s\nwant %q", stderrOf(e), warnNS)
	}
	if !slices.Equal(h.calls, []string{"read", "diff"}) {
		t.Errorf("kubectl calls = %v, want a read, then the diff", h.calls)
	}
	assertKubeRCOff(t, h.envs)
}

// --skip-client-side-owned: the dry run diffs without the objects of a
// client-side writer, as the real run applies without them, and names
// each object. An entry with no other change is unchanged.
func TestDryRunSkipClientSideOwnedDiffsWithoutThem(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	e.Options.SkipClientSideOwned = true
	f.manifest = csaManifest
	h := &csaHandler{reads: []string{nsLines}}
	f.handler = h.handle
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("err = %v\n%s", err, stderrOf(e))
	}
	if want := "bootstrap dry-run: testcni: unchanged (client-side owned, left out: 1)\n"; !strings.Contains(stdoutOf(e), want) {
		t.Errorf("stdout:\n%s\nwant %q", stdoutOf(e), want)
	}
	if want := "[warn] bootstrap: testcni: Namespace demo: left out of the apply (--skip-client-side-owned): a client-side writer (kubectl-client-side-apply) owns fields\n"; !strings.Contains(stderrOf(e), want) {
		t.Errorf("stderr:\n%s\nwant %q", stderrOf(e), want)
	}
	if len(h.stdins) != 1 || strings.Contains(h.stdins[0], "Namespace") || !strings.Contains(h.stdins[0], "name: cm") {
		t.Errorf("the diff got %q, want the ConfigMap only", h.stdins)
	}
}

// A read that fails: a dry run names it only when its diff worked (a diff
// that failed reports the cluster error itself); a missing CRD is no
// failure. Under --skip-client-side-owned the entry is an error: lo cannot
// find the objects to leave out.
func TestDryRunReadFailure(t *testing.T) {
	const failed = "[warn] bootstrap: testcni: lo cannot read the field managers of the live objects (kubectl get exited 1), so it cannot name the fields that the apply drops\n"
	for _, tc := range []struct {
		name, readErr string
		skip          bool
		wantErr       string
		status        string
	}{
		{"forbidden", "Error from server (Forbidden): namespaces is forbidden", false, failed, "unchanged"},
		{"a missing CRD", "error: resource mapping not found for name: x: no matches for kind", false, "", "unchanged"},
		{"a missing CRD and another error", "error: resource mapping not found for name: x: no matches for kind\nError from server (Forbidden): namespaces is forbidden", false, failed, "unchanged"},
		{"a missing CRD and another error, skip", "error: resource mapping not found for name: x: no matches for kind\nError from server (Forbidden): namespaces is forbidden", true,
			"[error] bootstrap: testcni: lo cannot read the field managers of the live objects (kubectl get exited 1), so --skip-client-side-owned cannot find the objects to leave out\n", "error"},
		{"forbidden, skip", "Error from server (Forbidden): namespaces is forbidden", true,
			"[error] bootstrap: testcni: lo cannot read the field managers of the live objects (kubectl get exited 1), so --skip-client-side-owned cannot find the objects to leave out\n", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, spec, kc := dryEngine(t, "testcni")
			e.Options.SkipClientSideOwned = tc.skip
			h := &csaHandler{readErr: tc.readErr}
			f.handler = h.handle
			_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if want := "bootstrap dry-run: testcni: " + tc.status + "\n"; !strings.Contains(stdoutOf(e), want) {
				t.Errorf("stdout:\n%s\nwant %q", stdoutOf(e), want)
			}
			if got := strings.Contains(stderrOf(e), "lo cannot read the field managers"); got != (tc.wantErr != "") {
				t.Errorf("stderr:\n%s", stderrOf(e))
			}
			if tc.wantErr != "" && !strings.Contains(stderrOf(e), tc.wantErr) {
				t.Errorf("stderr:\n%s\nwant %q", stderrOf(e), tc.wantErr)
			}
			if strings.Contains(stderrOf(e), tc.readErr) {
				t.Error("lo printed the kubectl message of the read")
			}
		})
	}
	// A diff that fails too: the read ran and failed, the entry is an
	// error, and the read failure is not named.
	e, f, spec, kc := dryEngine(t, "testcni")
	var calls []string
	f.handler = func(c execx.Cmd) error {
		if isFieldManagerRead(c) {
			calls = append(calls, "read")
		} else if len(c.Args) > 2 {
			calls = append(calls, c.Args[2])
		}
		fmt.Fprintln(c.Stderr, "error: the server could not be reached")
		return &rcError{2}
	}
	_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !slices.Equal(calls, []string{"read", "diff"}) {
		t.Errorf("kubectl calls = %v, want the read, then the diff", calls)
	}
	if want := "bootstrap dry-run: testcni: error\n"; !strings.Contains(stdoutOf(e), want) {
		t.Errorf("stdout:\n%s\nwant %q", stdoutOf(e), want)
	}
	if strings.Contains(stderrOf(e), "lo cannot read the field managers") {
		t.Errorf("a failed diff also named the read:\n%s", stderrOf(e))
	}
}

func csaJob(t *testing.T) (*Engine, *fakeRunner, Job) {
	t.Helper()
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	f.manifest = csaManifest
	kc := writeKubeconfig(t, p)
	return e, f, Job{Name: "testcni", Dir: filepath.Join(p.Lok8s, "addons", "testcni"), Kubeconfig: kc}
}

// A real run names each object of a client-side writer before the apply,
// and after the apply reads again and names the keys that the apply
// dropped. bats: "bootstrap::_apply_one: a client-side writer is named
// before the apply and the dropped keys after it".
func TestApplyOneNamesTheDroppedKeys(t *testing.T) {
	e, f, job := csaJob(t)
	after := "O\tv1\tNamespace\t\tdemo\n" +
		"M\tkubectl\tApply\t\tv1\t{\"f:app\":{}}\t\n" +
		"M\tkubectl-last-applied\tApply\t\tv1\t\t{\"f:kubectl.kubernetes.io/last-applied-configuration\":{}}\n"
	h := &csaHandler{reads: []string{nsLines, after}}
	f.handler = h.handle
	var errOut bytes.Buffer
	if rc := e.applyOne(t.Context(), job, io.Discard, &errOut); rc != 0 {
		t.Fatalf("rc = %d\n%s", rc, errOut.String())
	}
	if !slices.Equal(h.calls, []string{"read", "apply", "read"}) {
		t.Errorf("kubectl calls = %v, want a read, the apply, a read", h.calls)
	}
	want := warnNS + "[warn] bootstrap: testcni: Namespace demo: the apply dropped label team; annotation note\n"
	if errOut.String() != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", errOut.String(), want)
	}
	if len(h.stdins) != 1 || !strings.Contains(h.stdins[0], "kind: Namespace") {
		t.Errorf("the apply got %q, want the whole render", h.stdins)
	}
}

// No object of a client-side writer: one read, no read after the apply,
// no line.
func TestApplyOneWithoutAClientSideWriterReadsOnce(t *testing.T) {
	e, f, job := csaJob(t)
	h := &csaHandler{reads: []string{cmLines}}
	f.handler = h.handle
	var errOut bytes.Buffer
	if rc := e.applyOne(t.Context(), job, io.Discard, &errOut); rc != 0 || errOut.Len() != 0 {
		t.Fatalf("rc = %d, stderr:\n%s", rc, errOut.String())
	}
	if !slices.Equal(h.calls, []string{"read", "apply"}) {
		t.Errorf("kubectl calls = %v, want a read and the apply", h.calls)
	}
}

// --skip-client-side-owned: the apply gets the render without the objects
// of a client-side writer, each named. With no object left there is no
// apply. A read that fails stops the entry before the apply. Without the
// flag, a read that fails is a [warn] line, and the apply runs.
func TestApplyOneSkipClientSideOwned(t *testing.T) {
	t.Run("the rest is applied", func(t *testing.T) {
		e, f, job := csaJob(t)
		e.Options.SkipClientSideOwned = true
		h := &csaHandler{reads: []string{nsLines}}
		f.handler = h.handle
		var errOut bytes.Buffer
		if rc := e.applyOne(t.Context(), job, io.Discard, &errOut); rc != 0 {
			t.Fatalf("rc = %d\n%s", rc, errOut.String())
		}
		if !slices.Equal(h.calls, []string{"read", "apply"}) || strings.Contains(h.stdins[0], "Namespace") || !strings.Contains(h.stdins[0], "name: cm") {
			t.Errorf("calls %v, the apply got %q", h.calls, h.stdins)
		}
		if want := "[warn] bootstrap: testcni: Namespace demo: left out of the apply (--skip-client-side-owned): a client-side writer (kubectl-client-side-apply) owns fields\n"; errOut.String() != want {
			t.Errorf("stderr:\n%s\nwant %q", errOut.String(), want)
		}
	})
	t.Run("nothing is left", func(t *testing.T) {
		e, f, job := csaJob(t)
		e.Options.SkipClientSideOwned = true
		f.manifest = nsManifest
		h := &csaHandler{reads: []string{nsLines}}
		f.handler = h.handle
		if rc := e.applyOne(t.Context(), job, io.Discard, io.Discard); rc != 0 {
			t.Fatalf("rc = %d", rc)
		}
		if !slices.Equal(h.calls, []string{"read"}) {
			t.Errorf("kubectl calls = %v, want the read only", h.calls)
		}
	})
	t.Run("the read fails", func(t *testing.T) {
		e, f, job := csaJob(t)
		e.Options.SkipClientSideOwned = true
		h := &csaHandler{readErr: "Error from server (Forbidden)"}
		f.handler = h.handle
		var errOut bytes.Buffer
		if rc := e.applyOne(t.Context(), job, io.Discard, &errOut); rc != 1 {
			t.Fatalf("rc = %d, want 1", rc)
		}
		if !slices.Equal(h.calls, []string{"read"}) || !strings.Contains(errOut.String(), "so --skip-client-side-owned cannot find the objects to leave out") {
			t.Errorf("calls %v, stderr:\n%s", h.calls, errOut.String())
		}
	})
	t.Run("the read fails without the flag", func(t *testing.T) {
		e, f, job := csaJob(t)
		h := &csaHandler{readErr: "Error from server (Forbidden)"}
		f.handler = h.handle
		var errOut bytes.Buffer
		if rc := e.applyOne(t.Context(), job, io.Discard, &errOut); rc != 0 {
			t.Fatalf("rc = %d", rc)
		}
		want := "[warn] bootstrap: testcni: lo cannot read the field managers of the live objects (kubectl get exited 1), so it cannot name the fields that the apply drops\n"
		if !slices.Equal(h.calls, []string{"read", "apply"}) || errOut.String() != want {
			t.Errorf("calls %v, stderr:\n%s", h.calls, errOut.String())
		}
	})
}

// The exit of a dry run with an object of a client-side writer is 1, as
// for a diff with changes.
func TestDryRunClientSideExitCode(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.manifest = csaManifest
	f.handler = (&csaHandler{reads: []string{nsLines}}).handle
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); driver.ExitCode(err) != 1 {
		t.Fatalf("rc = %d (%v), want 1", driver.ExitCode(err), err)
	}
}

// A read that failed counts as the error of a missing CRD only when each
// line of its stderr is one, empty, or a warning of the API server. bats:
// "bootstrap::_csa_race_only: only the lines of a missing CRD and
// warnings".
func TestCSARaceOnly(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"error: resource mapping not found for name: \"x\" namespace: \"\" from \"STDIN\": no matches for kind \"X\" in version \"example.com/v1\"\nensure CRDs are installed first", true},
		{"Warning: v1 ComponentStatus is deprecated in v1.19+\n\nerror: resource mapping not found for name: x: no matches for kind", true},
		{"error: resource mapping not found for name: x: no matches for kind\nError from server (Forbidden): namespaces is forbidden", false},
		{"Error from server (Forbidden): namespaces is forbidden\nerror: resource mapping not found for name: x: no matches for kind", false},
		{"", false},
		{"Warning: v1 ComponentStatus is deprecated in v1.19+", false},
	} {
		if got := raceOnly(tc.stderr); got != tc.want {
			t.Errorf("raceOnly(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}

// A manifest that lo cannot read: under --skip-client-side-owned the entry
// fails, else a [warn] line. No object is named or left out, and lo prints
// no message of the YAML decoder. bats: "bootstrap::_csa_before: a
// manifest that does not parse".
func TestClientSideBeforeManifestThatDoesNotParse(t *testing.T) {
	const bad = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n  labels: [\n"
	for _, tc := range []struct {
		skip bool
		ok   bool
		want string
	}{
		{true, false, "[error] bootstrap: testcni: lo cannot read the objects of the manifest, so --skip-client-side-owned cannot find the objects to leave out\n"},
		{false, true, "[warn] bootstrap: testcni: lo cannot read the objects of the manifest, so it cannot name the fields that the apply drops\n"},
	} {
		e, f, job := csaJob(t)
		e.Options.SkipClientSideOwned = tc.skip
		h := &csaHandler{reads: []string{nsLines}}
		f.handler = h.handle
		var errOut bytes.Buffer
		c, ok := e.clientSideBefore(t.Context(), &errOut, job.Name, job.Kubeconfig, bad)
		if ok != tc.ok || errOut.String() != tc.want || len(c.objects) != 0 || c.leftOut != 0 {
			t.Errorf("skip %v: ok %v, objects %d, left out %d, stderr:\n%s\nwant %q", tc.skip, ok, len(c.objects), c.leftOut, errOut.String(), tc.want)
		}
		if !slices.Equal(h.calls, []string{"read"}) {
			t.Errorf("skip %v: kubectl calls = %v, want the read", tc.skip, h.calls)
		}
	}
}
