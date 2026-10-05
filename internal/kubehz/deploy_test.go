package kubehz

// deploy_test.go ports the render / dry-run / apply-order / wait cases of
// tests/unit/kubehz_live_agent_test.bats. kubectl is the fake Runner; the
// rendered manifests are compared against goldens generated ONCE from the
// bash kubehz::render_agent (testdata/golden/rendered-*.yaml).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func renderInto(t *testing.T, h *harness, owner, access string) string {
	t.Helper()
	work := filepath.Join(h.base, "render-"+owner+"-"+access)
	_ = os.MkdirAll(work, 0o755)
	mustOK(t, h.ctx.RenderAgent(work, "acme.example.com", "https://api.kubehz.cloud", owner, access), h.output())
	return work
}

func TestRenderAgentSubstitutesBothTrees(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	var leftovers []string
	_ = filepath.WalkDir(work, func(path string, d os.DirEntry, err error) error {
		if !d.IsDir() && strings.Contains(readFile(t, path), "PLACEHOLDER") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) > 0 {
		t.Fatalf("placeholders survived: %v", leftovers)
	}
	agentCM := readFile(t, filepath.Join(work, "agent", "configmap.yaml"))
	liveCM := readFile(t, filepath.Join(work, "live-agent", "base", "configmap.yaml"))
	mustContain(t, agentCM, `CLUSTER_ID: "acme.example.com"`)
	mustContain(t, liveCM, `CLUSTER_ID: "acme.example.com"`)
	mustContain(t, agentCM, `KUBEHZ_API_URL: "https://api.kubehz.cloud"`)
	mustContain(t, liveCM, `KUBEHZ_API_URL: "https://api.kubehz.cloud"`)
	mustContain(t, agentCM, `KUBEHZ_HEARTBEAT_OWNER: "operator"`)
}

func TestRenderAgentMatchesBashGoldens(t *testing.T) {
	t.Parallel()
	// The golden was rendered by the bash with an apiUrl carrying `&` — the
	// value sed would have expanded — so this pins both the byte-for-byte
	// render and the verbatim ampersand.
	h := newHarness(t)
	work := filepath.Join(h.base, "golden")
	_ = os.MkdirAll(work, 0o755)
	url := "https://api.example.com/heartbeat?cluster=a&mode=push"
	mustOK(t, h.ctx.RenderAgent(work, "acme.example.com", url, "operator", "managed"), h.output())
	for got, name := range map[string]string{
		filepath.Join(work, "agent", "configmap.yaml"):              "rendered-agent-configmap.yaml",
		filepath.Join(work, "live-agent", "base", "configmap.yaml"): "rendered-live-agent-configmap.yaml",
	} {
		testutil.Golden(t, filepath.Join("testdata", "golden", name), readFile(t, got), *update)
	}
	mustContain(t, readFile(t, filepath.Join(work, "agent", "configmap.yaml")), url)
}

func TestLiveAgentOverlay(t *testing.T) {
	t.Parallel()
	work := "/w"
	if liveAgentOverlay(work, "registered") != "/w/live-agent/base" || liveAgentOverlay(work, "managed") != "/w/live-agent/managed" {
		t.Fatal("overlay")
	}
	// Anything that is not 'managed' is read-only.
	if liveAgentOverlay(work, "") != "/w/live-agent/base" || liveAgentOverlay(work, "Managed") != "/w/live-agent/base" {
		t.Fatal("unknown tier fell through to the acting overlay")
	}
}

func TestRenderAgentRefusesSurvivingPlaceholder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	work := filepath.Join(h.base, "r3")
	_ = os.MkdirAll(work, 0o755)
	// A value that re-introduces the token is the same observable state as a
	// renamed placeholder in a manifest.
	mustErr(t, h.ctx.RenderAgent(work, "CLUSTER_ID_PLACEHOLDER", "https://api.kubehz.cloud", "operator", "managed"))
	mustContain(t, h.output(), "still carry a placeholder")
	mustContain(t, h.output(), "  agent/configmap.yaml")
}

