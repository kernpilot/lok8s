package initctx

// detect_test.go — Detect over temp directories with git scripted through
// the Runner seam. Nothing reaches a real git or the network.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// gitFake answers the two git reads. root "" = not a repository (rc 128);
// absent = git not installed.
type gitFake struct {
	absent bool
	root   string
	dirty  bool
	// branch answers symbolic-ref ("" = detached: the read fails).
	branch string
	cmds   []execx.Cmd
}

func (g *gitFake) Run(_ context.Context, c execx.Cmd) error {
	g.cmds = append(g.cmds, c)
	if c.Name != "git" {
		return fmt.Errorf("unexpected tool %s", c.Name)
	}
	if g.absent {
		return fmt.Errorf("git: %w", execx.ErrNotFound)
	}
	switch c.Args[0] {
	case "rev-parse":
		if g.root == "" {
			fmt.Fprintln(c.Stderr, "fatal: not a git repository")
			return errors.New("exit status 128")
		}
		fmt.Fprintln(c.Stdout, g.root)
	case "symbolic-ref":
		if g.branch == "" {
			return errors.New("exit status 1")
		}
		fmt.Fprintln(c.Stdout, g.branch)
	case "status":
		if g.dirty {
			fmt.Fprint(c.Stdout, " M file\n?? other\n")
		}
	}
	return nil
}

func detect(t *testing.T, cwd string, g *gitFake) State {
	t.Helper()
	var r execx.Runner
	if g != nil {
		r = g
	}
	s, err := Detect(context.Background(), cwd, r)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDetectEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	s := detect(t, dir, &gitFake{absent: true})
	if !s.Empty || s.Entries != 0 || s.Project != nil || s.ServiceDir {
		t.Errorf("state: %+v", s)
	}
	if s.Git.Available {
		t.Error("git absent, reported available")
	}
	if got := s.Situation(); got != SituationEmptyDir {
		t.Errorf("situation %v, want empty directory", got)
	}

	// A .git entry alone keeps the directory empty (git init ran first).
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	s = detect(t, dir, &gitFake{root: dir})
	if !s.Empty || !s.Git.Available || s.Git.Root != dir || !s.Git.AtRoot || s.Git.Submodule {
		t.Errorf("empty dir with .git: %+v", s)
	}
	if got := s.Situation(); got != SituationEmptyDir {
		t.Errorf("situation %v, want empty directory", got)
	}

	// Every git read runs in cwd through the seam.
	g := &gitFake{root: dir, branch: "main"}
	s = detect(t, dir, g)
	if len(g.cmds) != 3 || g.cmds[0].Dir != dir || g.cmds[1].Dir != dir || g.cmds[2].Dir != dir {
		t.Errorf("git cmds: %+v", g.cmds)
	}
	if !reflect.DeepEqual(g.cmds[0].Args, []string{"rev-parse", "--show-toplevel"}) ||
		!reflect.DeepEqual(g.cmds[1].Args, []string{"symbolic-ref", "--short", "-q", "HEAD"}) ||
		!reflect.DeepEqual(g.cmds[2].Args, []string{"status", "--porcelain"}) {
		t.Errorf("git argv: %+v", g.cmds)
	}
	if s.Git.Branch != "main" || s.Git.Uncommitted != 0 || s.Git.Dirty() {
		t.Errorf("git: %+v", s.Git)
	}
	// Detached HEAD: no branch, still a repository.
	if s := detect(t, dir, &gitFake{root: dir}); s.Git.Root != dir || s.Git.Branch != "" {
		t.Errorf("detached: %+v", s.Git)
	}
}

// git prints the repository root with symlinks resolved; the state keeps
// the form of the working directory (one path form on the card).
func TestDetectGitRootInTheWorkingDirectoryForm(t *testing.T) {
	real := t.TempDir()
	os.MkdirAll(filepath.Join(real, "repo", "sub"), 0o755)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	cwd := filepath.Join(link, "repo", "sub")
	s := detect(t, cwd, &gitFake{root: filepath.Join(real, "repo"), branch: "main"})
	if want := filepath.Join(link, "repo"); s.Git.Root != want {
		t.Errorf("git root %q, want %q", s.Git.Root, want)
	}
	if s.Git.AtRoot {
		t.Error("at root below it")
	}
	if s := detect(t, filepath.Join(link, "repo"), &gitFake{root: filepath.Join(real, "repo")}); !s.Git.AtRoot {
		t.Error("not at root at the root")
	}
	// A root git printed that the typed form cannot reach stays as printed.
	if got := cwdForm(cwd, "/elsewhere/repo"); got != "/elsewhere/repo" {
		t.Errorf("unrelated root rewritten: %q", got)
	}
}

