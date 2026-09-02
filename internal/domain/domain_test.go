package domain

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	clusters := t.TempDir()
	var warn bytes.Buffer

	// Terminal default with nothing set.
	os.Unsetenv("DOMAIN_NAME")
	if got := Resolve("", clusters, &warn); got != "lok8s.dev" {
		t.Errorf("default: got %q", got)
	}

	// .active wins over the default.
	writeFile(t, filepath.Join(clusters, ".active"), "alpha.dev\n")
	if got := Resolve("", clusters, &warn); got != "alpha.dev" {
		t.Errorf(".active: got %q", got)
	}

	// Env outranks .active, with a notice when they disagree.
	t.Setenv("DOMAIN_NAME", "beta.cloud")
	warn.Reset()
	if got := Resolve("", clusters, &warn); got != "beta.cloud" {
		t.Errorf("env: got %q", got)
	}
	if !strings.Contains(warn.String(), "notice: using DOMAIN_NAME=beta.cloud") {
		t.Errorf("expected disagreement notice, got %q", warn.String())
	}

	// Explicit flag outranks everything, silently.
	warn.Reset()
	if got := Resolve("gamma.app", clusters, &warn); got != "gamma.app" {
		t.Errorf("explicit: got %q", got)
	}
	if warn.Len() != 0 {
		t.Errorf("explicit resolution must not warn: %q", warn.String())
	}
}

func TestResolveInvalidActiveWarnsAndFallsBack(t *testing.T) {
	clusters := t.TempDir()
	os.Unsetenv("DOMAIN_NAME")
	writeFile(t, filepath.Join(clusters, ".active"), "../evil\n")
	var warn bytes.Buffer
	if got := Resolve("", clusters, &warn); got != "lok8s.dev" {
		t.Errorf("got %q, want fallback", got)
	}
	if !strings.Contains(warn.String(), "invalid domain in clusters/.active") {
		t.Errorf("expected invalid warning, got %q", warn.String())
	}
}

func TestSpecDriver(t *testing.T) {
	dir := t.TempDir()

	// Lowercased happy path.
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	writeFile(t, spec, "kind: KubeOne\n")
	if got, err := SpecDriver(spec, ""); err != nil || got != "kubeone" {
		t.Errorf("got %q, %v", got, err)
	}

	// Missing key: error without fallback, fallback otherwise.
	writeFile(t, spec, "metadata: {name: x}\n")
	if _, err := SpecDriver(spec, ""); !errors.Is(err, ErrNoDriver) {
		t.Errorf("missing kind: err = %v, want ErrNoDriver", err)
	}
	if got, err := SpecDriver(spec, "?"); err != nil || got != "?" {
		t.Errorf("fallback: got %q, %v", got, err)
	}

	// Missing file behaves like a missing key.
	if _, err := SpecDriver(filepath.Join(dir, "nope.yaml"), ""); !errors.Is(err, ErrNoDriver) {
		t.Errorf("missing file: err = %v, want ErrNoDriver", err)
	}

	// Malformed values are NEVER defaulted — even with a fallback.
	writeFile(t, spec, "kind: ../lo\n")
	if _, err := SpecDriver(spec, "?"); !errors.Is(err, ErrMalformedDriver) {
		t.Errorf("malformed kind: err = %v, want ErrMalformedDriver", err)
	}
}

func TestDriver(t *testing.T) {
	clusters := t.TempDir()
	writeFile(t, filepath.Join(clusters, "a.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	writeFile(t, filepath.Join(clusters, "d.app", "deploy.lok8s.yaml"), "kind: Deploy\n")

	if got, err := Driver(clusters, "a.dev"); err != nil || got != "lo" {
		t.Errorf("cluster domain: got %q, %v", got, err)
	}
	if got, err := Driver(clusters, "d.app"); err != nil || got != "deploy" {
		t.Errorf("deploy domain: got %q, %v", got, err)
	}
	if _, err := Driver(clusters, "missing.io"); err == nil {
		t.Error("missing domain: expected error")
	}
}

func TestRequireDriverMismatchMessage(t *testing.T) {
	clusters := t.TempDir()
	writeFile(t, filepath.Join(clusters, "b.cloud", "cluster.lok8s.yaml"), "kind: KubeOne\n")

	var buf bytes.Buffer
	err := RequireDriver("lo", clusters, "b.cloud", "registry management", &buf)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "error: domain 'b.cloud' uses the 'kubeone' driver — registry management is a 'lo'-driver (local cluster) feature."
	if !strings.Contains(buf.String(), want) {
		t.Errorf("message = %q, want to contain %q", buf.String(), want)
	}
}
