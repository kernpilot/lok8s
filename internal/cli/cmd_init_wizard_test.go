package cli

// cmd_init_wizard_test.go — bare `lo init` and the screen verbs: the
// help off a terminal, under CI and with --yes (byte-identical to
// cmd.Help()); --plan writes nothing and prints the mode's screen as
// text; the bootstrap (mode 1) and project mode (mode 2), driven through
// huh's accessible mode over a scripted reader, write only on a screen's
// Create and run git and the toolchain through fakes (never a real git,
// never the network).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/initctx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// initGitFake answers `git rev-parse` (not a repository unless repo is
// set), `symbolic-ref` (main) and records `git init`.
type initGitFake struct {
	calls []execx.Cmd
	repo  string // the root rev-parse reports; "" = not a repository
}

func (g *initGitFake) Run(_ context.Context, c execx.Cmd) error {
	g.calls = append(g.calls, c)
	if c.Name != "git" {
		return fmt.Errorf("unexpected tool under test: %s %v", c.Name, c.Args)
	}
	switch c.Args[0] {
	case "rev-parse":
		if g.repo == "" {
			return errors.New("exit status 128")
		}
		fmt.Fprintln(c.Stdout, g.repo)
	case "symbolic-ref":
		fmt.Fprintln(c.Stdout, "main")
	case "init":
		fmt.Fprintln(c.Stdout, "Initialized empty Git repository (fake)")
	}
	return nil
}

// initSeams installs the test seams: a terminal state, a scripted form,
// a git fake and a toolchain fake that records its call and lands a
// b.yaml with no pins plus an executable b (every pinned tool present).
type initSeams struct {
	git       *initGitFake
	toolchain []string
	formOut   bytes.Buffer
}

func installInitSeams(t *testing.T, interactive bool, script string) *initSeams {
	t.Helper()
	s := &initSeams{git: &initGitFake{}}
	prevTerm, prevIO, prevTool, prevRunner := initTerminal, initFormIO, initToolchainInstall, newRunner
	initTerminal = func(yes bool) initctx.Terminal {
		return initctx.Terminal{StdinTTY: interactive, StdoutTTY: interactive, Yes: yes}
	}
	initFormIO = func() initctx.IO {
		return initctx.IO{In: iotest.OneByteReader(strings.NewReader(script)), Out: &s.formOut, Accessible: true}
	}
	initToolchainInstall = func(_ context.Context, base string, groups []string, dryRun bool, out, _ io.Writer) error {
		s.toolchain = append(s.toolchain, fmt.Sprintf("%s %s dry=%v", base, strings.Join(groups, ","), dryRun))
		fmt.Fprintln(out, "toolchain (fake)")
		testutil.WriteFile(t, filepath.Join(base, ".bin", "b.yaml"), "binaries: {}\n")
		testutil.WriteFile(t, filepath.Join(base, ".bin", "b"), "#!/bin/sh\n")
		return os.Chmod(filepath.Join(base, ".bin", "b"), 0o755)
	}
	newRunner = func(*config.Paths) execx.Runner { return s.git }
	t.Cleanup(func() {
		initTerminal, initFormIO, initToolchainInstall, newRunner = prevTerm, prevIO, prevTool, prevRunner
	})
	return s
}

