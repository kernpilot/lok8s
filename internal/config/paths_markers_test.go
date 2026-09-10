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

// A SERVICE's lok8s.yaml (kind: Service — every kubehz-cluster submodule
// carries one) is not a project marker: `cd <service> && lo …` must keep
// walking up to the umbrella project instead of resolving Base to the
// service directory.
func TestResolvePathsServiceLok8sYAMLIsNotAMarker(t *testing.T) {
	serviceYAML := "apiVersion: lok8s.dev/v1\nkind: Service\nmetadata:\n  name: api\nspec:\n  build:\n    dockerfile: lok8s.Dockerfile\n"
	for _, tc := range []struct {
		name string
		body string
	}{
		{"kind Service", serviceYAML},
		{"no kind", "metadata:\n  name: x\n"},
		{"malformed", "kind: [\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			project := t.TempDir()
			if err := os.MkdirAll(filepath.Join(project, "clusters"), 0o755); err != nil {
				t.Fatal(err)
			}
			service := filepath.Join(project, "services", "api")
			if err := os.MkdirAll(filepath.Join(service, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(service, "lok8s.yaml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			// From the service dir itself and from below it.
			for _, wd := range []string{service, filepath.Join(service, "src")} {
				chdir(t, wd)
				p, err := ResolvePaths()
				if err != nil {
					t.Fatal(err)
				}
				if got, want := mustEval(p.Base), mustEval(project); got != want {
					t.Errorf("from %s: Base = %q, want the umbrella project %q", wd, got, want)
				}
			}
		})
	}
	// Without any project above it the walk-up falls back to the working
	// directory, never to the service file's directory by virtue of the file.
	clearEnv(t)
	lone := t.TempDir()
	os.WriteFile(filepath.Join(lone, "lok8s.yaml"), []byte(serviceYAML), 0o644)
	sub := filepath.Join(lone, "src")
	os.MkdirAll(sub, 0o755)
	chdir(t, sub)
	p, _ := ResolvePaths()
	if got := mustEval(p.Base); got != mustEval(sub) {
		t.Errorf("lone service file promoted to a root: Base = %q", got)
	}
}

// FindProjectRoot applies the marker rules from an explicit directory and
// ignores PATH_BASE — the seam for commands that must act where the user
// stands (lo init toolchain) rather than in the ambient project.
func TestFindProjectRootIgnoresPathBase(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv("PATH_BASE", elsewhere)
	project := t.TempDir()
	os.WriteFile(filepath.Join(project, "lok8s.yaml"), []byte("apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: p\n"), 0o644)
	nested := filepath.Join(project, "a", "b")
	os.MkdirAll(nested, 0o755)
	if got := FindProjectRoot(nested); got != project {
		t.Errorf("FindProjectRoot(%s) = %q, want %q", nested, got, project)
	}
	bare := t.TempDir()
	if got := FindProjectRoot(bare); got != bare {
		t.Errorf("no marker: got %q, want the directory itself %q", got, bare)
	}
}

func mustEval(p string) string {
	out, _ := filepath.EvalSymlinks(p)
	return out
}
