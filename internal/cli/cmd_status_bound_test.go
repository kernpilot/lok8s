package cli

// cmd_status_bound_test.go: the /readyz probe of `lo status` and the bound
// of each kubectl call of the report (statusReadBound). bats twin: the
// probe and bound tests in tests/unit/status_test.bats.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// statusBoundCase is a KubeOne domain with kubectl and a kubeconfig file,
// so the report makes its kubectl calls.
func statusBoundCase(t *testing.T) (statusDeps, *bytes.Buffer) {
	t.Helper()
	deps, _, out := statusHarness(t, true, nil)
	testutil.WriteFile(t, filepath.Join(deps.paths.Clusters, "x.dev", "cluster.lok8s.yaml"), "kind: KubeOne\n")
	kc := filepath.Join(deps.paths.Base, ".kubeconfig", "x.yaml")
	testutil.WriteFile(t, kc, "apiVersion: v1\n")
	t.Setenv("KUBECONFIG", kc)
	return deps, out
}

// statusUnreachableReport is the text report of statusBoundCase when the
// two reads give nothing.
const statusUnreachableReport = "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Nodes ---\n  (not reachable)\n\n" +
	"--- Targets ---\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"

// statusHealthyOutput is what a healthy cluster prints for a call.
func statusHealthyOutput(line string) string {
	switch {
	case strings.Contains(line, "get nodes -o wide"):
		return "NAME   STATUS\nx-control-plane   Ready\n"
	case strings.Contains(line, "get nodes -o json"):
		return `{"items":[{"metadata":{"name":"x-control-plane"}}]}`
	case strings.Contains(line, "clusterinventories"):
		return `{"spec":{"lok8sVersion":"0.1.0"}}`
	}
	return "ok"
}

// statusHangRunner is a kubectl whose calls do not end (an API server that
// accepts the connection and does not answer, or a discovery that kubectl
// retries). Each call returns when its context ends, or after fallback with
// the output of a healthy cluster and no error, as a call that ended on its
// own. With probeOK, the /readyz probe passes at once.
type statusHangRunner struct {
	probeOK  bool
	fallback time.Duration
	calls    []string
	cmds     []execx.Cmd
	bounded  []bool // the context of the call has a deadline
}

func (r *statusHangRunner) Run(ctx context.Context, c execx.Cmd) error {
	line := strings.Join(append([]string{c.Name}, c.Args...), " ")
	r.calls = append(r.calls, line)
	r.cmds = append(r.cmds, c)
	_, ok := ctx.Deadline()
	r.bounded = append(r.bounded, ok)
	if r.probeOK && line == statusProbeCall {
		return nil
	}
	select {
	case <-ctx.Done():
		return errors.New("signal: terminated")
	case <-time.After(r.fallback):
		io.WriteString(c.Stdout, statusHealthyOutput(line))
		return nil
	}
}

// checkBoundedCalls fails the test when a call has no deadline or does not
// run as its own process group with the grace of the bound.
func checkBoundedCalls(t *testing.T, r *statusHangRunner) {
	t.Helper()
	for i, c := range r.cmds {
		if !r.bounded[i] || c.WaitDelay != statusReadGrace || !c.ProcessGroup {
			t.Errorf("%s: deadline=%v WaitDelay=%s ProcessGroup=%v, want a deadline, %s and true",
				r.calls[i], r.bounded[i], c.WaitDelay, c.ProcessGroup, statusReadGrace)
		}
	}
}

// A failed /readyz probe skips both reads, in the text report and in
// `-o json`: kubectl gets no other call. The control (the probe passes)
// shows that the same runner gives the data of a healthy cluster.
func TestStatusFailedProbeSkipsBothReads(t *testing.T) {
	for _, probe := range []bool{false, true} {
		name := "probe fails"
		if probe {
			name = "control: probe passes"
		}
		t.Run(name, func(t *testing.T) {
			handler := func(r *scriptRunner) func(execx.Cmd) error {
				return func(c execx.Cmd) error {
					line := r.calls[len(r.calls)-1]
					if line == statusProbeCall && !probe {
						return errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")
					}
					io.WriteString(c.Stdout, statusHealthyOutput(line))
					return nil
				}
			}

			deps, out := statusBoundCase(t)
			r := deps.runner.(*scriptRunner)
			r.handler = handler(r)
			runStatus(t.Context(), out, deps, "x.dev")
			wantCalls, wantOut := []string{statusProbeCall}, statusUnreachableReport
			if probe {
				wantCalls = statusReadCalls
				wantOut = "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Nodes ---\nNAME   STATUS\nx-control-plane   Ready\n\n" +
					"--- Inventory (ClusterInventory/cluster) ---\n  lok8s:      0.1.0\n  driver:     ?\n  specHash:   ?…\n  renderedAt: ?\n  addons:     0\n\n" +
					"--- Targets ---\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"
			}
			if !slices.Equal(r.calls, wantCalls) {
				t.Errorf("text report: kubectl calls:\n%q\nwant:\n%q", r.calls, wantCalls)
			}
			if out.String() != wantOut {
				t.Errorf("text report:\n%q\nwant:\n%q", out.String(), wantOut)
			}

			deps, _ = statusBoundCase(t)
			r = deps.runner.(*scriptRunner)
			r.handler = handler(r)
			rep := gatherStatus(t.Context(), deps, "x.dev")
			wantCalls = []string{statusProbeCall}
			if probe {
				wantCalls = []string{statusProbeCall, "kubectl get nodes -o json --request-timeout=5s", "kubectl get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s"}
			}
			if !slices.Equal(r.calls, wantCalls) {
				t.Errorf("-o json: kubectl calls:\n%q\nwant:\n%q", r.calls, wantCalls)
			}
			if got := len(rep.Nodes) == 1 && rep.Inventory != nil; got != probe {
				t.Errorf("-o json: nodes=%v inventory=%v, want data only when the probe passes", rep.Nodes, rep.Inventory)
			}
		})
	}
}