func initHelpText(t *testing.T, p *config.Paths) string {
	t.Helper()
	root := NewRoot(p)
	cmd, _, err := root.Find([]string{"init"})
	if err != nil {
		t.Fatal(err)
	}
	// Execute adds the help flag before a RunE prints the help; the
	// reference must carry it too.
	cmd.InitDefaultHelpFlag()
	var b bytes.Buffer
	cmd.SetOut(&b)
	if err := cmd.Help(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// projectRoot writes a project with one cluster spec (alpha.dev) and
// returns its root; active names clusters/.active ("" = none).
func projectRoot(t *testing.T, active string) string {
	t.Helper()
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: acme\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: alpha\n")
	if active != "" {
		testutil.WriteFile(t, filepath.Join(root, "clusters", ".active"), active+"\n")
	}
	return root
}

func TestInitBareOffTerminalPrintsHelp(t *testing.T) {
	p := synthProject(t)
	want := initHelpText(t, p)
	if !strings.Contains(want, "Usage:") || !strings.Contains(want, "--plan") {
		t.Fatalf("help reference:\n%s", want)
	}
	cases := []struct {
		name string
		term initctx.Terminal
		args []string
	}{
		{"no tty", initctx.Terminal{}, []string{"init"}},
		{"stdout only", initctx.Terminal{StdoutTTY: true}, []string{"init"}},
		{"CI", initctx.Terminal{StdinTTY: true, StdoutTTY: true, CI: true}, []string{"init"}},
		{"--yes", initctx.Terminal{StdinTTY: true, StdoutTTY: true}, []string{"init", "--yes"}},
		{"-y", initctx.Terminal{StdinTTY: true, StdoutTTY: true}, []string{"init", "-y"}},
	}
	prev := initTerminal
	t.Cleanup(func() { initTerminal = prev })
	for _, c := range cases {
		term := c.term
		initTerminal = func(yes bool) initctx.Terminal { term.Yes = yes; return term }
		stdout, stderr, err := runLo(t, NewRoot(p), c.args...)
		if err != nil || stderr != "" {
			t.Errorf("%s: err=%v stderr=%q", c.name, err, stderr)
		}
		if stdout != want {
			t.Errorf("%s: help differs:\n%s\nwant:\n%s", c.name, stdout, want)
		}
	}
	// An argument is still the argsh usage error.
	_, stderr, err := runLo(t, NewRoot(p), "init", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: Invalid command: bogus") {
		t.Errorf("init bogus: err=%v stderr=%q", err, stderr)
	}
}

// --plan in an empty directory: the welcome and the bootstrap screen as
// text, no path, no colour, nothing written, no form.
func TestInitPlanEmptyDirectory(t *testing.T) {
	for _, args := range [][]string{{"init", "--plan"}, {"init", "--dry-run"}, {"init", "--yes", "--dry-run"}} {
		dir := filepath.Join(t.TempDir(), "shop")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		seams := installInitSeams(t, false, "")
		p := synthProject(t)
		stdout, stderr, err := runLo(t, NewRoot(p), args...)
		if err != nil || stderr != "" {
			t.Fatalf("%v: %v\n%s", args, err, stderr)
		}
		for _, want := range []string{
			"  lo init sets up a lok8s project in this directory: the project file, the first cluster spec, the toolchain.\n",
			"  New project\n",
			"  name         shop\n",
			"  domain       shop.dev\n",
			"  driver       lo · kind on local Docker (dev clusters)\n",
			"  environment  mise.toml\n",
			"  toolchain    install now · 8 tools · network\n",
			"  git          initialise a repository\n",
			"  writes       clusters/ · lok8s.yaml · .gitignore entries · mise.toml · clusters/shop.dev/cluster.lok8s.yaml · .bin/b.yaml · .bin/ (the pinned tools) · clusters/.active\n",
			"  runs         git init · lo toolchain install (network) · lo use shop.dev\n",
			"  equivalent   lo init project shop --env mise --cluster shop.dev --driver lo · git init · lo toolchain install --groups core,local · lo use shop.dev\n",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("%v: missing %q in:\n%s", args, want, stdout)
			}
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%v: --plan wrote %v", args, entries)
		}
		if len(seams.toolchain) != 0 || len(seams.git.calls) != 1 || seams.git.calls[0].Args[0] != "rev-parse" || seams.formOut.Len() != 0 {
			t.Errorf("%v: --plan ran something: toolchain=%v git=%v form=%q", args, seams.toolchain, seams.git.calls, seams.formOut.String())
		}
		if strings.Contains(stdout, p.Base) || strings.Contains(stdout, dir) || strings.Contains(stdout, "\033[") || strings.Contains(stdout, "Usage:") {
			t.Errorf("%v: a path, an escape sequence or the help:\n%s", args, stdout)
		}
	}
}

// --plan in a project: the card, the list as text with the twins, the
// next step; no doctor section, no path, no colour, no form.
func TestInitPlanProjectRoot(t *testing.T) {
	root := projectRoot(t, "alpha.dev")
	t.Chdir(root)
	seams := installInitSeams(t, false, "")
	seams.git.repo = root
	stdout, _, err := runLo(t, NewRoot(synthProject(t)), "init", "--plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  acme         project · go · main\n",
		"  clusters     alpha.dev (kind, active)\n",
		"! toolchain    none\n",
		"! environment  none\n",
		"  actions     Add a cluster · Add a service · Add the test suite · Install the toolchain · Eject the bash tree · Exit\n",
		"  equivalent  lo init cluster <domain> · lo init service <name> · lo init test · lo toolchain install · lo assets eject bash\n",
		"  next        lo toolchain install\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	for _, no := range []string{"--- ", root, "\033[", "New project"} {
		if strings.Contains(stdout, no) {
			t.Errorf("unexpected %q in:\n%s", no, stdout)
		}
	}
	if seams.formOut.Len() != 0 {
		t.Errorf("--plan opened a form:\n%s", seams.formOut.String())
	}
}

// Below the root the card names the position relative to it; the
// repository row appears only when the repository root is not the
// project root.
func TestInitPlanInsideProjectNamesThePosition(t *testing.T) {
	root := projectRoot(t, "")
	sub := filepath.Join(root, "services", "api")
	testutil.WriteFile(t, filepath.Join(sub, "lok8s.yaml"), "build:\n  context: .\n")
	t.Chdir(sub)
	seams := installInitSeams(t, false, "")
	seams.git.repo = root
	stdout, _, err := runLo(t, NewRoot(synthProject(t)), "init", "--plan")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  acme         project · go · main\n") || !strings.Contains(stdout, "  directory    services/api · service directory\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, root) || strings.Contains(stdout, "repository") {
		t.Errorf("a path or a repository row:\n%s", stdout)
	}
}

// Mode 1 into mode 2: the welcome, the bootstrap screen, Create; the
// files, git init through the fake, the toolchain through its seam, lo
// use; then the refreshed card with the result line and the list; Exit.
func TestInitBootstrapThenProjectMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	seams := installInitSeams(t, true, "1\n5\n")
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("init: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		"  \033[1mlo init sets up a lok8s project in this directory: the project file, the first cluster spec, the toolchain.\033[0m\n",
		"  \033[1mNew project\033[0m\n",
		"  \033[2mname\033[0m         acme\n",
		"==> lo init project acme --env mise --cluster acme.dev --driver lo\n",
		"Scaffolded " + filepath.Join(dir, "lok8s.yaml") + "\n",
		"Scaffolded " + filepath.Join(dir, "clusters", "acme.dev", "cluster.lok8s.yaml") + "\n",
		"==> git init\nInitialized empty Git repository (fake)\n",
		"==> lo toolchain install --groups core,local\ntoolchain (fake)\n",
		"==> lo use acme.dev\nActive domain: acme.dev\n",
		"Done.\n",
		// Mode 2: the card of the new project, the result line, then Exit
		// with the next step.
		"  \033[1macme\033[0m         project · go · no git repository\n",
		"  \033[2mclusters\033[0m     acme.dev (kind, active)\n",
		"  \033[2mtoolchain\033[0m    .bin/b.yaml · 1 tool present\n",
		"  \033[2menvironment\033[0m  mise.toml\n",
		"  \033[2mcreated\033[0m      acme · clusters/acme.dev · toolchain 8 tools · git initialised\n",
		"  \033[2mnext\033[0m  lo up\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if !strings.Contains(seams.formOut.String(), "1. Add a cluster\n2. Add a service\n3. Add the test suite\n4. Eject the bash tree\n5. Exit\n") {
		t.Errorf("the list:\n%s", seams.formOut.String())
	}
	for _, f := range []string{"lok8s.yaml", "mise.toml", ".gitignore", "clusters/.gitkeep", "clusters/acme.dev/cluster.lok8s.yaml", "clusters/.active", ".bin/b.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not written", f)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "clusters", ".active"))
	if string(raw) != "acme.dev\n" {
		t.Errorf(".active = %q", raw)
	}
	// git: the detection read, init in the project dir, the read again
	// for the refreshed card.
	if len(seams.git.calls) != 3 || seams.git.calls[1].Args[0] != "init" || seams.git.calls[1].Dir != dir {
		t.Errorf("git calls: %+v", seams.git.calls)
	}
	if len(seams.toolchain) != 1 || seams.toolchain[0] != dir+" core,local dry=false" {
		t.Errorf("toolchain calls: %v", seams.toolchain)
	}
	// Nothing was written before Create: the screen precedes the first
	// Scaffolded line.
	if strings.Index(stdout, "equivalent") > strings.Index(stdout, "Scaffolded ") {
		t.Error("files written before the screen")
	}
	cwd, _ := os.Getwd()
	if cwd != dir {
		t.Errorf("cwd not restored: %s", cwd)
	}
}

