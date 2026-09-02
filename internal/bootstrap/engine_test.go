package bootstrap

// engine_test.go — the Go port of the bootstrap::apply half of
// tests/unit/bootstrap_test.bats: the values-precedence chain through the
// real render staging, the topological-parallel scheduler (barriers,
// dependsOn, failure isolation, anti-starvation), the park + batched-heal
// flow, the buffered de-interleaved flush, the hosted gates, and the
// KubeOne cilium/ccm reconcile gate.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// writeStackAddon lays down the bats three-layer testcni fixture.
func writeStackAddon(t *testing.T, p *config.Paths) {
	t.Helper()
	dir := mkChartAddon(t, p, "testcni")
	writeFile(t, filepath.Join(dir, "values.yaml"),
		"only_base: \"base\"\nshared_all: \"base\"\nnested:\n  from_base: true\n  overridden: \"base\"\n")
	writeFile(t, filepath.Join(dir, "values.lo.yaml"),
		"only_driver: \"driver\"\nshared_all: \"driver\"\nnested:\n  from_driver: true\n  overridden: \"driver\"\n")
	writeFile(t, filepath.Join(dir, "values.hetzner.yaml"),
		"only_provider: \"provider\"\nshared_all: \"provider\"\nnested:\n  from_provider: true\n  overridden: \"provider\"\n")
}

func TestApplyMergesBaseDriverProvider(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	spec := writeClusterSpec(t, p, "testcni")
	kc := writeKubeconfig(t, p)

	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	merged := f.mergedOut
	if merged == "" {
		t.Fatal("no merged values staged")
	}
	for path, want := range map[string]string{
		"only_base": "base", "only_driver": "driver", "only_provider": "provider",
		"shared_all":         "provider", // provider wins over driver wins over base
		"nested.from_base":   "true",
		"nested.from_driver": "true", "nested.from_provider": "true",
		"nested.overridden": "provider",
	} {
		if got := yqLike(t, merged, path); got != want {
			t.Errorf("merged %s = %q, want %q\n%s", path, got, want, merged)
		}
	}
}

func TestApplyInlineBeatsAllLayers(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	spec := writeClusterSpec(t, p, "testcni: {shared_all: inline, nested: {overridden: inline, from_inline: true}}")
	kc := writeKubeconfig(t, p)

	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for path, want := range map[string]string{
		"shared_all": "inline", "nested.overridden": "inline",
		"only_base": "base", "only_driver": "driver", "only_provider": "provider",
		"nested.from_base": "true", "nested.from_driver": "true",
		"nested.from_provider": "true", "nested.from_inline": "true",
	} {
		if got := yqLike(t, f.mergedOut, path); got != want {
			t.Errorf("merged %s = %q, want %q", path, got, want)
		}
	}
}

func TestApplyValueFilesBeatProviderLoseToInline(t *testing.T) {
	// The full precedence chain through the real render staging:
	// base < driver < provider < valueFiles < values:
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	writeValueFile(t, p, "values/testcni.dev.yaml",
		"shared_all: \"valuefile\"\nonly_valuefile: \"valuefile\"\nnested:\n  overridden: \"valuefile\"\n  from_valuefile: true\n")
	spec := writeClusterSpec(t, p, "testcni: {valueFiles: [./values/testcni.dev.yaml], values: {shared_all: inline, nested: {overridden: inline}}}")
	kc := writeKubeconfig(t, p)

	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for path, want := range map[string]string{
		"shared_all": "inline", "nested.overridden": "inline",
		"only_valuefile": "valuefile", "nested.from_valuefile": "true",
		"only_base": "base", "only_driver": "driver", "only_provider": "provider",
	} {
		if got := yqLike(t, f.mergedOut, path); got != want {
			t.Errorf("merged %s = %q, want %q", path, got, want)
		}
	}
	if got := yqLike(t, f.mergedOut, "valueFiles"); got != "missing" {
		t.Errorf("reserved key leaked into helm values: %q", got)
	}
}

func TestApplyDefaultsToCiliumWhenBootstrapAbsent(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	dir := mkChartAddon(t, p, "cilium")
	writeFile(t, filepath.Join(dir, "values.yaml"), "marker: \"cilium-default\"\n")
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: e2e-test\nspec:\n  provider:\n    name: hetzner\n")
	kc := writeKubeconfig(t, p)

	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.buildDirs) != 1 {
		t.Fatalf("kustomize invoked %d times, want 1 (default cilium)", len(f.buildDirs))
	}
	if got := yqLike(t, f.mergedOut, "marker"); got != "cilium-default" {
		t.Errorf("marker = %q", got)
	}
}

