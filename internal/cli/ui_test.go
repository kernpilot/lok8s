package cli

// ui_test.go covers the presentation layer on the commands: piped output is the
// bash contract (no decoration change, no escapes), terminal output is the
// house style. Both modes render into buffers through the ui overrides.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// terminal renders f as if both streams were a colour terminal.
func terminal(t *testing.T, f func()) {
	t.Helper()
	restoreTTY := ui.ForceTTY(true)
	restoreColor := ui.ForceColor(true)
	defer restoreTTY()
	defer restoreColor()
	f()
}

// useProject is a synthetic project with two cluster domains and a deploy
// domain, alpha.dev active.
func useProject(t *testing.T) *config.Paths {
	t.Helper()
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "beta.cloud", "cluster.lok8s.yaml"), "kind: KubeOne\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "gamma.app", "deploy.lok8s.yaml"), "kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, ".active"), "alpha.dev\n")
	return p
}

// The doctor off a terminal: the bash title, the --- sections, plain
// markers, no escape anywhere (the one command that used to emit ANSI
// unconditionally). PATH is emptied so nothing on the machine is probed.
func TestDoctorPipedIsPlain(t *testing.T) {
	p := useProject(t)
	t.Setenv("PATH", t.TempDir())
	stdout, stderr, err := runLo(t, NewRoot(p), "doctor")
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v (every tool is missing)", err)
	}
	want := "=== lok8s doctor ===\n\n--- runtime ---\n  ✗ bash MISSING (required) — argsh needs >= 4.3 (macOS ships 3.2: brew install bash)\n\n--- tools ---\n  ✗ argsh MISSING (required) — argsh runtime (b install / argsh)\n"
	if !strings.HasPrefix(stdout, want) {
		t.Errorf("piped doctor:\n%s", stdout)
	}
	if !strings.Contains(stdout, "\n--- domain ---\n  ✓ active: alpha.dev (kind lo)\n") {
		t.Errorf("domain section:\n%s", stdout)
	}
	if strings.Contains(stdout, "\033") || strings.Contains(stderr, "\033") {
		t.Errorf("escape sequence off a terminal:\n%s%s", stdout, stderr)
	}
	if stderr != "[error] doctor: missing required prerequisites (see ✗ above)\n" {
		t.Errorf("stderr = %q", stderr)
	}
}

