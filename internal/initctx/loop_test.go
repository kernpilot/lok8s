package initctx

// loop_test.go: project mode: the list per state, the loop through an
// action and back with the card refreshed, Cancel and Exit.

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func entryKeys(s State) []string {
	var keys []string
	for _, e := range Entries(s) {
		keys = append(keys, e.Key)
	}
	return keys
}

func TestEntries(t *testing.T) {
	root := t.TempDir()
	if Entries(State{Cwd: root}) != nil {
		t.Error("entries without a project")
	}
	s := projectState(root, true)
	// No b.yaml, no tests, one tree-less project with no cluster.
	want := []string{EntryCluster, EntryService, EntryTests, EntryToolchain, EntryEject, EntryExit}
	if got := entryKeys(s); !reflect.DeepEqual(got, want) {
		t.Errorf("entries %v, want %v", got, want)
	}
	if Entries(s)[3].Label != "Install the toolchain" || Entries(s)[4].Label != "Eject the bash tree" {
		t.Errorf("labels: %+v", Entries(s))
	}

	// Missing tools: the install entry counts them. One cluster with a
	// valid active domain: no active-domain entry. Tests present: no
	// tests entry. A tree: the implementation switch.
	s.Project.BYAML, s.Project.Tools, s.Project.ToolsMissing = true, 17, []string{"b", "kind", "tilt"}
	s.Project.Tests = true
	s.Project.Domains = []Domain{{"a.dev", "lo"}}
	s.Project.Active = "a.dev"
	s.Project.BashTree = true
	want = []string{EntryCluster, EntryService, EntryToolchain, EntryImplementation, EntryExit}
	if got := entryKeys(s); !reflect.DeepEqual(got, want) {
		t.Errorf("entries %v, want %v", got, want)
	}
	e := Entries(s)
	if e[2].Label != "Install the toolchain (3 of 17 missing)" || e[3].Label != "Switch implementation (go → bash)" || e[3].Twin != "lo init project --implementation bash" {
		t.Errorf("labels: %+v", e)
	}

	// One cluster and no valid active domain (absent, or a stale
	// .active): the active-domain entry is offered. An unreadable pin
	// file: the toolchain entry says so.
	s.Project.Active = ""
	s.Project.ToolsMissing, s.Project.BYAMLInvalid = nil, true
	want = []string{EntryCluster, EntryService, EntryToolchain, EntryActive, EntryImplementation, EntryExit}
	if got := entryKeys(s); !reflect.DeepEqual(got, want) {
		t.Errorf("entries without an active domain %v, want %v", got, want)
	}
	if Entries(s)[2].Label != "Install the toolchain (pin file unreadable)" {
		t.Errorf("labels: %+v", Entries(s))
	}
	s.Project.Active = "gone.dev"
	if got := entryKeys(s); !reflect.DeepEqual(got, want) {
		t.Errorf("entries with a stale active domain %v, want %v", got, want)
	}
	s.Project.BYAMLInvalid = false

	// Every tool present, several clusters, bash active.
	s.Project.Active = "a.dev"
	s.Project.Domains = append(s.Project.Domains, Domain{"b.dev", "kubeone"})
	s.Project.Implementation = "bash"
	want = []string{EntryCluster, EntryService, EntryActive, EntryImplementation, EntryExit}
	if got := entryKeys(s); !reflect.DeepEqual(got, want) {
		t.Errorf("entries %v, want %v", got, want)
	}
	if Entries(s)[3].Label != "Switch implementation (bash → go)" {
		t.Errorf("labels: %+v", Entries(s))
	}

	var b bytes.Buffer
	WriteEntries(&b, Entries(s))
	if b.String() != "  actions     Add a cluster · Add a service · Set the active domain · Switch implementation (bash → go) · Exit\n"+
		"  equivalent  lo init cluster <domain> · lo init service <name> · lo use <domain> · lo init project --implementation go\n" {
		t.Errorf("entries text:\n%s", b.String())
	}
}

