package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Since the eject model a project needs no .lok8s tree: `clusters/` or
// `lok8s.yaml` marks the root, `.lok8s/lo` stays a fallback marker.
func TestResolvePathsRecognizesEjectModelMarkers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(base string)
	}{
		{"clusters dir", func(base string) { os.MkdirAll(filepath.Join(base, "clusters"), 0o755) }},
		{"lok8s.yaml", func(base string) { os.WriteFile(filepath.Join(base, "lok8s.yaml"), []byte("kind: Project\n"), 0o644) }},
		{".lok8s/lo fallback", func(base string) {
			os.MkdirAll(filepath.Join(base, ".lok8s"), 0o755)
			os.WriteFile(filepath.Join(base, ".lok8s", "lo"), []byte("#!/bin/bash\n"), 0o755)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			base := t.TempDir()
			tc.setup(base)
			sub := filepath.Join(base, "some", "nested", "dir")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			chdir(t, sub)
			p, err := ResolvePaths()
			if err != nil {
				t.Fatal(err)
			}
			wantBase, _ := filepath.EvalSymlinks(base)
			gotBase, _ := filepath.EvalSymlinks(p.Base)
			if gotBase != wantBase {
				t.Errorf("Base = %q, want %q", gotBase, wantBase)
			}
		})
	}
	// A lok8s.yaml that is a DIRECTORY, or a clusters FILE, is not a marker.
	clearEnv(t)
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "lok8s.yaml"), 0o755)
	os.WriteFile(filepath.Join(base, "clusters"), []byte("x"), 0o644)
	sub := filepath.Join(base, "deeper")
	os.MkdirAll(sub, 0o755)
	chdir(t, sub)
	p, _ := ResolvePaths()
	if got, _ := filepath.EvalSymlinks(p.Base); got != mustEval(sub) {
		t.Errorf("wrong-typed markers accepted: Base = %q", p.Base)
	}
}

func mustEval(p string) string {
	out, _ := filepath.EvalSymlinks(p)
	return out
}
