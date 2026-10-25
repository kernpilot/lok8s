package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// argshFixtures: what .bin/b.yaml holds, and the fix that follows.
var argshFixtures = []struct {
	name, byaml, fix string
}{
	{"kubehz-cluster preset", "binaries:\n  argsh: {}\n  kubectl: {}\n", ArgshFixInstall},
	{"github key", "binaries:\n  github.com/arg-sh/argsh:\n    asset: argsh\n", ArgshFixInstall},
	{"alias", "binaries:\n  github.com/example/runtime:\n    alias: argsh\n", ArgshFixInstall},
	{"commented out", Marker + "\nbinaries:\n  # github.com/arg-sh/argsh:\n  #   asset: argsh\n  kubectl: {}\n", ArgshFixAdd},
	{"a longer name", "binaries:\n  fooargsh: {}\n  github.com/x/argsh-tools: {}\n", ArgshFixAdd},
	{"no entry", "binaries:\n  kubectl: {}\n", ArgshFixAdd},
	{"no b.yaml", "", ArgshFixAdd},
}

// Argsh names the fix for a missing ${PATH_BIN}/argsh: `b install` when
// b.yaml lists argsh, else add it first.
func TestArgshNamesTheFix(t *testing.T) {
	for _, tc := range argshFixtures {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.byaml != "" {
				testutil.WriteFile(t, filepath.Join(bin, "b.yaml"), tc.byaml)
			}
			// A .bin/b changes nothing: the text is the bash entrypoint's.
			if err := os.WriteFile(filepath.Join(bin, "b"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			path, fix, ok := Argsh(bin)
			if path != filepath.Join(bin, "argsh") || ok {
				t.Errorf("path = %q ok = %v, want %q false", path, ok, filepath.Join(bin, "argsh"))
			}
			if fix != tc.fix {
				t.Errorf("fix = %q, want %q", fix, tc.fix)
			}
			testutil.WriteFile(t, filepath.Join(bin, "argsh"), "#!/usr/bin/env bash\n")
			if _, _, ok := Argsh(bin); !ok {
				t.Error("argsh present, ok = false")
			}
		})
	}
}

// The bash entrypoint derives the fix the same way: the same pattern
// (through grep -E) and the same two texts. Its guard runs here, with
// PATH_BIN on each fixture, and must print the Go fix.
func TestArgshFixMatchesTheBashEntrypoint(t *testing.T) {
	root := testutil.RepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "assets", "lok8s", "lo"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		"grep -Eq '" + ArgshListedPattern + "' \"${PATH_BIN}/b.yaml\"",
		`_lo_fix="` + ArgshFixAdd + `"`,
		`_lo_fix="` + ArgshFixInstall + `"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the entrypoint lacks %q", want)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for _, tc := range argshFixtures {
		bin := t.TempDir()
		if tc.byaml != "" {
			testutil.WriteFile(t, filepath.Join(bin, "b.yaml"), tc.byaml)
		}
		cmd := exec.Command(bash, filepath.Join(root, "internal", "assets", "lok8s", "lo"), "doctor")
		cmd.Env = append(os.Environ(), "PATH_BIN="+bin)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		want := "[error] argsh is missing at " + bin + "/argsh: lo runs on it (fix: " + tc.fix + ")\n"
		if err == nil || string(out) != want {
			t.Errorf("%s: err = %v, output = %q, want %q", tc.name, err, out, want)
		}
	}
}
