package toolchain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTool drops an executable placeholder at path; the Probe seam answers
// for it, so nothing is really executed.
func fakeTool(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func statuses(checks []Check) string {
	var s []string
	for _, c := range checks {
		s = append(s, map[Status]string{OK: "ok", Warn: "warn", Bad: "bad"}[c.Status])
	}
	return strings.Join(s, " ")
}

func TestDoctorFullyProvisionedCore(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	home := filepath.Join(base, ".kustomize")
	fakeTool(t, filepath.Join(bin, "b"))
	fakeTool(t, filepath.Join(bin, "kustomize"))
	fakeTool(t, filepath.Join(home, filepath.FromSlash(ChartRendererPluginRel)))
	fakeTool(t, filepath.Join(home, filepath.FromSlash(SecretPluginRel)))
	answers := map[string]string{
		"b --version":           BRelease.Version,
		"kustomize version":     KustomizeCLI,
		"ChartRenderer version": KhelmVersion + " (helm " + HelmVersion + ")",
		"Secret --version":      "v0.3.0",
	}
	probe := func(path string, args ...string) (string, error) {
		key := filepath.Base(path) + " " + strings.Join(args, " ")
		v, ok := answers[key]
		if !ok {
			return "", errors.New("unexpected probe " + key)
		}
		return v, nil
	}
	checks := Doctor(t.Context(), DoctorOptions{Base: base, Bin: bin, PluginHome: home, LoVersion: "0.3.0", Probe: probe})
	if got := statuses(checks); got != "ok ok ok ok" {
		t.Fatalf("statuses = %s\n%+v", got, checks)
	}
	want := []string{
		"b " + BRelease.Version + " (.bin/b)",
		"kustomize " + KustomizeCLI + " (.bin/kustomize)",
		"khelm ChartRenderer " + KhelmVersion + " (.kustomize/" + ChartRendererPluginRel + ")",
		"secrets.lok8s.dev Secret v0.3.0 (.kustomize/" + SecretPluginRel + ")",
	}
	for i, w := range want {
		if checks[i].Msg != w {
			t.Errorf("check %d = %q, want %q", i, checks[i].Msg, w)
		}
	}
}

func TestDoctorMismatchAndMissing(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	home := filepath.Join(base, ".kustomize")
	fakeTool(t, filepath.Join(bin, "kustomize"))
	fakeTool(t, filepath.Join(home, filepath.FromSlash(SecretPluginRel)))
	probe := func(path string, args ...string) (string, error) {
		switch filepath.Base(path) {
		case "kustomize":
			return "v5.0.0", nil
		case "Secret":
			return "", errors.New("exit status 1") // pre-flag build
		}
		return "", errors.New("unexpected")
	}
	// core: missing tools are fatal.
	checks := Doctor(t.Context(), DoctorOptions{Base: base, Bin: bin, PluginHome: home, LoVersion: "0.3.0", Probe: probe, PATH: "/nonexistent"})
	if got := statuses(checks); got != "bad warn bad warn" {
		t.Fatalf("core statuses = %s\n%+v", got, checks)
	}
	if !strings.Contains(checks[0].Msg, "b missing at .bin/b — "+Fix) {
		t.Errorf("b line: %s", checks[0].Msg)
	}
	if !strings.Contains(checks[1].Msg, "kustomize v5.0.0 at .bin/kustomize — expected "+KustomizeCLI) {
		t.Errorf("kustomize line: %s", checks[1].Msg)
	}
	if !strings.Contains(checks[2].Msg, "khelm ChartRenderer missing at .kustomize/"+ChartRendererPluginRel) || !strings.Contains(checks[2].Msg, Fix) {
		t.Errorf("khelm line: %s", checks[2].Msg)
	}
	if !strings.Contains(checks[3].Msg, "version unknown (built before --version; expected v0.3.0)") {
		t.Errorf("secret line: %s", checks[3].Msg)
	}
	// lo-full: the render tools are optional → warnings only.
	checks = Doctor(t.Context(), DoctorOptions{Base: base, Bin: bin, PluginHome: home, LoVersion: "0.3.0", Probe: probe, PATH: "/nonexistent", Full: true})
	if got := statuses(checks); got != "bad warn warn warn" {
		t.Fatalf("full statuses = %s\n%+v", got, checks)
	}
	if !strings.Contains(checks[2].Msg, "optional on lo-full") {
		t.Errorf("full khelm line: %s", checks[2].Msg)
	}
}

func TestDoctorFindsKustomizeOnPATH(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, ".bin")
	elsewhere := filepath.Join(t.TempDir(), "tools")
	fakeTool(t, filepath.Join(elsewhere, "kustomize"))
	probe := func(path string, args ...string) (string, error) { return KustomizeCLI, nil }
	checks := Doctor(t.Context(), DoctorOptions{Base: base, Bin: bin, PluginHome: filepath.Join(base, ".kustomize"), LoVersion: "0.3.0", Probe: probe, PATH: elsewhere})
	if checks[1].Status != OK || !strings.Contains(checks[1].Msg, elsewhere) {
		t.Fatalf("PATH kustomize: %+v", checks[1])
	}
}