// The doctor on a terminal: no title (the command is the title), bold
// sections, coloured markers, the [error] prefix coloured too.
func TestDoctorTerminalIsStyled(t *testing.T) {
	p := useProject(t)
	t.Setenv("PATH", t.TempDir())
	terminal(t, func() {
		stdout, stderr, err := runLo(t, NewRoot(p), "doctor")
		if !errors.Is(err, ErrHandled) {
			t.Fatalf("err = %v", err)
		}
		want := "\033[1mruntime\033[0m\n  \033[31m✗\033[0m bash MISSING (required) — argsh needs >= 4.3 (macOS ships 3.2: brew install bash)\n\n\033[1mtools\033[0m\n"
		if !strings.HasPrefix(stdout, want) {
			t.Errorf("terminal doctor:\n%q", stdout)
		}
		if strings.Contains(stdout, "===") || strings.Contains(stdout, "--- ") {
			t.Errorf("decoration on a terminal:\n%s", stdout)
		}
		if !strings.Contains(stdout, "\n\033[1mdomain\033[0m\n  \033[32m✓\033[0m active: alpha.dev (kind lo)\n") {
			t.Errorf("domain section:\n%q", stdout)
		}
		if stderr != "\033[31m[error]\033[0m doctor: missing required prerequisites (see ✗ above)\n" {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

// The status report: the bash `=== Domain ===` / `--- Section ---` shape
// piped, bold headers on a terminal, the body identical.
func TestStatusBothModes(t *testing.T) {
	deps, _, out := statusHarness(t, false, nil)
	runStatus(t.Context(), out, deps, "x.dev")
	want := "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Targets ---\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"
	if out.String() != want {
		t.Errorf("piped status:\n%q\nwant\n%q", out.String(), want)
	}

	deps, _, out = statusHarness(t, false, nil)
	terminal(t, func() { runStatus(t.Context(), out, deps, "x.dev") })
	want = "\033[1mDomain: x.dev\033[0m\n\n\033[1mCluster\033[0m\nRunning\n\n\033[1mTargets\033[0m\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"
	if out.String() != want {
		t.Errorf("terminal status:\n%q\nwant\n%q", out.String(), want)
	}
}

// `lo toolchain doctor`: a title in both modes, the section, and the
// `next:` hint after the error when a pinned tool is missing.
func TestToolchainDoctorBothModes(t *testing.T) {
	p := synthProject(t)
	t.Setenv("PATH", t.TempDir())
	stdout, stderr, err := runLo(t, NewRoot(p), "toolchain", "doctor")
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(stdout, "=== toolchain doctor ===\n\n--- toolchain (lo ") {
		t.Errorf("piped:\n%s", stdout)
	}
	if !strings.HasSuffix(stderr, "\nnext: lo toolchain install   # installs the pins of this lo build\n") {
		t.Errorf("piped stderr = %q", stderr)
	}
	terminal(t, func() {
		stdout, stderr, _ := runLo(t, NewRoot(p), "toolchain", "doctor")
		if !strings.HasPrefix(stdout, "\033[1mtoolchain doctor\033[0m\n\n\033[1mtoolchain (lo ") {
			t.Errorf("terminal:\n%q", stdout)
		}
		if !strings.HasSuffix(stderr, "\n\033[2mnext: lo toolchain install   # installs the pins of this lo build\033[0m\n") {
			t.Errorf("terminal stderr = %q", stderr)
		}
	})
}

// Bare `lo use` off a terminal is the listing, byte for byte the bash
// use::_show (hack/parity-test.sh diffs it).
func TestUsePipedIsTheListing(t *testing.T) {
	p := useProject(t)
	prev := useInteractive
	useInteractive = func() bool { return false }
	t.Cleanup(func() { useInteractive = prev })
	stdout, stderr, err := runLo(t, NewRoot(p), "use")
	if err != nil || stderr != "" {
		t.Fatalf("err = %v, stderr = %q", err, stderr)
	}
	want := "Active: alpha.dev\n\nAvailable domains:\n  alpha.dev (lo)\n  beta.cloud (kubeone)\n  gamma.app (Deploy -> beta.cloud)\n"
	if stdout != want {
		t.Errorf("listing:\n%q\nwant\n%q", stdout, want)
	}
}

// useSelectSeams routes a bare `lo use` into the select, run in huh's
// accessible mode over a scripted reader (one number, Enter).
func useSelectSeams(t *testing.T, input string) *bytes.Buffer {
	t.Helper()
	prevI, prevIO := useInteractive, useFormIO
	prompt := &bytes.Buffer{}
	useInteractive = func() bool { return true }
	useFormIO = func() useIO { return useIO{In: strings.NewReader(input), Out: prompt, Accessible: true} }
	t.Cleanup(func() { useInteractive, useFormIO = prevI, prevIO })
	return prompt
}

// The select lists every domain with what it is, preselects the active
// one, and Enter on a choice sets it through the same path as
// `lo use <domain>`.
func TestUseSelectSetsActive(t *testing.T) {
	p := useProject(t)
	prompt := useSelectSeams(t, "2\n")
	stdout, stderr, err := runLo(t, NewRoot(p), "use")
	if err != nil || stderr != "" {
		t.Fatalf("err = %v, stderr = %q\nprompt:\n%s", err, stderr, prompt)
	}
	if stdout != "Active domain: beta.cloud\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if raw, _ := os.ReadFile(filepath.Join(p.Clusters, ".active")); string(raw) != "beta.cloud\n" {
		t.Errorf(".active = %q", raw)
	}
	for _, line := range []string{"1. alpha.dev   lo", "2. beta.cloud  kubeone", "3. gamma.app   Deploy -> beta.cloud"} {
		if !strings.Contains(prompt.String(), line) {
			t.Errorf("option %q missing from the select:\n%s", line, prompt)
		}
	}
	// The default (an empty answer) is the active domain: preselected.
	prompt = useSelectSeams(t, "\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, ".active"), "gamma.app\n")
	stdout, _, err = runLo(t, NewRoot(p), "use")
	if err != nil || stdout != "Active domain: gamma.app\n" {
		t.Errorf("preselection: err = %v, stdout = %q\nprompt:\n%s", err, stdout, prompt)
	}
}

// With no cluster at all the select has nothing to offer: the hint on
// stderr (an error state), rc 1, nothing on stdout.
func TestUseSelectNoClusters(t *testing.T) {
	p := synthProject(t)
	useSelectSeams(t, "1\n")
	stdout, stderr, err := runLo(t, NewRoot(p), "use")
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v", err)
	}
	if stderr != "no clusters yet\nnext: lo init   # create a project or add a cluster\n" || stdout != "" {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

// The wrapper tells Esc from Ctrl-C: both abort the huh form, Ctrl-C is
// recorded before the form sees it (rc 130 in useSelect), Esc is not
// (rc 0).
func TestUseFormRecordsCtrlC(t *testing.T) {
	domains := []useDomain{{"alpha.dev", "lo"}, {"beta.cloud", "kubeone"}}
	for _, c := range []struct {
		key   tea.KeyPressMsg
		ctrlC bool
	}{
		{tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, true},
		{tea.KeyPressMsg{Code: tea.KeyEscape}, false},
	} {
		choice := "alpha.dev"
		m := &useForm{form: useBuildForm(domains, &choice)}
		m.Init()
		m.Update(c.key)
		if m.ctrlC != c.ctrlC {
			t.Errorf("%s: ctrlC = %v, want %v", c.key, m.ctrlC, c.ctrlC)
		}
		if m.form.State != huh.StateAborted {
			t.Errorf("%s: form state = %v, want aborted", c.key, m.form.State)
		}
	}
}

// `lo use <unknown>`: the [error] line alone off a terminal. On a terminal
// the closest name and the available domains follow.
func TestUseUnknownDomainSuggestsOnTerminal(t *testing.T) {
	p := useProject(t)
	errLine := "[error] domain not found: clusters/alpha.de/ (no cluster.lok8s.yaml or deploy.lok8s.yaml)\n"
	stdout, stderr, err := runLo(t, NewRoot(p), "use", "alpha.de")
	if !errors.Is(err, ErrHandled) || stdout != "" || stderr != errLine {
		t.Errorf("piped: err = %v, stdout = %q, stderr = %q", err, stdout, stderr)
	}
	restoreTTY, restoreColor := ui.ForceTTY(true), ui.ForceColor(false)
	defer restoreTTY()
	defer restoreColor()
	stdout, stderr, err = runLo(t, NewRoot(p), "use", "alpha.de")
	want := errLine + "Did you mean alpha.dev?\nAvailable domains: alpha.dev, beta.cloud, gamma.app\n"
	if !errors.Is(err, ErrHandled) || stdout != "" || stderr != want {
		t.Errorf("terminal: err = %v, stdout = %q\nstderr = %q\nwant   = %q", err, stdout, stderr, want)
	}
	_, stderr, _ = runLo(t, NewRoot(p), "use", "nothing.like.it")
	if stderr != "[error] domain not found: clusters/nothing.like.it/ (no cluster.lok8s.yaml or deploy.lok8s.yaml)\nAvailable domains: alpha.dev, beta.cloud, gamma.app\n" {
		t.Errorf("no close match: stderr = %q", stderr)
	}
	if raw, _ := os.ReadFile(filepath.Join(p.Clusters, ".active")); string(raw) != "alpha.dev\n" {
		t.Errorf(".active changed: %q", raw)
	}
}

// An unknown command is the root's parse error: cobra's message, the
// "Did you mean" block (edit distance, prefix, SuggestFor), the hint.
func TestRootUnknownCommand(t *testing.T) {
	p := synthProject(t)
	cases := []struct{ arg, want string }{
		{"boostrap", "Error: unknown command \"boostrap\" for \"lo\"\n\nDid you mean this?\n\tbootstrap\n\n  Run \"lo -h\" for more information.\n"},
		{"start", "Error: unknown command \"start\" for \"lo\"\n\nDid you mean this?\n\tup\n\n  Run \"lo -h\" for more information.\n"},
		{"bogus", "Error: unknown command \"bogus\" for \"lo\"\n\n  Run \"lo -h\" for more information.\n"},
	}
	for _, c := range cases {
		stdout, stderr, err := runLo(t, NewRoot(p), c.arg)
		if !errors.Is(err, ErrHandled) || stdout != "" || stderr != c.want {
			t.Errorf("lo %s: err = %v, stdout = %q\nstderr = %q\nwant   = %q", c.arg, err, stdout, stderr, c.want)
		}
	}
	// A bare `lo` is the help (an explicit empty argv: nil would be the
	// test binary's own os.Args).
	root := NewRoot(p)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil || !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "Cluster lifecycle:") {
		t.Errorf("bare lo: err = %v\n%s", err, out.String())
	}
}

// Every command's flag errors print in the one argsh shape, ported and
// Go-only alike (the handler sits on the root).
func TestFlagErrorsOneShape(t *testing.T) {
	p := synthProject(t)
	want := "Error: unknown flag: --bogus\n\n  Run \"lo -h\" for more information.\n"
	for _, argv := range [][]string{{"status", "--bogus"}, {"use", "--bogus"}, {"toolchain", "doctor", "--bogus"}, {"assets", "list", "--bogus"}, {"--bogus"}} {
		_, stderr, err := runLo(t, NewRoot(p), argv...)
		if !errors.Is(err, ErrHandled) || stderr != want {
			t.Errorf("lo %s: err = %v, stderr = %q", strings.Join(argv, " "), err, stderr)
		}
	}
}

// --no-color exports NO_COLOR for the children and turns the colour off on
// a terminal. The shape stays the terminal's.
func TestNoColorFlag(t *testing.T) {
	p := useProject(t)
	t.Setenv("NO_COLOR", "")
	os.Unsetenv("NO_COLOR")
	t.Cleanup(func() { ui.SetNoColor(false) })
	restore := ui.ForceTTY(true)
	defer restore()
	_, stderr, _ := runLo(t, NewRoot(p), "--no-color", "use", "alpha.de")
	if os.Getenv("NO_COLOR") != "1" {
		t.Error("NO_COLOR not exported")
	}
	if !strings.HasPrefix(stderr, "[error] domain not found") || strings.Contains(stderr, "\033") {
		t.Errorf("stderr = %q", stderr)
	}
}

// The addons table piped is the bash printf layout. On a terminal the
// header is bold and a long name widens its column instead of pushing
// its row.
func TestAddonsTableBothModes(t *testing.T) {
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, ".active"), "alpha.dev\n")
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "addons", "kube-prometheus-stack", "kustomization.yaml"), "resources: []\n")
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "addons", "kube-prometheus-stack", "chart.yaml"), "chart: kube-prometheus-stack\nrepository: https://prometheus-community.github.io/helm-charts\nversion: 65.1.1\n")
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "addons", "x", "kustomization.yaml"), "resources: []\n")
	stdout, stderr, err := runLo(t, NewRoot(p), "addons")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, stderr)
	}
	// The bash printf '%-20s  %-8s  %-12s  %s' layout: a 21-char name pushes
	// its own row by one column, the header stays at 20.
	for _, line := range []string{
		"NAME                  TYPE      VERSION       CHART/REPO\n",
		"----                  ----      -------       ----------\n",
		"kube-prometheus-stack  raw       65.1.1        kube-prometheus-stack (https://prometheus-community.github.io/helm-charts)\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("piped addons: %q missing from\n%s", line, stdout)
		}
	}
	terminal(t, func() {
		stdout, _, _ := runLo(t, NewRoot(p), "addons")
		lines := strings.Split(stdout, "\n")
		if !strings.HasPrefix(lines[0], "\033[1mNAME ") || !strings.HasSuffix(lines[0], "CHART/REPO\033[0m") || !strings.HasPrefix(lines[1], "\033[2m---- ") {
			t.Errorf("terminal header:\n%q", stdout)
		}
		// Every TYPE cell sits under the header's TYPE: the column widened
		// for the long name instead of that row overflowing.
		col := strings.Index(strings.TrimPrefix(lines[0], "\033[1m"), "TYPE")
		for _, l := range lines[2:] {
			if strings.HasPrefix(l, "kube-prometheus-stack") && strings.Index(l, "raw") != col {
				t.Errorf("terminal row not aligned to column %d:\n%q", col, l)
			}
		}
	})
}

// A writer that is neither the process stream nor a Styled wrapper stays
// plain even when the process runs on a terminal: the stream is what
// decides, not the process.
func TestPipedStreamStaysPlainOnATerminalProcess(t *testing.T) {
	var b bytes.Buffer
	ui.Section(io.Writer(&b), "x")
	if b.String() != "--- x ---\n" {
		t.Errorf("got %q", b.String())
	}
}