func kubectlLogger(h *harness, override func(c execx.Cmd) (bool, error)) *[]string {
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if override != nil {
			if handled, err := override(c); handled {
				return err
			}
		}
		if strings.Contains(argvLine(c), "get pods") {
			return nil // idle cluster
		}
		log = append(log, argvLine(c))
		return nil
	}
	return &log
}

func TestDeployPrint(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name != "kubectl" || c.Args[0] != "kustomize" {
			t.Fatalf("dry run must render with kubectl kustomize, got %s", argvLine(c))
		}
		io.WriteString(c.Stdout, "kind: Rendered\ndir: "+c.Args[1]+"\n")
		return nil
	}
	mustOK(t, h.ctx.deployPrint(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustContain(t, h.output(), "# --- CronJob agent (kubehz-heartbeat) — identity + enrollment; heartbeat owner: operator ---")
	mustNotContain(t, h.output(), "kubehz-agent-bind")
	mustContain(t, h.output(), "# --- Live agent (kubehz-live-agent) — managed tier RBAC ---")
	mustContain(t, h.output(), "dir: "+filepath.Join(work, "live-agent", "managed"))

	h.reset()
	mustOK(t, h.ctx.deployPrint(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
	mustContain(t, h.output(), "# The live agent is NOT deployed in cronjob mode; a previous install would be removed.")
	mustNotContain(t, h.output(), "live-agent")
}

// F5: with a bind secret on disk, the dry run says first that the Secret goes
// in first, and never prints the value.
func TestDeployPrintNamesTheBindSecretFirst(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "cronjob", "registered")
	writeBindSecret(t, h, "acme.example.com", testBindSecret)
	h.runner.handler = func(c execx.Cmd, _ string) error {
		io.WriteString(c.Stdout, "kind: Rendered\n")
		return nil
	}
	mustOK(t, h.ctx.deployPrint(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
	first, _, _ := strings.Cut(h.output(), "\n")
	if first != "# --- Secret kubehz-agent-bind (the bind secret from clusters/acme.example.com/.kubehz-bind) — applied first; the value is not printed ---" {
		t.Fatalf("first dry-run line = %q", first)
	}
	mustNotContain(t, h.output(), "9f1c2b3a4d5e6f70")
	for _, l := range h.runner.lines() {
		if !strings.HasPrefix(l, "kubectl kustomize ") {
			t.Fatalf("dry run must apply nothing: %s", l)
		}
	}
}

func TestDeployApplyToOperatorOrder(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	if len(*log) != 4 {
		t.Fatalf("calls: %v", *log)
	}
	mustContain(t, (*log)[0], "apply -k "+filepath.Join(work, "agent"))
	// The identity Secret is read between the two applies (B244): present
	// here, so no bootstrap job runs.
	mustContain(t, (*log)[1], "get secret kubehz-agent")
	mustContain(t, (*log)[2], "apply -k "+filepath.Join(work, "live-agent", "managed"))
	mustContain(t, (*log)[3], "rollout status deployment/kubehz-live-agent")
	mustContain(t, (*log)[3], "--timeout=120s")
	mustNotContain(t, strings.Join(*log, "\n"), "create job")
}

// writeBindSecret installs clusters/<domain>/.kubehz-bind, as a register would.
func writeBindSecret(t *testing.T, h *harness, domain, secret string) string {
	t.Helper()
	dir := filepath.Join(h.ctx.Paths.Clusters, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".kubehz-bind")
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testBindSecret = "9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"

// bindManifest is what the fake `kubectl create secret … --dry-run=client -o
// yaml` prints, so a test can follow it to the stdin of the apply.
const bindManifest = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: kubehz-agent-bind\n  namespace: kubehz-system\ndata:\n  bind-secret: OWYxYw==\n"

// bindStageLogger is kubectlLogger with the dry-run create answering
// bindManifest; fail names one argv substring that exits 1.
func bindStageLogger(h *harness, fail string) *[]string {
	return kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		line := argvLine(c)
		if fail != "" && strings.Contains(line, fail) {
			return true, exitErr(1)
		}
		if strings.Contains(line, "create secret generic kubehz-agent-bind") {
			io.WriteString(c.Stdout, bindManifest)
		}
		return false, nil
	})
}

// indexOf is the first log line that contains substr, or -1.
func indexOf(log []string, substr string) int {
	for i, l := range log {
		if strings.Contains(l, substr) {
			return i
		}
	}
	return -1
}

// assertBindStagedFirst checks the three staging calls and that they come
// before the first call that changes the agent, in their order. The value
// goes in through --from-file and stdin, never argv.
func assertBindStagedFirst(t *testing.T, h *harness, log []string, work, path, firstChange string) {
	t.Helper()
	ns := indexOf(log, "apply -f "+filepath.Join(work, "agent", "namespace.yaml"))
	create := indexOf(log, "-n kubehz-system create secret generic kubehz-agent-bind --from-file=bind-secret="+path+" --dry-run=client -o yaml")
	ssa := indexOf(log, "apply --server-side --force-conflicts -f -")
	change := indexOf(log, firstChange)
	if ns != 0 || create != 1 || ssa != 2 || change != 3 {
		t.Fatalf("want namespace(0) < dry-run create(1) < server-side apply(2) < %q(3), got %d %d %d %d: %v", firstChange, ns, create, ssa, change, log)
	}
	joined := strings.Join(log, "\n")
	mustNotContain(t, joined, "--from-literal")
	mustNotContain(t, joined, "9f1c2b3a4d5e6f70")
	mustNotContain(t, joined, "delete secret kubehz-agent-bind")
	// The apply reads what the dry run printed, on stdin.
	got := ""
	for i, c := range h.runner.calls {
		if strings.Contains(argvLine(c), "apply --server-side") {
			got = h.runner.stdins[i]
		}
	}
	if got != bindManifest {
		t.Fatalf("server-side apply stdin = %q, want the dry-run output", got)
	}
}

// B243 + B288: with a bind secret on disk, the operator deploy stages it
// before the CronJob agent apply, so neither a CronJob tick nor the identity
// bootstrap Job can register without it.
func TestDeployApplyStagesTheBindSecretOperator(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	p := writeBindSecret(t, h, "acme.example.com", testBindSecret)
	log := bindStageLogger(h, "")
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	assertBindStagedFirst(t, h, *log, work, p, "apply -k "+filepath.Join(work, "agent"))
	if idRead := indexOf(*log, "get secret kubehz-agent "); idRead != 4 {
		t.Fatalf("the identity read must follow the CronJob apply: %v", *log)
	}
	mustNotContain(t, h.output(), "no bind secret")
}

// B243 + B288: cronjob mode stages it before anything else too: before the
// live agent goes and before the CronJob agent apply.
func TestDeployApplyStagesTheBindSecretCronjob(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "cronjob", "registered")
	p := writeBindSecret(t, h, "acme.example.com", testBindSecret)
	log := bindStageLogger(h, "")
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
	assertBindStagedFirst(t, h, *log, work, p, "delete -k "+filepath.Join(work, "live-agent", "managed"))
	if agent := indexOf(*log, "apply -k "+filepath.Join(work, "agent")); agent != len(*log)-1 {
		t.Fatalf("the CronJob agent apply must come last: %v", *log)
	}
}

// B288: a failed stage with a bind secret on disk stops the deploy before the
// CronJob agent, in both directions and at each of the three steps. A CronJob
// without the Secret registers a separate pending row.
func TestDeployApplyFailedBindStageStopsBeforeTheCronJob(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		for _, step := range []string{"namespace.yaml", "--dry-run=client", "apply --server-side"} {
			t.Run(mode.owner+"/"+step, func(t *testing.T) {
				h := newHarness(t)
				work := renderInto(t, h, mode.owner, mode.access)
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
				log := bindStageLogger(h, step)
				mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access))
				mustContain(t, h.output(), "could not stage the bind secret from clusters/acme.example.com/.kubehz-bind")
				mustContain(t, h.output(), "The deploy stopped before the CronJob agent.")
				mustContain(t, h.output(), "registers a separate pending row, and the announced row gets no heartbeats.")
				mustContain(t, h.output(), "The kubeconfig needs patch on Secrets in kubehz-system, and create when the Secret is new.")
				mustContain(t, h.output(), "To deploy without the secret, remove the file")
				mustNotContain(t, h.output(), "pending-pool")
				joined := strings.Join(*log, "\n")
				mustNotContain(t, joined, "apply -k")
				mustNotContain(t, joined, "delete -k")
				mustNotContain(t, joined, "create job")
			})
		}
	}
}

