package hooks

// hooks_test.go — hermetic tests for the hooks port, mirroring
// tests/unit/hooks_test.bats: selector validation (security critical — the
// grammar guards what reaches the cluster), the artifact label-filter, image
// preservation across recreate, and the Tilt-owns-the-recreate path. All
// kubectl/tilt calls run through the fake runner.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/tilt"
)

type fakeExit struct{ code int }

func (e *fakeExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *fakeExit) ExitCode() int { return e.code }

type fakeRunner struct {
	calls   []string
	stdins  []string
	handler func(c execx.Cmd, stdin string) error
}

func (r *fakeRunner) Run(_ context.Context, c execx.Cmd) error {
	stdin := ""
	if c.Stdin != nil {
		var buf bytes.Buffer
		buf.ReadFrom(c.Stdin)
		stdin = buf.String()
	}
	r.calls = append(r.calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
	r.stdins = append(r.stdins, stdin)
	if r.handler != nil {
		return r.handler(c, stdin)
	}
	return nil
}

func (r *fakeRunner) matching(sub string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

func testCtx(t *testing.T) (*Context, *fakeRunner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	paths := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	runner := &fakeRunner{}
	var out, errOut bytes.Buffer
	c := &Context{Paths: paths, Runner: runner, Out: &out, ErrOut: &errOut, Domain: "d.dev"}
	// The command layer always exports this (bash: main::hooks); the
	// applier must see it so heals fail fast instead of prompting.
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	for _, v := range []string{"TILT_PORT", "DOMAIN_NAME", "LOK8S_FORCE_RECREATE", "DEBUG"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	t.Setenv("TILT_PORT", "14242")
	c.Tilt = &tilt.Context{Paths: paths, Runner: runner, Out: &out, ErrOut: &errOut}
	c.Applier = kapply.NewApplier(runner, &out, &errOut)
	os.MkdirAll(filepath.Join(paths.Clusters, "d.dev"), 0o755)
	return c, runner, &out, &errOut
}

func writeArtifact(t *testing.T, c *Context, content string) {
	t.Helper()
	os.WriteFile(filepath.Join(c.Paths.Clusters, "d.dev", "artifacts.yaml"), []byte(content), 0o644)
}

const twoJobs = `apiVersion: batch/v1
kind: Job
metadata:
  name: zitadel-provision
  labels:
    lok8s.dev/role: seed
    lok8s.dev/name: zitadel
---
apiVersion: batch/v1
kind: Job
metadata:
  name: zitadel-setup
  labels:
    lok8s.dev/name: zitadel
`

// ── selector validation (bash: hooks::_yq_filter) ────────

func TestSelectorMultiLabelANDs(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeArtifact(t, c, twoJobs)
	docs, err := c.selectObjects("d.dev", "lok8s.dev/name=zitadel,lok8s.dev/role=seed")
	if err != nil {
		t.Fatal(err)
	}
	if names := objectNames(docs); len(names) != 1 || names[0] != "zitadel-provision" {
		t.Errorf("names = %v (clauses must AND)", names)
	}
}

func TestSelectorRejectsInjection(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	if _, err := c.parseSelector("a=b;rm -rf /"); err != ErrHandled {
		t.Fatal("injection must be rejected")
	}
	if !strings.Contains(errOut.String(), "hooks: invalid selector clause 'a=b;rm -rf /' (key/value must be [a-zA-Z0-9._/-])") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSelectorRejectsClauseWithoutEquals(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	if _, err := c.parseSelector("noequalshere"); err != ErrHandled {
		t.Fatal("clause without '=' must be rejected")
	}
	if !strings.Contains(errOut.String(), "hooks: selector clause 'noequalshere' must be key=value") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSelectorRejectsEmpty(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	if _, err := c.parseSelector(""); err != ErrHandled {
		t.Fatal("empty selector must be rejected")
	}
	if !strings.Contains(errOut.String(), "hooks: --selector is required") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSelectorAcceptsSlashInValue(t *testing.T) {
	// Regression (bats round-1 fix): the value branch used to reject '/',
	// contradicting the error message and the Starlark _SEL_CHARS.
	c, _, _, _ := testCtx(t)
	if _, err := c.parseSelector("role=a/b"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestSelectorRejectsSpaceInValue(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	if _, err := c.parseSelector("role=a b"); err != ErrHandled {
		t.Fatal("space must be rejected (arg-split / injection)")
	}
	if !strings.Contains(errOut.String(), "invalid selector clause") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// ── hooks::_select ───────────────────────────────────────

func TestSelectFiltersByLabel(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeArtifact(t, c, twoJobs)
	docs, err := c.selectObjects("d.dev", "lok8s.dev/role=seed")
	if err != nil {
		t.Fatal(err)
	}
	out := marshalDocs(docs)
	if !strings.Contains(out, "zitadel-provision") || strings.Contains(out, "zitadel-setup") {
		t.Errorf("selected = %q", out)
	}
}

func TestSelectEmptyWhenNothingMatches(t *testing.T) {
	c, _, _, _ := testCtx(t)
	writeArtifact(t, c, "kind: Job\nmetadata:\n  name: x\n  labels: {lok8s.dev/name: other}\n")
	docs, err := c.selectObjects("d.dev", "lok8s.dev/role=seed")
	if err != nil || len(docs) != 0 {
		t.Errorf("docs = %v, err = %v", docs, err)
	}
}

func TestSelectMissingArtifactSelectsNothing(t *testing.T) {
	c, _, _, _ := testCtx(t)
	docs, err := c.selectObjects("d.dev", "a=b")
	if err != nil || len(docs) != 0 {
		t.Errorf("docs = %v, err = %v", docs, err)
	}
}

func TestSelectTypedLabelNeverMatchesStringValue(t *testing.T) {
	// yq `==` is type-strict: an unquoted `count: 5` int label never equals
	// the selector string "5".
	c, _, _, _ := testCtx(t)
	writeArtifact(t, c, "kind: Job\nmetadata:\n  name: x\n  labels: {count: 5}\n")
	docs, _ := c.selectObjects("d.dev", "count=5")
	if len(docs) != 0 {
		t.Error("int-typed label must not match the string selector (yq == semantics)")
	}
}

// ── image preservation across recreate ───────────────────

const renderedJob = `apiVersion: batch/v1
kind: Job
metadata:
  name: mig
  namespace: ns
spec:
  template:
    spec:
      containers:
        - name: migrate
          image: lok8s.local/kubehz-api-migrate
`

func TestOverlayImagesReplacesByContainerName(t *testing.T) {
	docs := parseDocs([]byte(renderedJob))
	overlayImages(docs[0], []liveImage{{name: "migrate", image: "reg/lok8s.local_kubehz-api-migrate:tilt-abc"}})
	out := marshalDocs(docs)
	if !strings.Contains(out, "reg/lok8s.local_kubehz-api-migrate:tilt-abc") ||
		strings.Contains(out, "image: lok8s.local/kubehz-api-migrate") {
		t.Errorf("overlaid = %q", out)
	}
}

func TestOverlayImagesKeepsContainersAbsentFromLive(t *testing.T) {
	docs := parseDocs([]byte(`kind: Job
metadata:
  name: mig
spec:
  template:
    spec:
      containers:
        - name: migrate
          image: lok8s.local/a
        - name: sidecar
          image: lok8s.local/b
`))
	overlayImages(docs[0], []liveImage{{name: "migrate", image: "reg/a:tilt-1"}})
	out := marshalDocs(docs)
	if !strings.Contains(out, "reg/a:tilt-1") || !strings.Contains(out, "lok8s.local/b") {
		t.Errorf("overlaid = %q", out)
	}
}

func TestOverlayImagesCoversInitContainers(t *testing.T) {
	docs := parseDocs([]byte(`kind: Deployment
metadata:
  name: d
spec:
  template:
    spec:
      initContainers:
        - name: wait
          image: lok8s.local/wait
      containers:
        - name: app
          image: lok8s.local/app
`))
	overlayImages(docs[0], []liveImage{{name: "wait", image: "reg/wait:tilt-9"}, {name: "app", image: "reg/app:tilt-9"}})
	out := marshalDocs(docs)
	if !strings.Contains(out, "reg/wait:tilt-9") || !strings.Contains(out, "reg/app:tilt-9") {
		t.Errorf("overlaid = %q", out)
	}
}

func TestOverlayImagesEmptyLiveListIsANoop(t *testing.T) {
	docs := parseDocs([]byte(renderedJob))
	overlayImages(docs[0], nil)
	if !strings.Contains(marshalDocs(docs), "lok8s.local/kubehz-api-migrate") {
		t.Error("rendered ref must survive an empty live list")
	}
}

func TestLiveImagesMissingObjectYieldsEmpty(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	runner.handler = func(execx.Cmd, string) error { return &fakeExit{1} }
	if imgs := c.liveImages(context.Background(), "Job", "nope", "ns"); len(imgs) != 0 {
		t.Errorf("imgs = %v", imgs)
	}
}

// ── object names + Tilt ownership ────────────────────────

func TestObjectNamesOnePerDocument(t *testing.T) {
	docs := parseDocs([]byte("kind: Job\nmetadata:\n  name: mig-a\n---\nkind: Job\nmetadata:\n  name: mig-b\n"))
	names := objectNames(docs)
	if len(names) != 2 || names[0] != "mig-a" || names[1] != "mig-b" {
		t.Errorf("names = %v", names)
	}
}

func TestTiltCanRecreateAllOrNothing(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "tilt" && len(cmd.Args) > 2 && cmd.Args[1] == "uiresource" {
			if cmd.Args[2] == "known" {
				return nil
			}
			return &fakeExit{1}
		}
		return nil
	}
	known := parseDocs([]byte("kind: Job\nmetadata:\n  name: known\n"))
	if !c.tiltCanRecreate(context.Background(), known, "14242") {
		t.Error("single known object must recreate through Tilt")
	}
	mixed := parseDocs([]byte("kind: Job\nmetadata:\n  name: known\n---\nkind: Job\nmetadata:\n  name: other\n"))
	if c.tiltCanRecreate(context.Background(), mixed, "14242") {
		t.Error("Tilt must own EVERY object (all-or-nothing)")
	}
	if c.tiltCanRecreate(context.Background(), nil, "14242") {
		t.Error("an empty selection is not Tilt-recreatable")
	}
}

// ── recreate / apply / restart flows ─────────────────────

func TestRecreateNoMatchWarnsAndSucceeds(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeArtifact(t, c, twoJobs)
	if err := c.Recreate(context.Background(), "lok8s.dev/role=ghost"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "hooks recreate: no objects match 'lok8s.dev/role=ghost'") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("nothing may run on no-match: %v", runner.calls)
	}
}

func TestRecreateThroughTiltWhenSessionOwnsObjects(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, twoJobs)
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "kubectl" {
			t.Errorf("kubectl must not run on the Tilt path: %v", cmd.Args)
		}
		return nil // session up, uiresources known, triggers succeed
	}
	if err := c.Recreate(context.Background(), "lok8s.dev/name=zitadel"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tilt trigger zitadel-provision --port 14242",
		"tilt trigger zitadel-setup --port 14242",
	}
	got := runner.matching("trigger")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("trigger calls = %v", got)
	}
}

func TestRecreateTiltTriggerFailureIsLoud(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeArtifact(t, c, twoJobs)
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "tilt" && cmd.Args[0] == "trigger" {
			return &fakeExit{1}
		}
		return nil
	}
	if err := c.Recreate(context.Background(), "lok8s.dev/role=seed"); err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "hooks recreate: tilt trigger failed for 'zitadel-provision'") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRecreateFallsBackToDeleteApplyPreservingLiveImages(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, renderedJob+"---\n"+`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
  labels:
    keep: "yes"
`)
	// Label the Job so the selector finds it.
	writeArtifact(t, c, strings.Replace(renderedJob, "name: mig\n", "name: mig\n  labels: {run: mig}\n", 1))
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "tilt" && cmd.Args[0] == "get" && cmd.Args[1] == "session" {
			return &fakeExit{1} // no session → kubectl path
		}
		if cmd.Name == "kubectl" && cmd.Args[0] == "get" {
			writeOut(cmd, `{"spec":{"template":{"spec":{"containers":[{"name":"migrate","image":"reg/mig:tilt-7"}]}}}}`)
			return nil
		}
		return nil
	}
	if err := c.Recreate(context.Background(), "run=mig"); err != nil {
		t.Fatal(err)
	}
	// delete streams the RENDERED objects; apply streams the OVERLAID ones.
	var deleteIn, applyIn string
	for i, call := range runner.calls {
		if strings.HasPrefix(call, "kubectl delete --ignore-not-found") {
			deleteIn = runner.stdins[i]
		}
		if strings.Contains(call, "apply --server-side") {
			applyIn = runner.stdins[i]
		}
	}
	if !strings.Contains(deleteIn, "lok8s.local/kubehz-api-migrate") {
		t.Errorf("delete stdin = %q", deleteIn)
	}
	if !strings.Contains(applyIn, "reg/mig:tilt-7") || strings.Contains(applyIn, "image: lok8s.local/kubehz-api-migrate") {
		t.Errorf("apply stdin must carry the live ref: %q", applyIn)
	}
}

func TestApplyNoDeleteJustKapply(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, twoJobs)
	if err := c.Apply(context.Background(), "lok8s.dev/role=seed"); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("delete"); len(got) != 0 {
		t.Errorf("apply must not delete: %v", got)
	}
	got := runner.matching("apply --server-side --force-conflicts")
	if len(got) != 1 {
		t.Errorf("apply calls = %v", runner.calls)
	}
}

