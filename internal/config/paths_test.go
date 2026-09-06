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
