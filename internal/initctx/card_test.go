package initctx

// card_test.go — the state card, the plan summary and the option hints.

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

func TestWriteCardNewProject(t *testing.T) {
	var b bytes.Buffer
	cwd := filepath.Join(t.TempDir(), "acme")
	WriteCard(&b, State{Cwd: cwd, Empty: true, Git: Git{}})
	contains(t, b.String(), "lo init — "+cwd+"\n", "situation: empty directory\n", "git: not installed\n", "directory: empty\n")

	b.Reset()
	WriteCard(&b, State{Cwd: cwd, Entries: 3, Git: Git{Available: true}})
	contains(t, b.String(), "situation: directory without a project", "git: not a repository", "directory: 3 entries, no project marker")

	b.Reset()
	root := filepath.Dir(cwd)
	WriteCard(&b, State{Cwd: cwd, Entries: 3, ServiceDir: true, Git: Git{Available: true, Root: root, Dirty: true}})
	contains(t, b.String(), "situation: git repository, below its root, no project", "git: repository at "+root+" (you are acme below it), uncommitted changes", "a service directory (lok8s.yaml), no project above")
}

func TestWriteCardProject(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}, {"gamma.app", "deploy"}}
	s.Project.Active = "alpha.dev"
	s.Project.EnvFile = "mise"
	s.Project.BYAML, s.Project.BYAMLMarker = true, true
	s.Project.Services, s.Project.Tests = true, true
	var b bytes.Buffer
	WriteCard(&b, s)
	contains(t, b.String(),
		"situation: project root",
		"git: repository at "+root+", clean",
		"project: acme at "+root+"; you are at the root",
		"clusters: alpha.dev (lo), gamma.app (deploy); active alpha.dev",
		"environment file: mise.toml",
		"toolchain: .bin/b.yaml (lo toolchain install), every pinned tool present",
		"implementation: go; bash tree .lok8s/lo absent (lo assets eject bash)",
		"also: services.yaml, tests/")

	// The other branches: no domains, no env, a hand-written b.yaml with
	// missing tools, an invalid implementation block, a service dir under
	// a submodule.
	s = projectState(root, false)
	s.Cwd = filepath.Join(root, "api")
	s.ServiceDir = true
	s.Git = Git{Available: true, Root: s.Cwd, AtRoot: true, Submodule: true}
	s.Project.Name = ""
	s.Project.BYAML = true
	s.Project.ToolsMissing = []string{"b", "kustomize"}
	s.Project.ImplementationErr = "lok8s.yaml: bad"
	s.Project.BashTree = true
	b.Reset()
	WriteCard(&b, s)
	contains(t, b.String(),
		"situation: inside a project",
		"git: repository at "+s.Cwd+", a submodule or worktree, clean",
		"project: "+filepath.Base(root)+" (no lok8s.yaml; marked by clusters/) at "+root+"; service directory api (api), a submodule under the umbrella project",
		"clusters: clusters/ is empty (lo init project --cluster <domain>)",
		"environment file: none (lo init project --env mise|direnv)",
		"toolchain: .bin/b.yaml present; missing: b, kustomize (lo toolchain install)",
		"implementation: invalid (lok8s.yaml: bad); bash tree .lok8s/lo present")

	s.Project.Clusters = false
	s.Project.BYAMLMarker, s.Project.ToolsMissing = false, nil
	b.Reset()
	WriteCard(&b, s)
	contains(t, b.String(), "clusters: no clusters/ directory", "toolchain: .bin/b.yaml (not written by lo toolchain install), every pinned tool present")
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
