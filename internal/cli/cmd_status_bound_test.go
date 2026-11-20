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
	"regexp"
	"slices"
	"strconv"
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

// statusHealthyReport is the text report of statusBoundCase when the probe
// passes and the two reads give the data of statusHealthyOutput.
const statusHealthyReport = "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Nodes ---\nNAME   STATUS\nx-control-plane   Ready\n\n" +
	"--- Inventory (ClusterInventory/cluster) ---\n  lok8s:      0.1.0\n  driver:     ?\n  specHash:   ?…\n  renderedAt: ?\n  addons:     0\n\n" +
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
// own. With probeOK, the /readyz probe passes at once. With late, a call
// whose context ended returns nil and the output of a healthy cluster, as
// a kubectl that handled SIGTERM and exited 0.
type statusHangRunner struct {
	probeOK  bool
	late     bool
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
		if r.late {
			io.WriteString(c.Stdout, statusHealthyOutput(line))
			return nil
		}
		return errors.New("signal: terminated")
	case <-time.After(r.fallback):
		io.WriteString(c.Stdout, statusHealthyOutput(line))
		return nil
	}
}

// checkBoundedCalls fails the test when a call has no deadline, does not
// run as its own process group with the grace of the bound, or does not
// get an empty stdin (a nil Stdin is the terminal of lo).
func checkBoundedCalls(t *testing.T, r *statusHangRunner) {
	t.Helper()
	for i, c := range r.cmds {
		if !r.bounded[i] || c.WaitDelay != statusReadGrace || !c.ProcessGroup {
			t.Errorf("%s: deadline=%v WaitDelay=%s ProcessGroup=%v, want a deadline, %s and true",
				r.calls[i], r.bounded[i], c.WaitDelay, c.ProcessGroup, statusReadGrace)
		}
		if c.Stdin == nil {
			t.Errorf("%s: no Stdin (the terminal), want an empty stdin", r.calls[i])
		} else if b, _ := io.ReadAll(c.Stdin); len(b) != 0 {
			t.Errorf("%s: stdin %q, want an empty stdin", r.calls[i], b)
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
						io.WriteString(c.Stderr, "The connection to the server 10.0.0.1:6443 was refused - did you specify the right host or port?\n")
						return errors.New("exit status 1")
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
				wantCalls, wantOut = statusReadCalls, statusHealthyReport
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

// statusProbeAnswers are the stderr of a /readyz probe that fails, as
// kubectl prints it (k8s.io/kubectl/pkg/cmd/util, StandardErrorMessage).
// answer: the form of an HTTP answer, so the API server is reachable. bats
// twin: the probe answer tests in tests/unit/status_test.bats.
var statusProbeAnswers = []struct {
	name, stderr string
	answer       bool
}{
	{"401", "error: You must be logged in to the server (Unauthorized)\n", true},
	{"403", `Error from server (Forbidden): forbidden: User "system:anonymous" cannot get path "/readyz"` + "\n", true},
	{"403 after a warning", "W1120 04:12:00.000000   12345 warnings.go:70] a warning\n" +
		`Error from server (Forbidden): forbidden: User "system:anonymous" cannot get path "/readyz"` + "\n", true},
	{"404", "Error from server (NotFound): the server could not find the requested resource\n", true},
	{"500", `Error from server (InternalError): an error on the server ("[-]etcd failed: reason withheld\nreadyz check failed") has prevented the request from succeeding` + "\n", true},
	{"connection refused", "The connection to the server 10.0.0.1:6443 was refused - did you specify the right host or port?\n", false},
	{"DNS", "Unable to connect to the server: dial tcp: lookup api.x.dev on 127.0.0.53:53: no such host\n", false},
	{"TLS", "Unable to connect to the server: tls: failed to verify certificate: x509: certificate signed by unknown authority\n", false},
	{"request timeout", "Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded while awaiting headers)\n", false},
	{"no stderr", "", false},
}

// A /readyz probe that fails with an HTTP answer (a 401, a 403 of a
// cluster that removed the system:public-info-viewer grant, a 404 of an
// older API server or a proxy, a 500 of an API server that is not ready)
// does not skip the reads: the API server answers, and the report shows
// the data, in the text report and in `-o json`. A probe with no HTTP
// answer (a connection, DNS or TLS error, the request timeout) skips them.
func TestStatusProbeHTTPAnswerRunsTheReads(t *testing.T) {
	for _, tc := range statusProbeAnswers {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(r *scriptRunner) func(execx.Cmd) error {
				return func(c execx.Cmd) error {
					line := r.calls[len(r.calls)-1]
					if line == statusProbeCall {
						io.WriteString(c.Stderr, tc.stderr)
						return errors.New("exit status 1")
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
			if tc.answer {
				wantCalls, wantOut = statusReadCalls, statusHealthyReport
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
			if got := len(rep.Nodes) == 1 && rep.Inventory != nil; got != tc.answer {
				t.Errorf("-o json: nodes=%v inventory=%v, want data only after an HTTP answer", rep.Nodes, rep.Inventory)
			}
		})
	}
}

// A call that the bound stopped is a failed call, also when kubectl handled
// SIGTERM and exited 0 (the ctx.Err() guard of statusKubectlCall). The fake
// returns nil, with the output of a healthy cluster, only after the context
// of the call ended. A probe that the bound stopped skips both reads. A
// read that the bound stopped shows its output and (not reachable), as a
// failed read does, and an inventory read that it stopped leaves out the
// Inventory section.
func TestStatusBoundStopIsAFailedCallAlsoAtExitZero(t *testing.T) {
	defer func(d time.Duration) { statusReadBound = d }(statusReadBound)
	statusReadBound = 20 * time.Millisecond
	const fallback = 5 * time.Second

	t.Run("probe", func(t *testing.T) {
		deps, out := statusBoundCase(t)
		r := &statusHangRunner{late: true, fallback: fallback}
		deps.runner = r
		runStatus(context.Background(), out, deps, "x.dev")
		if out.String() != statusUnreachableReport {
			t.Errorf("report:\n%q\nwant:\n%q", out.String(), statusUnreachableReport)
		}
		if want := []string{statusProbeCall}; !slices.Equal(r.calls, want) {
			t.Errorf("kubectl calls:\n%q\nwant:\n%q", r.calls, want)
		}
	})

	t.Run("reads, text report", func(t *testing.T) {
		deps, out := statusBoundCase(t)
		r := &statusHangRunner{probeOK: true, late: true, fallback: fallback}
		deps.runner = r
		runStatus(context.Background(), out, deps, "x.dev")
		want := "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Nodes ---\nNAME   STATUS\nx-control-plane   Ready\n  (not reachable)\n\n" +
			"--- Targets ---\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"
		if out.String() != want {
			t.Errorf("report:\n%q\nwant:\n%q", out.String(), want)
		}
		if !slices.Equal(r.calls, statusReadCalls) {
			t.Errorf("kubectl calls:\n%q\nwant:\n%q", r.calls, statusReadCalls)
		}
	})

	t.Run("reads, -o json", func(t *testing.T) {
		deps, _ := statusBoundCase(t)
		r := &statusHangRunner{probeOK: true, late: true, fallback: fallback}
		deps.runner = r
		rep := gatherStatus(context.Background(), deps, "x.dev")
		if len(rep.Nodes) != 0 || rep.Inventory != nil {
			t.Errorf("nodes=%v inventory=%v, want none (the bound stopped the reads)", rep.Nodes, rep.Inventory)
		}
		if len(r.calls) != 3 {
			t.Errorf("kubectl calls: %q, want the probe and the two reads", r.calls)
		}
	})
}

// Each kubectl call of the report gets an empty stdin, not the terminal of
// lo: the call runs as its own process group, where a credential plugin
// that reads the terminal would stop (SIGTTIN) until the bound. The real
// runner (execx.NewRunner, which gives a nil Stdin the stdin of lo) and a
// stub kubectl that logs the first line of its stdin. os.Stdin of this test
// is a file with lines in it, as a terminal with typed input. PATH holds
// only the stub.
func TestStatusKubectlCallsGetAnEmptyStdin(t *testing.T) {
	deps, out := statusBoundCase(t)
	deps.runner = execx.NewRunner(deps.paths)
	dir := t.TempDir()
	logFile := filepath.Join(dir, "kubectl.log")
	stdinFile := filepath.Join(dir, "stdin")
	testutil.WriteFile(t, stdinFile, "typed\ntyped\ntyped\n")
	f, err := os.Open(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer func(old *os.File) { os.Stdin = old }(os.Stdin)
	os.Stdin = f
	stub := filepath.Join(deps.paths.Bin, "kubectl")
	testutil.WriteFile(t, stub, "#!/bin/sh\nPATH=/usr/bin:/bin\n"+
		"if IFS= read -r line; then in=\"$line\"; else in=empty; fi\n"+
		"echo \"$1 $2 stdin=$in\" >> '"+logFile+"'\n"+
		"case \"$*\" in\n"+
		"  'get --raw /readyz '*) echo ok ;;\n"+
		"  'get nodes '*) printf 'NAME   STATUS\\nx-control-plane   Ready\\n' ;;\n"+
		"  *) exit 1 ;;\n"+
		"esac\n")
	if err := os.Chmod(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", deps.paths.Bin)
	if got, ok := execx.Look(deps.paths, "kubectl"); !ok || got != stub {
		t.Fatalf("kubectl resolves to %q (%v), want the stub %s", got, ok, stub)
	}

	runStatus(t.Context(), out, deps, "x.dev")
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "get --raw stdin=empty\nget nodes stdin=empty\nget clusterinventories.lok8s.dev stdin=empty\n"; string(raw) != want {
		t.Errorf("kubectl log:\n%s\nwant:\n%s", raw, want)
	}
	want := "=== Domain: x.dev ===\n\n--- Cluster ---\nRunning\n\n--- Nodes ---\nNAME   STATUS\nx-control-plane   Ready\n\n" +
		"--- Targets ---\n  No targets directory\n  artifacts.yaml: not built (run 'lo build')\n\n"
	if out.String() != want {
		t.Errorf("report:\n%q\nwant:\n%q", out.String(), want)
	}
}

// statusReadBoundDefault is statusReadBound before a test makes it short.
var statusReadBoundDefault = statusReadBound

// TestStatusReadBoundMatchesTheBashTwin: the bound and the grace of the
// kubectl calls of the report are the same in both twins. bash:
// status::_read_bound, and the -k option of timeout(1) in
// status::_kubectl. The Capi status read has its own pair and test
// (internal/driver/capi). The test reads the embedded mirror; .lok8s is the
// same bytes (TestEmbeddedMirrorMatchesLegacyTree).
func TestStatusReadBoundMatchesTheBashTwin(t *testing.T) {
	const file = "internal/assets/lok8s/libs/status"
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what, pattern string
		want          time.Duration
	}{
		{"status::_read_bound (statusReadBound)", `(?m)^status::_read_bound\(\) \{ echo ([0-9]+); \}$`, statusReadBoundDefault},
		{"the -k option of timeout (statusReadGrace)", `(?m)^\s+timeout -k ([0-9]+) "\$\(status::_read_bound\)" kubectl `, statusReadGrace},
	} {
		m := regexp.MustCompile(tc.pattern).FindAllSubmatch(raw, -1)
		if len(m) != 1 {
			t.Errorf("%s: %d matches for %s, want one", file, len(m), tc.what)
			continue
		}
		n, err := strconv.Atoi(string(m[0][1]))
		if err != nil {
			t.Fatal(err)
		}
		if got := time.Duration(n) * time.Second; got != tc.want {
			t.Errorf("%s: %s is %s, want %s (internal/cli/cmd_status.go)", file, tc.what, got, tc.want)
		}
	}
}