// releaseBelow is a release one step below the pin, so the test keeps
// working after each bump. It is bare, as a b release prints it.
func releaseBelow(t *testing.T) string {
	t.Helper()
	var maj, mnr, pat int
	if _, err := fmt.Sscanf(BRelease.Version, "%d.%d.%d", &maj, &mnr, &pat); err != nil {
		t.Fatalf("BRelease.Version %q: %v", BRelease.Version, err)
	}
	switch {
	case pat > 0:
		pat--
	case mnr > 0:
		mnr, pat = mnr-1, 999
	default:
		maj, mnr, pat = maj-1, 999, 999
	}
	return fmt.Sprintf("%d.%d.%d", maj, mnr, pat)
}

// The b line warns when the project's .bin/b is older than the b this lo
// ships, and names the command that updates it (lo toolchain install keeps
// an existing .bin/b). A b release prints its version bare (1.2.3), a
// build from source with a v (v1.2.3); the older line prints both versions
// bare. The same release, a newer one and a pre-release of the pinned
// release keep the plain line. A version that does not parse is a warning
// with the words of a failed probe.
func TestDoctorFlagsAnOlderB(t *testing.T) {
	t.Parallel()
	pin := BRelease.Version
	older := func(v string) string {
		return "b " + v + " at .bin/b is older than " + pin + ", the b this lo ships — " + BUpdateFix
	}
	unknown := func(why string) string { return "b at .bin/b (version unknown: " + why + ")" }
	below := releaseBelow(t)
	for _, c := range []struct {
		printed string
		err     error
		status  Status
		msg     string
	}{
		{below, nil, Warn, older(below)},
		{"0.1.0", nil, Warn, older("0.1.0")},
		{"0.1.0-rc.1", nil, Warn, older("0.1.0-rc.1")},
		{"v0.1.0", nil, Warn, older("0.1.0")},
		{pin, nil, OK, "b " + pin + " (.bin/b)"},
		{"v" + pin, nil, OK, "b v" + pin + " (.bin/b)"},
		{pin + "-rc.1", nil, OK, "b " + pin + "-rc.1 (.bin/b)"},
		{"999.0.0", nil, OK, "b 999.0.0 (.bin/b)"},
		{"dev", nil, Warn, unknown(`--version printed "dev", not a version`)},
		{"", nil, Warn, unknown(`--version printed "", not a version`)},
		{"", errors.New("exit status 1"), Warn, unknown("exit status 1")},
	} {
		name := fmt.Sprintf("%q", c.printed)
		if c.err != nil {
			name = "a failed probe"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			bin := filepath.Join(base, ".bin")
			fakeTool(t, filepath.Join(bin, "b"))
			probe := func(path string, args ...string) (string, error) {
				if filepath.Base(path) == "b" {
					return c.printed, c.err
				}
				return "", errors.New("not installed")
			}
			checks := Doctor(t.Context(), DoctorOptions{Base: base, Bin: bin, PluginHome: filepath.Join(base, ".kustomize"), LoVersion: "0.3.0", Probe: probe, PATH: "/nonexistent"})
			if checks[0].Status != c.status || checks[0].Msg != c.msg {
				t.Errorf("b line = %v %q, want %v %q", checks[0].Status, checks[0].Msg, c.status, c.msg)
			}
		})
	}
}

// OlderRelease names no tool in its errors: lo doctor uses it for kind
// too. A got that is not a version wraps errNotRelease, which the b check
// turns into its own words (TestDoctorFlagsAnOlderB); a pin that is not
// a version does not.
func TestOlderReleaseErrorsNameNoTool(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		got, want, msg string
		notRelease     bool
	}{
		{"dev", "0.18.0", `"dev" is not a version`, true},
		{"", "0.18.0", `"" is not a version`, true},
		{"0.18.0", "pin", `the pin "pin" is not a version`, false},
	} {
		older, err := OlderRelease(c.got, c.want)
		if older || err == nil || err.Error() != c.msg || errors.Is(err, errNotRelease) != c.notRelease {
			t.Errorf("OlderRelease(%q, %q) = %v, %v; want false, %q (errNotRelease %v)", c.got, c.want, older, err, c.msg, c.notRelease)
		}
	}
	for _, c := range []struct {
		got, want string
		older     bool
	}{
		{"0.17.0", "0.18.0", true},
		{"v0.18.0", "v0.18.0", false},
		{"0.18.0-rc.1", "0.18.0", false},
		{"1.0.0", "0.18.0", false},
	} {
		if older, err := OlderRelease(c.got, c.want); err != nil || older != c.older {
			t.Errorf("OlderRelease(%q, %q) = %v, %v; want %v, nil", c.got, c.want, older, err, c.older)
		}
	}
}