func TestApplyFailureWithoutForceFailsFast(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeArtifact(t, c, twoJobs)
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if cmd.Name == "kubectl" && cmd.Args[0] == "apply" {
			writeOut(cmd, "Error from server (Invalid): Job \"zitadel-provision\" is invalid: field is immutable\n")
			return &fakeExit{1}
		}
		return nil
	}
	if err := c.Apply(context.Background(), "lok8s.dev/role=seed"); err != ErrHandled {
		t.Fatalf("err = %v", err)
	}
	// Non-interactive without --force-recreate → the remediation hint, no
	// heal, no retry loop.
	if !strings.Contains(errOut.String(), "re-run with --force-recreate to recreate the affected objects") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if got := runner.matching("replace --force"); len(got) != 0 {
		t.Errorf("heal must not run without consent: %v", got)
	}
}

func TestRestartRolloutRestartsWorkloads(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, `kind: Deployment
metadata:
  name: web
  namespace: apps
  labels: {app: web}
---
kind: Job
metadata:
  name: not-restartable
  labels: {app: web}
`)
	if err := c.Restart(context.Background(), "app=web"); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("rollout restart"); len(got) != 1 || got[0] != "kubectl -n apps rollout restart deployment/web" {
		t.Errorf("rollout calls = %v", got)
	}
}

