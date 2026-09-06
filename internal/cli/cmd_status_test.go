package cli

// cmd_status_test.go — the Go twin of tests/unit/status_test.bats
// (status::tilt liveness from lok8s's own pid file, lo clusters only) plus
// the section order and the inventory jq rendering, byte-exact.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func statusHarness(t *testing.T, kubectl bool, alive func(string) bool) (statusDeps, *scriptRunner, *bytes.Buffer) {
	t.Helper()
	p := synthProject(t)
	r := &scriptRunner{}
	out := &bytes.Buffer{}
	deps := statusDeps{
		paths:  p,
		runner: r,
		dispatchStatus: func(_ context.Context, out io.Writer, d string) error {
			io.WriteString(out, "Running\n")
			return nil
		},
		hasKubectl: func() bool { return kubectl },
		pidAlive:   alive,
	}
	return deps, r, out
}

// bats: "status::tilt is silent for non-lo kinds"
func TestStatusTiltSectionOnlyForLo(t *testing.T) {
	for _, kind := range []string{"KubeOne", "Capi", "Kkp", ""} {
		deps, _, out := statusHarness(t, false, nil)
		if kind != "" {
			writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "cluster.lok8s.yaml"), "kind: "+kind+"\n")
		}
		runStatus(context.Background(), out, deps, "x.dev")
		if strings.Contains(out.String(), "--- Tilt ---") {
			t.Errorf("kind %q: Tilt section printed:\n%s", kind, out.String())
		}
	}
}

// bats: "status::tilt reports not running / running (pid) / empty pidfile"
func TestStatusTiltLiveness(t *testing.T) {
	cases := []struct {
		name, pid, want string
	}{
		{"no pidfile", "", "  (not running)\n"},
		{"stale pid", "2147480000\n", "  (not running)\n"},
		{"live pid", strconv.Itoa(os.Getpid()) + "\n", "  running (pid " + strconv.Itoa(os.Getpid()) + ")\n"},
		{"empty pidfile", "", "  (not running)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, out := statusHarness(t, false, nil) // the REAL kill -0
			writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
			if tc.name != "no pidfile" {
				writeFile(t, filepath.Join(deps.paths.Base, ".tilt.pid"), tc.pid)
			}
			runStatus(context.Background(), out, deps, "x.dev")
			if !strings.HasSuffix(out.String(), "--- Tilt ---\n"+tc.want) {
				t.Errorf("tail = %q", out.String())
			}
		})
	}
}

func TestStatusSectionsByteExact(t *testing.T) {
	deps, r, out := statusHarness(t, true, func(string) bool { return true })
	writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "targets", "zitadel", "kustomization.yaml"), "")
	writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "targets", "api", "kustomization.yaml"), "")
	writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "targets", "stray-file"), "")
	writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "artifacts.yaml"), "")
	writeFile(t, filepath.Join(deps.paths.Base, ".tilt.pid"), "4242\n")
	kc := filepath.Join(deps.paths.Base, ".kubeconfig", "x.yaml")
	writeFile(t, kc, "apiVersion: v1\n")
	t.Setenv("KUBECONFIG", kc)
	r.handler = func(c execx.Cmd) error {
		switch strings.Join(c.Args, " ") {
		case "get nodes -o wide":
			io.WriteString(c.Stdout, "NAME   STATUS\nx-control-plane   Ready\n")
			return nil
		case "get clusterinventories.lok8s.dev cluster -o json":
			io.WriteString(c.Stdout, `{"spec":{"lok8sVersion":"0.1.0","kind":"lo","kubernetesVersion":"v1.31.12","specHash":"abcdef0123456789","renderedAt":"2026-09-02T10:00:00Z","addons":[{"name":"cilium","chartVersion":"1.16.0","category":"cni"},{"name":"metallb"}]},"status":{"lastReported":"2026-09-02T11:00:00Z"}}`)
			return nil
		}
		return errors.New("unexpected")
	}
	runStatus(context.Background(), out, deps, "x.dev")
	want := `=== Domain: x.dev ===

--- Cluster ---
Running

--- Nodes ---
NAME   STATUS
x-control-plane   Ready

--- Inventory (ClusterInventory/cluster) ---
  lok8s:      0.1.0
  driver:     lo
  kubernetes: v1.31.12
  specHash:   abcdef012345…
  renderedAt: 2026-09-02T10:00:00Z
  addons:     2
    cilium 1.16.0 (cni)
    metallb
  agent:      last reported 2026-09-02T11:00:00Z

--- Targets ---
  api
  zitadel
  artifacts.yaml: built

--- Tilt ---
  running (pid 4242)
`
	if out.String() != want {
		t.Errorf("status output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusNodesUnreachableAndNoTargets(t *testing.T) {
	deps, r, out := statusHarness(t, true, nil)
	writeFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "cluster.lok8s.yaml"), "kind: KubeOne\n")
	kc := filepath.Join(deps.paths.Base, ".kubeconfig", "x.yaml")
	writeFile(t, kc, "")
	t.Setenv("KUBECONFIG", kc)
	r.handler = func(c execx.Cmd) error { return errors.New("connection refused") }
	runStatus(context.Background(), out, deps, "x.dev")
	want := "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Nodes ---\n  (not reachable)\n\n--- Targets ---\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"
	if out.String() != want {
		t.Errorf("status output:\n%q\nwant:\n%q", out.String(), want)
	}
}

// Without a kubeconfig FILE the kubectl sections are skipped even when the
// tool exists (bash: `[[ -f "${KUBECONFIG}" ]]`).
func TestStatusSkipsKubectlWithoutKubeconfig(t *testing.T) {
	deps, r, out := statusHarness(t, true, nil)
	t.Setenv("KUBECONFIG", filepath.Join(deps.paths.Base, ".kubeconfig", "missing.yaml"))
	runStatus(context.Background(), out, deps, "x.dev")
	if len(r.calls) != 0 || strings.Contains(out.String(), "--- Nodes ---") {
		t.Errorf("calls=%v out=%q", r.calls, out.String())
	}
}

func TestRenderInventoryJqSemantics(t *testing.T) {
	// Missing everything: the `// "?"` fallbacks, `null | length` = 0, the
	// conditional lines absent.
	lines, ok := renderInventory(`{"spec":{}}`)
	if !ok {
		t.Fatal("expected ok")
	}
	got := strings.Join(lines, "\n")
	want := "  lok8s:      ?\n  driver:     ?\n  specHash:   ?…\n  renderedAt: ?\n  addons:     0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// provider present → appended; numbers interpolate jq-style.
	lines, _ = renderInventory(`{"spec":{"kind":"kubeone","provider":"hetzner","lok8sVersion":1.2,"specHash":"short"}}`)
	if lines[0] != "  lok8s:      1.2" || lines[1] != "  driver:     kubeone · hetzner" || lines[2] != "  specHash:   short…" {
		t.Errorf("lines = %q", lines)
	}
	// jq errors → unreadable: `.name` on a non-object addon, string + number.
	for _, bad := range []string{`not json`, `{"spec":{"addons":["x"]}}`, `{"spec":{"provider":3}}`, `{"spec":{"specHash":12}}`} {
		if _, ok := renderInventory(bad); ok {
			t.Errorf("%s: expected unreadable", bad)
		}
	}
}

func TestPidAliveRejectsGarbage(t *testing.T) {
	for _, pid := range []string{"", "abc", "-1", "0", "2147480000"} {
		if pidAlive(pid) {
			t.Errorf("%q: alive", pid)
		}
	}
	if !pidAlive(strconv.Itoa(os.Getpid())) {
		t.Error("own pid must be alive")
	}
}
