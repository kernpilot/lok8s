package initctx

// card_test.go — the state card (goldens off and on a terminal, the five
// situations, the path rule, the counts), the plan summary and the
// option hints.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func contains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func card(s State) string {
	var b bytes.Buffer
	WriteCard(&b, s)
	return b.String()
}

func wantCard(t *testing.T, s State, want string) {
	t.Helper()
	if got := card(s); got != want {
		t.Errorf("card:\n%s\nwant:\n%s", got, want)
	}
}

// Outside a project: the directory's name heads the card; the repository
// row appears only when the git root is elsewhere, with the path relative
// to the working directory.
func TestWriteCardDirectory(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "acme")
	wantCard(t, State{Cwd: cwd, Empty: true}, "  acme  empty directory · no git\n")
	wantCard(t, State{Cwd: cwd, Entries: 3, Git: Git{Available: true}},
		"  acme  directory · 3 entries · no project · no git repository\n")
	wantCard(t, State{Cwd: cwd, Entries: 1, ServiceDir: true, Git: Git{Available: true, Root: filepath.Dir(cwd), Branch: "main", Uncommitted: 2}},
		"  acme        service directory · 1 entry · no project above\n"+
			"  repository  .. · main, 2 uncommitted\n")
	wantCard(t, State{Cwd: cwd, Entries: 3, Git: Git{Available: true, Root: cwd, AtRoot: true, Submodule: true}},
		"  acme  directory · 3 entries · no project · detached HEAD · submodule or worktree\n")
	if out := card(State{Cwd: cwd, Empty: true, Git: Git{Available: true, Root: cwd, AtRoot: true, Branch: "main"}}); strings.Contains(out, cwd) {
		t.Errorf("the working directory printed as a path:\n%s", out)
	}
}

// A project root: every fact on its row, no path (the working directory
// is the root), the domains grouped by driver with the active one first.
func TestWriteCardProjectRoot(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Git.Branch, s.Git.Uncommitted = "main", 6
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}, {"beta.cloud", "kubeone"}, {"delta.app", "deploy"}, {"gamma.app", "deploy"}}
	s.Project.Active = "alpha.dev"
	s.Project.EnvFile = "mise"
	s.Project.BYAML, s.Project.Tools = true, 17
	s.Project.Services, s.Project.Tests = true, true
	wantCard(t, s,
		"  acme         project · go · main, 6 uncommitted\n"+
			"  clusters     alpha.dev (kind, active) · beta.cloud (kubeone) · delta.app, gamma.app (deploy)\n"+
			"  toolchain    .bin/b.yaml · 17 tools present\n"+
			"  environment  mise.toml\n"+
			"  services     services.yaml\n"+
			"  tests        tests/\n")
	if strings.Contains(card(s), root) {
		t.Error("the project root printed as a path at the root")
	}

	// A clean tree on one tool, no services file, no tests.
	s.Git.Uncommitted = 0
	s.Project.Tools = 1
	s.Project.Services, s.Project.Tests = false, false
	out := card(s)
	contains(t, out, "  acme         project · go · main\n", "  toolchain    .bin/b.yaml · 1 tool present\n")
	if strings.Contains(out, "services") || strings.Contains(out, "tests") {
		t.Errorf("absent files listed:\n%s", out)
	}
}

// The rows the user can act on carry the `!` marker and end with the
// command: missing tools, no b.yaml, no clusters, no active domain, an
// active domain without a spec, no environment file, a bash
// implementation without its tree, an invalid implementation block.
func TestWriteCardActionableRows(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Git.Branch = "main"
	s.Project.BYAML, s.Project.Tools, s.Project.ToolsMissing = true, 17, []string{"b", "kustomize", "khelm"}
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}}
	contains(t, card(s),
		"! toolchain    .bin/b.yaml · 3 of 17 missing · lo toolchain install\n",
		"! clusters     alpha.dev (kind) · none active · lo use <domain>\n",
		"! environment  none · lo init project --env mise|direnv\n")

	s.Project.Active = "zeta.dev"
	contains(t, card(s), "! clusters     alpha.dev (kind) · active zeta.dev has no spec · lo use <domain>\n")

	s.Project.BYAML, s.Project.Tools, s.Project.ToolsMissing = false, 0, nil
	s.Project.Clusters, s.Project.Domains, s.Project.Active = false, nil, ""
	s.Project.Implementation, s.Project.BashTree = "bash", false
	contains(t, card(s),
		"  acme         project · bash · main\n",
		"! toolchain    none · lo toolchain install\n",
		"! clusters     no clusters/ directory · lo init project --cluster <domain>\n",
		"! environment  none · lo init project --env mise|direnv · bash tree missing · lo assets eject bash\n")

	s.Project.Clusters = true
	s.Project.ImplementationErr = "lok8s.yaml: bad"
	s.Project.BashTree = true
	s.Project.EnvFile = "direnv"
	contains(t, card(s),
		"! acme         project · invalid implementation (lok8s.yaml: bad) · main\n",
		"! clusters     none · lo init project --cluster <domain>\n",
		"  environment  .envrc · bash tree present\n")
}