func TestApplySkipsOnExplicitEmptyList(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, "kind: Kkp\nmetadata:\n  name: e2e-test\nspec:\n  provider:\n    name: hetzner\n  bootstrap: []\n")
	kc := writeKubeconfig(t, p)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.buildDirs) != 0 {
		t.Errorf("kustomize invoked on an explicit opt-out: %v", f.buildDirs)
	}
}

func TestApplyFailsWhenKubeconfigMissing(t *testing.T) {
	e, _, _, errOut, p := testEngine(t)
	writeStackAddon(t, p)
	spec := writeClusterSpec(t, p, "testcni")
	err := e.Apply(context.Background(), "test.lok8s.dev", spec, p.Base+"/.kubeconfig/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "kubeconfig not found") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestApplyFailsWhenAddonDirMissing(t *testing.T) {
	e, _, _, errOut, p := testEngine(t)
	spec := writeClusterSpec(t, p, "doesnotexist")
	kc := writeKubeconfig(t, p)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "addon not found") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestApplyEnvOverridesReachTheRender(t *testing.T) {
	// The `env:` map rides Job.EnvLines into the render's process env +
	// envsubst lookup (bash: exported in the entry's subshell only).
	e, f, _, _, p := testEngine(t)
	mkChartAddon(t, p, "testcni")
	spec := writeClusterSpec(t, p, "testcni:\n      env:\n        LOK8S_USER_TESTVAR: hello")
	kc := writeKubeconfig(t, p)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.kustomizeEnvs) != 1 {
		t.Fatalf("kustomize calls = %d", len(f.kustomizeEnvs))
	}
	found := false
	for _, kv := range f.kustomizeEnvs[0] {
		if kv == "LOK8S_USER_TESTVAR=hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("env override missing from render env: %v", f.kustomizeEnvs[0])
	}
	if os.Getenv("LOK8S_USER_TESTVAR") != "" {
		t.Errorf("per-entry env leaked into the process environment")
	}
}

// --- scheduler: barriers, DAG, failure isolation -----------------------------

// stubApply builds an ApplyOne stub that logs START/END events with a fixed
// per-entry sleep, failing the named entries.
func stubApply(log *eventLog, delay time.Duration, fail ...string) func(context.Context, Job, io.Writer, io.Writer) int {
	failSet := map[string]bool{}
	for _, f := range fail {
		failSet[f] = true
	}
	return func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add("START " + job.Name)
		time.Sleep(delay)
		log.add("END " + job.Name)
		if failSet[job.Name] {
			return 1
		}
		return 0
	}
}

func schedEngine(t *testing.T, entries ...string) (*Engine, *eventLog, string, string, *config.Paths) {
	t.Helper()
	e, _, _, _, p := testEngine(t)
	names := map[string]bool{}
	for _, en := range entries {
		name, _, _ := strings.Cut(en, ":")
		names[strings.TrimSpace(name)] = true
	}
	for n := range names {
		mkAddonDirs(t, p, n)
	}
	spec := writeClusterSpec(t, p, entries...)
	kc := writeKubeconfig(t, p)
	return e, &eventLog{}, spec, kc, p
}

func TestSchedulerBarrierSerializesWaitTrue(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, log, spec, kc, _ := schedEngine(t, "a", "b", "c: { wait: true }", "d", "e")
	e.ApplyOne = stubApply(log, 60*time.Millisecond)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// a and b overlap: each STARTED before the other ENDED.
	if !(log.pos("START a") < log.pos("END b") && log.pos("START b") < log.pos("END a")) {
		t.Errorf("a/b did not overlap: %v", log.events)
	}
	// Barrier c STARTS only after BOTH a and b have ENDED.
	if !(log.pos("START c") > log.pos("END a") && log.pos("START c") > log.pos("END b")) {
		t.Errorf("barrier c started early: %v", log.events)
	}
	// d and e START only after c has ENDED.
	if !(log.pos("START d") > log.pos("END c") && log.pos("START e") > log.pos("END c")) {
		t.Errorf("post-barrier entries started early: %v", log.events)
	}
}