// No bind secret on disk: the deploy touches kubehz-agent-bind not at all,
// keeps today's order, and warns what a register in another checkout means.
func TestDeployApplyNoBindSecretWarnsAndContinues(t *testing.T) {
	for _, mode := range []struct{ owner, access, first string }{
		{"operator", "managed", "apply -k "}, {"cronjob", "registered", "delete -k "},
	} {
		h := newHarness(t)
		work := renderInto(t, h, mode.owner, mode.access)
		log := kubectlLogger(h, nil)
		mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access), h.output())
		joined := strings.Join(*log, "\n")
		mustNotContain(t, joined, "kubehz-agent-bind")
		mustNotContain(t, joined, "namespace.yaml")
		mustContain(t, (*log)[0], mode.first)
		mustContain(t, h.output(), "[warn] kubehz: no bind secret at clusters/acme.example.com/.kubehz-bind.")
		mustContain(t, h.output(), "the agent registers a separate pending row.")
		mustContain(t, h.output(), "The announced row stays as it is and gets no heartbeats.")
		mustContain(t, h.output(), "Claim the new row with the code from 'lo kubehz claim-code'.")
	}
}

// F6: a file that is not a bind secret (the register check: a readable
// regular file of exactly 64 lowercase hex) is never staged. The deploy warns
// that the file holds no bind secret and keeps today's order.
func TestDeployApplyMalformedBindSecretIsNotStaged(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, path string){
		"a directory":          func(t *testing.T, p string) { mustMkdir(t, p) },
		"64 hex and a newline": func(t *testing.T, p string) { mustWrite(t, p, testBindSecret+"\n", 0o600) },
		"upper-case hex":       func(t *testing.T, p string) { mustWrite(t, p, strings.ToUpper(testBindSecret), 0o600) },
		"63 hex":               func(t *testing.T, p string) { mustWrite(t, p, testBindSecret[1:], 0o600) },
		"unreadable": func(t *testing.T, p string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a 0000 file")
			}
			mustWrite(t, p, testBindSecret, 0o000)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			work := renderInto(t, h, "cronjob", "registered")
			path := filepath.Join(h.ctx.Paths.Clusters, "acme.example.com", ".kubehz-bind")
			mustMkdir(t, filepath.Dir(path))
			setup(t, path)
			log := kubectlLogger(h, nil)
			mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
			mustNotContain(t, strings.Join(*log, "\n"), "kubehz-agent-bind")
			mustContain(t, (*log)[0], "delete -k ")
			mustContain(t, h.output(), "[warn] kubehz: clusters/acme.example.com/.kubehz-bind holds no bind secret (a readable file of exactly 64 lowercase hex characters).")
			mustContain(t, h.output(), "The announced row stays as it is and gets no heartbeats.")
		})
	}
}

