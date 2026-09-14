package initctx

// plan_test.go — the decision layer: the new-project defaults and plan
// per situation, the verb plans, the result line, the next step.

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func kinds(p Plan) []ActionKind {
	out := make([]ActionKind, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, a.Kind)
	}
	return out
}

func wantKinds(t *testing.T, p Plan, want ...ActionKind) {
	t.Helper()
	got := kinds(p)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("actions %v, want %v\ncommands: %q", got, want, p.Commands())
	}
}

func wantCommands(t *testing.T, p Plan, want ...string) {
	t.Helper()
	if got := p.Commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestDefaultName(t *testing.T) {
	for dir, want := range map[string]string{
		"/x/shop": "shop", "/x/My Project": "my-project", "/x/_lead": "lead", "/x/---": "project", "/x/a.b_c-d": "a.b_c-d",
	} {
		if got := DefaultName(dir); got != want {
			t.Errorf("DefaultName(%s) = %q, want %q", dir, got, want)
		}
	}
}

func TestDecideEmptyDirectory(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "acme")
	s := State{Cwd: cwd, Empty: true, Git: Git{Available: true}}

	// The defaults: the files here with the first cluster <name>.dev,
	// git init, the toolchain, lo use.
	a := DefaultAnswers(s)
	if a.Dir != cwd || a.Name != "acme" || a.Domain != "acme.dev" || a.Driver != "lo" || !a.Use || a.Env != "mise" || !a.GitInit || !a.Toolchain {
		t.Errorf("defaults: %+v", a)
	}
	p := Decide(s, a)
	if p.Dir != cwd || p.Name != "acme" {
		t.Errorf("plan: %+v", p)
	}
	wantKinds(t, p, ActionWriteProjectFiles, ActionGitInit, ActionToolchainInstall, ActionUse)
	wantCommands(t, p, "lo init project acme --env mise --cluster acme.dev --driver lo", "git init", "lo toolchain install --groups core,local", "lo use acme.dev")
	if !p.Network() || !reflect.DeepEqual(p.Actions[0].Files, []string{"clusters/", "lok8s.yaml", ".gitignore entries", "mise.toml", "clusters/acme.dev/cluster.lok8s.yaml"}) {
		t.Errorf("project files action: %+v", p.Actions[0])
	}
	if !reflect.DeepEqual(p.Runs(), []string{"git init", "lo toolchain install (network)", "lo use acme.dev"}) {
		t.Errorf("runs: %v", p.Runs())
	}
	if verb, what := p.Result(); verb != "created" || what != "acme · clusters/acme.dev · toolchain 8 tools · git initialised" {
		t.Errorf("result: %s %s", verb, what)
	}

	// Changed details: name, env, driver, groups.
	p = Decide(s, Answers{Name: "shop", Env: "direnv", GitInit: true, Domain: "shop.dev", Driver: "kubeone", Use: true, Toolchain: true, Groups: []string{"core", "local", "cloud"}})
	wantCommands(t, p,
		"lo init project shop --env direnv --cluster shop.dev --driver kubeone",
		"git init",
		"lo toolchain install --groups core,local,cloud",
		"lo use shop.dev")
	if a := p.Actions[0]; a.Name != "shop" || a.Env != "direnv" || a.Domain != "shop.dev" || a.Driver != "kubeone" || !strings.Contains(strings.Join(a.Files, " "), ".envrc") {
		t.Errorf("project files action: %+v", a)
	}

	// Nothing but the files: no git init, no toolchain, no domain.
	p = Decide(s, Answers{Env: "none"})
	wantCommands(t, p, "lo init project acme --env none")
	if p.Network() || p.Runs() != nil {
		t.Errorf("files only: network %v runs %v", p.Network(), p.Runs())
	}
	if verb, what := p.Result(); verb != "created" || what != "acme" {
		t.Errorf("result: %s %s", verb, what)
	}

	// A `.git` already there, or no git at all: the git init answer is
	// ignored and the default is off.
	s.Git.Root, s.Git.AtRoot = cwd, true
	if DefaultAnswers(s).GitInit {
		t.Error("git init defaulted on inside a repository")
	}
	wantKinds(t, Decide(s, Answers{GitInit: true}), ActionWriteProjectFiles)
	s.Git = Git{}
	wantKinds(t, Decide(s, Answers{GitInit: true}), ActionWriteProjectFiles)
}