func TestSchedulerFailedLeafSkipsNothing(t *testing.T) {
	// THE live-run regression pin: a failed LEAF must NOT halt the
	// bootstrap — every unrelated entry still applies, rc non-zero, no
	// entry skipped.
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, log, spec, kc, _ := schedEngine(t, "a", "b", "c")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(log, 10*time.Millisecond, "b")
	err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc)
	if err == nil {
		t.Fatal("expected non-zero (b failed)")
	}
	for _, n := range []string{"a", "b", "c"} {
		if !log.has("END " + n) {
			t.Errorf("entry %s starved: %v", n, log.events)
		}
	}
	if strings.Contains(errOut.String(), "skipping") {
		t.Errorf("a leaf failure skipped something:\n%s", errOut.String())
	}
}

func TestSchedulerParallel1AntiStarvation(t *testing.T) {
	// THE anti-starvation pin: cap=1 forces one entry per wave; `a` fails
	// and is reaped ALONE before `b` ever gets a slot. `b` must STILL
	// launch in the next wave (the old stop-the-world break starved it).
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "1")
	e, log, spec, kc, _ := schedEngine(t, "a", "b")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(log, 10*time.Millisecond, "a")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected non-zero (a failed)")
	}
	if !log.has("END a") || !log.has("END b") {
		t.Errorf("b was starved behind a's failure: %v", log.events)
	}
	if strings.Contains(errOut.String(), "skipping") {
		t.Errorf("leaf failure skipped something:\n%s", errOut.String())
	}
}

func TestSchedulerThrottleFreesAnySlot(t *testing.T) {
	// With a low cap and one slow leading entry, the faster trailing
	// entries must still complete promptly — never blocked on the oldest.
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "2")
	e, log, spec, kc, _ := schedEngine(t, "a", "b", "c", "d")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		if job.Name == "a" {
			time.Sleep(200 * time.Millisecond)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		log.add(job.Name)
		return 0
	}
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if log.count() != 4 {
		t.Fatalf("completed %d of 4: %v", log.count(), log.events)
	}
	if log.pos("a") != 4 {
		t.Errorf("slow 'a' not last — throttle blocked on the oldest: %v", log.events)
	}
}

func TestSchedulerDependsOnGatesDependentOnly(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, log, spec, kc, _ := schedEngine(t, "a", "b:\n      dependsOn: [a]", "c")
	e.ApplyOne = stubApply(log, 60*time.Millisecond)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// a and c overlap (c is NOT gated behind a).
	if !(log.pos("START a") < log.pos("END c") && log.pos("START c") < log.pos("END a")) {
		t.Errorf("a/c did not overlap: %v", log.events)
	}
	// b (dependsOn a) starts only after a has ENDED.
	if log.pos("START b") <= log.pos("END a") {
		t.Errorf("b started before its dependency finished: %v", log.events)
	}
}

func TestSchedulerGatePlusDependsOn(t *testing.T) {
	e, log, spec, kc, _ := schedEngine(t, "g:\n      wait: true", "y", "x:\n      dependsOn: [y]")
	e.ApplyOne = stubApply(log, 40*time.Millisecond)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !(log.pos("START y") > log.pos("END g") && log.pos("START x") > log.pos("END g")) {
		t.Errorf("gate did not gate: %v", log.events)
	}
	if log.pos("START x") <= log.pos("END y") {
		t.Errorf("x started before y ended: %v", log.events)
	}
}

func TestSchedulerOnlyDepTargetsGetTheReadinessWait(t *testing.T) {
	// Job.WaitFlag non-empty ⇔ the post-apply readiness wait runs. The
	// scheduler must set it for a dep-target and leave it empty for a pure
	// leaf.
	e, _, spec, kc, _ := schedEngine(t, "a", "b:\n      dependsOn: [a]", "c")
	waited := &eventLog{}
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		if job.WaitFlag != "" {
			waited.add(job.Name)
		}
		return 0
	}
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !waited.has("a") {
		t.Errorf("dep-target a did not wait: %v", waited.events)
	}
	if waited.has("b") || waited.has("c") {
		t.Errorf("pure leaves waited: %v", waited.events)
	}
}

func TestSchedulerFailedDependencySkipsDependentNotSiblings(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, log, spec, kc, _ := schedEngine(t, "a", "b:\n      dependsOn: [a]", "c")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(log, 10*time.Millisecond, "a")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected non-zero")
	}
	if !log.has("END a") || !log.has("END c") {
		t.Errorf("independent sibling starved: %v", log.events)
	}
	if log.has("START b") {
		t.Errorf("dependent of a failed entry was applied: %v", log.events)
	}
	if !strings.Contains(errOut.String(), "skipping 'b' — a dependency failed (a)") {
		t.Errorf("skip not logged with cause:\n%s", errOut.String())
	}
}

