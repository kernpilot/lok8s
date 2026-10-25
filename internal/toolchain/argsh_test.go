package toolchain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// Argsh names the fix for a missing ${PATH_BIN}/argsh by what .bin holds:
// a b.yaml that lists argsh needs only an install; the `lo toolchain
// install` template keeps it in the bash group; any other b.yaml needs the
// entry first. `.bin/b` when it is there, else the b on PATH.
func TestArgshNamesTheFix(t *testing.T) {
	for _, tc := range []struct {
		name, byaml string
		withB       bool
		fix         string
	}{
		{"kubehz-cluster preset", "binaries:\n  argsh: {}\n  kubectl: {}\n", false, "b install"},
		{"github key", "binaries:\n  github.com/arg-sh/argsh:\n    asset: argsh\n", true, ".bin/b install"},
		{"alias", "binaries:\n  github.com/example/runtime:\n    alias: argsh\n", false, "b install"},
		{"template without the bash group", Marker + "\nbinaries:\n  # github.com/arg-sh/argsh:\n  kubectl: {}\n", true, "uncomment the bash group in .bin/b.yaml, then .bin/b install"},
		{"no entry", "binaries:\n  kubectl: {}\n", false, "add github.com/arg-sh/argsh to .bin/b.yaml, then b install"},
		{"no b.yaml", "", false, "add github.com/arg-sh/argsh to .bin/b.yaml, then b install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.byaml != "" {
				testutil.WriteFile(t, filepath.Join(bin, "b.yaml"), tc.byaml)
			}
			if tc.withB {
				if err := os.WriteFile(filepath.Join(bin, "b"), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
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