// F2: a failure after the stage names what the deploy changed so far: the
// bind Secret when one was staged, nothing otherwise.
func TestDeployApplyFailureNamesWhatChanged(t *testing.T) {
	for _, tc := range []struct{ owner, access, fail, prefix string }{
		{"operator", "managed", "apply -k ", "could not apply the CronJob agent (identity bootstrap) — "},
		{"cronjob", "registered", "delete -k ", "could not remove the live agent — "},
		{"cronjob", "registered", "delete deployment -l", "could not sweep live-agent Deployments by label — "},
	} {
		for _, staged := range []bool{true, false} {
			h := newHarness(t)
			work := renderInto(t, h, tc.owner, tc.access)
			if staged {
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
			}
			bindStageLogger(h, tc.fail)
			mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", tc.owner, tc.access))
			want := tc.prefix + "nothing else was changed"
			if staged {
				want = tc.prefix + "only the bind Secret and its namespace changed"
			}
			mustContain(t, h.output(), want)
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask; set the mode the case asks for.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// B244: a first deploy finds no identity Secret. The deploy runs the
// CronJob's bootstrap once, waits for its Job to complete, removes the one-off job,
// and only then applies the live agent.
func TestDeployApplyBootstrapsTheIdentitySecret(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	// Absent before the bootstrap, present once the one-off Job completed.
	completed := false
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "wait --for=condition=complete") {
			completed = true
			return false, nil
		}
		if strings.Contains(argvLine(c), "get secret kubehz-agent") && !completed {
			io.WriteString(c.Stderr, "Error from server (NotFound): secrets \"kubehz-agent\" not found\n")
			return true, exitErr(1)
		}
		return false, nil
	})
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustContain(t, h.output(), "running the CronJob's bootstrap once")
	joined := strings.Join(*log, "\n")
	mustContain(t, joined, "create job kubehz-heartbeat-bootstrap-")
	mustContain(t, joined, "--from=cronjob/kubehz-heartbeat")
	mustContain(t, joined, "wait --for=condition=complete job/kubehz-heartbeat-bootstrap-")
	mustContain(t, joined, "--timeout=150s")
	// The wait and the delete name the Job the create made, not another.
	name := regexp.MustCompile(`create job (kubehz-heartbeat-bootstrap-\d+-[0-9a-f]{4}) `).FindStringSubmatch(joined)
	if name == nil {
		t.Fatalf("no create job with the <unix>-<hex4> name in %v", *log)
	}
	mustContain(t, joined, "wait --for=condition=complete job/"+name[1]+" ")
	mustContain(t, joined, "delete job "+name[1]+" ")
	if !strings.Contains((*log)[0], "apply -k "+filepath.Join(work, "agent")) {
		t.Fatalf("the CronJob agent must be applied first: %v", *log)
	}
	// create, wait, delete, then the live agent — in that order.
	live := strings.Index(joined, "apply -k "+filepath.Join(work, "live-agent", "managed"))
	create := strings.Index(joined, "create job")
	wait := strings.Index(joined, "wait --for=condition=complete")
	del := strings.Index(joined, "delete job kubehz-heartbeat-bootstrap-")
	if create < 0 || wait < 0 || del < 0 || live < 0 || create >= wait || wait >= del || del >= live {
		t.Fatalf("expected create < wait < delete < live apply: %v", *log)
	}
}