func TestDecideGitBelowRoot(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, ".envrc"), "PATH_add .bin\n")
	cwd := filepath.Join(root, "services", "api")
	s := State{Cwd: cwd, Entries: 2, Git: Git{Available: true, Root: root}}
	if s.Situation() != SituationGitBelowRoot {
		t.Fatalf("situation %v", s.Situation())
	}

	// The defaults: the git root, the env file it already has, no git
	// init (a repository exists).
	a := DefaultAnswers(s)
	if a.Dir != root || a.Env != "direnv" || a.GitInit || !a.Toolchain || a.Name != filepath.Base(root) {
		t.Errorf("defaults: %+v", a)
	}
	p := Decide(s, a)
	if p.Dir != root {
		t.Errorf("plan: %+v", p)
	}
	wantKinds(t, p, ActionWriteProjectFiles, ActionToolchainInstall, ActionUse)
	// The root lies above cwd: the twin names it absolutely.
	if want := "lo init project " + a.Name + " --env direnv --path " + root + " --cluster " + a.Name + ".dev --driver lo"; p.Commands()[0] != want {
		t.Errorf("command %q, want %q", p.Commands()[0], want)
	}

	// The user overrides: here instead (relative), a mise file.
	p = Decide(s, Answers{Dir: ".", Env: "mise", Name: "api"})
	wantCommands(t, p, "lo init project api --env mise")
	if p.Dir != cwd {
		t.Errorf("dir %s", p.Dir)
	}
	p = Decide(s, Answers{Dir: "../..", Name: "top", Env: "none"})
	if p.Dir != root {
		t.Errorf("dir %s, want the root", p.Dir)
	}
}

func TestDecideBareDirectory(t *testing.T) {
	cwd := t.TempDir()
	s := State{Cwd: cwd, Entries: 3, Git: Git{Available: true}}
	if s.Situation() != SituationBareDir {
		t.Fatalf("situation %v", s.Situation())
	}

	// A subdirectory, relative: the twin carries --path, the name comes
	// from it, git init lands there.
	p := Decide(s, Answers{Dir: "platform", GitInit: true, Toolchain: true})
	if p.Dir != filepath.Join(cwd, "platform") || p.Name != "platform" {
		t.Errorf("plan: %+v", p)
	}
	wantCommands(t, p, "lo init project platform --env mise --path platform", "git init", "lo toolchain install --groups core,local")

	// Here, at the git root: no git init even when asked.
	s.Git.Root, s.Git.AtRoot = cwd, true
	p = Decide(s, Answers{GitInit: true})
	wantKinds(t, p, ActionWriteProjectFiles)
	if p.Actions[0].Command != "lo init project "+DefaultName(cwd)+" --env mise" {
		t.Errorf("command %q", p.Actions[0].Command)
	}
}

// A spec that exists is kept, and the files line says so (the executor
// never overwrites it).
func TestDecideKeepsAnExistingClusterSpec(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "clusters", "old.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	s := State{Cwd: root, Entries: 1, Git: Git{Available: true}}
	p := Decide(s, Answers{Domain: "old.dev"})
	if !slices.Contains(p.Actions[0].Files, "clusters/old.dev/cluster.lok8s.yaml (exists, kept)") {
		t.Errorf("files %v", p.Actions[0].Files)
	}
	p = ClusterPlan(root, "old.dev", "kubeone", false)
	if !reflect.DeepEqual(p.Actions[0].Files, []string{"clusters/old.dev/cluster.lok8s.yaml (exists, kept)"}) {
		t.Errorf("files %v", p.Actions[0].Files)
	}
}

func projectState(root string, atRoot bool) State {
	cwd := root
	if !atRoot {
		cwd = filepath.Join(root, "sub")
	}
	return State{Cwd: cwd, Entries: 4, Git: Git{Available: true, Root: root, AtRoot: atRoot},
		Project: &Project{Root: root, AtRoot: atRoot, Name: "acme", ProjectFile: true, Clusters: true, Implementation: "go"}}
}