// A read that does not end stops at the bound (statusReadBound), in the
// text report and in `-o json`. The parent context never ends: only the
// bound can stop the calls. The Nodes section then shows (not reachable),
// and the Inventory section is not there.
func TestStatusReadBoundStopsAHungRead(t *testing.T) {
	defer func(d time.Duration) { statusReadBound = d }(statusReadBound)
	statusReadBound = 20 * time.Millisecond
	const fallback = 5 * time.Second

	t.Run("text report", func(t *testing.T) {
		deps, out := statusBoundCase(t)
		r := &statusHangRunner{probeOK: true, fallback: fallback}
		deps.runner = r
		start := time.Now()
		runStatus(context.Background(), out, deps, "x.dev")
		if took := time.Since(start); took >= fallback {
			t.Errorf("the report took %s: the bound did not stop the reads", took)
		}
		if out.String() != statusUnreachableReport {
			t.Errorf("report:\n%q\nwant:\n%q", out.String(), statusUnreachableReport)
		}
		if !slices.Equal(r.calls, statusReadCalls) {
			t.Errorf("kubectl calls:\n%q\nwant:\n%q", r.calls, statusReadCalls)
		}
		checkBoundedCalls(t, r)
	})

	t.Run("-o json", func(t *testing.T) {
		deps, _ := statusBoundCase(t)
		r := &statusHangRunner{probeOK: true, fallback: fallback}
		deps.runner = r
		start := time.Now()
		rep := gatherStatus(context.Background(), deps, "x.dev")
		if took := time.Since(start); took >= fallback {
			t.Errorf("the report took %s: the bound did not stop the reads", took)
		}
		if len(rep.Nodes) != 0 || rep.Inventory != nil {
			t.Errorf("nodes=%v inventory=%v, want none (the reads did not end)", rep.Nodes, rep.Inventory)
		}
		if len(r.calls) != 3 {
			t.Errorf("kubectl calls: %q, want the probe and the two reads", r.calls)
		}
		checkBoundedCalls(t, r)
	})
}

// A /readyz probe that does not end (for example, an exec credential
// plugin that waits for a login) stops at the same bound, and the report
// skips both reads.
func TestStatusReadBoundStopsAHungProbe(t *testing.T) {
	defer func(d time.Duration) { statusReadBound = d }(statusReadBound)
	statusReadBound = 20 * time.Millisecond
	const fallback = 5 * time.Second
	deps, out := statusBoundCase(t)
	r := &statusHangRunner{fallback: fallback}
	deps.runner = r
	start := time.Now()
	runStatus(context.Background(), out, deps, "x.dev")
	if took := time.Since(start); took >= fallback {
		t.Errorf("the report took %s: the bound did not stop the probe", took)
	}
	if out.String() != statusUnreachableReport {
		t.Errorf("report:\n%q\nwant:\n%q", out.String(), statusUnreachableReport)
	}
	if want := []string{statusProbeCall}; !slices.Equal(r.calls, want) {
		t.Errorf("kubectl calls:\n%q\nwant:\n%q", r.calls, want)
	}
	checkBoundedCalls(t, r)
}

// The bound with the real runner (execx.NewRunner) and a real process: a
// kubectl that passes the /readyz probe and then does not end on a read
// (it sleeps, as a discovery that it retries). PATH holds only the stub, so
// no other kubectl can run. The report ends after the two bounds, long
// before the sleep ends, and the stub logs the probe and the two reads.
func TestStatusReadBoundStopsARealKubectl(t *testing.T) {
	defer func(d time.Duration) { statusReadBound = d }(statusReadBound)
	statusReadBound = 300 * time.Millisecond
	deps, out := statusBoundCase(t)
	deps.runner = execx.NewRunner(deps.paths)
	logFile := filepath.Join(t.TempDir(), "kubectl.log")
	stub := filepath.Join(deps.paths.Bin, "kubectl")
	testutil.WriteFile(t, stub, "#!/bin/sh\nPATH=/usr/bin:/bin\n"+
		"echo \"$*\" >> '"+logFile+"'\n"+
		"case \"$*\" in 'get --raw /readyz '*) echo ok; exit 0 ;; esac\n"+
		"exec sleep 10\n")
	if err := os.Chmod(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", deps.paths.Bin)
	if got, ok := execx.Look(deps.paths, "kubectl"); !ok || got != stub {
		t.Fatalf("kubectl resolves to %q (%v), want the stub %s", got, ok, stub)
	}

	start := time.Now()
	runStatus(t.Context(), out, deps, "x.dev")
	took := time.Since(start)
	if limit := 2*(statusReadBound+statusReadGrace) + time.Second; took >= limit {
		t.Errorf("the report took %s, want less than %s", took, limit)
	}
	if out.String() != statusUnreachableReport {
		t.Errorf("report:\n%q\nwant:\n%q", out.String(), statusUnreachableReport)
	}
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "get --raw /readyz --request-timeout=5s\n" +
		"get nodes -o wide --request-timeout=5s\n" +
		"get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s\n"
	if string(raw) != want {
		t.Errorf("kubectl log:\n%s\nwant:\n%s", raw, want)
	}
}