func TestDeployApplyIdentitySecretNeverAppearsFails(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	h.env["KUBEHZ_IDENTITY_BOOTSTRAP_SECONDS"] = "10"
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent") {
			io.WriteString(c.Stderr, "Error from server (NotFound): secrets \"kubehz-agent\" not found\n")
			return true, exitErr(1)
		}
		if strings.Contains(argvLine(c), "wait --for=condition=complete") {
			io.WriteString(c.Stderr, "error: timed out waiting for the condition on jobs/kubehz-heartbeat-bootstrap\n")
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "did not complete within 10s")
	mustContain(t, h.output(), "describe job kubehz-heartbeat-bootstrap-")
	joined := strings.Join(*log, "\n")
	mustNotContain(t, joined, "apply -k "+filepath.Join(work, "live-agent", "managed"))
	// The Job stays for the operator to read; the deploy does not delete it.
	mustNotContain(t, joined, "delete job")
}

// A Secret read that fails for any reason but NotFound (RBAC, an unreachable
// apiserver) is not "absent": the deploy stops, starts no bootstrap and applies
// no live agent.
func TestDeployApplyIdentitySecretUnreadableStops(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent") {
			io.WriteString(c.Stderr, "Error from server (Forbidden): secrets \"kubehz-agent\" is forbidden\n")
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "could not read Secret kubehz-agent")
	mustContain(t, h.output(), "Forbidden")
	mustContain(t, h.output(), "reporting NOTHING")
	joined := strings.Join(*log, "\n")
	mustNotContain(t, joined, "create job")
	mustNotContain(t, joined, "apply -k "+filepath.Join(work, "live-agent", "managed"))
}