func TestSchedulerTransitiveDependentsAllSkipped(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, log, spec, kc, _ := schedEngine(t, "a", "b:\n      dependsOn: [a]", "c:\n      dependsOn: [b]")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(log, 10*time.Millisecond, "a")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected non-zero")
	}
	if log.has("START b") || log.has("START c") {
		t.Errorf("transitive dependents applied: %v", log.events)
	}
	// Both skips logged, cause = the root failed entry a.
	for _, want := range []string{
		"skipping 'b' — a dependency failed (a)",
		"skipping 'c' — a dependency failed (a)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("missing %q:\n%s", want, errOut.String())
		}
	}
}

func TestSchedulerFailedGateSkipsAllAfter(t *testing.T) {
	e, log, spec, kc, _ := schedEngine(t, "g:\n      wait: true", "x", "y")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(log, 10*time.Millisecond, "g")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected non-zero")
	}
	if log.has("START x") || log.has("START y") {
		t.Errorf("entries behind the failed gate applied: %v", log.events)
	}
	for _, want := range []string{
		"skipping 'x' — a dependency failed (g)",
		"skipping 'y' — a dependency failed (g)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("missing %q:\n%s", want, errOut.String())
		}
	}
}

// --- name: override + collisions at the graph level --------------------------

func TestSchedulerNameOverrideIsTheDependsOnTarget(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, log, spec, kc, p := schedEngine(t, "foo", "./x:\n      name: bar", "c:\n      dependsOn: [bar]")
	os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "x"), 0o755)
	_ = spec
	e.ApplyOne = stubApply(log, 60*time.Millisecond)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The ./x entry is keyed by its OVERRIDE name "bar".
	if !log.has("START bar") {
		t.Fatalf("renamed entry not scheduled by its override: %v", log.events)
	}
	// c (dependsOn bar) starts only AFTER bar ends; foo stays parallel.
	if log.pos("START c") <= log.pos("END bar") {
		t.Errorf("c started before bar ended: %v", log.events)
	}
	if !(log.pos("START foo") < log.pos("END bar") && log.pos("START bar") < log.pos("END foo")) {
		t.Errorf("foo did not overlap bar: %v", log.events)
	}
}

func TestSchedulerNameOverrideReplacesBasename(t *testing.T) {
	e, _, spec, kc, p := schedEngine(t, "foo", "./x:\n      name: bar", "c:\n      dependsOn: [x]")
	os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "x"), 0o755)
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "unknown entry 'x'") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSchedulerAmbiguousCollisionReferencedIsError(t *testing.T) {
	e, _, spec, kc, p := schedEngine(t, "rook-ceph", "./targets/rook-ceph", "consumer:\n      dependsOn: [rook-ceph]")
	os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "targets", "rook-ceph"), 0o755)
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "ambiguous entry 'rook-ceph'") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSchedulerUnreferencedCollisionWarnsOnly(t *testing.T) {
	// The compatibility guarantee: a basename collision NOTHING dependsOn
	// (the barrier-only kubehz config) still applies.
	e, _, spec, kc, p := schedEngine(t, "rook-ceph", "./targets/rook-ceph")
	os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "targets", "rook-ceph"), 0o755)
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(errOut.String(), "duplicate entry name 'rook-ceph'") {
		t.Errorf("collision not warned:\n%s", errOut.String())
	}
}

func TestSchedulerDuplicateExplicitNameIsError(t *testing.T) {
	e, _, spec, kc, _ := schedEngine(t, "a:\n      name: dup", "b:\n      name: dup")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "name: must be unique") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSchedulerExplicitNameCollidingWithResolvedIsError(t *testing.T) {
	e, _, spec, kc, _ := schedEngine(t, "dup", "b:\n      name: dup")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "name: must be unique") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSchedulerUnknownDependsOnIsError(t *testing.T) {
	e, _, spec, kc, _ := schedEngine(t, "a", "b:\n      dependsOn: [nope]")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "unknown entry 'nope'") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestSchedulerCycleIsError(t *testing.T) {
	e, _, spec, kc, _ := schedEngine(t, "a:\n      dependsOn: [b]", "b:\n      dependsOn: [a]")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = stubApply(&eventLog{}, 0)
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errOut.String(), "cycle detected") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// --- immutable/terminating conflict: park + batched heal ---------------------

