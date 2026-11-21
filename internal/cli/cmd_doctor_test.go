package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// TestDoctorReportsPathSecretsAsItIs: the binary defaults PATH_SECRETS to
// nothing (every store is per domain), so doctor says so when the variable
// is unset and prints the directory when it is set (D35).
func TestDoctorReportsPathSecretsAsItIs(t *testing.T) {
	p := synthProject(t)
	t.Setenv("PATH", t.TempDir())
	p.SecretsEnv = ""
	stdout, _, _ := runLo(t, NewRoot(p), "doctor")
	if !strings.Contains(stdout, "ℹ PATH_SECRETS unset (per-domain stores; the flat store is retired)") {
		t.Errorf("unset: no info line:\n%s", stdout)
	}
	if strings.Contains(stdout, ".secrets") {
		t.Errorf("unset: doctor still names a flat store:\n%s", stdout)
	}

	p.SecretsEnv = p.Clusters
	stdout, _, _ = runLo(t, NewRoot(p), "doctor")
	if !strings.Contains(stdout, " PATH_SECRETS="+p.Clusters+"\n") {
		t.Errorf("set: no directory line:\n%s", stdout)
	}
}

// The git worktree case (kubehz.in.net recreate, 2026-10-23): argsh is on
// PATH (the main checkout's .bin), but the project's own .bin/argsh, the one
// every bash child sources, is missing. `lo recover --dry-run` failed in the
// provider plugin. Doctor names the file and the fix, and fails when the
// active domain's provider plugin needs it. The provider section does not
// start a bash child that cannot source argsh.
func TestDoctorNamesTheMissingProjectArgsh(t *testing.T) {
	p := synthProject(t)
	elsewhere := t.TempDir()
	testutil.WriteFile(t, filepath.Join(elsewhere, "argsh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(elsewhere, "argsh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", elsewhere)
	testutil.WriteFile(t, filepath.Join(p.Bin, "b.yaml"), "binaries:\n  argsh: {}\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "metal.dev", "cluster.lok8s.yaml"),
		"kind: KubeOne\nmetadata:\n  name: metal\nspec:\n  provider:\n    name: hetzner\n")

	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", "metal.dev")
	if !strings.Contains(stdout, "✓ argsh — argsh runtime") {
		t.Fatalf("the fixture has no argsh on PATH:\n%s", stdout)
	}
	want := "✗ bash mode (spec.implementation, provider plugins): not runnable: argsh missing at .bin/argsh; metal.dev uses provider hetzner, a bash plugin (fix: b install)\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("no failure line for the missing .bin/argsh, want %q in:\n%s", want, stdout)
	}
	if !strings.Contains(stdout, "! provider section skipped: argsh missing at .bin/argsh (fix: b install)\n") {
		t.Errorf("the provider section did not name the missing argsh:\n%s", stdout)
	}

	var out strings.Builder
	if doctorBashMode(&out, p, "metal.dev") {
		t.Error("a provider-backed domain without .bin/argsh passes the bash-mode check")
	}
	out.Reset()
	if !doctorBashMode(&out, p, "") || !strings.Contains(out.String(), "! bash mode (spec.implementation, provider plugins): not runnable: argsh missing at .bin/argsh (fix: b install)\n") {
		t.Errorf("without a provider the missing argsh is a warning, got:\n%s", out.String())
	}

	testutil.WriteFile(t, filepath.Join(p.Bin, "argsh"), "#!/usr/bin/env bash\n")
	out.Reset()
	if !doctorBashMode(&out, p, "metal.dev") || strings.Contains(out.String(), "not runnable") {
		t.Errorf("argsh present, got:\n%s", out.String())
	}
}

// doctorStubBin is a PATH directory that holds the named tools as
// executables. LookPath finds them; the fake runner answers for them, so
// they never run.
func doctorStubBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		testutil.WriteFile(t, p, "#!/bin/sh\nexit 99\n")
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// kindVersionRunner answers `kind version` with out and err. Every other
// command prints nothing and succeeds.
func kindVersionRunner(out string, err error) *scriptRunner {
	return &scriptRunner{handler: func(c execx.Cmd) error {
		if filepath.Base(c.Name) == "kind" && len(c.Args) == 1 && c.Args[0] == "version" {
			io.WriteString(c.Stdout, out)
			return err
		}
		return nil
	}}
}

// The kind config that lo up renders sets networking.dnsSearch, which
// needs kind v0.18.0. doctorKind reads `kind version`: an older release
// fails, the minimum and later releases pass (a pre-release of a later
// release too), and an output it cannot read is a warning. The bash twin
// (doctor::_kind, tests/unit/doctor_test.bats) prints the same lines.
func TestDoctorKindMinimumRelease(t *testing.T) {
	const purpose = "local clusters (Lo driver)"
	const need = "lo up needs kind v0.18.0 or later for networking.dnsSearch"
	pass := "  ✓ kind — " + purpose + "\n"
	unknown := "  ! kind — " + purpose + "; version unknown, " + need + "\n"
	older := func(v string) string {
		return "  ✗ kind " + v + " — " + purpose + "; " + need + " (fix: b update kind)\n"
	}
	cases := []struct {
		name string
		out  string
		err  error
		want string
		ok   bool
	}{
		{"an older release fails", "kind v0.17.0 go1.19.4 linux/amd64\n", nil, older("v0.17.0"), false},
		{"the minimum passes", "kind v0.18.0 go1.20.2 linux/amd64\n", nil, pass, true},
		{"a pre-release of a later release passes", "kind v0.31.0-alpha.12+0123456789abcd go1.25.5 linux/amd64\n", nil, pass, true},
		{"a later major passes", "kind v1.0.0 go1.26.1 linux/amd64\n", nil, pass, true},
		{"the minor compares as a number", "kind v0.9.0 go1.15.2 linux/amd64\n", nil, older("v0.9.0"), false},
		{"the patch counts", "kind v0.17.9\n", nil, older("v0.17.9"), false},
		{"a pre-release of an older release fails with its numbers only", "kind v0.17.0-rc.1+abc go1.19 linux/amd64\n", nil, older("v0.17.0"), false},
		{"a pre-release of the minimum passes: only major.minor.patch count", "kind v0.18.0-alpha.1 go1.20 linux/amd64\n", nil, pass, true},
		{"leading zeros compare as decimal", "kind v0.018.0 go1.20 linux/amd64\n", nil, pass, true},
		{"an unparseable version warns", "kind vdev go1.25.5 linux/amd64\n", nil, unknown, true},
		{"text after the patch number warns", "kind v0.18.0rc1 go1.20 linux/amd64\n", nil, unknown, true},
		{"a number of more than 9 digits warns", "kind v0.1234567890.0 go1.20 linux/amd64\n", nil, unknown, true},
		{"an output without the kind field warns", "v0.18.0\n", nil, unknown, true},
		{"a first field other than kind warns", "kindest v0.32.0 go1.25.5 linux/amd64\n", nil, unknown, true},
		{"an empty output warns", "", nil, unknown, true},
		{"the version must be on the first line", "\nkind v0.32.0 go1.25.5 linux/amd64\n", nil, unknown, true},
		{"a carriage return ends no field", "kind v0.32.0\r\n", nil, unknown, true},
		{"a tab separates the fields too", "kind\tv0.17.0\tgo1.19.4 linux/amd64\n", nil, older("v0.17.0"), false},
		{"leading spaces and tabs are skipped", " \t kind v0.32.0 go1.25.5 linux/amd64\n", nil, pass, true},
		{"a leading space before an older release", " kind v0.17.0 go1.19.4 linux/amd64\n", nil, older("v0.17.0"), false},
		{"a failed kind version warns", "kind v0.32.0 go1.25.5 linux/amd64\n", errors.New("exit status 1"), unknown, true},
	}
	bin := doctorStubBin(t, "kind")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := kindVersionRunner(tc.out, tc.err)
			var out strings.Builder
			ok := doctorKind(context.Background(), r, &out, bin, purpose, true)
			if got := out.String(); got != tc.want || ok != tc.ok {
				t.Errorf("kind version %q: %q ok=%v, want %q ok=%v", tc.out, got, ok, tc.want, tc.ok)
			}
			if want := filepath.Join(bin, "kind") + " version"; len(r.calls) != 1 || r.calls[0] != want {
				t.Errorf("calls %q, want [%q]", r.calls, want)
			}
		})
	}
}

