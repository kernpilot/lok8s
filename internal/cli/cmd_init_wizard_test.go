package cli

// cmd_init_wizard_test.go — bare `lo init`: the help off a terminal,
// under CI and with --yes (byte-identical to cmd.Help()); --plan writes
// nothing and prints the card and the commands; the wizard, driven
// through huh's accessible mode over a scripted reader, writes only
// after the summary is confirmed and runs git and the toolchain through
// fakes (never a real git, never the network).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/initctx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// initGitFake answers `git rev-parse` (not a repository) and records
// `git init`.
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
	case "init":
		fmt.Fprintln(c.Stdout, "Initialized empty Git repository (fake)")
	}
	return nil
}

// initSeams installs the test seams: a terminal state, a scripted form,
// a git fake and a toolchain fake that records its call.
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
		return nil
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
		{"--dry-run off a tty", initctx.Terminal{}, []string{"init", "--dry-run"}},
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

func TestInitPlanEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	seams := installInitSeams(t, false, "")
	p := synthProject(t)
	stdout, stderr, err := runLo(t, NewRoot(p), "init", "--plan")
	if err != nil || stderr != "" {
		t.Fatalf("init --plan: %v\n%s", err, stderr)
	}
	for _, want := range []string{
		"lo init — " + dir + "\n",
		"situation: empty directory\n",
		"git: not a repository\n",
		"Plan (empty directory):\n",
		"  1. project files: clusters/, lok8s.yaml, .gitignore entries, mise.toml\n",
		"  2. git init: a git repository in .\n",
		"  3. toolchain: .bin/b.yaml, b and the pinned toolchain into .bin/ (groups core,local; network)\n",
		"Equivalent commands:\n  lo init project shop --env mise\n  git init\n  lo toolchain install --groups core,local\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("--plan wrote %v", entries)
	}
	if len(seams.toolchain) != 0 || len(seams.git.calls) != 1 || seams.git.calls[0].Args[0] != "rev-parse" {
		t.Errorf("--plan ran something: toolchain=%v git=%v", seams.toolchain, seams.git.calls)
	}
	if strings.Contains(stdout, p.Base) {
		t.Error("the ambient project leaked into the plan")
	}
}

func TestInitPlanProjectRoot(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: acme\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: alpha\n")
	testutil.WriteFile(t, filepath.Join(root, "clusters", ".active"), "alpha.dev\n")
	t.Chdir(root)
	seams := installInitSeams(t, false, "")
	seams.git.repo = root
	stdout, _, err := runLo(t, NewRoot(synthProject(t)), "init", "--plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"situation: project root\n",
		"project: acme at " + root + "; you are at the root\n",
		"clusters: alpha.dev (lo); active alpha.dev\n",
		"--- domain ---\n",
		"active: alpha.dev (kind lo)\n",
		"Nothing to do.\n",
		"Available (lo init on a terminal asks; or run the command):\n",
		"lo init project --env none --cluster <domain> --driver <driver>",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "--- toolchain") {
		t.Error("the toolchain section printed without the b.yaml marker")
	}
}

// The welcome conversation end to end: every answer, the confirmation,
// then the files, git init through the fake and the toolchain through
// its seam.
func TestInitWizardEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	seams := installInitSeams(t, true, strings.Join([]string{
		".",        // directory
		"shop",     // name
		"1",        // env: mise
		"y",        // git init
		"demo.dev", // domain
		"1",        // driver: lo
		"y",        // toolchain
		"0",        // groups: defaults
		"y",        // confirm
	}, "\n")+"\n")
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("wizard: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		"situation: empty directory",
		"Equivalent commands:\n  lo init project shop --env mise --cluster demo.dev --driver lo\n  git init\n  lo toolchain install --groups core,local\n  lo use demo.dev\n",
		"==> lo init project shop --env mise --cluster demo.dev --driver lo\n",
		"Scaffolded " + filepath.Join(dir, "lok8s.yaml") + "\n",
		"Scaffolded " + filepath.Join(dir, "clusters", "demo.dev", "cluster.lok8s.yaml") + "\n",
		"==> git init\nInitialized empty Git repository (fake)\n",
		"==> lo toolchain install --groups core,local\ntoolchain (fake)\n",
		"==> lo use demo.dev\nActive domain: demo.dev\n",
		"Done.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	for _, f := range []string{"lok8s.yaml", "mise.toml", ".gitignore", "clusters/.gitkeep", "clusters/demo.dev/cluster.lok8s.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not written", f)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "lok8s.yaml"))
	if !strings.Contains(string(raw), "  name: shop\n") {
		t.Errorf("lok8s.yaml:\n%s", raw)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "clusters", ".active"))
	if string(raw) != "demo.dev\n" {
		t.Errorf(".active = %q", raw)
	}
	// git: the detection read, then init in the project dir.
	if len(seams.git.calls) != 2 || seams.git.calls[1].Args[0] != "init" || seams.git.calls[1].Dir != dir {
		t.Errorf("git calls: %+v", seams.git.calls)
	}
	if len(seams.toolchain) != 1 || seams.toolchain[0] != dir+" core,local dry=false" {
		t.Errorf("toolchain calls: %v", seams.toolchain)
	}
	// Nothing was written before the confirmation: the summary precedes
	// the first Scaffolded line.
	if strings.Index(stdout, "Equivalent commands:") > strings.Index(stdout, "Scaffolded ") {
		t.Error("files written before the summary")
	}
}

