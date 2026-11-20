package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
// for a KubeOne domain. The stub kind is the only tool on PATH. Bash twin:
// "lo doctor reads the active domain for the kind line".
func TestDoctorKindLineReadsTheActiveDomain(t *testing.T) {
	p := doctorKindDomains(t)
	bin := t.TempDir()
	testutil.WriteFile(t, filepath.Join(bin, "kind"), "#!/bin/sh\necho 'kind v0.17.0 go1.19.4 linux/amd64'\n")
	if err := os.Chmod(filepath.Join(bin, "kind"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	const line = "kind v0.17.0 — local clusters (Lo driver); lo up needs kind v0.18.0 or later for networking.dnsSearch (fix: b update kind)\n"
	for d, mark := range map[string]string{"lo.dev": "✗", "metal.dev": "!"} {
		stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", d)
		if !strings.Contains(stdout, "\n  "+mark+" "+line) {
			t.Errorf("doctor --domain %s: no line %q in:\n%s", d, mark+" "+line, stdout)
		}
	}
}