// The bash twin keeps its own copy of the minimum (DOCTOR_KIND_MIN in the
// embedded mirror of .lok8s/libs/doctor). Two hand-kept copies drift; this
// fails first. The file must set it once: a second assignment would win.
// An assignment counts also when it is indented, or when it follows
// readonly, declare (with options such as -g), typeset or export.
func TestDoctorKindMinMatchesBash(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "libs", "doctor"))
	if err != nil {
		t.Fatal(err)
	}
	m := doctorKindMinAssignRe.FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("libs/doctor sets DOCTOR_KIND_MIN %d times, want once", len(m))
	}
	if got := string(m[0][1]); got != doctorKindMin {
		t.Errorf("bash DOCTOR_KIND_MIN %q != Go doctorKindMin %q", got, doctorKindMin)
	}
}

// doctorKindMinAssignRe matches an assignment of DOCTOR_KIND_MIN at the
// start of a line: after spaces or tabs, and after readonly, declare,
// typeset or export with their options. The value is the text up to white
// space.
var doctorKindMinAssignRe = regexp.MustCompile(`(?m)^[ \t]*(?:(?:readonly|declare|typeset|export)(?:[ \t]+-[A-Za-z]+)*[ \t]+)?DOCTOR_KIND_MIN=(\S*)`)

