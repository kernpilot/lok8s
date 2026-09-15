package config

import (
	"os"
	"path/filepath"
	"testing"
)

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PATH_BASE", "PATH_BIN", "PATH_LOK8S", "PATH_CLUSTERS", "PATH_SECRETS"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func TestResolvePathsWalksUpToProjectRoot(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".lok8s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".lok8s", "lo"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(base, "clusters", "kubehz.dev")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)

	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks on both sides — t.TempDir may live behind one (macOS /var).
	wantBase, _ := filepath.EvalSymlinks(base)
	gotBase, _ := filepath.EvalSymlinks(p.Base)
	if gotBase != wantBase {
		t.Errorf("Base = %q, want %q", gotBase, wantBase)
	}
	if p.Bin != filepath.Join(p.Base, ".bin") || p.Lok8s != filepath.Join(p.Base, ".lok8s") || p.Clusters != filepath.Join(p.Base, "clusters") {
		t.Errorf("derived paths wrong: %+v", p)
	}
	if p.SecretsEnvSet {
		t.Error("SecretsEnvSet = true with no PATH_SECRETS in env")
	}
}

func TestResolvePathsEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PATH_BASE", "/proj")
	t.Setenv("PATH_CLUSTERS", "/elsewhere/clusters")
	t.Setenv("PATH_SECRETS", "/proj/clusters/kubehz.dev/secrets")

	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.Base != "/proj" {
		t.Errorf("Base = %q", p.Base)
	}
	if p.Clusters != "/elsewhere/clusters" {
		t.Errorf("Clusters env override ignored: %q", p.Clusters)
	}
	if !p.SecretsEnvSet || p.SecretsEnv != "/proj/clusters/kubehz.dev/secrets" {
		t.Errorf("SecretsEnv = %q set=%v", p.SecretsEnv, p.SecretsEnvSet)
	}
}

func TestResolvePathsFallsBackToCwdOutsideProject(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	chdir(t, dir)

	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	wantDir, _ := filepath.EvalSymlinks(dir)
	gotBase, _ := filepath.EvalSymlinks(p.Base)
	if gotBase != wantDir {
		t.Errorf("Base = %q, want cwd %q", gotBase, wantDir)
	}
}

// Since the eject model a project needs no .lok8s tree: `clusters/` or
// `lok8s.yaml` marks the root. `.lok8s/lo` is not a marker (WP9).
func TestResolvePathsRecognizesEjectModelMarkers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(base string)
	}{
		{"clusters dir", func(base string) { os.MkdirAll(filepath.Join(base, "clusters"), 0o755) }},
		{"lok8s.yaml", func(base string) { os.WriteFile(filepath.Join(base, "lok8s.yaml"), []byte("kind: Project\n"), 0o644) }},
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

// A vendored bash tree alone (`.lok8s/lo`, no clusters/, no project file)
// does not mark a root any more: the walk passes it and falls back to the
// working directory. The same tree beside a `kind: Project` lok8s.yaml
// resolves through the file.
func TestResolvePathsLok8sTreeIsNotAMarker(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".lok8s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".lok8s", "lo"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(base, "some", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if got := mustEval(p.Base); got != mustEval(sub) {
		t.Errorf(".lok8s/lo alone promoted to a root: Base = %q, want the working directory %q", got, mustEval(sub))
	}
	if got := FindProjectRoot(sub); got != sub {
		t.Errorf("FindProjectRoot(%s) = %q, want the directory itself", sub, got)
	}
	if err := os.WriteFile(filepath.Join(base, "lok8s.yaml"), []byte("kind: Project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, _ := ResolvePaths(); mustEval(p.Base) != mustEval(base) {
		t.Errorf("with the project file: Base = %q, want %q", p.Base, base)
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

// PATH_LOK8S naming a tree the binary extracted (the cache manifest at
// its root) is not the project's .lok8s: the shim exports it to bash
// children, and a nested Go lo must not take the cache for the project.
func TestResolvePathsIgnoresACacheTreeInPathLok8s(t *testing.T) {
	base := t.TempDir()
	cache := filepath.Join(t.TempDir(), "lok8s")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, CacheMarker), []byte("lo: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH_BASE", base)
	t.Setenv("PATH_LOK8S", cache)
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.Lok8s != filepath.Join(base, ".lok8s") {
		t.Fatalf("Lok8s = %s, want the project default", p.Lok8s)
	}
	// A plain checkout in PATH_LOK8S is honoured as before.
	checkout := t.TempDir()
	t.Setenv("PATH_LOK8S", checkout)
	if p, _ := ResolvePaths(); p.Lok8s != checkout {
		t.Fatalf("Lok8s = %s, want %s", p.Lok8s, checkout)
	}
}

func TestKustomizePluginHomeDefaultsToProjectDir(t *testing.T) {
	p := &Paths{Base: "/proj"}
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	if got, want := KustomizePluginHome(p), filepath.Join("/proj", ".kustomize"); got != want {
		t.Errorf("unset: got %q, want %q", got, want)
	}
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "/elsewhere/plugins")
	if got := KustomizePluginHome(p); got != "/elsewhere/plugins" {
		t.Errorf("exported: got %q, want the exported value", got)
	}
	// An exported empty value counts as unset (bash: `${KUSTOMIZE_PLUGIN_HOME:-…}`).
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	if got, want := KustomizePluginHome(p), filepath.Join("/proj", ".kustomize"); got != want {
		t.Errorf("exported empty: got %q, want %q", got, want)
	}
}