// The verb plans: one action each (two for a cluster made active), the
// twin with the flags given, the files as the screen lists them.
func TestVerbPlans(t *testing.T) {
	root := t.TempDir()
	p := ClusterPlan(root, "beta.cloud", "kubeone", true)
	wantKinds(t, p, ActionWriteClusterSpec, ActionUse)
	wantCommands(t, p, "lo init cluster beta.cloud --driver kubeone", "lo use beta.cloud")
	if verb, what := p.Result(); verb != "added" || what != "clusters/beta.cloud/cluster.lok8s.yaml · active" {
		t.Errorf("result: %s %s", verb, what)
	}
	p = ClusterPlan(root, "beta.cloud", "", false)
	wantCommands(t, p, "lo init cluster beta.cloud --driver lo --no-active")
	if a := p.Actions[0]; a.Driver != "lo" || a.Domain != "beta.cloud" {
		t.Errorf("cluster action: %+v", a)
	}
	if verb, what := p.Result(); verb != "added" || what != "clusters/beta.cloud/cluster.lok8s.yaml" {
		t.Errorf("result: %s %s", verb, what)
	}

	p = ServicePlan(root, "api", "")
	wantCommands(t, p, "lo init service api")
	if a := p.Actions[0]; a.Service != "api" || a.Path != "" || !reflect.DeepEqual(a.Files, []string{"api/lok8s.yaml", "services.yaml entry", "Tiltfile"}) {
		t.Errorf("service action: %+v", a)
	}
	p = ServicePlan(root, "api", "./services/api")
	wantCommands(t, p, "lo init service api --path ./services/api")
	if a := p.Actions[0]; a.Path != "./services/api" || a.Files[0] != "services/api/lok8s.yaml" {
		t.Errorf("service action: %+v", a)
	}

	p = TestsPlan(root, "")
	wantCommands(t, p, "lo init test")
	if a := p.Actions[0]; a.Path != "" || a.Files[0] != "tests/ (the Playwright suite)" {
		t.Errorf("tests action: %+v", a)
	}
	p = TestsPlan(root, "./e2e/")
	wantCommands(t, p, "lo init test --path ./e2e/")
	if a := p.Actions[0]; a.Files[0] != "e2e/ (the Playwright suite)" {
		t.Errorf("tests action: %+v", a)
	}

	p = ToolchainPlan(root, []string{"core", "local", "bash"})
	wantCommands(t, p, "lo toolchain install --groups core,local,bash")
	if !p.Network() {
		t.Error("the toolchain reported as offline")
	}
	if verb, what := p.Result(); verb != "installed" || what != ".bin/b.yaml · .bin/ (the pinned tools)" {
		t.Errorf("result: %s %s", verb, what)
	}
	p = UsePlan(root, "x.dev")
	wantCommands(t, p, "lo use x.dev")
	if verb, what := p.Result(); verb != "active" || what != "x.dev" {
		t.Errorf("result: %s %s", verb, what)
	}
	p = EjectPlan(root)
	wantCommands(t, p, "lo assets eject bash")
	if verb, what := p.Result(); verb != "ejected" || what != ".lok8s/" {
		t.Errorf("result: %s %s", verb, what)
	}
	p = ImplementationPlan(root, "acme", "bash")
	wantCommands(t, p, "lo init project --env none --implementation bash")
	if a := p.Actions[0]; a.Name != "acme" || a.Implementation != "bash" {
		t.Errorf("implementation action: %+v", a)
	}
	if verb, what := p.Result(); verb != "set" || what != "implementation bash" {
		t.Errorf("result: %s %s", verb, what)
	}
	if verb, what := (Plan{}).Result(); verb != "done" || what != "nothing" {
		t.Errorf("empty result: %s %s", verb, what)
	}
}

// The next step, in order of need.
func TestNext(t *testing.T) {
	root := t.TempDir()
	if got := Next(State{Cwd: root}, false); got != "lo init" {
		t.Errorf("outside a project: %q", got)
	}
	s := projectState(root, true)
	if got := Next(s, false); got != "lo toolchain install" {
		t.Errorf("no b.yaml: %q", got)
	}
	s.Project.BYAML, s.Project.Tools, s.Project.ToolsMissing = true, 4, []string{"b"}
	if got := Next(s, false); got != "lo toolchain install" {
		t.Errorf("missing tools: %q", got)
	}
	s.Project.ToolsMissing = nil
	if got := Next(s, false); got != "lo init cluster" {
		t.Errorf("no clusters: %q", got)
	}
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}}
	if got := Next(s, false); got != "lo use <domain>" {
		t.Errorf("no active domain: %q", got)
	}
	s.Project.Active = "alpha.dev"
	if got := Next(s, true); got != "lo assets diff" {
		t.Errorf("drift: %q", got)
	}
	if got := Next(s, false); got != "lo up" {
		t.Errorf("no kubeconfig: %q", got)
	}
	s.Project.Kubeconfig = true
	if got := Next(s, false); got != "lo status" {
		t.Errorf("provisioned: %q", got)
	}
}

func TestRelDir(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "a")
	cases := map[string]string{
		cwd:                           "",
		filepath.Join(cwd, "b", "c"):  filepath.Join("b", "c"),
		filepath.Dir(cwd):             filepath.Dir(cwd),
		filepath.Join(cwd, "..", "z"): filepath.Join(filepath.Dir(cwd), "z"),
	}
	for dir, want := range cases {
		if got := relDir(cwd, dir); got != want {
			t.Errorf("relDir(%s, %s) = %q, want %q", cwd, dir, got, want)
		}
	}
}