func immutableStub(failName string) func(context.Context, Job, io.Writer, io.Writer) int {
	return func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		if job.Name == failName && !job.Force {
			fmt.Fprintf(stderr, "The Deployment %q is invalid: spec.selector: field is immutable\n", job.Name)
			return 1
		}
		return 0
	}
}

func TestParkThenFailFastNonInteractive(t *testing.T) {
	// A background kapply can't prompt — it errors; the reap must PARK the
	// entry, and at the drain the batch resolve fails fast (no tty, no
	// --force) with a --force hint + skips its dependents.
	e, _, spec, kc, _ := schedEngine(t, "a", "b:\n      wait: true", "c")
	out := e.Stdout.(interface{ String() string })
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = immutableStub("b")
	// Force must NOT ride in from the ambient env of the dev shell.
	t.Setenv("LOK8S_FORCE_RECREATE", "")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected non-zero (parked heal failed)")
	}
	combined := out.String() + errOut.String()
	for _, want := range []string{
		"will confirm at the end", // it PARKED (deferred), not plain-failed
		"field is immutable",      // conflict detail surfaced, not dropped
		"needs recreate",          // the batched drain resolve
		"--force",                 // the actionable hint
		"skipping 'c' — a dependency failed (b)", // dependents BFS
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("missing %q in output:\n%s", want, combined)
		}
	}
}

func TestParkedInteractiveAcceptRecreatesAndUnblocks(t *testing.T) {
	// The accept branch: the operator answers y at the batched prompt; the
	// parked entry re-applies FOREGROUND under force, completes, and its
	// dependent runs in the next wave.
	e, log, spec, kc, _ := schedEngine(t, "a", "b:\n      wait: true", "c")
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name + fmt.Sprintf(" force=%v", job.Force))
		return immutableStub("b")(ctx, job, stdout, stderr)
	}
	e.Interactive = func() bool { return true }
	e.Ask = func(string) bool { return true }
	t.Setenv("LOK8S_FORCE_RECREATE", "")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(errOut.String(), "· recreated") {
		t.Errorf("missing recreated marker:\n%s", errOut.String())
	}
	if !log.has("b force=true") {
		t.Errorf("parked entry not re-applied under force: %v", log.events)
	}
	if !log.has("c force=false") {
		t.Errorf("dependent did not run after the heal: %v", log.events)
	}
	if strings.Contains(errOut.String(), "skipping") {
		t.Errorf("clean heal skipped dependents:\n%s", errOut.String())
	}
}