func TestDetectGitBelowRootWithoutProject(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	sub := filepath.Join(repo, "services", "api")
	testutil.WriteFile(t, filepath.Join(sub, "main.go"), "package main\n")
	s := detect(t, sub, &gitFake{root: repo, dirty: true})
	if s.Empty || s.Entries != 1 || s.Project != nil {
		t.Errorf("state: %+v", s)
	}
	// The porcelain fake lists two paths.
	if s.Git.Root != repo || s.Git.AtRoot || !s.Git.Dirty() || s.Git.Uncommitted != 2 {
		t.Errorf("git: %+v", s.Git)
	}
	if got := s.Situation(); got != SituationGitBelowRoot {
		t.Errorf("situation %v, want git below root", got)
	}
}

func TestDetectBareDirectory(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteFile(t, filepath.Join(dir, "README.md"), "# hi\n")
	testutil.WriteFile(t, filepath.Join(dir, "src", "a.go"), "package a\n")

	// git installed, not a repository (rev-parse says no).
	s := detect(t, dir, &gitFake{})
	if s.Empty || s.Entries != 2 || !s.Git.Available || s.Git.Root != "" {
		t.Errorf("state: %+v git %+v", s, s.Git)
	}
	if got := s.Situation(); got != SituationBareDir {
		t.Errorf("situation %v, want bare directory", got)
	}

	// git binary absent (execx.ErrNotFound): unavailable, and the
	// situation is the same bare directory.
	s = detect(t, dir, &gitFake{absent: true})
	if s.Git.Available || s.Git.Root != "" || s.Situation() != SituationBareDir {
		t.Errorf("git absent: %+v", s.Git)
	}

	// At the git root, still no project: the bare conversation, without
	// the git init offer (Git.Root is set).
	s = detect(t, dir, &gitFake{root: dir})
	if got := s.Situation(); got != SituationBareDir || !s.Git.AtRoot {
		t.Errorf("situation %v at root %v, want bare directory at the git root", got, s.Git.AtRoot)
	}

	// A nil runner: git is simply unavailable.
	s = detect(t, dir, nil)
	if s.Git.Available {
		t.Error("nil runner reported git available")
	}
}

// fullProject writes a project with everything Detect reports.
func fullProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: acme\nspec:\n  implementation:\n    default: bash\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", "beta.cloud", "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: beta\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: alpha\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", "gamma.app", "deploy.lok8s.yaml"), "kind: Deploy\nspec:\n  clusterRef:\n    domain: alpha.dev\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", "nospec", "notes.txt"), "")
	testutil.WriteFile(t, filepath.Join(root, "clusters", ".active"), "alpha.dev\n")
	testutil.WriteFile(t, filepath.Join(root, "mise.toml"), "[env]\n")
	// Three pins the way b.yaml spells them: a bare tool, one installed
	// as a file under .kustomize, one under an alias.
	testutil.WriteFile(t, filepath.Join(root, ".bin", "b.yaml"), "binaries:\n  kustomize: {}\n  github.com/mgoltzsche/khelm:\n    file: ../.kustomize/khelm/ChartRenderer\n  renvsubst:\n    alias: envsubst\n")
	testutil.WriteFile(t, filepath.Join(root, ".lok8s", "lo"), "#!/bin/bash\n")
	testutil.WriteFile(t, filepath.Join(root, "services.yaml"), "services: {}\n")
	os.MkdirAll(filepath.Join(root, "tests"), 0o755)
	return root
}