// The pattern of TestDoctorKindMinMatchesBash finds each form of the
// assignment, and no other name.
func TestDoctorKindMinAssignmentForms(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"DOCTOR_KIND_MIN=v0.18.0", true},
		{"  DOCTOR_KIND_MIN=v0.18.0", true},
		{"\tDOCTOR_KIND_MIN=v0.18.0", true},
		{"readonly DOCTOR_KIND_MIN=v0.18.0", true},
		{"declare DOCTOR_KIND_MIN=v0.18.0", true},
		{"declare -g DOCTOR_KIND_MIN=v0.18.0", true},
		{"  declare -gr DOCTOR_KIND_MIN=v0.18.0", true},
		{"typeset DOCTOR_KIND_MIN=v0.18.0", true},
		{"export DOCTOR_KIND_MIN=v0.18.0", true},
		{"# DOCTOR_KIND_MIN=v0.18.0", false},
		{"XDOCTOR_KIND_MIN=v0.18.0", false},
		{"echo ${DOCTOR_KIND_MIN}", false},
	} {
		if got := doctorKindMinAssignRe.MatchString(tc.line); got != tc.want {
			t.Errorf("%q: match %v, want %v", tc.line, got, tc.want)
		}
	}
}

// The tools section prints one kind line, the line of doctorKind. kind is
// also a row of doctorToolsPost (for its purpose): without the skip of
// that row, the generic doctorTool line follows it. Each state of the line
// counts: older, the minimum, version unknown and absent.
func TestDoctorToolsSectionPrintsOneKindLine(t *testing.T) {
	tools := []string{"argsh", "yq", "kustomize", "kubectl", "envsubst", "docker", "jq"}
	withKind := doctorStubBin(t, append(tools, "kind")...)
	withoutKind := doctorStubBin(t, tools...)
	p := synthProject(t)
	for _, tc := range []struct{ bin, out string }{
		{withKind, "kind v0.17.0 go1.19.4 linux/amd64\n"},
		{withKind, "kind v0.18.0 go1.20.2 linux/amd64\n"},
		{withKind, "kind vdev\n"},
		{withoutKind, ""},
	} {
		var out strings.Builder
		doctorToolsSection(context.Background(), kindVersionRunner(tc.out, nil), p, "", tc.bin, &out)
		n := 0
		for line := range strings.SplitSeq(out.String(), "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[1] == "kind" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("kind version %q: %d kind lines, want 1:\n%s", tc.out, n, out.String())
		}
	}
}

