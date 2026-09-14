package initctx

// screen_test.go — the screens driven headlessly through huh's
// accessible mode: every prompt reads one line from a scripted reader
// (an empty line or EOF takes the default), so the rows, the details
// and the plan each screen yields are asserted without a terminal.

import (
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

// script is the scripted stdin of one run. huh's accessible prompts each
// open a bufio.Scanner over the reader, and a Scanner reads ahead; a
// one-byte reader keeps every prompt to its own line.
func script(lines ...string) IO {
	return scripted(strings.Join(lines, "\n") + "\n")
}

func scripted(in string) IO {
	return IO{In: iotest.OneByteReader(strings.NewReader(in)), Out: &bytes.Buffer{}, Accessible: true}
}

func formOut(tio IO) string { return tio.Out.(*bytes.Buffer).String() }

func emptyDir(t *testing.T) State {
	t.Helper()
	return State{Cwd: filepath.Join(t.TempDir(), "shop"), Empty: true, Git: Git{Available: true}}
}

// The new-project screen with the defaults, then Create: the golden rows
// off a terminal, and the plan of the defaults.
func TestNewProjectCreateDefaults(t *testing.T) {
	s := emptyDir(t)
	var out bytes.Buffer
	tio := script("1")
	p, err := NewProject(s, &out, tio)
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init project shop --env mise --cluster shop.dev --driver lo", "git init", "lo toolchain install --groups core,local", "lo use shop.dev")
	want := "  New project\n" +
		"  name         shop\n" +
		"  domain       shop.dev\n" +
		"  driver       lo · kind on local Docker (dev clusters)\n" +
		"  environment  mise.toml\n" +
		"  toolchain    install now · 8 tools · network\n" +
		"  git          initialise a repository\n" +
		"  writes       clusters/ · lok8s.yaml · .gitignore entries · mise.toml · clusters/shop.dev/cluster.lok8s.yaml · .bin/b.yaml · .bin/ (the pinned tools) · clusters/.active\n" +
		"  runs         git init · lo toolchain install (network) · lo use shop.dev\n" +
		"  equivalent   lo init project shop --env mise --cluster shop.dev --driver lo · git init · lo toolchain install --groups core,local · lo use shop.dev\n\n"
	if out.String() != want {
		t.Errorf("screen:\n%s\nwant:\n%s", out.String(), want)
	}
	// The closing choice: Create, Change details, Cancel.
	contains(t, formOut(tio), "1. Create\n2. Change details\n3. Cancel\n")
	// The name is bold, the keys dim and the equivalent row dim on a
	// terminal; the values plain.
	s.Terminal.StdoutTTY = true
	out.Reset()
	if _, err := NewProject(s, &out, script("1")); err != nil {
		t.Fatal(err)
	}
	contains(t, out.String(), "  \033[1mNew project\033[0m\n", "  \033[2mname\033[0m         shop\n", "  \033[2mequivalent\033[0m   \033[2mlo init project shop")
}

// Change details: every field at its prompt, then the screen again with
// the changed values, then Create.
func TestNewProjectChangeDetails(t *testing.T) {
	s := emptyDir(t)
	var out bytes.Buffer
	tio := script(
		"2",          // change details
		"Bad Name",   // name: refused
		"platform",   // name
		"../x",       // domain: refused
		"plat.cloud", // domain
		"2",          // driver: kubeone
		"2",          // environment: .envrc
		"2",          // toolchain: skip
		"2",          // git: skip
		"1",          // create
	)
	p, err := NewProject(s, &out, tio)
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init project platform --env direnv --cluster plat.cloud --driver kubeone", "lo use plat.cloud")
	contains(t, out.String(),
		"  name         platform\n",
		"  domain       plat.cloud\n",
		"  driver       kubeone · KubeOne on VMs or bare metal (self-managed production)\n",
		"  environment  .envrc\n",
		"  toolchain    skip\n",
		"  git          skip\n")
	contains(t, formOut(tio), "must match ^[a-z0-9][a-z0-9._-]*$", "invalid domain name: ../x", "Domain", "Environment file")
	// The screen was printed twice: the defaults, then the changed values.
	if n := strings.Count(out.String(), "  New project\n"); n != 2 {
		t.Errorf("screen printed %d times, want 2", n)
	}

	// Every detail at its default: the same plan as Create.
	out.Reset()
	p, err = NewProject(s, &out, script("2", "", "", "", "", "", "", "1"))
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init project shop --env mise --cluster shop.dev --driver lo", "git init", "lo toolchain install --groups core,local", "lo use shop.dev")

	// An empty domain (cleared in the interactive input): no cluster, no
	// lo use, the row says none.
	a := DefaultAnswers(s)
	a.Domain = ""
	sc := NewProjectScreen(s, &a)
	wantCommands(t, sc.Plan, "lo init project shop --env mise", "git init", "lo toolchain install --groups core,local")
	out.Reset()
	WriteScreen(&out, sc, false)
	contains(t, out.String(), "  domain       none\n")
	if strings.Contains(out.String(), "runs         git init · lo use") {
		t.Errorf("lo use without a domain:\n%s", out.String())
	}
}

// A required value left empty (EOF at the prompt) stops the screen.
func TestScreenIncomplete(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if _, err := Run(&out, scripted(""), false, func() Screen { return ClusterScreen(root, &ClusterInput{}) }); !errors.Is(err, ErrIncomplete) {
		t.Errorf("incomplete: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("screen printed without a domain:\n%s", out.String())
	}
}

// Cancel returns ErrCancelled; EOF takes the default (Create).
func TestNewProjectCancel(t *testing.T) {
	s := emptyDir(t)
	var out bytes.Buffer
	if _, err := NewProject(s, &out, script("3")); !errors.Is(err, ErrCancelled) {
		t.Errorf("cancel: %v", err)
	}
	if p, err := NewProject(s, &out, scripted("")); err != nil || len(p.Actions) == 0 {
		t.Errorf("EOF: %v %+v", err, p)
	}
}

// Below a git root the directory is a row and a detail; inside a
// repository the git row is absent.
func TestNewProjectBelowGitRoot(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "svc")
	s := State{Cwd: cwd, Entries: 1, Git: Git{Available: true, Root: root, Branch: "main"}}
	var out bytes.Buffer
	p, err := NewProject(s, &out, script("1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != root {
		t.Errorf("dir %s, want the root", p.Dir)
	}
	contains(t, out.String(), "  directory    ..\n")
	if strings.Contains(out.String(), "  git ") {
		t.Errorf("git row inside a repository:\n%s", out.String())
	}
	// The directory detail: "." puts the project here.
	out.Reset()
	p, err = NewProject(s, &out, script("2", "", ".", "", "", "", "", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != cwd {
		t.Errorf("dir %s, want cwd", p.Dir)
	}
	if strings.Contains(out.String()[strings.LastIndex(out.String(), "New project"):], "directory") {
		t.Errorf("directory row at cwd:\n%s", out.String())
	}
}

var flagLike = regexp.MustCompile(`--[a-z]|\blo [a-z]+`)

// No field carries a flag or a command: the help says what the value is
// and gives an example; the flag twins live on the equivalent row.
func TestPromptsCarryNoFlag(t *testing.T) {
	for key, p := range prompts {
		for _, s := range []string{p.title, p.help, p.placeholder} {
			if flagLike.MatchString(s) {
				t.Errorf("prompt %s carries a flag or a command: %q", key, s)
			}
		}
		if p.title == "" {
			t.Errorf("prompt %s has no title", key)
		}
	}
	// And the forms are built from them: every detail form of every
	// screen prints nothing flag-like (the screens' rows go elsewhere).
	root := t.TempDir()
	s := projectState(root, true)
	s.Project.Domains = []Domain{{"a.dev", "lo"}, {"b.dev", "kubeone"}}
	var out bytes.Buffer
	var groups []string
	dom := ""
	empty := emptyDir(t)
	a := DefaultAnswers(empty)
	cin, sin, tin := &ClusterInput{Driver: "lo"}, &ServiceInput{}, &TestsInput{}
	for _, c := range []struct {
		name  string
		build func() Screen
		tio   IO
	}{
		{"project", func() Screen { return NewProjectScreen(empty, &a) }, script("2", "", "", "", "", "", "", "3")},
		{"cluster", func() Screen { return ClusterScreen(root, cin) }, script("x.dev", "", "", "3")},
		{"service", func() Screen { return ServiceScreen(root, sin) }, script("api", "", "3")},
		{"tests", func() Screen { return TestsScreen(root, tin) }, script("2", "", "3")},
		{"toolchain", func() Screen { return ToolchainScreen(root, &groups) }, script("2", "0", "3")},
		{"active", func() Screen { return ActiveScreen(s, &dom) }, script("2", "3")},
	} {
		if _, err := Run(&out, c.tio, false, c.build); !errors.Is(err, ErrCancelled) {
			t.Errorf("%s: %v", c.name, err)
		}
		if got := formOut(c.tio); flagLike.MatchString(strings.ReplaceAll(got, "lo init: ", "")) {
			t.Errorf("%s form carries a flag or a command:\n%s", c.name, got)
		}
	}
}

// The cluster screen: without a domain the details open first; with
// every value given there are no details and the choice is Create or
// Cancel; the active row names the domain.
func TestClusterScreen(t *testing.T) {
	root := t.TempDir()
	in := &ClusterInput{Driver: "lo", Active: true}
	var out bytes.Buffer
	tio := script("staging.dev", "", "", "1")
	p, err := Run(&out, tio, false, func() Screen { return ClusterScreen(root, in) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init cluster staging.dev --driver lo", "lo use staging.dev")
	contains(t, out.String(),
		"  New cluster\n",
		"  domain      staging.dev\n",
		"  driver      lo · kind on local Docker (dev clusters)\n",
		"  active      yes (lo use staging.dev)\n",
		"  writes      clusters/staging.dev/cluster.lok8s.yaml · clusters/.active\n",
		"  runs        lo use staging.dev\n",
		"  equivalent  lo init cluster staging.dev --driver lo · lo use staging.dev\n")
	// The details opened before the first render: the domain prompt
	// precedes the closing choice.
	if f := formOut(tio); strings.Index(f, "Domain") > strings.Index(f, "1. Create") {
		t.Errorf("details after the screen:\n%s", f)
	}

	in = &ClusterInput{Domain: "x.dev", Driver: "kkp", Active: false, DomainGiven: true, DriverGiven: true, ActiveGiven: true}
	out.Reset()
	tio = script("1")
	p, err = Run(&out, tio, false, func() Screen { return ClusterScreen(root, in) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init cluster x.dev --driver kkp --no-active")
	contains(t, out.String(), "  active      no\n")
	if strings.Contains(formOut(tio), "Change details") {
		t.Error("details offered with every value given")
	}
	contains(t, formOut(tio), "1. Create\n2. Cancel\n")
}

func TestServiceAndTestsScreens(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	in := &ServiceInput{}
	p, err := Run(&out, script("api", "./services/api", "1"), false, func() Screen { return ServiceScreen(root, in) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init service api --path ./services/api")
	contains(t, out.String(), "  New service\n", "  name        api\n", "  path        ./services/api\n", "  writes      services/api/lok8s.yaml · services.yaml entry · Tiltfile\n")

	out.Reset()
	in = &ServiceInput{Name: "web", NameGiven: true}
	p, err = Run(&out, script("", "1"), false, func() Screen { return ServiceScreen(root, in) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init service web")
	contains(t, out.String(), "  path        ./web\n")

	out.Reset()
	tin := &TestsInput{}
	p, err = Run(&out, script("1"), false, func() Screen { return TestsScreen(root, tin) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init test")
	contains(t, out.String(), "  Test suite\n", "  path        tests\n", "  writes      tests/ (the Playwright suite)\n", "  equivalent  lo init test\n")
	out.Reset()
	p, err = Run(&out, script("2", "e2e", "1"), false, func() Screen { return TestsScreen(root, tin) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init test --path e2e")
}

func TestToolchainActiveEjectImplementationScreens(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)
	s.Project.Domains = []Domain{{"a.dev", "lo"}, {"b.dev", "kubeone"}}
	s.Project.Active = "a.dev"
	var out bytes.Buffer

	var groups []string
	p, err := Run(&out, script("2", "2", "0", "1"), false, func() Screen { return ToolchainScreen(root, &groups) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo toolchain install --groups core,local,cloud")
	contains(t, out.String(), "  Toolchain\n", "  groups      core · local\n", "  tools       8 tools\n", "  groups      core · local · cloud\n", "  tools       10 tools\n")

	out.Reset()
	dom := ""
	p, err = Run(&out, script("2", "1"), false, func() Screen { return ActiveScreen(s, &dom) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo use b.dev")
	contains(t, out.String(), "  Active domain\n", "  domain      b.dev\n")

	out.Reset()
	p, err = Run(&out, script("1"), false, func() Screen { return EjectScreen(s) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo assets eject bash")
	contains(t, out.String(), "  Bash tree\n", "  writes      .lok8s/ (the bash implementation, plus every data asset the project lacks)\n")

	out.Reset()
	p, err = Run(&out, script("1"), false, func() Screen { return ImplementationScreen(s) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init project --env none --implementation bash")
	contains(t, out.String(), "  Implementation\n", "  implementation  bash (now go)\n")
	s.Project.Implementation = "bash"
	out.Reset()
	if _, err := Run(&out, script("1"), false, func() Screen { return ImplementationScreen(s) }); err != nil {
		t.Fatal(err)
	}
	contains(t, out.String(), "  implementation  go (now bash)\n")
}

// Enter on an empty required field re-asks the same field with the
// error; the screen never ends on it. The accessible prompt repeats
// until a value comes (on a terminal huh keeps the field focused).
func TestRequiredFieldReAsks(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	tio := script("", "", "x.dev", "", "", "1")
	in := &ClusterInput{Driver: "lo", Active: true}
	p, err := Run(&out, tio, false, func() Screen { return ClusterScreen(root, in) })
	if err != nil {
		t.Fatal(err)
	}
	wantCommands(t, p, "lo init cluster x.dev --driver lo", "lo use x.dev")
	if n := strings.Count(formOut(tio), "give a domain"); n != 2 {
		t.Errorf("the empty answer re-asked %d times, want 2:\n%s", n, formOut(tio))
	}
	// A prefilled required field keeps its value on an empty answer.
	empty := emptyDir(t)
	out.Reset()
	p, err = NewProject(empty, &out, script("2", "", "", "", "", "", "", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "shop" {
		t.Errorf("name %q, want the prefilled shop", p.Name)
	}
}