func TestRecreatePromptNamesEveryNamespace(t *testing.T) {
	// The prompt IS the safety control: accepting re-applies under force,
	// which silences kapply's pointed per-namespace confirm — whatever this
	// prompt does not say is not said at all. (2026-07-30: a prompt that
	// said only "restarts pods" force-finalized kubehz-system, mla and
	// element — CNPG volumes, PATs and buckets went with them.)
	p := RecreatePrompt(1, " addonA", []string{"kubehz-system", "mla", "element"})
	for _, want := range []string{
		"kubehz-system", "mla", "element",
		"FORCE-FINALIZED", "COMPLETES their deletion", "DESTROYED IRREVERSIBLY",
		"RE-KEYED", "[y/N]",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestRecreatePromptNoNamespaceNoWarning(t *testing.T) {
	// A warning printed on every heal is one nobody reads by the third
	// time — with nothing to force-finalize the destructive block is absent.
	p := RecreatePrompt(1, " addonA", nil)
	for _, absent := range []string{"FORCE-FINALIZED", "DESTROYED IRREVERSIBLY"} {
		if strings.Contains(p, absent) {
			t.Errorf("prompt has %q without stuck namespaces:\n%s", absent, p)
		}
	}
	for _, want := range []string{"need recreate to reconcile", "RE-KEYED", "[y/N]"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestTerminating403ReachesThePromptFromRealApplyOutput(t *testing.T) {
	// The wiring test: starts where the incident did — a background apply
	// failing with the apiserver's actual 403 text — and asserts the
	// namespace named in THAT text is the one the prompt warns about.
	e, _, spec, kc, _ := schedEngine(t, "a", "b:\n      wait: true", "c")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		if job.Name != "b" {
			return 0
		}
		fmt.Fprintln(stderr, `Error from server (Forbidden): error when creating "x": secrets "s" is forbidden: unable to create new content in namespace kubehz-system because it is being terminated`)
		return 1
	}
	var prompt string
	e.Interactive = func() bool { return true }
	e.Ask = func(p string) bool { prompt = p; return false }
	t.Setenv("LOK8S_FORCE_RECREATE", "")
	_ = e.Apply(context.Background(), "test.lok8s.dev", spec, kc)
	if prompt == "" {
		t.Fatal("the prompt was never composed and offered")
	}
	if !strings.Contains(prompt, "kubehz-system") || !strings.Contains(prompt, "FORCE-FINALIZED") {
		t.Errorf("prompt does not name the namespace from the 403:\n%s", prompt)
	}
}

func TestForceHealsInlineNeverParks(t *testing.T) {
	// With --force set, the background kapply auto-recreates in-job, so the
	// entry never reaches the park branch — proves the force actually
	// reaches the backgrounded apply.
	t.Setenv("LOK8S_FORCE_RECREATE", "1")
	e, _, spec, kc, _ := schedEngine(t, "a", "b:\n      wait: true", "c")
	out := e.Stdout.(interface{ String() string })
	errOut := e.Stderr.(interface{ String() string })
	e.ApplyOne = immutableStub("b")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, "will confirm at the end") || strings.Contains(combined, "needs recreate") {
		t.Errorf("force run parked/prompted:\n%s", combined)
	}
}

// --- buffered parallel output → de-interleaved collapsed blocks --------------

func TestFlushCollapsedBlocksNoRawInterleaving(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, _, spec, kc, _ := schedEngine(t, "a", "b")
	out := e.Stdout.(interface{ String() string })
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		fmt.Fprintf(stdout, "configmap/%s-one serverside-applied\n", job.Name)
		fmt.Fprintf(stdout, "deployment.apps/%s-two serverside-applied\n", job.Name)
		time.Sleep(10 * time.Millisecond)
		return 0
	}
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, want := range []string{"a ", "b ", "· 2 resources"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "serverside-applied") {
		t.Errorf("raw per-object lines reached the terminal:\n%s", out.String())
	}
}

func TestFlushFailedEntryMarkedWithSurfacedErrors(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	e, _, spec, kc, _ := schedEngine(t, "a", "b")
	out := e.Stdout.(interface{ String() string })
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		fmt.Fprintf(stdout, "secret/%s serverside-applied\n", job.Name)
		if job.Name == "b" {
			fmt.Fprintln(stdout, "Error from server: admission webhook denied")
			fmt.Fprintln(stdout, "Error from server: admission webhook denied")
			return 1
		}
		return 0
	}
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err == nil {
		t.Fatal("expected non-zero")
	}
	for _, want := range []string{"✗", "b", "· 1 resource", "admission webhook denied", "×2", "✓"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

func TestFlushDebugVerbatimNeverCollapsed(t *testing.T) {
	// kapply's contract: verbose (lo -v) prints everything, no aggregation.
	t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
	t.Setenv("DEBUG", "1")
	e, _, spec, kc, _ := schedEngine(t, "a")
	out := e.Stdout.(interface{ String() string })
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		fmt.Fprintf(stdout, "configmap/%s-one serverside-applied\n", job.Name)
		return 0
	}
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(out.String(), "configmap/a-one serverside-applied") {
		t.Errorf("verbatim line lost:\n%s", out.String())
	}
	if strings.Contains(out.String(), "· 1 resource") {
		t.Errorf("DEBUG output was collapsed:\n%s", out.String())
	}
}

// --- hosted platform-owned skip ---------------------------------------------

func applyOneDirect(t *testing.T, e *Engine, job Job) (int, string) {
	t.Helper()
	var buf strings.Builder
	rc := e.applyOne(context.Background(), job, &buf, &buf)
	return rc, buf.String()
}

func TestHostedApplyOneSkipsCilium(t *testing.T) {
	e, _, _, _, _ := testEngine(t)
	e.Hosted = true
	rc, out := applyOneDirect(t, e, Job{Name: "cilium", Dir: "/nonexistent/addons/cilium", Kind: "kubeone", Provider: "hetzner", Kubeconfig: "/kc"})
	if rc != 0 || !strings.Contains(out, "platform-owned on a hosted cluster") {
		t.Errorf("rc=%d out=%q", rc, out)
	}
}

func TestHostedApplyOneSkipsCCM(t *testing.T) {
	e, _, _, _, _ := testEngine(t)
	e.Hosted = true
	rc, out := applyOneDirect(t, e, Job{Name: "ccm", Dir: "/nonexistent/addons/ccm", Kind: "capi", Provider: "hetzner", Kubeconfig: "/kc"})
	if rc != 0 || !strings.Contains(out, "platform-owned on a hosted cluster") {
		t.Errorf("rc=%d out=%q", rc, out)
	}
}

func TestHostedRenamedCiliumStillSkips(t *testing.T) {
	// Anchored on the addon DIR: a name: override can't defeat the skip.
	e, _, _, _, _ := testEngine(t)
	e.Hosted = true
	rc, out := applyOneDirect(t, e, Job{Name: "my-cni", Dir: "/nonexistent/addons/cilium", Kind: "kubeone", Provider: "hetzner", Kubeconfig: "/kc"})
	if rc != 0 || !strings.Contains(out, "platform-owned on a hosted cluster") {
		t.Errorf("rc=%d out=%q", rc, out)
	}
}

func TestSelfHostedDoesNotTriggerHostedSkip(t *testing.T) {
	// Non-kubeone kind so the driver skip does not fire either; prove the
	// call REACHES the render step by making it fail loudly.
	e, _, _, _, _ := testEngine(t)
	e.Hosted = false
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error {
		if c.Name == "kustomize" {
			return fmt.Errorf("RENDER_CALLED")
		}
		return nil
	})
	rc, out := applyOneDirect(t, e, Job{Name: "cilium", Dir: "/nonexistent/addons/cilium", Kind: "capi", Provider: "hetzner", Kubeconfig: "/kc"})
	if rc == 0 {
		t.Fatal("expected render failure")
	}
	if strings.Contains(out, "platform-owned") {
		t.Errorf("hosted skip fired on self-hosted: %q", out)
	}
	if !strings.Contains(out, "render failed for cilium") {
		t.Errorf("did not reach the render: %q", out)
	}
}