// A probe that exits without output (a kubectl that could not start) names
// the exec error, not an empty string.
func TestDeployApplyIdentitySecretProbeWithoutOutputNamesTheError(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent") {
			return true, exitErr(127)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "could not read Secret kubehz-agent in kubehz-system: kubectl exited without output (exit status 127)")
}

// A warning line before the NotFound (a kubeconfig deprecation, say) still
// reads as "absent": the bootstrap runs.
func TestDeployApplyIdentitySecretNotFoundBehindAWarningBootstraps(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	completed := false
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "wait --for=condition=complete") {
			completed = true
			return false, nil
		}
		if strings.Contains(argvLine(c), "get secret kubehz-agent") && !completed {
			io.WriteString(c.Stderr, "Warning: the kubeconfig field exec.apiVersion v1alpha1 is deprecated\nError from server (NotFound): secrets \"kubehz-agent\" not found\n")
			return true, exitErr(1)
		}
		return false, nil
	})
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustContain(t, strings.Join(*log, "\n"), "create job kubehz-heartbeat-bootstrap-")
}

func TestDeployApplyNeverReadyFails(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "rollout status") {
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "never became Ready")
	mustContain(t, h.output(), "NOTHING owns the heartbeat")
}

func TestDeployApplyRolloutTimeoutFromEnv(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_LIVE_AGENT_ROLLOUT_SECONDS"] = "600"
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustContain(t, strings.Join(*log, "\n"), "--timeout=600s")
}

func TestDeployApplyToCronjobOrder(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "cronjob", "registered")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
	if len(*log) != 3 {
		t.Fatalf("calls: %v", *log)
	}
	mustContain(t, (*log)[0], "delete -k "+filepath.Join(work, "live-agent", "managed")+" --ignore-not-found=true")
	mustContain(t, (*log)[1], "delete deployment -l app.kubernetes.io/part-of=kubehz,app.kubernetes.io/component=live-view")
	mustContain(t, (*log)[2], "apply -k "+filepath.Join(work, "agent"))
	mustNotContain(t, strings.Join(*log, "\n"), "apply -k "+filepath.Join(work, "live-agent"))
}

func TestDeployApplyFailedDeleteNeverRearms(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "cronjob", "registered")
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "delete -k") {
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"))
	mustContain(t, h.output(), "could not remove the live agent")
	mustNotContain(t, strings.Join(*log, "\n"), "apply -k "+filepath.Join(work, "agent"))
}

func TestDeployApplyPodWontTerminateBlocks(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_LIVE_AGENT_DRAIN_SECONDS"] = "10"
	work := renderInto(t, h, "cronjob", "registered")
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "get pods") {
			io.WriteString(c.Stdout, "pod/kubehz-live-agent-abc123\n")
			return nil
		}
		log = append(log, argvLine(c))
		return nil
	}
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"))
	mustContain(t, h.output(), "still running")
	mustNotContain(t, strings.Join(log, "\n"), "apply -k "+filepath.Join(work, "agent"))
}

func TestDeployApplyBlindProbeNeverRearms(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_LIVE_AGENT_DRAIN_SECONDS"] = "10"
	work := renderInto(t, h, "cronjob", "registered")
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "get pods") {
			io.WriteString(c.Stderr, `Error from server (Forbidden): pods is forbidden: User "deployer" cannot list resource "pods" in API group "" in the namespace "kubehz-system"`+"\n")
			return exitErr(1)
		}
		log = append(log, argvLine(c))
		return nil
	}
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"))
	mustContain(t, h.output(), "could not tell whether")
	mustContain(t, h.output(), "Forbidden")
	mustNotContain(t, strings.Join(log, "\n"), "apply -k "+filepath.Join(work, "agent"))
}

func TestDeployApplyUnreadableHeartbeatProbeWarnsAndContinues(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "get pods") {
			io.WriteString(c.Stderr, "Error from server (Forbidden): pods is forbidden\n")
			return exitErr(1)
		}
		log = append(log, argvLine(c))
		return nil
	}
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustContain(t, h.output(), "could not check for an in-flight heartbeat pod")
	mustContain(t, log[1], "get secret kubehz-agent")
	mustContain(t, log[2], "apply -k "+filepath.Join(work, "live-agent", "managed"))
}

