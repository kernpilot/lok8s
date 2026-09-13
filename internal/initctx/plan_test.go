package initctx

// plan_test.go — the situation table: one case per conversation row,
// asserting the ordered action kinds and the flag-twin command lines.

import (
	"path/filepath"
	"reflect"
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

func TestDecideEmptyDirectory(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "acme")
	s := State{Cwd: cwd, Empty: true, Git: Git{Available: true}}

	// The --plan defaults: files here, git init, the toolchain; no cluster
	// (no domain to assume), so no `lo use`.
	p := Decide(s, DefaultAnswers(s))
	if p.Situation != SituationEmptyDir || p.Dir != cwd || p.Name != "acme" {
		t.Errorf("plan: %+v", p)
	}
	wantKinds(t, p, ActionWriteProjectFiles, ActionGitInit, ActionToolchainInstall)
	wantCommands(t, p, "lo init project acme --env mise", "git init", "lo toolchain install --groups core,local")
	if !p.Network() || p.Actions[0].Summary != "clusters/, lok8s.yaml, .gitignore entries, mise.toml" {
		t.Errorf("plan: %+v", p.Actions[0])
	}

	// The full welcome: name, env, cluster, toolchain groups, use.
	p = Decide(s, Answers{Name: "shop", Env: "direnv", GitInit: true, Domain: "shop.dev", Driver: "lo", Use: true, Toolchain: true, Groups: []string{"core", "local", "cloud"}})
	wantKinds(t, p, ActionWriteProjectFiles, ActionGitInit, ActionToolchainInstall, ActionUse)
	wantCommands(t, p,
		"lo init project shop --env direnv --cluster shop.dev --driver lo",
		"git init",
		"lo toolchain install --groups core,local,cloud",
		"lo use shop.dev")
	if a := p.Actions[0]; a.Name != "shop" || a.Env != "direnv" || a.Domain != "shop.dev" || a.Driver != "lo" || !strings.Contains(a.Summary, "clusters/shop.dev/cluster.lok8s.yaml (lo)") || !strings.Contains(a.Summary, ".envrc") {
		t.Errorf("project files action: %+v", a)
	}

	// Nothing but the files: no git init, no toolchain, a domain without
	// use, the driver defaulted.
	p = Decide(s, Answers{Domain: "d.dev", Env: "none"})
	wantCommands(t, p, "lo init project acme --env none --cluster d.dev --driver lo")
	if p.Network() {
		t.Error("files only reported as needing the network")
	}

	// A `.git` already there: the git init answer is ignored.
	s.Git.Root = cwd
	s.Git.AtRoot = true
	p = Decide(s, Answers{GitInit: true})
	wantKinds(t, p, ActionWriteProjectFiles)

	// No git installed: the same.
	s.Git = Git{}
	p = Decide(s, Answers{GitInit: true})
	wantKinds(t, p, ActionWriteProjectFiles)
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
	if a.Dir != root || a.Env != "direnv" || a.GitInit || !a.Toolchain {
		t.Errorf("defaults: %+v", a)
	}
	p := Decide(s, a)
	if p.Dir != root || p.Name != filepath.Base(root) {
		t.Errorf("plan: %+v", p)
	}
	wantKinds(t, p, ActionWriteProjectFiles, ActionToolchainInstall)
	// The root lies above cwd: the twin names it absolutely.
	if want := "lo init project " + filepath.Base(root) + " --env direnv --path " + root; p.Commands()[0] != want {
		t.Errorf("command %q, want %q", p.Commands()[0], want)
	}

	// The user overrides: here instead, a mise file beside the .envrc.
	p = Decide(s, Answers{Dir: cwd, Env: "mise", Name: "api"})
	wantCommands(t, p, "lo init project api --env mise")
	if p.Dir != cwd {
		t.Errorf("dir %s", p.Dir)
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
	if p.Actions[1].Summary != "a git repository in platform" {
		t.Errorf("git init summary %q", p.Actions[1].Summary)
	}

	// Here, at the git root: no git init offered even when asked.
	s.Git.Root, s.Git.AtRoot = cwd, true
	p = Decide(s, Answers{GitInit: true})
	wantKinds(t, p, ActionWriteProjectFiles)
	if p.Actions[0].Command != "lo init project "+filepath.Base(cwd)+" --env mise" {
		t.Errorf("command %q", p.Actions[0].Command)
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

func TestDecideProjectRoot(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)

	// Nothing chosen: nothing to do (the --plan default).
	p := Decide(s, DefaultAnswers(s))
	if p.Situation != SituationProjectRoot || p.Dir != root || p.Name != "acme" || len(p.Actions) != 0 {
		t.Errorf("plan: %+v", p)
	}

	// Everything chosen, in the documented order: env file, cluster spec,
	// the implementation switch (eject first: no tree), toolchain, use,
	// service, tests. A registration is ignored at the root.
	p = Decide(s, Answers{Env: "direnv", Domain: "beta.cloud", Driver: "kubeone", Use: true,
		Implementation: "bash", Toolchain: true, Groups: []string{"core", "local", "bash"},
		Service: "api", Tests: true, Register: true})
	wantKinds(t, p, ActionWriteEnvFile, ActionWriteClusterSpec, ActionEjectBash, ActionSetImplementation,
		ActionToolchainInstall, ActionUse, ActionAddService, ActionAddTests)
	wantCommands(t, p,
		"lo init project --env direnv",
		"lo init project --env none --cluster beta.cloud --driver kubeone",
		"lo assets eject bash",
		"lo init project --env none --implementation bash",
		"lo toolchain install --groups core,local,bash",
		"lo use beta.cloud",
		"lo init service api",
		"lo init test")
	if a := p.Actions[6]; a.Service != "api" || a.ServicePath != "./api" {
		t.Errorf("service action: %+v", a)
	}
	if a := p.Actions[0]; a.Summary != ".envrc" || a.Env != "direnv" || a.Name != "acme" {
		t.Errorf("env action: %+v", a)
	}

	// An env file exists: the env answer is ignored. The tree exists: no
	// eject. The same implementation: no switch. A domain without use.
	s.Project.EnvFile = "mise"
	s.Project.BashTree = true
	p = Decide(s, Answers{Env: "direnv", Implementation: "bash", Domain: "x.dev"})
	wantKinds(t, p, ActionWriteClusterSpec, ActionSetImplementation)
	s.Project.Implementation = "bash"
	p = Decide(s, Answers{Implementation: "bash"})
	wantKinds(t, p)
	// Back to go: no eject either way.
	p = Decide(s, Answers{Implementation: "go"})
	wantCommands(t, p, "lo init project --env none --implementation go")

	// A project marked by clusters/ alone: the name is the directory.
	s.Project.Name = ""
	if p := Decide(s, Answers{}); p.Name != filepath.Base(root) {
		t.Errorf("name %q", p.Name)
	}
}

func TestDecideInsideProject(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, false)
	s.Cwd = filepath.Join(root, "services", "api")
	s.ServiceDir = true
	if s.Situation() != SituationInsideProject {
		t.Fatalf("situation %v", s.Situation())
	}

	// The registration: the service directory into services.yaml, the
	// path relative to the root. Everything runs in the root.
	p := Decide(s, Answers{Register: true, Tests: true})
	if p.Dir != root {
		t.Errorf("dir %s, want the root", p.Dir)
	}
	wantKinds(t, p, ActionAddTests, ActionRegisterService)
	wantCommands(t, p, "lo init test", "lo init service api --path ./services/api")
	if a := p.Actions[1]; a.Service != "api" || a.ServicePath != "./services/api" || !strings.Contains(a.Summary, "services.api.path = ./services/api") {
		t.Errorf("register action: %+v", a)
	}

	// Not a service directory: the registration is ignored.
	s.ServiceDir = false
	p = Decide(s, Answers{Register: true})
	wantKinds(t, p)
}

func TestOptions(t *testing.T) {
	root := t.TempDir()
	if Options(State{Cwd: root}) != nil {
		t.Error("options without a project")
	}

	s := projectState(root, true)
	keysOf := func(opts []Option) []string {
		var keys []string
		for _, o := range opts {
			keys = append(keys, o.Key)
		}
		return keys
	}
	got := keysOf(Options(s))
	want := []string{"cluster", "service", "tests", "env", "toolchain", "implementation"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("root options %v, want %v", got, want)
	}
	opts := Options(s)
	if opts[4].Label != "Install the pinned toolchain (network)" || opts[5].Label != "Switch the implementation to bash (now go)" {
		t.Errorf("labels: %q %q", opts[4].Label, opts[5].Label)
	}

	// Tests and env present, tools installed, bash active: the menu
	// shrinks and relabels.
	s.Project.Tests, s.Project.EnvFile, s.Project.BYAML, s.Project.Implementation = true, "mise", true, "bash"
	opts = Options(s)
	got = keysOf(opts)
	want = []string{"cluster", "service", "toolchain", "implementation"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options %v, want %v", got, want)
	}
	if opts[2].Label != "Reinstall the pinned toolchain (network)" || opts[3].Label != "Switch the implementation to go (now bash)" {
		t.Errorf("labels: %q %q", opts[2].Label, opts[3].Label)
	}

	// A service directory below the root: the registration comes first.
	s = projectState(root, false)
	s.Cwd = filepath.Join(root, "api")
	s.ServiceDir = true
	opts = Options(s)
	if opts[0].Key != "register" || opts[0].Command != "lo init service api --path ./api" {
		t.Errorf("first option: %+v", opts[0])
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
	if shortDir(cwd, cwd) != "." || shortDir(cwd, filepath.Join(cwd, "x")) != "x" {
		t.Error("shortDir")
	}
}
