package tilt

// preflight_test.go — the GATE in tilt::preflight, mirrored from
// tests/unit/tilt_preflight_test.bats: the kill switch, the non-kind driver
// refusal, the LOK8S_FORCE_CLEAR_TERMINATING override, the flag/spec
// precedence. The sweep itself (kapply.Preflight) is stubbed to a recorder —
// these tests never touch kubectl.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sweepRecorder struct {
	calls []string
}

func (s *sweepRecorder) preflight(_ context.Context, manifest string, args ...string) error {
	s.calls = append(s.calls, strings.Join(append([]string{"kapply::preflight"}, args...), " "))
	return nil
}

func preflightCtx(t *testing.T) (*Context, *sweepRecorder, *strings.Builder) {
	t.Helper()
	c, _, _, _ := testCtx(t)
	rec := &sweepRecorder{}
	c.Preflighter = rec.preflight
	var errOut strings.Builder
	c.ErrOut = &errOut
	c.Stdin = strings.NewReader("kind: ConfigMap")

	os.MkdirAll(filepath.Join(c.Paths.Clusters, "dev.test"), 0o755)
	os.MkdirAll(filepath.Join(c.Paths.Clusters, "prod.test"), 0o755)
	os.WriteFile(filepath.Join(c.Paths.Clusters, "dev.test", "cluster.lok8s.yaml"), []byte("kind: Lo\n"), 0o644)
	os.WriteFile(filepath.Join(c.Paths.Clusters, "prod.test", "cluster.lok8s.yaml"), []byte("kind: KubeOne\n"), 0o644)
	return c, rec, &errOut
}

func TestPreflightKillSwitchZeroDisables(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	t.Setenv("LOK8S_PREFLIGHT", "0")
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("sweep ran: %v", rec.calls)
	}
}

func TestPreflightKillSwitchFalseDisablesToo(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	t.Setenv("LOK8S_PREFLIGHT", "false")
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("sweep ran: %v", rec.calls)
	}
}

func TestPreflightNonKindDriverRefusesWithoutOverride(t *testing.T) {
	c, rec, errOut := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "prod.test")
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	want := "preflight: 'prod.test' uses the 'kubeone' driver — not force-clearing stuck objects on a non-kind cluster (set LOK8S_FORCE_CLEAR_TERMINATING=1 to override)"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr = %q", errOut.String())
	}
	if len(rec.calls) != 0 {
		t.Errorf("sweep ran: %v", rec.calls)
	}
}

func TestPreflightForceClearOverridesDriverRefusal(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "prod.test")
	t.Setenv("LOK8S_FORCE_CLEAR_TERMINATING", "1")
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("sweep calls = %v", rec.calls)
	}
}

func TestPreflightLoDriverSweepsAndAgePassesThrough(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	if err := c.Preflight(context.Background(), "", "120", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || !strings.Contains(rec.calls[0], "--age 120") {
		t.Errorf("sweep calls = %v", rec.calls)
	}
}

func TestPreflightNoSpecSectionPassesNoFlags(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || strings.Contains(rec.calls[0], " --") {
		t.Errorf("sweep calls = %v (kapply owns the defaults)", rec.calls)
	}
}

func TestPreflightSpecEnabledFalseDisables(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	os.WriteFile(filepath.Join(c.Paths.Clusters, "dev.test", "cluster.lok8s.yaml"),
		[]byte("kind: Lo\nspec:\n  tilt:\n    preflight:\n      enabled: false\n"), 0o644)
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("sweep ran: %v", rec.calls)
	}
}

func TestPreflightScalarShorthandFalseDisables(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	os.WriteFile(filepath.Join(c.Paths.Clusters, "dev.test", "cluster.lok8s.yaml"),
		[]byte("kind: Lo\nspec:\n  tilt:\n    preflight: false\n"), 0o644)
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("sweep ran: %v", rec.calls)
	}
}

func TestPreflightSpecPolicyFlowsThrough(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	os.WriteFile(filepath.Join(c.Paths.Clusters, "dev.test", "cluster.lok8s.yaml"),
		[]byte(`kind: Lo
spec:
  tilt:
    preflight:
      age: 60
      crds: force
      crdForceAllow:
        - kubehzclusters.kubehz.dev
        - kubehzmigrations.kubehz.dev
`), 0o644)
	if err := c.Preflight(context.Background(), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("sweep calls = %v", rec.calls)
	}
	for _, want := range []string{"--age 60", "--crds force", "--crd-allow kubehzclusters.kubehz.dev,kubehzmigrations.kubehz.dev"} {
		if !strings.Contains(rec.calls[0], want) {
			t.Errorf("sweep call %q missing %q", rec.calls[0], want)
		}
	}
}

func TestPreflightCLIFlagOutranksSpec(t *testing.T) {
	c, rec, _ := preflightCtx(t)
	t.Setenv("DOMAIN_NAME", "dev.test")
	os.WriteFile(filepath.Join(c.Paths.Clusters, "dev.test", "cluster.lok8s.yaml"),
		[]byte("kind: Lo\nspec:\n  tilt:\n    preflight:\n      crds: skip\n"), 0o644)
	if err := c.Preflight(context.Background(), "", "", "drain", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || !strings.Contains(rec.calls[0], "--crds drain") || strings.Contains(rec.calls[0], "--crds skip") {
		t.Errorf("sweep calls = %v", rec.calls)
	}
}

func TestPreflightConfigDefaultsWhenSpecMissing(t *testing.T) {
	c, _, _, _ := testCtx(t)
	enabled, age, crds, allow := c.PreflightConfig("ghost.test")
	if enabled != "true" || age != "-" || crds != "-" || allow != "-" {
		t.Errorf("got %q %q %q %q", enabled, age, crds, allow)
	}
}
