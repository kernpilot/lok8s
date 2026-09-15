package initctx

// card_test.go: the state card (goldens off and on a terminal, the path
// rule, the counts, the `!` rows) and the next line.

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/ui"
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

// tty renders w as a terminal that allows colour.
func tty(w io.Writer) io.Writer { return ui.Styled(w, ui.Style{TTY: true, Color: true}) }

func cardTTY(s State) string {
	var b bytes.Buffer
	WriteCard(tty(&b), s)
	return b.String()
}

func wantCard(t *testing.T, s State, want string) {
	t.Helper()
	if got := card(s); got != want {
		t.Errorf("card:\n%s\nwant:\n%s", got, want)
	}
}

// A project root: every fact on its row, no path (the working directory
// is the root), the domains grouped by driver with the active one first.
// Outside a project there is no card.
func TestWriteCardProjectRoot(t *testing.T) {
	root := t.TempDir()
	if out := card(State{Cwd: root, Empty: true}); out != "" {
		t.Errorf("a card without a project:\n%s", out)
	}
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

// The rows the user can act on carry the `!` marker and facts only (no
// command): missing tools, no b.yaml, an unreadable b.yaml, no clusters,
// no active domain, an active domain without a spec, no environment
// file, a bash implementation without its tree, an invalid
// implementation block.
func TestWriteCardActionableRows(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Git.Branch = "main"
	s.Project.BYAML, s.Project.Tools, s.Project.ToolsMissing = true, 17, []string{"b", "kustomize", "khelm"}
	s.Project.Domains = []Domain{{"alpha.dev", "lo"}}
	contains(t, card(s),
		"! toolchain    .bin/b.yaml · 3 of 17 missing\n",
		"! clusters     alpha.dev (kind) · none active\n",
		"! environment  none\n")

	s.Project.Active = "zeta.dev"
	contains(t, card(s), "! clusters     alpha.dev (kind) · active zeta.dev has no spec\n")

	s.Project.BYAMLInvalid, s.Project.Tools, s.Project.ToolsMissing = true, 0, nil
	contains(t, card(s), "! toolchain    .bin/b.yaml unreadable\n")

	s.Project.BYAML, s.Project.BYAMLInvalid = false, false
	s.Project.Clusters, s.Project.Domains, s.Project.Active = false, nil, ""
	s.Project.Implementation, s.Project.BashTree = "bash", false
	contains(t, card(s),
		"  acme         project · bash · main\n",
		"! toolchain    none\n",
		"! clusters     no clusters/ directory\n",
		"! environment  none · bash tree missing\n")

	s.Project.Clusters = true
	s.Project.ImplementationErr = "lok8s.yaml: bad"
	s.Project.BashTree = true
	s.Project.EnvFile = "direnv"
	contains(t, card(s),
		"! acme         project · invalid implementation (lok8s.yaml: bad) · main\n",
		"! clusters     none\n",
		"  environment  .envrc · bash tree present\n")
	// No row names a command: the list and --plan carry those.
	if out := card(s); strings.Contains(out, "lo ") || strings.Contains(out, "--") {
		t.Errorf("a command inside a card row:\n%s", out)
	}
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
	// A detached HEAD, no git at all.
	s.Git = Git{Available: true, Root: root, AtRoot: true}
	contains(t, card(s), "  acme         project · go · detached HEAD\n")
	s.Git = Git{}
	contains(t, card(s), "  acme         project · go · no git\n")
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
	contains(t, cardTTY(s),
		"  \033[1macme\033[0m         project · go · main\n",
		"  \033[2mclusters\033[0m     alpha.dev (kind, active)\n",
		"\033[33m!\033[0m \033[2mtoolchain\033[0m    none\n")
}

func TestPlural(t *testing.T) {
	for in, want := range map[int]string{0: "0 entries", 1: "1 entry", 2: "2 entries"} {
		if got := plural(in, "entry"); got != want {
			t.Errorf("plural(%d, entry) = %q", in, got)
		}
	}
	if got := plural(1, "tool"); got != "1 tool" {
		t.Errorf("plural: %q", got)
	}
}

func TestWriteNext(t *testing.T) {
	var b bytes.Buffer
	WriteNext(&b, "lo up")
	if b.String() != "  next  lo up\n" {
		t.Errorf("next: %q", b.String())
	}
	b.Reset()
	WriteNext(tty(&b), "lo up")
	if b.String() != "  \033[2mnext\033[0m  lo up\n" {
		t.Errorf("next on a terminal: %q", b.String())
	}
}