// loopFake is the loop's two seams: the states Detect hands back in
// order, and the plans Execute received.
type loopFake struct {
	states []State
	plans  []Plan
}

func (f *loopFake) detect() (State, error) {
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}

func (f *loopFake) execute(p Plan) error {
	f.plans = append(f.plans, p)
	return nil
}

func TestLoopActionThenRefreshedCard(t *testing.T) {
	root := t.TempDir()
	before := projectState(root, true)
	before.Git.Branch = "main"
	before.Project.BYAML, before.Project.Tools = true, 4
	before.Project.EnvFile = "mise"
	after := before
	after.Project = &Project{}
	*after.Project = *before.Project
	after.Project.Domains = []Domain{{"beta.dev", "lo"}}
	after.Project.Active = "beta.dev"
	fake := &loopFake{states: []State{after}}
	var out bytes.Buffer
	// Add a cluster (1): domain, driver default, active default (yes: no
	// active domain yet), Create; then Exit (the list has five entries).
	tio := script("1", "beta.dev", "", "", "1", "5")
	l := Loop{Out: &out, IO: tio, Detect: fake.detect, Execute: fake.execute}
	if err := l.Run(before, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.plans) != 1 {
		t.Fatalf("executed %d plans", len(fake.plans))
	}
	wantCommands(t, fake.plans[0], "lo init cluster beta.dev --driver lo", "lo use beta.dev")
	got := out.String()
	contains(t, got,
		"! clusters     none\n",
		"  New cluster\n",
		"  clusters     beta.dev (kind, active)\n",
		"  added        clusters/beta.dev/cluster.lok8s.yaml · active\n",
		"  next  lo up\n")
	// The card was printed twice: before, and refreshed after the action.
	if n := strings.Count(got, "  acme         project · go · main\n"); n != 2 {
		t.Errorf("card printed %d times, want 2:\n%s", n, got)
	}
	contains(t, formOut(tio), "1. Add a cluster\n2. Add a service\n3. Add the test suite\n4. Eject the bash tree\n5. Exit\n")
}

// Cancel on an action's screen returns to the list with the cancelled
// line; nothing was executed. Exit leaves with the next line; EOF (the
// first entry by default, then nothing to read at its prompt) leaves
// with nothing executed.
func TestLoopCancelAndExit(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Git.Branch = "main"
	fake := &loopFake{states: []State{s}}
	var out bytes.Buffer
	// Add a service (2): name, path default, Cancel (3); then Exit (6).
	tio := script("2", "api", "", "3", "6")
	if err := (Loop{Out: &out, IO: tio, Detect: fake.detect, Execute: fake.execute}).Run(s, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.plans) != 0 {
		t.Errorf("executed after a cancel: %+v", fake.plans)
	}
	contains(t, out.String(), "  cancelled    nothing written\n", "  next  lo toolchain install\n")

	out.Reset()
	if err := (Loop{Out: &out, IO: scripted(""), Detect: fake.detect, Execute: fake.execute}).Run(s, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.plans) != 0 || strings.Contains(out.String(), "New cluster") {
		t.Errorf("EOF: plans %+v out:\n%s", fake.plans, out.String())
	}

	// A failed step returns to the list with the failed and not-run rows
	// under the refreshed card; an error that is not a step ends the run.
	out.Reset()
	step := &StepError{Command: "lo init test", Err: errors.New("disk full (fake)"), NotRun: []string{"lo use x.dev"}}
	l := Loop{Out: &out, IO: script("3", "1", "6"), Detect: fake.detect, Execute: func(Plan) error { return step }}
	if err := l.Run(s, nil); err != nil {
		t.Errorf("a failed step ended the run: %v", err)
	}
	contains(t, out.String(), "! failed       lo init test · disk full (fake)\n", "  not run      lo use x.dev\n", "  next  lo toolchain install\n")
	if n := strings.Count(out.String(), "  acme         project · go · main\n"); n != 2 {
		t.Errorf("card printed %d times after a failure, want 2:\n%s", n, out.String())
	}
	fail := errors.New("boom")
	l = Loop{Out: &out, IO: script("3", "1"), Detect: fake.detect, Execute: func(Plan) error { return fail }}
	if err := l.Run(s, nil); !errors.Is(err, fail) {
		t.Errorf("failure: %v", err)
	}
}