func TestDetectProjectRoot(t *testing.T) {
	root := fullProject(t)
	s := detect(t, root, &gitFake{root: root})
	if s.Project == nil {
		t.Fatal("no project detected at the root")
	}
	p := s.Project
	if got := s.Situation(); got != SituationProjectRoot {
		t.Errorf("situation %v, want project root", got)
	}
	if !p.AtRoot || p.Root != root || p.Name != "acme" || !p.ProjectFile || !p.Clusters {
		t.Errorf("project: %+v", p)
	}
	wantDomains := []Domain{{"alpha.dev", "lo"}, {"beta.cloud", "kubeone"}, {"gamma.app", "deploy"}}
	if !reflect.DeepEqual(p.Domains, wantDomains) {
		t.Errorf("domains %+v, want %+v", p.Domains, wantDomains)
	}
	if p.Active != "alpha.dev" || p.EnvFile != "mise" || !p.BYAML || !p.BashTree {
		t.Errorf("project: %+v", p)
	}
	if p.Implementation != "bash" || p.ImplementationErr != "" || !p.Services || !p.Tests {
		t.Errorf("project: %+v", p)
	}
	// b plus the three pins; nothing under .bin/.kustomize is executable
	// yet, so every one is missing: b first, then the pins by key, each
	// under the name b installs it as.
	if p.Tools != 4 || !reflect.DeepEqual(p.ToolsMissing, []string{"b", "khelm", "kustomize", "envsubst"}) {
		t.Errorf("tools: %d missing %v", p.Tools, p.ToolsMissing)
	}
	if s.Empty || s.ServiceDir {
		t.Errorf("root state: %+v", s)
	}

	// The pins resolve at the paths b installs to: <bin>/<name>, the
	// alias, the file relative to the b.yaml directory.
	for _, f := range []string{".bin/b", ".bin/envsubst", ".kustomize/khelm/ChartRenderer"} {
		testutil.WriteFile(t, filepath.Join(root, filepath.FromSlash(f)), "#!/bin/sh\n")
		os.Chmod(filepath.Join(root, filepath.FromSlash(f)), 0o755)
	}
	p = detect(t, root, &gitFake{root: root}).Project
	if p.Tools != 4 || !reflect.DeepEqual(p.ToolsMissing, []string{"kustomize"}) {
		t.Errorf("tools after install: %d missing %v", p.Tools, p.ToolsMissing)
	}
}

func TestDetectMinimalProjectAndInvalidPieces(t *testing.T) {
	// clusters/ alone is a marker: no project file, no name.
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "clusters"), 0o755)
	testutil.WriteFile(t, filepath.Join(root, ".envrc"), "PATH_add .bin\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", ".active"), "../evil\n")
	testutil.WriteFile(t, filepath.Join(root, ".bin", "b.yaml"), "# hand written\n")
	s := detect(t, root, &gitFake{})
	p := s.Project
	if p == nil || p.ProjectFile || p.Name != "" || !p.Clusters || p.Domains != nil || p.Active != "" {
		t.Errorf("project: %+v", p)
	}
	if p.EnvFile != "direnv" || !p.BYAML || p.Implementation != "go" || p.BashTree {
		t.Errorf("project: %+v", p)
	}
	// A b.yaml without binaries pins b alone; no b.yaml pins nothing.
	if p.Tools != 1 || !reflect.DeepEqual(p.ToolsMissing, []string{"b"}) {
		t.Errorf("tools: %d missing %v", p.Tools, p.ToolsMissing)
	}
	if n, missing := pinnedTools(filepath.Join(root, "nowhere", "b.yaml")); n != 0 || missing != nil {
		t.Errorf("tools without b.yaml: %d %v", n, missing)
	}

	// An invalid implementation block is reported, not fatal.
	testutil.WriteFile(t, filepath.Join(root, "lok8s.yaml"), "kind: Project\nmetadata:\n  name: x\nspec:\n  implementation:\n    default: python\n")
	s = detect(t, root, &gitFake{})
	if s.Project.ImplementationErr == "" {
		t.Error("invalid implementation block not reported")
	}
}

func TestDetectInsideProject(t *testing.T) {
	root := fullProject(t)

	// A plain subdirectory.
	sub := filepath.Join(root, "docs", "adr")
	testutil.WriteFile(t, filepath.Join(sub, "0001.md"), "")
	s := detect(t, sub, &gitFake{root: root})
	if s.Project == nil || s.Project.AtRoot || s.Project.Root != root || s.ServiceDir {
		t.Errorf("subdir state: %+v", s)
	}
	if got := s.Situation(); got != SituationInsideProject {
		t.Errorf("situation %v, want inside project", got)
	}
	if s.ServiceName() != "" {
		t.Errorf("service name %q outside a service dir", s.ServiceName())
	}

	// A service directory: the kind-less lok8s.yaml `lo init service`
	// writes. It is not a marker, so the project stays the umbrella.
	svc := filepath.Join(root, "api")
	testutil.WriteFile(t, filepath.Join(svc, "lok8s.yaml"), "build:\n  context: .\n  dockerfile: Dockerfile\n")
	s = detect(t, svc, &gitFake{root: root})
	if !s.ServiceDir || s.ServiceName() != "api" || s.Project == nil || s.Project.Root != root {
		t.Errorf("service dir state: %+v", s)
	}
	if got := s.Situation(); got != SituationInsideProject {
		t.Errorf("situation %v, want inside project", got)
	}

	// A `kind: Service` file counts as a service directory too; a
	// `kind: Project` file never does.
	testutil.WriteFile(t, filepath.Join(svc, "lok8s.yaml"), "kind: Service\nbuild: {}\n")
	if s := detect(t, svc, nil); !s.ServiceDir {
		t.Error("kind: Service not a service dir")
	}
	if s := detect(t, root, nil); s.ServiceDir {
		t.Error("the project file counted as a service dir")
	}
}

