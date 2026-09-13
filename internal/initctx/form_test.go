package initctx

// form_test.go — the forms driven headlessly through huh's accessible
// mode: every prompt reads one line from a scripted reader (an empty
// line or EOF takes the default), so the answers each situation yields
// are asserted without a terminal.

import (
	"bytes"
	"path/filepath"
	"reflect"
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

func TestAskNewProjectFullAnswers(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "acme")
	s := State{Cwd: cwd, Empty: true, Git: Git{Available: true}}
	tio := script(
		"platform", // project directory
		"Bad Name", // project name: refused
		"shop",     // project name
		"2",        // environment file: direnv
		"n",        // git init: no
		"../x",     // domain: refused
		"shop.dev", // domain
		"2",        // driver: kubeone
		"y",        // toolchain
		"2",        // groups: toggle cloud on (local stays on)
		"0",        // groups: done
	)
	a, err := Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	want := Answers{Dir: "platform", Name: "shop", Env: "direnv", GitInit: false, Domain: "shop.dev", Driver: "kubeone",
		Use: true, Toolchain: true, Groups: []string{"core", "local", "cloud"}}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("answers %+v\nwant    %+v", a, want)
	}
	out := tio.Out.(*bytes.Buffer).String()
	for _, s := range []string{"Project directory", "must match ^[a-z0-9][a-z0-9._-]*$", "invalid domain name: ../x", "First cluster domain", "Toolchain groups"} {
		if !strings.Contains(out, s) {
			t.Errorf("prompt output lacks %q:\n%s", s, out)
		}
	}

	// The plan the answers make: the twins name every choice.
	p := Decide(s, a)
	wantCommands(t, p,
		"lo init project shop --env direnv --path platform --cluster shop.dev --driver kubeone",
		"lo toolchain install --groups core,local,cloud",
		"lo use shop.dev")
}

func TestAskNewProjectDefaults(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "acme")
	s := State{Cwd: cwd, Empty: true, Git: Git{Available: true}}
	// Every prompt at its default: EOF on the first read.
	tio := scripted("")
	a, err := Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	want := Answers{Dir: cwd, Name: "acme", Env: "mise", GitInit: true, Driver: "lo", Toolchain: true, Groups: []string{"core", "local"}}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("answers %+v\nwant    %+v", a, want)
	}
	p := Decide(s, a)
	wantCommands(t, p, "lo init project acme --env mise", "git init", "lo toolchain install --groups core,local")

	// Inside a repository the git question is not asked: the same script
	// lands one prompt earlier.
	s.Git.Root, s.Git.AtRoot = cwd, true
	tio = script(".", "acme", "1", "", "1", "n", "0")
	a, err = Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	if a.GitInit != true || a.Toolchain || a.Domain != "" || a.Use {
		t.Errorf("answers %+v", a)
	}
	if strings.Contains(tio.Out.(*bytes.Buffer).String(), "git init") {
		t.Error("git init asked inside a repository")
	}
}

func TestAskGitBelowRootSuggestsTheRoot(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "svc")
	s := State{Cwd: cwd, Entries: 1, Git: Git{Available: true, Root: root}}
	tio := scripted("")
	a, err := Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	if a.Dir != root || a.Name != filepath.Base(root) {
		t.Errorf("answers %+v", a)
	}
}

func TestAskExistingProjectMenu(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, true)

	// Nothing chosen: no second form, empty answers.
	tio := script("0")
	a, err := Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, Answers{}) {
		t.Errorf("answers %+v", a)
	}
	if strings.Contains(tio.Out.(*bytes.Buffer).String(), "Cluster domain") {
		t.Error("details asked with nothing chosen")
	}

	// cluster (1), tests (3), toolchain (5), implementation (6); then the
	// cluster details and the toolchain groups.
	tio = script("1", "3", "5", "6", "0", "beta.cloud", "4", "n", "3", "0")
	a, err = Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	want := Answers{Domain: "beta.cloud", Driver: "kkp", Use: false, Tests: true, Toolchain: true, Groups: []string{"core", "local", "bash"}, Implementation: "bash"}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("answers %+v\nwant    %+v", a, want)
	}
	wantCommands(t, Decide(s, a),
		"lo init project --env none --cluster beta.cloud --driver kkp",
		"lo assets eject bash",
		"lo init project --env none --implementation bash",
		"lo toolchain install --groups core,local,bash",
		"lo init test")

	// service (2) and env (4): the name is validated, the env file chosen.
	tio = script("2", "4", "0", "API", "api", "2")
	a, err = Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	if a.Service != "api" || a.Env != "direnv" {
		t.Errorf("answers %+v", a)
	}
	wantCommands(t, Decide(s, a), "lo init project --env direnv", "lo init service api")

	// The use question defaults to yes without an active domain and to no
	// with one.
	tio = script("1", "0", "x.dev", "", "")
	a, _ = Ask(s, tio)
	if !a.Use {
		t.Error("use defaulted to no without an active domain")
	}
	s.Project.Active = "alpha.dev"
	tio = script("1", "0", "x.dev", "", "")
	a, _ = Ask(s, tio)
	if a.Use {
		t.Error("use defaulted to yes with an active domain")
	}
}

func TestAskInsideServiceDir(t *testing.T) {
	root := t.TempDir()
	s := projectState(root, false)
	s.Cwd = filepath.Join(root, "api")
	s.ServiceDir = true
	// register (1) is first in the menu.
	tio := script("1", "0")
	a, err := Ask(s, tio)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Register {
		t.Errorf("answers %+v", a)
	}
	wantCommands(t, Decide(s, a), "lo init service api --path ./api")
}

func TestConfirm(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{{"y\n", true}, {"n\n", false}, {"\n", true}, {"", true}} {
		tio := scripted(c.in)
		got, err := Confirm(tio, true)
		if err != nil || got != c.want {
			t.Errorf("confirm %q: %v %v", c.in, got, err)
		}
		if !strings.Contains(tio.Out.(*bytes.Buffer).String(), "uses the network") {
			t.Error("the network note missing")
		}
	}
	tio := script("n")
	if got, _ := Confirm(tio, false); got || strings.Contains(tio.Out.(*bytes.Buffer).String(), "network") {
		t.Error("confirm without network")
	}
}

func TestWithCore(t *testing.T) {
	if got := withCore([]string{"bash", "local"}); !reflect.DeepEqual(got, []string{"core", "local", "bash"}) {
		t.Errorf("withCore: %v", got)
	}
	if got := withCore(nil); !reflect.DeepEqual(got, []string{"core"}) {
		t.Errorf("withCore(nil): %v", got)
	}
}