// A project without a repository: the welcome names what it lacks, the
// bootstrap screen keeps the project's name and clusters (no new domain),
// Create runs `lo init project` (every existing file kept), `git init`
// and the toolchain, then project mode with the result line; `--plan`
// prints the same screen.
func TestInitBootstrapProjectWithoutGit(t *testing.T) {
	root := projectRoot(t, "alpha.dev")
	t.Chdir(root)
	before, _ := os.ReadFile(filepath.Join(root, "lok8s.yaml"))
	installInitSeams(t, false, "")
	stdout, _, err := runLo(t, NewRoot(synthProject(t)), "init", "--plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  lo init completes the project acme: a git repository, the toolchain.\n",
		"  New project\n",
		"  name         acme\n",
		"  domain       none\n",
		"  toolchain    install now · 8 tools · network\n",
		"  git          initialise a repository\n",
		"  writes       .gitignore entries · mise.toml · .bin/b.yaml · .bin/ (the pinned tools)\n",
		"  runs         git init · lo toolchain install (network)\n",
		"  equivalent   lo init project acme --env mise · git init · lo toolchain install --groups core,local\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "actions") || strings.Contains(stdout, "clusters/acme.dev") {
		t.Errorf("project mode or a new cluster in the plan:\n%s", stdout)
	}

	// Create, then Exit (cluster, service, tests, eject, Exit).
	seams := installInitSeams(t, true, "1\n5\n")
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("init: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		"==> lo init project acme --env mise\n",
		"==> git init\nInitialized empty Git repository (fake)\n",
		"==> lo toolchain install --groups core,local\ntoolchain (fake)\n",
		"  \033[2mclusters\033[0m     alpha.dev (kind, active)\n",
		"  \033[2mtoolchain\033[0m    .bin/b.yaml · 1 tool present\n",
		"  \033[2mcreated\033[0m      acme · toolchain 8 tools · git initialised\n",
		"  \033[2mnext\033[0m  lo up\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	after, _ := os.ReadFile(filepath.Join(root, "lok8s.yaml"))
	if string(after) != string(before) {
		t.Errorf("lok8s.yaml rewritten:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(root, "clusters", "acme.dev")); err == nil {
		t.Error("a new cluster spec written into a project that has clusters")
	}
	if len(seams.git.calls) != 3 || seams.git.calls[1].Args[0] != "init" || seams.git.calls[1].Dir != root {
		t.Errorf("git calls: %+v", seams.git.calls)
	}
}

// Cancel on the bootstrap screen: nothing written, rc 1 with the line.
func TestInitBootstrapCancelWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	seams := installInitSeams(t, true, "3\n")
	_, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "lo init: cancelled, nothing written") {
		t.Errorf("cancel: err=%v stderr=%q", err, stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("cancel wrote %v", entries)
	}
	if len(seams.git.calls) != 1 || len(seams.toolchain) != 0 {
		t.Errorf("cancel ran git=%v toolchain=%v", seams.git.calls, seams.toolchain)
	}
}

// Mode 2: Add a cluster, its screen, Create; the card again with the
// result line and the new cluster; Exit.
func TestInitProjectModeAddClusterThenBack(t *testing.T) {
	root := projectRoot(t, "")
	t.Chdir(root)
	// cluster (1): domain, driver default, active default (yes: no active
	// domain), Create; then Exit, 7th once "Set the active domain" joins
	// the list.
	seams := installInitSeams(t, true, "1\nbeta.dev\n\n\n1\n7\n")
	seams.git.repo = root
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("init: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		"\033[33m!\033[0m \033[2mclusters\033[0m     alpha.dev (kind) · none active\n",
		"  \033[1mNew cluster\033[0m\n",
		"  \033[2mactive\033[0m      yes (lo use beta.dev)\n",
		"==> lo init cluster beta.dev --driver lo\n",
		"Scaffolded " + filepath.Join(root, "clusters", "beta.dev", "cluster.lok8s.yaml") + "\n",
		"==> lo use beta.dev\nActive domain: beta.dev\n",
		"  \033[2mclusters\033[0m     beta.dev (kind, active) · alpha.dev (kind)\n",
		"  \033[2madded\033[0m        clusters/beta.dev/cluster.lok8s.yaml · active\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if n := strings.Count(stdout, "  \033[1macme\033[0m         project · go · main\n"); n != 2 {
		t.Errorf("card printed %d times, want 2:\n%s", n, stdout)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "clusters", ".active"))
	if string(raw) != "beta.dev\n" {
		t.Errorf(".active = %q", raw)
	}
	// The list after the action offers the active-domain entry (two
	// clusters now); Exit wrote nothing more.
	if !strings.Contains(seams.formOut.String(), "5. Set the active domain\n6. Eject the bash tree\n7. Exit\n") {
		t.Errorf("the refreshed list:\n%s", seams.formOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".lok8s")); err == nil {
		t.Error("Exit ran an action")
	}
}

// Exit leaves project mode with the next line and nothing written; the
// list was shown, no screen opened. EOF at the first entry's prompt
// (nothing more to read) leaves with nothing written too.
func TestInitProjectModeExitWritesNothing(t *testing.T) {
	for _, script := range []string{"6\n", ""} {
		root := projectRoot(t, "alpha.dev")
		t.Chdir(root)
		before, _ := os.ReadDir(root)
		seams := installInitSeams(t, true, script)
		seams.git.repo = root
		stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
		if err != nil || stderr != "" {
			t.Fatalf("%q: %v\n%s", script, err, stderr)
		}
		after, _ := os.ReadDir(root)
		if len(after) != len(before) {
			t.Errorf("%q: wrote %v", script, after)
		}
		if !strings.Contains(seams.formOut.String(), "6. Exit\n") || strings.Contains(stdout, "==> ") {
			t.Errorf("%q: form:\n%s\nstdout:\n%s", script, seams.formOut.String(), stdout)
		}
		if script != "" && !strings.Contains(stdout, "  \033[2mnext\033[0m  lo toolchain install\n") {
			t.Errorf("%q: no next line:\n%s", script, stdout)
		}
	}
}

// `lo init cluster`: off a terminal the domain is required and the spec
// is written with lo use; --no-active keeps the active domain; --plan
// prints the screen; on a terminal the details are asked.
func TestInitClusterVerb(t *testing.T) {
	p := synthProject(t)
	installInitSeams(t, false, "")
	_, stderr, err := runLo(t, NewRoot(p), "init", "cluster")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "give a domain: lo init cluster <domain>") {
		t.Errorf("no domain: err=%v stderr=%q", err, stderr)
	}
	stdout, _, err := runLo(t, NewRoot(p), "init", "cluster", "x.dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "==> lo init cluster x.dev --driver lo\n") || !strings.Contains(stdout, "==> lo use x.dev\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(p.Clusters, "x.dev", "cluster.lok8s.yaml")); err != nil {
		t.Error("spec not written")
	}
	raw, _ := os.ReadFile(filepath.Join(p.Clusters, ".active"))
	if string(raw) != "x.dev\n" {
		t.Errorf(".active = %q", raw)
	}

	stdout, _, err = runLo(t, NewRoot(p), "init", "cluster", "y.cloud", "--driver", "kubeone", "--no-active")
	if err != nil || strings.Contains(stdout, "lo use") {
		t.Errorf("no-active: %v\n%s", err, stdout)
	}
	raw, _ = os.ReadFile(filepath.Join(p.Clusters, ".active"))
	if string(raw) != "x.dev\n" {
		t.Errorf(".active changed: %q", raw)
	}
	if _, _, err := runLo(t, NewRoot(p), "init", "cluster", "y.cloud", "--driver", "nope", "-y"); err == nil {
		t.Error("an unknown driver accepted")
	}

	// --plan without the value a screen needs: refused with the same
	// line as off a terminal, rc 1, nothing on stdout. The test suite
	// needs no value.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"init", "cluster", "--plan"}, initctx.MissingDomain},
		{[]string{"init", "service", "--plan"}, initctx.MissingName},
		{[]string{"init", "service", "--dry-run"}, initctx.MissingName},
	} {
		stdout, stderr, err := runLo(t, NewRoot(p), c.args...)
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, c.want) || stdout != "" {
			t.Errorf("%v: err=%v stderr=%q stdout=%q", c.args, err, stderr, stdout)
		}
	}
	if stdout, _, err := runLo(t, NewRoot(p), "init", "test", "--plan"); err != nil || !strings.Contains(stdout, "  Test suite\n") {
		t.Errorf("test --plan: %v\n%s", err, stdout)
	}

	stdout, _, err = runLo(t, NewRoot(p), "init", "cluster", "z.dev", "--plan")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  New cluster\n  domain      z.dev\n") || !strings.Contains(stdout, "  equivalent  lo init cluster z.dev --driver lo · lo use z.dev\n") {
		t.Errorf("plan:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(p.Clusters, "z.dev")); err == nil {
		t.Error("--plan wrote the spec")
	}

	// On a terminal: the domain and the driver asked, active kept as
	// given on the command line, Create.
	installInitSeams(t, true, "w.dev\n2\n1\n")
	stdout, _, err = runLo(t, NewRoot(p), "init", "cluster", "--no-active")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  \033[2mequivalent\033[0m  \033[2mlo init cluster w.dev --driver kubeone --no-active\033[0m\n") || !strings.Contains(stdout, "Scaffolded "+filepath.Join(p.Clusters, "w.dev", "cluster.lok8s.yaml")+"\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	// --yes asks nothing.
	installInitSeams(t, true, "")
	if _, _, err := runLo(t, NewRoot(p), "init", "cluster", "v.dev", "--yes"); err != nil {
		t.Errorf("--yes: %v", err)
	}
}

// `lo init service` and `lo init test` on a terminal open their screen;
// the values given are fixed rows.
func TestInitServiceAndTestVerbScreens(t *testing.T) {
	p := synthProject(t)
	seams := installInitSeams(t, true, "api\n\n1\n")
	stdout, _, err := runLo(t, NewRoot(p), "init", "service")
	if err != nil {
		t.Fatal(err)
	}
	// One step: no echo line, the verb's own output.
	if !strings.Contains(stdout, "  \033[1mNew service\033[0m\n") || !strings.Contains(stdout, "Scaffolded ./api/lok8s.yaml\n") || strings.Contains(stdout, "==> ") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(p.Base, "api", "lok8s.yaml")); err != nil {
		t.Error("service not written")
	}
	if !strings.Contains(seams.formOut.String(), "Name") {
		t.Errorf("the name not asked:\n%s", seams.formOut.String())
	}

	seams = installInitSeams(t, true, "1\n")
	stdout, _, err = runLo(t, NewRoot(p), "init", "service", "web", "--path", "./services/web")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seams.formOut.String(), "Change details") || !strings.Contains(stdout, "  \033[2mpath\033[0m        ./services/web\n") {
		t.Errorf("given values asked, or the row missing:\nform:\n%s\nstdout:\n%s", seams.formOut.String(), stdout)
	}

	installInitSeams(t, true, "1\n")
	stdout, _, err = runLo(t, NewRoot(p), "init", "test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  \033[1mTest suite\033[0m\n") || !strings.Contains(stdout, "Scaffolded Playwright test suite into ") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(p.Base, "tests", "playwright.config.ts")); err != nil {
		t.Error("tests not written")
	}
	// Cancel writes nothing: the name and the path asked, then Cancel.
	installInitSeams(t, true, "shop\n\n3\n")
	if _, _, err := runLo(t, NewRoot(p), "init", "service"); !errors.Is(err, ErrHandled) {
		t.Errorf("cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Base, "shop")); err == nil {
		t.Error("cancel wrote the service")
	}
}

// A step that fails in the bootstrap: the run continues into project
// mode, the card carries the failed row (the command and the reason)
// and the not-run row, `lo use` never ran; Exit ends with rc 0. A verb
// prints the same as lines and returns the error.
func TestInitBootstrapFailureShowsTheRows(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Create; then Exit (cluster, service, tests, toolchain, the active
	// domain (the cluster exists, .active does not), eject, Exit).
	installInitSeams(t, true, "1\n7\n")
	prev := initToolchainInstall
	initToolchainInstall = func(context.Context, string, []string, bool, io.Writer, io.Writer) error {
		return errors.New("download refused (fake)")
	}
	t.Cleanup(func() { initToolchainInstall = prev })
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("init: %v\n%s\n%s", err, stdout, stderr)
	}
	name := initctx.DefaultName(dir)
	for _, want := range []string{
		"\033[33m!\033[0m \033[2mfailed\033[0m       lo toolchain install --groups core,local · download refused (fake)\n",
		"  \033[2mnot run\033[0m      lo use " + name + ".dev\n",
		"\033[33m!\033[0m \033[2mclusters\033[0m     " + name + ".dev (kind) · none active\n",
		"  \033[2mnext\033[0m  lo toolchain install\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\nDone.\n") || strings.Contains(stdout, "failed: ") {
		t.Errorf("Done or the verb's lines after a failure:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "clusters", ".active")); err == nil {
		t.Error("lo use ran after the failure")
	}

	// The verb: the lines, then the error (the suite's target is a file).
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Base, "notadir"), "x")
	installInitSeams(t, true, "1\n")
	stdout, _, err = runLo(t, NewRoot(p), "init", "test", "--path", "./notadir")
	if err == nil {
		t.Error("the verb's failed step returned no error")
	}
	if !strings.Contains(stdout, "failed: lo init test --path ./notadir\n") || strings.Contains(stdout, "not run:") {
		t.Errorf("verb stdout:\n%s", stdout)
	}
}

// Ctrl-C (ErrAborted): rc 130 through the exit seam, nothing printed.
// Esc or Cancel (ErrCancelled): the line, rc 1.
func TestInitAbort(t *testing.T) {
	var codes []int
	saved := osExit
	osExit = func(code int) { codes = append(codes, code) }
	t.Cleanup(func() { osExit = saved })
	var b bytes.Buffer
	if err := initAbort(initctx.ErrAborted, &b); !errors.Is(err, ErrHandled) || b.Len() != 0 || !reflect.DeepEqual(codes, []int{130}) {
		t.Errorf("Ctrl-C: err %v out %q codes %v", err, b.String(), codes)
	}
	if err := initAbort(initctx.ErrCancelled, &b); !errors.Is(err, ErrHandled) || !strings.Contains(b.String(), "lo init: cancelled, nothing written") {
		t.Errorf("cancel: %v %q", err, b.String())
	}
	if err := initAbort(nil, &b); err != nil {
		t.Errorf("nil mapped: %v", err)
	}
	other := errors.New("boom")
	if err := initAbort(other, io.Discard); !errors.Is(err, other) {
		t.Errorf("other error mapped: %v", err)
	}
}