func TestDetectUmbrellaAboveSubmodule(t *testing.T) {
	root := fullProject(t)
	// A submodule checkout: its .git is a FILE pointing into the umbrella's
	// modules dir. Its own git root is the submodule, the project is the
	// umbrella above.
	sub := filepath.Join(root, "kubehz-api")
	testutil.WriteFile(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/kubehz-api\n")
	testutil.WriteFile(t, filepath.Join(sub, "lok8s.yaml"), "build:\n  context: .\n")
	s := detect(t, sub, &gitFake{root: sub})
	if s.Project == nil || s.Project.Root != root || s.Project.AtRoot {
		t.Fatalf("umbrella not found: %+v", s.Project)
	}
	if !s.Git.Submodule || !s.Git.AtRoot || s.Git.Root != sub {
		t.Errorf("git: %+v", s.Git)
	}
	if !s.ServiceDir || s.ServiceName() != "kubehz-api" {
		t.Errorf("service dir: %+v", s)
	}
	if got := s.Situation(); got != SituationInsideProject {
		t.Errorf("situation %v, want inside project", got)
	}
}

func TestDetectServiceDirWithoutProject(t *testing.T) {
	// A service checkout on its own (no umbrella above): a bare directory
	// that happens to be a service; the wizard offers a project, not a
	// registration.
	dir := t.TempDir()
	testutil.WriteFile(t, filepath.Join(dir, "lok8s.yaml"), "build:\n  context: .\n")
	s := detect(t, dir, &gitFake{root: dir})
	if s.Project != nil || !s.ServiceDir {
		t.Errorf("state: %+v", s)
	}
	if got := s.Situation(); got != SituationBareDir {
		t.Errorf("situation %v, want bare directory", got)
	}
}

func TestDetectMissingDirectory(t *testing.T) {
	if _, err := Detect(context.Background(), filepath.Join(t.TempDir(), "gone"), nil); err == nil {
		t.Error("missing cwd accepted")
	}
}

func TestTerminalInteractive(t *testing.T) {
	cases := []struct {
		term Terminal
		want bool
	}{
		{Terminal{StdinTTY: true, StdoutTTY: true}, true},
		{Terminal{StdinTTY: false, StdoutTTY: true}, false},
		{Terminal{StdinTTY: true, StdoutTTY: false}, false},
		{Terminal{StdinTTY: true, StdoutTTY: true, CI: true}, false},
		{Terminal{StdinTTY: true, StdoutTTY: true, Yes: true}, false},
		{Terminal{}, false},
	}
	for _, c := range cases {
		if got := c.term.Interactive(); got != c.want {
			t.Errorf("%+v: interactive %v, want %v", c.term, got, c.want)
		}
	}

	// DetectTerminal: a pipe is not a terminal; CI is read as presence.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	t.Setenv("CI", "")
	got := DetectTerminal(r, w, true)
	if got.StdinTTY || got.StdoutTTY || !got.CI || !got.Yes {
		t.Errorf("pipe terminal: %+v", got)
	}
	os.Unsetenv("CI")
	if got := DetectTerminal(r, w, false); got.CI || got.Yes {
		t.Errorf("CI unset: %+v", got)
	}
	if got := DetectTerminal(nil, nil, false); got.StdinTTY || got.StdoutTTY {
		t.Errorf("nil streams: %+v", got)
	}
}

func TestSituationString(t *testing.T) {
	for s, want := range map[Situation]string{
		SituationEmptyDir: "empty directory", SituationGitBelowRoot: "git repository, below its root, no project",
		SituationBareDir: "directory without a project", SituationProjectRoot: "project root",
		SituationInsideProject: "inside a project", SituationUnknown: "unknown",
	} {
		if s.String() != want {
			t.Errorf("%d: %q, want %q", s, s.String(), want)
		}
	}
}