type runnerFunc func(ctx context.Context, c execx.Cmd) error

func (f runnerFunc) Run(ctx context.Context, c execx.Cmd) error { return f(ctx, c) }

// --- hosted gates in Apply ---------------------------------------------------

func hostedHarness(t *testing.T) (*Engine, *fakeRunner, *strings.Builder, string, string) {
	t.Helper()
	e, f, _, _, p := testEngine(t)
	var errBuf strings.Builder
	e.Stderr = &errBuf
	mkAddonDirs(t, p, "cert-manager")
	spec := filepath.Join(p.Clusters, "h.test", "cluster.lok8s.yaml")
	writeFile(t, spec, `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: h }
spec:
  kubehz: { hosting: hosted }
  bootstrap: [cert-manager]
`)
	kc := filepath.Join(p.Base, ".kubeconfig", "h.yaml")
	writeFile(t, kc, "kc\n")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int { return 0 }
	return e, f, &errBuf, spec, kc
}

func TestHostedGateUnreachableWarnsOIDCAndSkips(t *testing.T) {
	e, f, errBuf, spec, kc := hostedHarness(t)
	f.handler = func(c execx.Cmd) error {
		fmt.Fprintln(c.Stderr, "error: You must be logged in")
		return &rcError{1}
	}
	if err := e.Apply(context.Background(), "h.test", spec, kc); err != nil {
		t.Fatalf("hosted skip must not fail the provision: %v", err)
	}
	if !strings.Contains(errBuf.String(), "cannot reach the cluster") || !strings.Contains(errBuf.String(), "kubelogin") {
		t.Errorf("missing OIDC guidance:\n%s", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "no Ready workers") {
		t.Errorf("conflated messages:\n%s", errBuf.String())
	}
}

func TestHostedGateZeroWorkersSkips(t *testing.T) {
	e, f, errBuf, spec, kc := hostedHarness(t)
	f.handler = func(c execx.Cmd) error {
		fmt.Fprintln(c.Stdout, "cp1 NotReady control-plane 1d v1.33")
		return nil
	}
	if err := e.Apply(context.Background(), "h.test", spec, kc); err != nil {
		t.Fatalf("hosted skip must not fail: %v", err)
	}
	if !strings.Contains(errBuf.String(), "no Ready workers yet") {
		t.Errorf("missing workers notice:\n%s", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "kubelogin") {
		t.Errorf("conflated messages:\n%s", errBuf.String())
	}
}

func TestHostedGateSchedulingDisabledCounts(t *testing.T) {
	e, f, errBuf, spec, kc := hostedHarness(t)
	f.handler = func(c execx.Cmd) error {
		fmt.Fprintln(c.Stdout, "w1 Ready,SchedulingDisabled worker 1d v1.33")
		return nil
	}
	if err := e.Apply(context.Background(), "h.test", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(errBuf.String(), "1 Ready worker(s)") {
		t.Errorf("prefix match failed:\n%s", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "no Ready workers") {
		t.Errorf("false negative:\n%s", errBuf.String())
	}
}

func TestHostedGateControlPlaneNodesDoNotCount(t *testing.T) {
	e, f, errBuf, spec, kc := hostedHarness(t)
	f.handler = func(c execx.Cmd) error {
		fmt.Fprintln(c.Stdout, "cp1 Ready control-plane 1d v1.33")
		fmt.Fprintln(c.Stdout, "cp2 Ready master 1d v1.33")
		return nil
	}
	if err := e.Apply(context.Background(), "h.test", spec, kc); err != nil {
		t.Fatalf("hosted skip must not fail: %v", err)
	}
	if !strings.Contains(errBuf.String(), "no Ready workers yet") {
		t.Errorf("CP nodes counted as workers:\n%s", errBuf.String())
	}
}

func TestHostedGateEmptyBootstrapNeverProbes(t *testing.T) {
	e, f, _, spec, kc := hostedHarness(t)
	writeFile(t, spec, `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: h }
spec:
  kubehz: { hosting: hosted }
`)
	f.handler = func(c execx.Cmd) error {
		t.Errorf("PROBE_MUST_NOT_RUN: kubectl %v", c.Args)
		return &rcError{1}
	}
	if err := e.Apply(context.Background(), "h.test", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// --- KubeOne cilium/ccm reconcile gate ---------------------------------------

func kubeoneAddonSpec(t *testing.T, p *config.Paths, name string) (string, string) {
	t.Helper()
	dir := mkChartAddon(t, p, name)
	writeFile(t, filepath.Join(dir, "values.yaml"), "marker: "+name+"\n")
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, "apiVersion: cluster.lok8s.dev/v1beta1\nkind: KubeOne\nmetadata:\n  name: e2e-test\nspec:\n  provider:\n    name: hetzner\n  bootstrap:\n    - "+name+"\n")
	return spec, writeKubeconfig(t, p)
}

func TestKubeOneDefersCiliumOnFullProvision(t *testing.T) {
	for _, name := range []string{"cilium", "ccm"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LOK8S_BOOTSTRAP_ONLY", "0")
			e, f, out, _, p := testEngine(t)
			spec, kc := kubeoneAddonSpec(t, p, name)
			if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !strings.Contains(out.String(), "applied by the KubeOne driver on a full provision") {
				t.Errorf("no defer notice:\n%s", out.String())
			}
			// Skip fires BEFORE render → kustomize never ran.
			if len(f.buildDirs) != 0 {
				t.Errorf("render ran despite the defer: %v", f.buildDirs)
			}
		})
	}
}

func TestKubeOneReconcilesCiliumOnBootstrapOnly(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_ONLY", "1")
	e, f, out, _, p := testEngine(t)
	spec, kc := kubeoneAddonSpec(t, p, "cilium")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(out.String(), "applied by the KubeOne driver on a full provision") {
		t.Errorf("skip fired in reconcile mode:\n%s", out.String())
	}
	if f.mergedOut == "" {
		t.Error("render did not run in reconcile mode")
	}
}

func TestKubeOneUnsetGateDefaultsToDefer(t *testing.T) {
	// No dispatch set it → safe fallback: defer to the driver, never a
	// spurious re-apply racing `kubeone apply` for SSA field ownership.
	t.Setenv("LOK8S_BOOTSTRAP_ONLY", "")
	e, f, out, _, p := testEngine(t)
	spec, kc := kubeoneAddonSpec(t, p, "cilium")
	if err := e.Apply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(out.String(), "applied by the KubeOne driver on a full provision") {
		t.Errorf("unset gate did not defer:\n%s", out.String())
	}
	if len(f.buildDirs) != 0 {
		t.Errorf("render ran: %v", f.buildDirs)
	}
}
