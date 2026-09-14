package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

// runBare runs `lo` with no arguments (plus flags) and returns stdout,
// stderr and the error.
func runBare(t *testing.T, paths *config.Paths, tty bool, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("DOMAIN_NAME", "")
	prev := orientationIsTerminal
	orientationIsTerminal = func() bool { return tty }
	t.Cleanup(func() { orientationIsTerminal = prev })
	root := NewRoot(paths)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestBareLoPipedPrintsTheFullHelp(t *testing.T) {
	paths := completionProject(t)
	out, errOut, err := runBare(t, paths, false)
	if err != nil || errOut != "" {
		t.Fatalf("bare lo piped: err %v, stderr %q", err, errOut)
	}
	// The reference is `lo --help`: the same help text a bare `lo` printed
	// before the root had a RunE (both go through cobra's ErrHelp path).
	help, _, _ := runBare(t, paths, false, "--help")
	if out != help {
		t.Errorf("bare lo piped is not `lo --help`:\n--- got ---\n%s--- want ---\n%s", out, help)
	}
	if !strings.Contains(out, "Cluster lifecycle:") || strings.Contains(out, "Everyday commands:") {
		t.Errorf("bare lo piped: expected the grouped help, got:\n%s", out)
	}
}

func TestBareLoOnATerminalPrintsTheOrientation(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Base, "lok8s.yaml"), []byte("kind: Project\nmetadata:\n  name: acme\n"), 0o644)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)

	out, errOut, err := runBare(t, paths, true)
	if err != nil || errOut != "" {
		t.Fatalf("bare lo on a terminal: err %v, stderr %q", err, errOut)
	}
	want := "lok8s · acme\n" +
		"  domain      alpha.dev (lo)\n" +
		"\n" +
		"Everyday commands:\n" +
		"  lo up        Start cluster\n" +
		"  lo status    Cluster health and status\n" +
		"  lo build     Build kustomize targets\n" +
		"  lo deploy    Deploy platform\n" +
		"  lo lint      Validate structure and specs\n" +
		"  lo down      Stop cluster\n" +
		"\n" +
		"next: lo up\n" +
		"All commands: lo --help\n"
	if out != want {
		t.Errorf("orientation:\n--- got ---\n%s--- want ---\n%s", out, want)
	}

	// A written kubeconfig is shown and moves the next step to status.
	os.MkdirAll(filepath.Join(paths.Base, ".kubeconfig"), 0o755)
	os.WriteFile(filepath.Join(paths.Base, ".kubeconfig", "alpha.yaml"), []byte("{}"), 0o644)
	out, _, _ = runBare(t, paths, true)
	if !strings.Contains(out, "  kubeconfig  .kubeconfig/alpha.yaml\n") || !strings.Contains(out, "next: lo status\n") {
		t.Errorf("orientation with a kubeconfig:\n%s", out)
	}

	// --domain names another domain; a deploy domain shows its ref.
	out, _, _ = runBare(t, paths, true, "--domain", "gamma.app")
	if !strings.Contains(out, "  domain      gamma.app (Deploy -> ?)\n") {
		t.Errorf("orientation --domain gamma.app:\n%s", out)
	}
}

func TestBareLoOrientationWithoutADomain(t *testing.T) {
	paths := completionProject(t)
	out, _, err := runBare(t, paths, true)
	if err != nil {
		t.Fatal(err)
	}
	// No lok8s.yaml: clusters/ marks the project, the directory names it.
	if !strings.HasPrefix(out, "lok8s · "+filepath.Base(paths.Base)+"\n  domain      none (3 available: lo use)\n") {
		t.Errorf("orientation without an active domain:\n%s", out)
	}
	if !strings.Contains(out, "next: lo use <domain>\n") {
		t.Errorf("orientation without an active domain: next step:\n%s", out)
	}
}

func TestBareLoOrientationOutsideAProject(t *testing.T) {
	base := t.TempDir()
	paths := &config.Paths{Base: base, Clusters: filepath.Join(base, "clusters")}
	out, _, err := runBare(t, paths, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "lok8s: no project here (no lok8s.yaml with kind: Project, no clusters/)\n\nnext: lo init\nAll commands: lo --help\n"
	if out != want {
		t.Errorf("orientation outside a project:\n--- got ---\n%s--- want ---\n%s", out, want)
	}
}

func TestBareLoIgnoresAnInvalidImplementationBlock(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Base, "lok8s.yaml"), []byte("kind: Project\nspec:\n  implementation:\n    default: cobol\n"), 0o644)
	out, errOut, err := runBare(t, paths, false)
	if err != nil || errOut != "" || !strings.Contains(out, "Cluster lifecycle:") {
		t.Errorf("bare lo piped on an invalid block: err %v, stderr %q, out:\n%s", err, errOut, out)
	}
	out, errOut, err = runBare(t, paths, true)
	if err != nil || errOut != "" || !strings.Contains(out, "Everyday commands:") {
		t.Errorf("bare lo on a terminal on an invalid block: err %v, stderr %q, out:\n%s", err, errOut, out)
	}
	// Any other command still refuses.
	_, errOut, err = runBare(t, paths, true, "version")
	if err == nil || !strings.Contains(errOut, "lo: lok8s.yaml") {
		t.Errorf("lo version on an invalid block: err %v, stderr %q", err, errOut)
	}
}

func TestBareLoWithAnUnknownCommandIsStillAnError(t *testing.T) {
	paths := completionProject(t)
	_, _, err := runBare(t, paths, true, "nosuch")
	if err == nil || !strings.Contains(err.Error(), `unknown command "nosuch"`) {
		t.Errorf("lo nosuch: err %v", err)
	}
}