func TestWaitsIgnoreApiserverWarnings(t *testing.T) {
	for _, warning := range []string{
		"Warning: v1 ComponentStatus is deprecated in v1.19+",
		`Warning: metadata.finalizers: "foregroundDeletion": prefer a domain-qualified finalizer name`,
	} {
		h := newHarness(t)
		slept := 0
		h.ctx.Sleep = func(context.Context, time.Duration) error { slept++; return nil }
		h.runner.handler = func(c execx.Cmd, _ string) error {
			io.WriteString(c.Stderr, warning+"\n")
			return nil
		}
		h.ctx.waitHeartbeatIdle(t.Context())
		if h.output() != "" || slept != 0 {
			t.Fatalf("heartbeat wait tripped on a warning: %q slept=%d", h.output(), slept)
		}
		mustOK(t, h.ctx.waitLiveAgentGone(t.Context()), h.output())
		if h.output() != "" || slept != 0 {
			t.Fatalf("live-agent wait tripped on a warning: %q slept=%d", h.output(), slept)
		}
	}
}

func TestDeployAgentRefusals(t *testing.T) {
	h := newHarness(t)
	mustErr(t, h.ctx.DeployAgent(t.Context(), &Config{Hosting: "shared", Access: "none", APIURL: "https://api.kubehz.cloud", Agent: "cronjob"}, "acme.example.com", false))
	mustContain(t, h.output(), "no in-cluster agent to deploy")
	h.reset()
	mustErr(t, h.ctx.DeployAgent(t.Context(), &Config{Hosting: "self", Access: "none", APIURL: "https://api.kubehz.cloud", Agent: "cronjob"}, "acme.example.com", false))
	mustContain(t, h.output(), "access is 'none'")
	h.reset()
	mustErr(t, h.ctx.DeployAgent(t.Context(), &Config{Hosting: "self", Access: "registered", APIURL: "http://api.kubehz.cloud", Agent: "cronjob"}, "acme.example.com", false))
	mustContain(t, h.output(), "must use HTTPS")
	for _, bad := range []string{"Operator", "cronjob\"extra", "operator\nbeats"} {
		h.reset()
		mustErr(t, h.ctx.DeployAgent(t.Context(), &Config{Hosting: "self", Access: "registered", APIURL: "https://api.kubehz.cloud", Agent: bad}, "acme.example.com", false))
		mustContain(t, h.output(), "must be 'cronjob' or 'operator'")
	}
	if len(h.runner.calls) != 0 {
		t.Fatal("nothing may reach kubectl")
	}
}

func TestDeploySummary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ctx.deploySummary("acme.example.com", "operator", "managed")
	mustContain(t, h.output(), "live agent deployed and Ready for acme.example.com (deployment/kubehz-live-agent, managed tier).")
	mustContain(t, h.output(), "Acting RBAC is applied")
	h.reset()
	h.ctx.deploySummary("acme.example.com", "operator", "registered")
	mustContain(t, h.output(), "Acting is NOT enabled: access is 'registered'")
	h.reset()
	h.ctx.deploySummary("acme.example.com", "cronjob", "registered")
	mustContain(t, h.output(), "heartbeat CronJob deployed for acme.example.com (cronjob/kubehz-heartbeat).")
	mustContain(t, h.output(), "Claim this cluster (once): lo kubehz claim-code")
}

func TestDeploySubcommandDryRun(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("acme.example.com", specYAML("KubeOne", "    access: registered\n    apiUrl: https://api.kubehz.cloud\n"))
	h.runner.handler = func(c execx.Cmd, _ string) error {
		io.WriteString(c.Stdout, "kind: CronJob\n")
		return nil
	}
	mustOK(t, h.ctx.Deploy(t.Context(), "acme.example.com", true), h.output())
	mustContain(t, h.output(), "kind: CronJob")
	for _, l := range h.runner.lines() {
		if !strings.HasPrefix(l, "kubectl kustomize ") {
			t.Fatalf("dry run must apply nothing: %s", l)
		}
	}
}