// Inside a project: the position below the root as a relative path, the
// service directory named, a submodule's repository on its own row (the
// path relative to the root); a project marked by clusters/ alone says
// so; the root path never appears.
func TestWriteCardInsideProject(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, false)
	s.Git.Branch = "main"
	s.Project.BYAML, s.Project.Tools = true, 4
	s.Project.EnvFile = "mise"
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}}
	s.Project.Active = "alpha.dev"
	out := card(s)
	contains(t, out, "  acme         project · go · main\n", "  directory    sub\n")
	if strings.Contains(out, root) || strings.Contains(out, "repository") {
		t.Errorf("a path or a repository row at the project's own repository:\n%s", out)
	}

	s.Cwd = filepath.Join(root, "api")
	s.ServiceDir = true
	s.Git = Git{Available: true, Root: s.Cwd, AtRoot: true, Submodule: true, Branch: "feat", Uncommitted: 1}
	s.Project.Name, s.Project.ProjectFile = "", false
	out = card(s)
	name := filepath.Base(root)
	contains(t, out,
		"  "+name+strings.Repeat(" ", 13-len(name))+"project · no lok8s.yaml · go\n",
		"  directory    api · service directory\n",
		"  repository   api · feat, 1 uncommitted · submodule or worktree\n")
	if strings.Contains(out, root) {
		t.Errorf("the root printed as a path:\n%s", out)
	}

	// A project inside a larger repository: the repository above the root.
	s = projectState(root, true)
	s.Git = Git{Available: true, Root: filepath.Dir(root), Branch: "main"}
	contains(t, card(s), "  acme         project · go\n", "  repository   .. · main\n")
}

// On a terminal the keys are dim, the name bold, the marker yellow; the
// values stay plain. Off a terminal no escape sequence is written.
func TestWriteCardTerminalColours(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Git.Branch = "main"
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}}
	s.Project.Active = "alpha.dev"
	s.Project.EnvFile = "mise"
	if out := card(s); strings.Contains(out, "\033[") {
		t.Errorf("escape sequences off a terminal:\n%q", out)
	}
	s.Terminal.StdoutTTY = true
	contains(t, card(s),
		"  \033[1macme\033[0m         project · go · main\n",
		"  \033[2mclusters\033[0m     alpha.dev (kind, active)\n",
		"\033[33m!\033[0m \033[2mtoolchain\033[0m    none · lo toolchain install\n")
}

func TestPlural(t *testing.T) {
	for in, want := range map[int]string{0: "0 entries", 1: "1 entry", 2: "2 entries"} {
		if got := entries(in); got != want {
			t.Errorf("entries(%d) = %q", in, got)
		}
	}
	if got := plural(1, "tool"); got != "1 tool" {
		t.Errorf("plural: %q", got)
	}
}

func TestWriteSummaryAndOptions(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "acme")
	s := State{Cwd: cwd, Empty: true, Git: Git{Available: true}}
	var b bytes.Buffer
	WriteSummary(&b, Decide(s, Answers{Dir: "sub", Domain: "d.dev", Use: true, GitInit: true}), cwd)
	contains(t, b.String(),
		"Plan (empty directory):\n",
		"  1. project files: clusters/, lok8s.yaml, .gitignore entries, mise.toml, clusters/d.dev/cluster.lok8s.yaml (lo)\n",
		"  2. git init: a git repository in sub\n",
		"  3. use: clusters/.active = d.dev\n",
		"Equivalent commands:\n  cd sub\n  lo init project sub --env mise --path sub --cluster d.dev --driver lo\n  git init\n  lo use d.dev\n")

	b.Reset()
	WriteSummary(&b, Plan{}, cwd)
	if b.String() != "Nothing to do.\n" {
		t.Errorf("empty plan: %q", b.String())
	}

	b.Reset()
	WriteOptions(&b, s)
	if b.String() != "" {
		t.Errorf("options without a project: %q", b.String())
	}
	root := t.TempDir()
	WriteOptions(&b, projectState(root, true))
	contains(t, b.String(), "Available (lo init on a terminal asks; or run the command):\n", "Add a cluster spec", "lo init project --env none --cluster <domain> --driver <driver>", "lo init test")
}