// kind is optional: without it the line is the warning of every optional
// tool, and nothing runs.
func TestDoctorKindAbsentWarns(t *testing.T) {
	r := kindVersionRunner("kind v0.17.0\n", nil)
	var out strings.Builder
	if !doctorKind(context.Background(), r, &out, t.TempDir(), "local clusters (Lo driver)", true) {
		t.Error("an absent kind fails")
	}
	if want := "  ! kind not found (optional) — local clusters (Lo driver)\n"; out.String() != want {
		t.Errorf("absent: %q, want %q", out.String(), want)
	}
	if len(r.calls) != 0 {
		t.Errorf("calls without kind: %q", r.calls)
	}
}

// A kind file without the exec bit is not found (toolchain.LookPath): the
// line is the warning of an absent kind, and nothing runs. Before a kind
// that can run, it is skipped, and the second one runs. Bash twin: the
// doctor::_kind exec-bit cases in tests/unit/doctor_test.bats.
func TestDoctorKindNeedsTheExecBit(t *testing.T) {
	const purpose = "local clusters (Lo driver)"
	noexec := t.TempDir()
	testutil.WriteFile(t, filepath.Join(noexec, "kind"), "#!/bin/sh\nexit 99\n")
	if err := os.Chmod(filepath.Join(noexec, "kind"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := kindVersionRunner("kind v0.18.0 go1.20.2 linux/amd64\n", nil)
	var out strings.Builder
	ok := doctorKind(context.Background(), r, &out, noexec, purpose, true)
	if want := "  ! kind not found (optional) — " + purpose + "\n"; !ok || out.String() != want || len(r.calls) != 0 {
		t.Errorf("a kind without the exec bit: %q ok=%v calls %q, want %q ok=true and no call", out.String(), ok, r.calls, want)
	}

	bin := doctorStubBin(t, "kind")
	r = kindVersionRunner("kind v0.18.0 go1.20.2 linux/amd64\n", nil)
	out.Reset()
	ok = doctorKind(context.Background(), r, &out, noexec+string(os.PathListSeparator)+bin, purpose, true)
	if want := "  ✓ kind — " + purpose + "\n"; !ok || out.String() != want {
		t.Errorf("a kind that can run after one without the exec bit: %q ok=%v, want %q", out.String(), ok, want)
	}
	if want := filepath.Join(bin, "kind") + " version"; len(r.calls) != 1 || r.calls[0] != want {
		t.Errorf("calls %q, want [%q]", r.calls, want)
	}
}

// doctorKindDomains is a project with one domain of each kind that
// kindFails tells apart. The bash twin writes the same specs
// (tests/unit/doctor_test.bats, _kind_domains).
func doctorKindDomains(t *testing.T) *config.Paths {
	t.Helper()
	p := synthProject(t)
	for d, spec := range map[string]string{
		"lo.dev/cluster.lok8s.yaml":      "kind: Lo\nmetadata:\n  name: lo\n",
		"metal.dev/cluster.lok8s.yaml":   "kind: KubeOne\nmetadata:\n  name: metal\n",
		"app.dev/deploy.lok8s.yaml":      "kind: Deploy\nspec:\n  clusterRef:\n    domain: lo.dev\n",
		"nokind.dev/cluster.lok8s.yaml":  "metadata:\n  name: nokind\n",
		"badkind.dev/cluster.lok8s.yaml": "kind: ../lo\nmetadata:\n  name: badkind\n",
	} {
		testutil.WriteFile(t, filepath.Join(p.Clusters, filepath.FromSlash(d)), spec)
	}
	return p
}

// An older kind fails the tools section (and so the doctor exit code)
// when the active domain uses the Lo driver, and when there is no active
// domain: no name, no spec, or a spec whose driver doctor cannot read.
// For a domain of another driver, a deploy domain included, the line is
// a warning with the same text, and the section passes. The minimum kind
// passes for every domain. Bash twin: "doctor::_kind fails an older kind
// only where the domain needs it" in tests/unit/doctor_test.bats.
func TestDoctorToolsSectionOlderKindByDomain(t *testing.T) {
	bin := doctorStubBin(t, "argsh", "yq", "kustomize", "kubectl", "envsubst", "docker", "jq", "kind")
	p := doctorKindDomains(t)
	const line = "kind v0.17.0 — local clusters (Lo driver); lo up needs kind v0.18.0 or later for networking.dnsSearch (fix: b update kind)\n"
	for _, tc := range []struct {
		d    string
		ok   bool
		mark string
	}{
		{"lo.dev", false, "✗"},
		{"", false, "✗"},
		{"missing.dev", false, "✗"},
		{"nokind.dev", false, "✗"},
		{"badkind.dev", false, "✗"},
		{"metal.dev", true, "!"},
		{"app.dev", true, "!"},
	} {
		var out strings.Builder
		got := doctorToolsSection(context.Background(), kindVersionRunner("kind v0.17.0 go1.19.4 linux/amd64\n", nil), p, tc.d, bin, &out)
		if got != tc.ok || !strings.Contains(out.String(), "\n  "+tc.mark+" "+line) {
			t.Errorf("domain %q, an older kind: section ok=%v, want %v and the line %q:\n%s", tc.d, got, tc.ok, tc.mark+" "+line, out.String())
		}
		out.Reset()
		got = doctorToolsSection(context.Background(), kindVersionRunner("kind v0.18.0 go1.20.2 linux/amd64\n", nil), p, tc.d, bin, &out)
		if !got || !strings.Contains(out.String(), "\n  ✓ kind — local clusters (Lo driver)\n") {
			t.Errorf("domain %q, the minimum kind: section ok=%v, want true:\n%s", tc.d, got, out.String())
		}
	}
}

// `lo doctor --domain` gives the active domain to the kind line: an older
// kind on PATH is a failure for a domain of the Lo driver and a warning
// for a KubeOne domain. PATH holds only stubs: bash (it prints its
// version), every required tool and kind. So the exit code comes from the
// kind line alone, and the minimum kind passes for the Lo domain. Bash
// twin: "lo doctor reads the active domain for the kind line".
func TestDoctorKindLineReadsTheActiveDomain(t *testing.T) {
	p := doctorKindDomains(t)
	bin := doctorStubBin(t, "argsh", "yq", "kustomize", "kubectl", "envsubst", "docker", "jq")
	stub := func(name, body string) {
		testutil.WriteFile(t, filepath.Join(bin, name), "#!/bin/sh\n"+body+"\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("bash", "echo 5.2")
	t.Setenv("PATH", bin)
	const line = "kind v0.17.0 — local clusters (Lo driver); lo up needs kind v0.18.0 or later for networking.dnsSearch (fix: b update kind)\n"
	for _, tc := range []struct {
		d, release, want string
		ok               bool
	}{
		{"lo.dev", "v0.17.0", "✗ " + line, false},
		{"metal.dev", "v0.17.0", "! " + line, true},
		{"lo.dev", "v0.18.0", "✓ kind — local clusters (Lo driver)\n", true},
	} {
		stub("kind", "echo 'kind "+tc.release+" go1.19.4 linux/amd64'")
		stdout, _, err := runLo(t, NewRoot(p), "doctor", "--domain", tc.d)
		if (err == nil) != tc.ok || !strings.Contains(stdout, "\n  "+tc.want) {
			t.Errorf("doctor --domain %s, kind %s: err=%v, want ok=%v and the line %q in:\n%s", tc.d, tc.release, err, tc.ok, tc.want, stdout)
		}
		if strings.Contains(stdout, "MISSING (required)") {
			t.Errorf("doctor --domain %s: a required tool is missing:\n%s", tc.d, stdout)
		}
	}
}

// The answers of `stat --version` in TestDoctorCoreutils: stdout, stderr.
const (
	statGNU     = "stat (GNU coreutils) 9.12\nCopyright (C) 2026 Free Software Foundation, Inc.\n"
	statBSD     = "stat: illegal option -- -\nusage: stat [-FLnq] [-f format | -l | -r | -s | -x] [-t timefmt] [file ...]\n"
	statBusyBox = "stat: unrecognized option '--version'\nBusyBox v1.36.1 (2024-06-10 07:11:47 UTC) multi-call binary.\n\nUsage: stat [-ltf] [-c FMT] FILE...\n"
	statUutils  = "stat (uutils coreutils) 0.2.2\n"
)

// statVersionRunner answers `stat --version` with stdout, stderr and err.
// Any other command fails: the probe runs nothing else.
func statVersionRunner(stdout, stderr string, err error) *scriptRunner {
	return &scriptRunner{handler: func(c execx.Cmd) error {
		if filepath.Base(c.Name) == "stat" && len(c.Args) == 1 && c.Args[0] == "--version" {
			io.WriteString(c.Stdout, stdout)
			io.WriteString(c.Stderr, stderr)
			return err
		}
		return errors.New("unexpected command: " + c.Name)
	}}
}

// The frozen bash libs (kubehz, bootstrap) use stat -c, mv -T, ln -T and
// realpath -e, which GNU coreutils and uutils coreutils have. Doctor warns
// when lok8s.yaml routes commands to bash and the first line of
// `stat --version` contains neither "GNU coreutils" nor "uutils
// coreutils". With the Go implementation selected, or a routing whose tree
// is missing (doctorImplementation warns about that), it prints nothing
// and runs no stat. The probe runs the stat that LookPath found (its
// absolute path). The bash twin (doctor::_coreutils,
// tests/unit/doctor_test.bats) has the same cases, except the missing tree.
func TestDoctorCoreutils(t *testing.T) {
	warn := "  ! " + doctorCoreutilsWarning + "\n"
	exit1 := errors.New("exit status 1")
	const (
		bash  = "    default: bash\n"
		mixed = "    bash:\n      commands: [version]\n"
	)
	withStat := doctorStubBin(t, "stat")
	for _, tc := range []struct {
		name           string
		block          string
		noProject      bool // a lok8s.yaml that is not a kind: Project document
		noTree         bool // no .lok8s/lo: the routing has a problem
		bin            string
		stdout, stderr string
		err            error
		want           string
		probe          bool // stat --version runs
	}{
		{name: "GNU coreutils", block: bash, bin: withStat, stdout: statGNU, probe: true},
		{name: "GNU coreutils, a mixed route", block: mixed, bin: withStat, stdout: statGNU, probe: true},
		{name: "GNU coreutils with an exit code: the literal decides", block: bash, bin: withStat, stdout: statGNU, err: exit1, probe: true},
		{name: "a BSD-like answer", block: bash, bin: withStat, stderr: statBSD, err: exit1, want: warn, probe: true},
		{name: "a BSD-like answer, a mixed route", block: mixed, bin: withStat, stderr: statBSD, err: exit1, want: warn, probe: true},
		{name: "a BusyBox answer", block: bash, bin: withStat, stderr: statBusyBox, err: exit1, want: warn, probe: true},
		{name: "uutils coreutils: the GNU forms work", block: bash, bin: withStat, stdout: statUutils, probe: true},
		{name: "GNU coreutils only on the second line", block: bash, bin: withStat, stdout: "stat 1.0\nGNU coreutils\n", want: warn, probe: true},
		{name: "no stat on PATH", block: bash, bin: t.TempDir(), want: warn},
		{name: "the Go implementation: no block", block: "", bin: withStat, stderr: statBSD, err: exit1},
		{name: "the Go implementation: default go", block: "    default: go\n", bin: withStat, stderr: statBSD, err: exit1},
		{name: "the Go implementation: an empty command list", block: "    default: go\n    bash:\n      commands: []\n", bin: withStat, stderr: statBSD, err: exit1},
		{name: "the Go implementation: not a Project document", block: bash, noProject: true, bin: withStat, stderr: statBSD, err: exit1},
		{name: "an unknown default selects nothing", block: "    default: Bash\n", bin: withStat, stderr: statBSD, err: exit1},
		{name: "the tree is missing: nothing selected", block: bash, noTree: true, bin: withStat, stderr: statBSD, err: exit1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := routedProject(t, tc.block, !tc.noTree)
			if tc.noProject {
				testutil.WriteFile(t, filepath.Join(p.Base, config.ProjectFile), "kind: Service\nspec:\n  implementation:\n"+tc.block)
			}
			r := statVersionRunner(tc.stdout, tc.stderr, tc.err)
			var out strings.Builder
			doctorCoreutils(context.Background(), r, &out, p, tc.bin)
			if out.String() != tc.want {
				t.Errorf("got %q, want %q", out.String(), tc.want)
			}
			if ran := len(r.calls) > 0; ran != tc.probe {
				t.Errorf("stat --version ran: %v, want %v (calls %q)", ran, tc.probe, r.calls)
			}
			// The probe runs the stat that LookPath found, not a bare name.
			if want := filepath.Join(tc.bin, "stat") + " --version"; tc.probe && (len(r.calls) != 1 || r.calls[0] != want) {
				t.Errorf("calls %q, want [%q]", r.calls, want)
			}
		})
	}
}

// The environment section prints the coreutils line after the
// implementation line, and the line does not change the exit code (a
// warning). The project has its own .bin/argsh, so the bash-mode check
// passes.
func TestDoctorEnvironmentSectionCoreutilsLine(t *testing.T) {
	p := routedProject(t, "    bash:\n      commands: [version]\n", true)
	testutil.WriteFile(t, filepath.Join(p.Bin, "argsh"), "#!/usr/bin/env bash\n")
	if err := os.Chmod(filepath.Join(p.Bin, "argsh"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	ok := doctorEnvironmentSection(context.Background(), statVersionRunner("", statBSD, errors.New("exit status 1")), p, "", doctorStubBin(t, "stat"), false, &out)
	if !strings.Contains(out.String(), "\n  ! "+doctorCoreutilsWarning+"\n") {
		t.Errorf("no coreutils line in:\n%s", out.String())
	}
	if impl, cu := strings.Index(out.String(), "implementation: go; bash for version"), strings.Index(out.String(), doctorCoreutilsWarning); impl < 0 || impl > cu {
		t.Errorf("the coreutils line is not after the implementation line:\n%s", out.String())
	}
	if !ok {
		t.Errorf("the section failed:\n%s", out.String())
	}
}

// The bash twin keeps its own copy of the warning text (doctor::_coreutils
// in the embedded mirror of .lok8s/libs/doctor). Two hand-kept copies
// drift; this fails first.
func TestDoctorCoreutilsWarningMatchesBash(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "libs", "doctor"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), `doctor::_warn "`+doctorCoreutilsWarning+`"`); n != 1 {
		t.Errorf("libs/doctor prints the coreutils warning %d times, want once with the text of doctorCoreutilsWarning", n)
	}
}