// A new cluster becomes the active domain when the project has no valid
// one; with a valid active domain the default is no.
func TestLoopClusterDefaultsActive(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Project.Domains = []Domain{{"a.dev", "lo"}}
	s.Project.Active = "gone.dev"
	fake := &loopFake{states: []State{s}}
	var out bytes.Buffer
	// Add a cluster (1): domain, driver default, active default, Create;
	// the list has seven entries (the active-domain entry is offered).
	tio := script("1", "b.dev", "", "", "1", "7")
	if err := (Loop{Out: &out, IO: tio, Detect: fake.detect, Execute: fake.execute}).Run(s, nil); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, fake.plans[0], "lo init cluster b.dev --driver lo", "lo use b.dev")
	s.Project.Active = "a.dev"
	fake = &loopFake{states: []State{s}}
	if err := (Loop{Out: &out, IO: script("1", "c.dev", "", "", "1", "6"), Detect: fake.detect, Execute: fake.execute}).Run(s, nil); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, fake.plans[0], "lo init cluster c.dev --driver lo --no-active")
}

// The first plan (the bootstrap) puts its result under the first card.
func TestLoopFirstResult(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Git.Branch = "main"
	first := Decide(State{Cwd: root, Empty: true, Git: Git{Available: true}}, DefaultAnswers(State{Cwd: root, Empty: true, Git: Git{Available: true}}))
	var out bytes.Buffer
	fake := &loopFake{states: []State{s}}
	if err := (Loop{Out: &out, IO: script("6"), Detect: fake.detect, Execute: fake.execute}).Run(s, []Result{ResultOf(first)}); err != nil {
		t.Fatal(err)
	}
	contains(t, out.String(), "  created      "+first.Name+" · clusters/"+first.Name+".dev · toolchain 8 tools · git initialised\n")
}

func TestWelcome(t *testing.T) {
	var b bytes.Buffer
	Welcome(&b, State{Cwd: "/x/shop", Empty: true})
	if b.String() != "  lo init sets up a lok8s project in this directory: the project file, the first cluster spec, the toolchain.\n" {
		t.Errorf("welcome: %q", b.String())
	}
	b.Reset()
	Welcome(&b, State{Cwd: "/x/repo/sub", Entries: 1, Git: Git{Available: true, Root: "/x/repo"}})
	if !strings.Contains(b.String(), "at the repository root") {
		t.Errorf("welcome below a root: %q", b.String())
	}
	// A project without a repository: what it lacks.
	s := projectState(t.TempDir(), true)
	s.Git = Git{Available: true}
	b.Reset()
	Welcome(&b, s)
	if b.String() != "  lo init completes the project acme: a git repository, the first cluster spec, the toolchain.\n" {
		t.Errorf("welcome in a project: %q", b.String())
	}
	s.Project.Domains = []Domain{{"a.dev", "lo"}}
	s.Project.BYAML = true
	b.Reset()
	Welcome(&b, s)
	if b.String() != "  lo init completes the project acme: a git repository.\n" {
		t.Errorf("welcome in a complete project: %q", b.String())
	}
}

// Bootstrap: no project, or a project without a repository (git
// installed); a project with a repository, or without git at all, is
// project mode.
func TestBootstrap(t *testing.T) {
	root := t.TempDir()
	if !Bootstrap(State{Cwd: root, Empty: true}) {
		t.Error("an empty directory is not bootstrap")
	}
	s := projectState(root, true)
	if Bootstrap(s) {
		t.Error("a project with a repository is bootstrap")
	}
	s.Git = Git{Available: true}
	if !Bootstrap(s) {
		t.Error("a project without a repository is not bootstrap")
	}
	s.Git = Git{}
	if Bootstrap(s) {
		t.Error("a project without git installed is bootstrap")
	}
}