func TestRestartNamespaceDefaultsToDefault(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	writeArtifact(t, c, "kind: StatefulSet\nmetadata:\n  name: db\n  labels: {app: db}\n")
	if err := c.Restart(context.Background(), "app=db"); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("rollout restart"); len(got) != 1 || got[0] != "kubectl -n default rollout restart statefulset/db" {
		t.Errorf("rollout calls = %v", got)
	}
}

func TestRestartNoneRestartableWarns(t *testing.T) {
	c, _, _, errOut := testCtx(t)
	writeArtifact(t, c, "kind: Job\nmetadata:\n  name: j\n  labels: {app: j}\n")
	if err := c.Restart(context.Background(), "app=j"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "hooks restart: matched objects but none are restartable (Deployment/StatefulSet/DaemonSet) for 'app=j'") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRestartFailedWorkloadWarnsButCountsOthers(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	writeArtifact(t, c, `kind: Deployment
metadata:
  name: ok
  labels: {app: x}
---
kind: DaemonSet
metadata:
  name: broken
  labels: {app: x}
`)
	runner.handler = func(cmd execx.Cmd, _ string) error {
		if strings.Contains(strings.Join(cmd.Args, " "), "daemonset/broken") {
			return &fakeExit{1}
		}
		return nil
	}
	if err := c.Restart(context.Background(), "app=x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "hooks restart: broken") &&
		!strings.Contains(errOut.String(), "hooks restart: DaemonSet/broken (-n default) failed") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func writeOut(c execx.Cmd, s string) {
	if c.Stdout != nil {
		fmt.Fprint(c.Stdout, s)
	}
}