func TestInitWizardDryRunAndDecline(t *testing.T) {
	for _, c := range []struct {
		name   string
		args   []string
		script string
		want   string
	}{
		{"dry-run", []string{"init", "--dry-run"}, ".\nacme\n1\ny\n\n1\nn\n0\ny\n", "dry run — nothing was written\n"},
		{"declined", []string{"init"}, ".\nacme\n1\ny\n\n1\nn\n0\nn\n", "Nothing written.\n"},
	} {
		dir := t.TempDir()
		t.Chdir(dir)
		seams := installInitSeams(t, true, c.script)
		stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), c.args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, stderr)
		}
		if !strings.Contains(stdout, c.want) || !strings.Contains(stdout, "lo init project acme --env mise\n") {
			t.Errorf("%s: stdout:\n%s", c.name, stdout)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Errorf("%s: wrote %v", c.name, entries)
		}
		if len(seams.git.calls) != 1 || len(seams.toolchain) != 0 {
			t.Errorf("%s: ran git=%v toolchain=%v", c.name, seams.git.calls, seams.toolchain)
		}
		if c.name == "dry-run" && strings.Contains(seams.formOut.String(), "Write these files") {
			t.Error("dry-run asked for confirmation")
		}
	}
}

// Inside a service directory: the registration lands in the root's
// services.yaml with the path relative to the root, and the Tiltfile is
// ensured; the service file itself is untouched.
func TestInitWizardRegistersServiceDirectory(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: acme\n")
	os.MkdirAll(filepath.Join(root, "clusters"), 0o755)
	svc := filepath.Join(root, "services", "api")
	testutil.WriteFile(t, filepath.Join(svc, "lok8s.yaml"), "build:\n  context: .\n  dockerfile: Mine\n")
	t.Chdir(svc)
	seams := installInitSeams(t, true, "1\n0\ny\n")
	seams.git.repo = root
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "==> lo init service api --path ./services/api\n") || !strings.Contains(stdout, "Registered services.api.path = ./services/api in "+filepath.Join(root, "services.yaml")+"\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "services.yaml"))
	if !strings.Contains(string(raw), "  api:\n    path: ./services/api\n") {
		t.Errorf("services.yaml:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(root, "Tiltfile")); err != nil {
		t.Error("Tiltfile not ensured")
	}
	raw, _ = os.ReadFile(filepath.Join(svc, "lok8s.yaml"))
	if !strings.Contains(string(raw), "dockerfile: Mine") {
		t.Error("the service file was rewritten")
	}
	cwd, _ := os.Getwd()
	if cwd != svc {
		t.Errorf("cwd not restored: %s", cwd)
	}
}

// The project menu end to end: a cluster spec, the tests, the
// implementation switch (the tree ejected first) and an environment
// file, through the same functions the subcommands run.
func TestInitWizardProjectRootMenu(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: acme\n")
	os.MkdirAll(filepath.Join(root, "clusters"), 0o755)
	t.Chdir(root)
	// menu: cluster (1), tests (3), env (4), implementation (6); details:
	// domain, driver kubeone (2), use yes; env direnv (2); confirm.
	seams := installInitSeams(t, true, "1\n3\n4\n6\n0\nbeta.cloud\n2\ny\n2\ny\n")
	seams.git.repo = root
	stdout, stderr, err := runLo(t, NewRoot(synthProject(t)), "init")
	if err != nil {
		t.Fatalf("wizard: %v\n%s\n%s", err, stdout, stderr)
	}
	for _, want := range []string{
		"==> lo init project --env direnv\n",
		"==> lo init project --env none --cluster beta.cloud --driver kubeone\n",
		"==> lo assets eject bash\n",
		"==> lo init project --env none --implementation bash\n",
		"Set spec.implementation.default: bash in " + filepath.Join(root, "lok8s.yaml") + "\n",
		"==> lo use beta.cloud\nActive domain: beta.cloud\n",
		"==> lo init test\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	for _, f := range []string{".envrc", "clusters/beta.cloud/cluster.lok8s.yaml", ".lok8s/lo", "tests/playwright.config.ts"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s not written", f)
		}
	}
	impl, err := config.LoadImplementation(root)
	if err != nil || impl.Default != config.ImplBash {
		t.Errorf("implementation: %+v %v", impl, err)
	}
	if _, err := os.Stat(filepath.Join(root, "mise.toml")); err == nil {
		t.Error("mise.toml written beside the chosen .envrc")
	}
}

func TestInitAbort(t *testing.T) {
	var b bytes.Buffer
	if err := initAbort(initctx.ErrAborted, &b); !errors.Is(err, ErrHandled) || !strings.Contains(b.String(), "lo init: aborted, nothing written") {
		t.Errorf("abort: %v %q", err, b.String())
	}
	other := errors.New("boom")
	if err := initAbort(other, &b); !errors.Is(err, other) {
		t.Errorf("other error mapped: %v", err)
	}
}
