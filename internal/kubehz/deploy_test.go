package kubehz

// deploy_test.go ports the render / dry-run / apply-order / wait cases of
// tests/unit/kubehz_live_agent_test.bats. kubectl is the fake Runner; the
// rendered manifests are compared against goldens generated ONCE from the
// bash kubehz::render_agent (testdata/golden/rendered-*.yaml).

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
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

// kubectlLogger logs each kubectl call that override does not handle. A pod
// read answers no pods; only the poll for the pods of the CronJob agent's
// Jobs is logged, so a test can see it in the order.
func kubectlLogger(h *harness, override func(c execx.Cmd) (bool, error)) *[]string {
	var log []string
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if override != nil {
			if handled, err := override(c); handled {
				return err
			}
		}
		if strings.Contains(argvLine(c), "get pods") && argvLine(c) != agentJobsPollArgv {
			return nil // idle cluster
		}
		log = append(log, argvLine(c))
		// The identity Secret holds an agent-token: the identity probe
		// prints the word, as the go-template does.
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") && c.Stdout != nil {
			io.WriteString(c.Stdout, "agent-token")
		}
		return nil
	}
	return &log
}

// The argv of the three calls around the stage: the Job delete, the pod poll
// and the patch that ends the suspend. Literal on purpose: a change of the
// selector or of the patch in deploy.go fails here.
const (
	agentJobsDeleteArgv = "kubectl -n kubehz-system delete job -l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent --ignore-not-found=true"
	agentJobsPollArgv   = "kubectl -n kubehz-system get pods -l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent --field-selector=status.phase!=Succeeded,status.phase!=Failed -o name"
	agentResumeArgv     = `kubectl -n kubehz-system patch cronjob kubehz-heartbeat --type=merge -p {"spec":{"suspend":false}}`
)

// agentJobsFake switches the fake answers of the calls around the stage:
// deleteFails fails the Job delete, pollFails fails each pod poll, podsStay
// makes each pod poll answer a pod that has not ended (pods, when it is set,
// is the answer), and patchFails fails the patch that ends the suspend. Use
// it as the override of kubectlLogger.
type agentJobsFake struct {
	deleteFails, pollFails, podsStay, patchFails bool
	pods                                         string
}

func (f agentJobsFake) answer(c execx.Cmd) (bool, error) {
	line := argvLine(c)
	switch {
	case f.deleteFails && line == agentJobsDeleteArgv:
		io.WriteString(c.Stderr, "Error from server (Forbidden): jobs.batch is forbidden\n")
		return true, exitErr(1)
	case f.pollFails && line == agentJobsPollArgv:
		io.WriteString(c.Stderr, "Error from server (Forbidden): pods is forbidden\n")
		return true, exitErr(1)
	case f.podsStay && line == agentJobsPollArgv:
		pods := f.pods
		if pods == "" {
			pods = "pod/kubehz-heartbeat-29000000-x2k4q\n"
		}
		io.WriteString(c.Stdout, pods)
		return true, nil
	case f.patchFails && line == agentResumeArgv:
		io.WriteString(c.Stderr, "Error from server (Forbidden): cronjobs.batch is forbidden\n")
		return true, exitErr(1)
	}
	return false, nil
}

// The texts of the stop of the pod wait and of an interrupt while the
// CronJob agent is suspended. Literal on purpose: a change in deploy.go
// fails here.
const (
	agentStaysSuspendedText = "The CronJob agent stays suspended, so heartbeats can stop until the deploy runs again."
	agentSuspendSafeText    = "That is the safe state: a suspended CronJob starts no Job."
	agentPodsStuckText      = "A pod ends within about 30 s (its grace period) after the Job delete, unless its node does not answer: then the pod stays Terminating. Check the node of each pod: kubectl -n kubehz-system get pods -l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent -o wide. If the node is gone, delete the pod: kubectl -n kubehz-system delete pod <name> --force --grace-period=0. Then run 'lo kubehz deploy' again."
	agentInterruptedText    = "[error] kubehz: the deploy was interrupted. The CronJob agent can stay suspended, so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again: it ends the suspend."
)

// errSleepRunaway ends a wait whose fake sleep ran more often than a
// bounded wait can.
var errSleepRunaway = errors.New("the fake sleep ran too often: the wait has no bound")

// guardSleep makes the sleep of h count its calls and fail after limit of
// them. The sleep is instant, so only the bound of a wait loop ends it: a
// wait without its bound then stops with errSleepRunaway and does not hang.
func guardSleep(h *harness, limit int) *int {
	n := 0
	h.ctx.Sleep = func(ctx context.Context, _ time.Duration) error {
		n++
		if n > limit {
			return errSleepRunaway
		}
		return ctx.Err()
	}
	return &n
}

// The manifest applies the CronJob suspended, and the jobTemplate and the pod
// template carry the labels that the Job delete and the pod poll select, in
// the embedded manifest and in a render of it.
func TestCronJobAgentManifestIsSuspendedAndLabelsItsJobs(t *testing.T) {
	t.Parallel()
	embedded, err := fs.ReadFile(Manifests(), "agent/cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	for name, raw := range map[string]string{
		"embedded": string(embedded),
		"rendered": readFile(t, filepath.Join(work, "agent", "cronjob.yaml")),
	} {
		var cj struct {
			Spec struct {
				Suspend     *bool `yaml:"suspend"`
				JobTemplate struct {
					Metadata struct {
						Labels map[string]string `yaml:"labels"`
					} `yaml:"metadata"`
					Spec struct {
						Template struct {
							Metadata struct {
								Labels map[string]string `yaml:"labels"`
							} `yaml:"metadata"`
						} `yaml:"template"`
					} `yaml:"spec"`
				} `yaml:"jobTemplate"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(raw), &cj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
			t.Fatalf("%s: spec.suspend must be true, so that the apply leaves the CronJob suspended", name)
		}
		for _, labels := range []map[string]string{cj.Spec.JobTemplate.Metadata.Labels, cj.Spec.JobTemplate.Spec.Template.Metadata.Labels} {
			if labels["app"] != "kubehz-heartbeat" || labels["app.kubernetes.io/name"] != "kubehz-agent" {
				t.Fatalf("%s: labels %v do not match the selector %s", name, labels, agentJobsSelector)
			}
		}
	}
	if agentJobsSelector != "app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent" {
		t.Fatalf("selector %q", agentJobsSelector)
	}
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
	mustContain(t, h.output(), "# The CronJob agent is applied suspended. The deploy then deletes its Jobs (-l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent) and waits until their pods have ended.")
	mustContain(t, h.output(), "# The deploy then sets spec.suspend to false on cronjob/kubehz-heartbeat.")
	mustNotContain(t, h.output(), "kubehz-agent-bind")
	mustContain(t, h.output(), "# --- Live agent (kubehz-live-agent) — managed tier RBAC ---")
	mustContain(t, h.output(), "dir: "+filepath.Join(work, "live-agent", "managed"))

	h.reset()
	mustOK(t, h.ctx.deployPrint(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
	mustContain(t, h.output(), "# The live agent is NOT deployed in cronjob mode; a previous install would be removed.")
	mustNotContain(t, h.output(), "live-agent")
}

// With a bind secret on disk, the dry run names the Secret right after
// the CronJob agent, in its apply order, and never prints the value.
func TestDeployPrintNamesTheBindSecretAfterTheCronJob(t *testing.T) {
	for _, mode := range []struct{ owner, access, next string }{
		{"operator", "managed", "# --- Live agent (kubehz-live-agent) — managed tier RBAC ---"},
		{"cronjob", "registered", "# The live agent is NOT deployed in cronjob mode; a previous install would be removed."},
	} {
		t.Run(mode.owner, func(t *testing.T) {
			h := newHarness(t)
			work := renderInto(t, h, mode.owner, mode.access)
			writeBindSecret(t, h, "acme.example.com", testBindSecret)
			h.runner.handler = func(c execx.Cmd, _ string) error {
				io.WriteString(c.Stdout, "kind: Rendered\n")
				return nil
			}
			mustOK(t, h.ctx.deployPrint(t.Context(), work, "acme.example.com", mode.owner, mode.access), h.output())
			lines := strings.Split(h.output(), "\n")
			want := []string{
				"# --- CronJob agent (kubehz-heartbeat) — identity + enrollment; heartbeat owner: " + mode.owner + " ---",
				"kind: Rendered",
				"---",
				"# The CronJob agent is applied suspended. The deploy then deletes its Jobs (-l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent) and waits until their pods have ended.",
				"---",
				"# --- Secret kubehz-agent-bind (the bind secret from clusters/acme.example.com/.kubehz-bind) — applied after the CronJob agent; the value is not printed ---",
				"---",
				"# The deploy then sets spec.suspend to false on cronjob/kubehz-heartbeat.",
				"---",
				mode.next,
			}
			if len(lines) < len(want) || !slices.Equal(lines[:len(want)], want) {
				t.Fatalf("dry-run lines = %q, want them to start with %q", lines, want)
			}
			mustNotContain(t, h.output(), "9f1c2b3a4d5e6f70")
			for _, l := range h.runner.lines() {
				if !strings.HasPrefix(l, "kubectl kustomize ") {
					t.Fatalf("dry run must apply nothing: %s", l)
				}
			}
		})
	}
}

func TestDeployApplyToOperatorOrder(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, nil)
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	if len(*log) != 7 {
		t.Fatalf("calls: %v", *log)
	}
	mustContain(t, (*log)[0], "apply -k "+filepath.Join(work, "agent"))
	// The CronJob agent is applied suspended: its Jobs are deleted, their
	// pods are waited for, and only then the suspend ends.
	if (*log)[1] != agentJobsDeleteArgv || (*log)[2] != agentJobsPollArgv || (*log)[3] != agentResumeArgv {
		t.Fatalf("want the Job delete, the pod poll and the resume after the CronJob agent apply: %v", *log)
	}
	// The identity Secret is read between the two applies (the live agent
	// mounts it): it holds an agent-token here, so no bootstrap job runs.
	// The probe prints only whether the key is there, never a value.
	mustContain(t, (*log)[4], `get secret kubehz-agent -o go-template={{with .data}}{{if index . "agent-token"}}agent-token{{end}}{{end}}`)
	mustNotContain(t, (*log)[4], "jsonpath")
	mustContain(t, (*log)[5], "apply -k "+filepath.Join(work, "live-agent", "managed"))
	mustContain(t, (*log)[6], "rollout status deployment/kubehz-live-agent")
	mustContain(t, (*log)[6], "--timeout=120s")
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
// yaml` prints for the value it read on stdin, so a test can follow the
// value to the stdin of the apply.
func bindManifest(value string) string {
	return "apiVersion: v1\nkind: Secret\nmetadata:\n  name: kubehz-agent-bind\n  namespace: kubehz-system\ndata:\n  bind-secret: " +
		base64.StdEncoding.EncodeToString([]byte(value)) + "\n"
}

// bindStageLogger is kubectlLogger with the dry-run create answering
// bindManifest of its stdin; fail names one argv substring that exits 1.
// Each byte the create reads lands in h.runner.stdins.
func bindStageLogger(h *harness, fail string) *[]string {
	return bindStageLoggerWhen(h, func(line string) bool { return fail != "" && strings.Contains(line, fail) })
}

// bindStageLoggerAfter is bindStageLogger, but a call that contains fail
// exits 1 only after a call that contains after ran: so the preflight of the
// operator direction passes, and the real stage after the CronJob agent
// apply fails.
func bindStageLoggerAfter(h *harness, fail, after string) *[]string {
	seen := false
	return bindStageLoggerWhen(h, func(line string) bool {
		if seen && strings.Contains(line, fail) {
			return true
		}
		if strings.Contains(line, after) {
			seen = true
		}
		return false
	})
}

// bindStageLoggerWhen is bindStageLogger with a call that fail(argv) holds
// for exiting 1.
func bindStageLoggerWhen(h *harness, fail func(line string) bool) *[]string {
	return bindStageLoggerFake(h, fail, agentJobsFake{})
}

// bindStageLoggerFake is bindStageLoggerWhen, and fake answers the calls
// around the stage.
func bindStageLoggerFake(h *harness, fail func(line string) bool, fake agentJobsFake) *[]string {
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if fail(argvLine(c)) {
			return true, exitErr(1)
		}
		return fake.answer(c)
	})
	logged := h.runner.handler
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		if err := logged(c, stdin); err != nil {
			return err
		}
		if strings.Contains(argvLine(c), "create secret generic kubehz-agent-bind") {
			io.WriteString(c.Stdout, bindManifest(stdin))
		}
		return nil
	}
	return log
}

// stdinsOf are the stdins of each call whose argv contains substr, in order.
func stdinsOf(h *harness, substr string) []string {
	var out []string
	for i, c := range h.runner.calls {
		if strings.Contains(argvLine(c), substr) {
			out = append(out, h.runner.stdins[i])
		}
	}
	return out
}

// The argv of the stage calls.
const (
	bindCreateArgv = "-n kubehz-system create secret generic kubehz-agent-bind --from-file=bind-secret=/dev/stdin --dry-run=client -o yaml"
	bindApplyArgv  = "apply --server-side --force-conflicts -f -"
	bindProofArgv  = "apply --server-side --force-conflicts --dry-run=server -f -"
)

// indexOf is the first log line that contains substr, or -1.
func indexOf(log []string, substr string) int {
	for i, l := range log {
		if strings.Contains(l, substr) {
			return i
		}
	}
	return -1
}

// assertBindStagedAfter checks the calls from the CronJob agent apply at log
// index agent on, in their order: the suspended CronJob agent apply, the Job
// delete, the pod poll, the three staging calls, and the patch that ends the
// suspend. Each dry-run create (the preflight's too) reads the value on
// stdin, byte for byte the value, and each server-side apply reads what the
// dry run printed. The value is never on argv.
func assertBindStagedAfter(t *testing.T, h *harness, log []string, work string, agent int, value string) {
	t.Helper()
	want := []string{
		"apply -k " + filepath.Join(work, "agent"),
		agentJobsDeleteArgv,
		agentJobsPollArgv,
		"apply -f " + filepath.Join(work, "agent", "namespace.yaml"),
		bindCreateArgv,
		bindApplyArgv,
		agentResumeArgv,
	}
	const order = "CronJob agent(%d) < Job delete < pod poll < namespace < dry-run create < server-side apply < resume"
	if len(log) < agent+len(want) {
		t.Fatalf("want "+order+": %v", agent, log)
	}
	for i, w := range want {
		if !strings.HasSuffix(log[agent+i], w) {
			t.Fatalf("want "+order+", got %q at %d: %v", agent, log[agent+i], agent+i, log)
		}
	}
	joined := strings.Join(log, "\n")
	mustNotContain(t, joined, "--from-literal")
	mustNotContain(t, joined, value)
	mustNotContain(t, joined, "delete secret kubehz-agent-bind")
	creates := stdinsOf(h, "create secret generic kubehz-agent-bind")
	if len(creates) == 0 {
		t.Fatalf("no dry-run create: %v", log)
	}
	for _, got := range creates {
		if got != value {
			t.Fatalf("dry-run create stdin = %q, want the bind secret %q", got, value)
		}
	}
	for _, got := range stdinsOf(h, "apply --server-side") {
		if got != bindManifest(value) {
			t.Fatalf("server-side apply stdin = %q, want the dry-run output", got)
		}
	}
}

// assertBindProvedFirst checks the preflight of the operator direction: the
// first three calls are the namespace apply, the dry-run create and the
// server-side apply with --dry-run=server, and the CronJob agent apply
// follows them.
func assertBindProvedFirst(t *testing.T, log []string, work string) {
	t.Helper()
	want := []string{
		"apply -f " + filepath.Join(work, "agent", "namespace.yaml"),
		bindCreateArgv,
		bindProofArgv,
		"apply -k " + filepath.Join(work, "agent"),
	}
	if len(log) < len(want) {
		t.Fatalf("want namespace < dry-run create < server dry run < CronJob agent: %v", log)
	}
	for i, w := range want {
		if !strings.HasSuffix(log[i], w) {
			t.Fatalf("want namespace < dry-run create < server dry run < CronJob agent, got %q at %d: %v", log[i], i, log)
		}
	}
}

// stagedBindSecrets are the two forms of a bind secret that the deploy
// stages: 64 hex, and khzb_ and 64 hex.
var stagedBindSecrets = []struct{ name, value string }{
	{"64 hex", testBindSecret},
	{"khzb_ and 64 hex", prefixedBindSecretFixture},
}

// With a bind secret on disk, the operator deploy runs the preflight of the
// stage (a server-side dry run) before the CronJob agent apply, stages it
// while the CronJob is suspended and its Jobs are gone, and before the
// identity bootstrap Job can register. The preflight and the stage send the
// same bytes.
func TestDeployApplyStagesTheBindSecretOperator(t *testing.T) {
	for _, s := range stagedBindSecrets {
		t.Run(s.name, func(t *testing.T) {
			h := newHarness(t)
			work := renderInto(t, h, "operator", "managed")
			writeBindSecret(t, h, "acme.example.com", s.value)
			log := bindStageLogger(h, "")
			mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
			assertBindProvedFirst(t, *log, work)
			assertBindStagedAfter(t, h, *log, work, 3, s.value)
			if n := len(stdinsOf(h, "create secret generic kubehz-agent-bind")); n != 2 {
				t.Fatalf("want 2 dry-run creates (the preflight and the stage), got %d: %v", n, *log)
			}
			if idRead := indexOf(*log, "get secret kubehz-agent "); idRead != 10 {
				t.Fatalf("the identity read must follow the stage and the resume: %v", *log)
			}
			mustNotContain(t, h.output(), "no bind secret")
		})
	}
}

// Cronjob mode stages it the same way, after the live agent is gone, and
// the patch that ends the suspend is the last step.
func TestDeployApplyStagesTheBindSecretCronjob(t *testing.T) {
	for _, s := range stagedBindSecrets {
		t.Run(s.name, func(t *testing.T) {
			h := newHarness(t)
			work := renderInto(t, h, "cronjob", "registered")
			writeBindSecret(t, h, "acme.example.com", s.value)
			log := bindStageLogger(h, "")
			mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
			mustContain(t, (*log)[0], "delete -k "+filepath.Join(work, "live-agent", "managed"))
			assertBindStagedAfter(t, h, *log, work, 2, s.value)
			if len(*log) != 9 {
				t.Fatalf("the patch that ends the suspend must come last: %v", *log)
			}
		})
	}
}

// The CronJob stays suspended from the CronJob agent apply to the end of the
// stage, in both directions, with or without a bind secret on disk: the
// apply leaves the CronJob suspended, the Job delete and the pod poll come
// right after it,
// and the patch that ends the suspend comes after the stage. Between the
// apply and the patch, only these calls run.
func TestDeployApplyKeepsTheCronJobSuspendedUntilTheStage(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		for _, onDisk := range []bool{true, false} {
			h := newHarness(t)
			work := renderInto(t, h, mode.owner, mode.access)
			if onDisk {
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
			}
			log := bindStageLogger(h, "")
			mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access), h.output())
			cron := indexOf(*log, "apply -k "+filepath.Join(work, "agent"))
			resume := indexOf(*log, agentResumeArgv)
			if cron < 0 || resume < cron+3 {
				t.Fatalf("%s/%v: want the CronJob agent apply, then the Job delete and the pod poll, then the resume: %v", mode.owner, onDisk, *log)
			}
			if (*log)[cron+1] != agentJobsDeleteArgv || (*log)[cron+2] != agentJobsPollArgv {
				t.Fatalf("%s/%v: the Job delete and the pod poll must follow the CronJob agent apply: %v", mode.owner, onDisk, *log)
			}
			between := (*log)[cron+3 : resume]
			if !onDisk && len(between) != 0 {
				t.Fatalf("%s: without a bind secret, the resume must follow the pod poll: %v", mode.owner, *log)
			}
			for _, l := range between {
				if !strings.HasSuffix(l, "namespace.yaml") && !strings.Contains(l, bindCreateArgv) && !strings.HasSuffix(l, bindApplyArgv) {
					t.Fatalf("%s/%v: only the stage may run between the pod poll and the resume, got %q: %v", mode.owner, onDisk, l, *log)
				}
			}
			if onDisk && indexOf(*log, bindApplyArgv) > resume {
				t.Fatalf("%s: the resume must follow the stage: %v", mode.owner, *log)
			}
			if n := h.runner.countCalls("patch cronjob"); n != 1 {
				t.Fatalf("%s/%v: want one patch, got %d", mode.owner, onDisk, n)
			}
		}
	}
}

// A step around the stage that fails stops the deploy and leaves the
// CronJob suspended, in both directions: a Job delete that fails, a pod poll
// that keeps failing, and a pod that does not end. The error says so and
// names what to do: after the pod wait, how to remove a pod that stays
// Terminating. No stage, no resume, no live agent and no bootstrap Job
// follow.
func TestDeployApplyAgentJobsThatDoNotGoLeaveTheCronJobSuspended(t *testing.T) {
	suspended := agentStaysSuspendedText
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		for _, tc := range []struct {
			name string
			fake agentJobsFake
			want []string
			poll bool
		}{
			{"the Job delete fails", agentJobsFake{deleteFails: true}, []string{
				"kubehz: could not delete the Jobs of the CronJob agent (-l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent). The delete can have removed some of them. " + suspended + " The kubeconfig needs list and delete on Jobs in kubehz-system. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again.",
			}, false},
			{"the pod poll fails", agentJobsFake{pollFails: true}, []string{
				"kubehz: could not list the pods of the CronJob agent's Jobs in kubehz-system for 1s: Error from server (Forbidden): pods is forbidden. " + suspended + " " + agentSuspendSafeText + " The kubeconfig needs list on Pods in kubehz-system. " + agentPodsStuckText,
				"[warn] kubehz: cannot list the pods of the CronJob agent's Jobs in kubehz-system (Error from server (Forbidden): pods is forbidden). The deploy counts that as a pod that has not ended, and tries again.",
			}, true},
			{"a pod does not end", agentJobsFake{podsStay: true}, []string{
				"kubehz: pods of the CronJob agent's Jobs did not end within 1s after the deploy deleted the Jobs. The pods that have not ended: kubehz-heartbeat-29000000-x2k4q. " + suspended + " " + agentSuspendSafeText + " " + agentPodsStuckText,
			}, true},
		} {
			t.Run(mode.owner+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t)
				h.env["KUBEHZ_AGENT_JOBS_DRAIN_SECONDS"] = "1"
				guardSleep(h, 100)
				work := renderInto(t, h, mode.owner, mode.access)
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
				bindStageLoggerFake(h, func(string) bool { return false }, tc.fake)
				mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access))
				for _, w := range tc.want {
					mustContain(t, h.output(), w)
				}
				// The pod wait names the force delete for a pod on a node
				// that is gone; the Job delete has no pod to name.
				if tc.poll {
					mustContain(t, h.output(), "--force --grace-period=0")
				} else {
					mustNotContain(t, h.output(), "--force")
				}
				calls := strings.Join(h.runner.lines(), "\n")
				if got := strings.Contains(calls, agentJobsPollArgv); got != tc.poll {
					t.Fatalf("pod poll ran: %v, want %v:\n%s", got, tc.poll, calls)
				}
				// The real stage, the resume, the bootstrap Job and the live
				// agent never ran (the preflight's server dry run is no stage).
				for _, l := range h.runner.lines() {
					if strings.HasSuffix(l, " "+bindApplyArgv) {
						t.Fatalf("the Secret was staged: %s", calls)
					}
				}
				for _, not := range []string{"patch cronjob", "create job", "get secret kubehz-agent ", "apply -k " + filepath.Join(work, "live-agent")} {
					mustNotContain(t, calls, not)
				}
			})
		}
	}
}

// A pod that never ends (a pod on a node that does not answer stays
// Terminating, in the phase Running) stops the deploy at the end of the
// window, in both directions: KUBEHZ_AGENT_JOBS_DRAIN_SECONDS is unset, so
// 130 s, which is 26 polls, each followed by a sleep of 5 s. The error names
// the pods that have not ended (only names of the DNS shape), says that the
// CronJob agent stays suspended and that this is the safe state, and says
// how to remove such a pod. No stage and no patch follow, so spec.suspend
// stays true. The sleep fails after 1000 calls, so a wait without its bound
// fails here and does not hang.
func TestDeployApplyAgentPodThatNeverEndsStopsAtTheEndOfTheWindow(t *testing.T) {
	want := "[error] kubehz: pods of the CronJob agent's Jobs did not end within 130s after the deploy deleted the Jobs. " +
		"The pods that have not ended: kubehz-heartbeat-29000000-x2k4q, kubehz-heartbeat-29000005-b7m2p. " +
		agentStaysSuspendedText + " " + agentSuspendSafeText + " " + agentPodsStuckText + "\n"
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		t.Run(mode.owner, func(t *testing.T) {
			h := newHarness(t)
			sleeps := guardSleep(h, 1000)
			work := renderInto(t, h, mode.owner, mode.access)
			writeBindSecret(t, h, "acme.example.com", testBindSecret)
			bindStageLoggerFake(h, func(string) bool { return false }, agentJobsFake{
				podsStay: true,
				pods:     "pod/kubehz-heartbeat-29000000-x2k4q\npod/kubehz-heartbeat-29000005-b7m2p\npod/Bad\x1b[31mName\n",
			})
			err := h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v, want ErrHandled (exit 1):\n%s", err, h.output())
			}
			mustContain(t, h.output(), want)
			mustNotContain(t, h.output(), "Bad")
			if n := h.runner.countCalls(agentJobsPollArgv); n != 26 || *sleeps != 26 {
				t.Fatalf("polls = %d, sleeps = %d, want 26 of each", n, *sleeps)
			}
			calls := strings.Join(h.runner.lines(), "\n")
			if strings.Contains(calls, " "+bindApplyArgv) {
				t.Fatalf("the Secret was staged:\n%s", calls)
			}
			for _, not := range []string{"patch cronjob", "create job", "apply -k " + filepath.Join(work, "live-agent")} {
				mustNotContain(t, calls, not)
			}
		})
	}
}

// An INT or a TERM (the context ends) from the CronJob agent apply to the
// end of the suspend stops the deploy, in both directions and at each step:
// the CronJob agent apply, the Job delete, the pod poll, the sleep of the
// pod wait, the stage and the patch that ends the suspend. The deploy prints
// one line, that the CronJob agent can stay suspended and that a run of
// 'lo kubehz deploy' ends the suspend, in place of the error of the step,
// and returns the context's error: lo then prints nothing more and exits
// 128+n (130 for an INT). No step runs after it.
func TestDeployApplyInterruptWhileSuspendedSaysSo(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		for _, step := range []struct {
			name string
			argv func(work string) string
			// sleep: the signal comes in the sleep after a poll that
			// lists a pod, not in a kubectl call.
			sleep bool
		}{
			{"the CronJob agent apply", func(work string) string { return "kubectl apply -k " + filepath.Join(work, "agent") }, false},
			{"the Job delete", func(string) string { return agentJobsDeleteArgv }, false},
			{"the pod poll", func(string) string { return agentJobsPollArgv }, false},
			{"the sleep of the pod wait", func(string) string { return agentJobsPollArgv }, true},
			{"the stage", func(string) string { return "kubectl " + bindApplyArgv }, false},
			{"the end of the suspend", func(string) string { return agentResumeArgv }, false},
		} {
			t.Run(mode.owner+"/"+step.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				h := newHarness(t)
				work := renderInto(t, h, mode.owner, mode.access)
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
				target := step.argv(work)
				bindStageLoggerFake(h, func(string) bool { return false }, agentJobsFake{podsStay: step.sleep})
				logged := h.runner.handler
				h.runner.handler = func(c execx.Cmd, stdin string) error {
					if !step.sleep && argvLine(c) == target {
						cancel()
						return exitErr(130) // a kubectl that the signal stopped
					}
					return logged(c, stdin)
				}
				if step.sleep {
					h.ctx.Sleep = func(ctx context.Context, _ time.Duration) error {
						cancel()
						return ctx.Err()
					}
				}
				err := h.ctx.deployApply(ctx, work, "acme.example.com", mode.owner, mode.access)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled:\n%s", err, h.output())
				}
				if n := strings.Count(h.output(), agentInterruptedText+"\n"); n != 1 {
					t.Fatalf("want the interrupt line once, got %d:\n%s", n, h.output())
				}
				mustNotContain(t, h.output(), "could not")
				mustNotContain(t, h.output(), "cannot list")
				lines := h.runner.lines()
				if last := lines[len(lines)-1]; last != target {
					t.Fatalf("the deploy ran %q after the interrupt at %q", last, target)
				}
				if n := h.runner.countCalls(target); n != 1 {
					t.Fatalf("%q ran %d times, want 1", target, n)
				}
				calls := strings.Join(lines, "\n")
				if target != agentResumeArgv {
					mustNotContain(t, calls, "patch cronjob")
				}
				mustNotContain(t, calls, "create job")
				mustNotContain(t, calls, "apply -k "+filepath.Join(work, "live-agent"))
			})
		}
	}
}

// A patch that ends the suspend and fails stops the deploy after the stage:
// the error says that the CronJob can stay suspended and names the patch. In
// the operator direction no bootstrap Job and no live agent follow.
func TestDeployApplyFailedResumeSaysTheCronJobCanStaySuspended(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		t.Run(mode.owner, func(t *testing.T) {
			h := newHarness(t)
			work := renderInto(t, h, mode.owner, mode.access)
			writeBindSecret(t, h, "acme.example.com", testBindSecret)
			log := bindStageLoggerFake(h, func(string) bool { return false }, agentJobsFake{patchFails: true})
			mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access))
			mustContain(t, h.output(), "kubehz: could not set spec.suspend to false on cronjob/kubehz-heartbeat. The CronJob agent can stay suspended, so heartbeats can stop until the deploy runs again. The kubeconfig needs patch on CronJobs in kubehz-system. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. The deploy sets it with: kubectl -n kubehz-system patch cronjob kubehz-heartbeat --type=merge -p '{\"spec\":{\"suspend\":false}}'.")
			if last := argvLine(h.runner.calls[len(h.runner.calls)-1]); last != agentResumeArgv {
				t.Fatalf("the deploy stopped at %q, want the patch", last)
			}
			if indexOf(*log, bindApplyArgv) < 0 {
				t.Fatalf("the stage must come before the patch: %v", *log)
			}
			joined := strings.Join(*log, "\n")
			mustNotContain(t, joined, "create job")
			mustNotContain(t, joined, "apply -k "+filepath.Join(work, "live-agent"))
		})
	}
}

// The deploy stages the bytes that it checked. A file that changes after the
// check (here: on the first namespace apply, between the check and the
// dry-run create; in the operator direction that is the preflight's) does not
// reach the Secret, in either form and in both directions.
func TestDeployApplyStagesTheBytesItChecked(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		for _, s := range stagedBindSecrets {
			t.Run(mode.owner+"/"+s.name, func(t *testing.T) {
				h := newHarness(t)
				work := renderInto(t, h, mode.owner, mode.access)
				p := writeBindSecret(t, h, "acme.example.com", s.value)
				swapped := "khzb_" + strings.Repeat("0f", 32)
				log := bindStageLogger(h, "")
				staging := h.runner.handler
				h.runner.handler = func(c execx.Cmd, stdin string) error {
					if strings.Contains(argvLine(c), "namespace.yaml") {
						if err := os.WriteFile(p, []byte(swapped), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return staging(c, stdin)
				}
				mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access), h.output())
				if got := readFile(t, p); got != swapped {
					t.Fatalf("the file was not swapped: %q", got)
				}
				agent := 3
				if mode.owner == "cronjob" {
					agent = 2
				}
				assertBindStagedAfter(t, h, *log, work, agent, s.value)
			})
		}
	}
}

// A failed stage with a bind secret on disk stops the deploy after the
// CronJob agent apply, the Job delete and the pod poll, in both directions
// and at each of the three steps. The CronJob stays suspended: no patch ends
// the suspend, and the error says so. In the operator mode the preflight
// before it passed.
//
// The subtest names hold no fail substring: t.TempDir puts the test name in
// the render path, and a fail substring there fails the namespace apply.
func TestDeployApplyFailedBindStageLeavesTheCronJobSuspended(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		for _, step := range []struct{ name, fail string }{
			{"namespace apply", "namespace.yaml"},
			{"dry-run create", "--dry-run=client"},
			{"server-side apply", "apply --server-side"},
		} {
			t.Run(mode.owner+"/"+step.name, func(t *testing.T) {
				h := newHarness(t)
				work := renderInto(t, h, mode.owner, mode.access)
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
				log := bindStageLoggerAfter(h, step.fail, "apply -k "+filepath.Join(work, "agent"))
				mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access))
				// The deploy stopped at the call of this step: the last call
				// that ran (failed calls are recorded too).
				if last := argvLine(h.runner.calls[len(h.runner.calls)-1]); !strings.Contains(last, step.fail) {
					t.Fatalf("the deploy stopped at %q, want the %s call", last, step.name)
				}
				out := h.output()
				mustContain(t, out, "kubehz: could not stage the bind secret from clusters/acme.example.com/.kubehz-bind into Secret kubehz-system/kubehz-agent-bind. The stage can have applied the namespace or the Secret. The CronJob agent stays suspended, so heartbeats can stop until the deploy runs again. The kubeconfig needs patch on Secrets in kubehz-system, and create when the Secret is new. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. If you deploy without the secret, the agent can register a separate pending row if it has no binding and no staged value yet. To deploy without the secret, remove the file and the Secret (kubectl -n kubehz-system delete secret kubehz-agent-bind --ignore-not-found), then claim the new row with the code from 'lo kubehz claim-code'.")
				for _, old := range []string{"reporting NOTHING", "The deploy applied the CronJob agent and then stopped.", "pending-pool"} {
					mustNotContain(t, out, old)
				}
				// The CronJob agent apply is the last call that changes an
				// agent. Only the Job delete, the pod poll and staging calls
				// follow it: no resume, no live agent, no bootstrap Job.
				cron := indexOf(*log, "apply -k "+filepath.Join(work, "agent"))
				if cron < 0 {
					t.Fatalf("no CronJob agent apply: %v", *log)
				}
				if mode.owner == "operator" {
					assertBindProvedFirst(t, *log, work)
				}
				if len(*log) < cron+3 || (*log)[cron+1] != agentJobsDeleteArgv || (*log)[cron+2] != agentJobsPollArgv {
					t.Fatalf("want the Job delete and the pod poll after the CronJob agent apply: %v", *log)
				}
				for _, l := range (*log)[cron+3:] {
					if !strings.Contains(l, "namespace.yaml") && !strings.Contains(l, "create secret generic kubehz-agent-bind") {
						t.Fatalf("only staging calls may follow the pod poll, got %q: %v", l, *log)
					}
				}
				if h.runner.anyCall("patch cronjob") {
					t.Fatalf("a failed stage must leave the CronJob suspended: %v", h.runner.lines())
				}
				joined := strings.Join(*log, "\n")
				mustNotContain(t, joined, "apply -k "+filepath.Join(work, "live-agent"))
				mustNotContain(t, joined, "create job")
				mustNotContain(t, joined, "get secret kubehz-agent ")
				mustNotContain(t, joined, "rollout status")
			})
		}
	}
}

// No bind secret on disk: the deploy touches kubehz-agent-bind not at all,
// still deletes the Jobs, waits for their pods and ends the suspend, and
// warns what a register in another checkout means.
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
		cron := indexOf(*log, "apply -k "+filepath.Join(work, "agent"))
		if cron < 0 || len(*log) < cron+4 || (*log)[cron+1] != agentJobsDeleteArgv || (*log)[cron+2] != agentJobsPollArgv || (*log)[cron+3] != agentResumeArgv {
			t.Fatalf("%s: want the Job delete, the pod poll and the resume right after the CronJob agent apply: %v", mode.owner, *log)
		}
		mustContain(t, h.output(), "[warn] kubehz: no bind secret at clusters/acme.example.com/.kubehz-bind. If 'lo kubehz register' ran in another checkout, the agent can register a separate pending row if it has no binding and no staged value yet. The announced row then stays as it is and gets no heartbeats. Claim the new row with the code from 'lo kubehz claim-code'.")
	}
}

// A file that is not a bind secret (the register check: a readable
// regular file of exactly 64 lowercase hex, or of khzb_ and 64 lowercase hex)
// is never staged. The deploy warns that the file holds no bind secret and
// keeps today's order.
func TestDeployApplyMalformedBindSecretIsNotStaged(t *testing.T) {
	setups := map[string]func(t *testing.T, path string){
		"a directory": func(t *testing.T, p string) { mustMkdir(t, p) },
		"unreadable": func(t *testing.T, p string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a 0000 file")
			}
			mustWrite(t, p, testBindSecret, 0o000)
		},
	}
	for name, content := range unusableBindSecrets() {
		setups[name] = func(t *testing.T, p string) { mustWrite(t, p, content, 0o600) }
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			work := renderInto(t, h, "cronjob", "registered")
			path := filepath.Join(h.ctx.Paths.Clusters, "acme.example.com", ".kubehz-bind")
			mustMkdir(t, filepath.Dir(path))
			setup(t, path)
			log := bindStageLogger(h, "")
			mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "cronjob", "registered"), h.output())
			mustNotContain(t, strings.Join(*log, "\n"), "kubehz-agent-bind")
			mustContain(t, (*log)[0], "delete -k ")
			out := h.output()
			mustContain(t, out, "[warn] kubehz: clusters/acme.example.com/.kubehz-bind holds no bind secret (a readable file of 64 lowercase hex characters, or khzb_ and 64 lowercase hex characters). The deploy does not stage it, and a Secret kubehz-agent-bind from an earlier deploy stays as it is. Then the agent can register a separate pending row if it has no binding and no staged value yet. The announced row then stays as it is and gets no heartbeats. Claim the new row with the code from 'lo kubehz claim-code'.")
			mustContain(t, out, "If the file holds a newer form of bind secret, upgrade lok8s (the lo binary and, for a bash install, the synced tree), then run 'lo kubehz deploy' again.")
		})
	}
}

// A failed apply or delete of several objects can have changed part of
// them: its error says so, and says that heartbeats can stop until the deploy
// runs again. It never says that nothing else was changed. With or without a
// bind secret on disk, a failure before the stage stages no Secret.
func TestDeployApplyFailedAgentStepsSayWhatCanHaveChanged(t *testing.T) {
	stop := "heartbeats can stop until the deploy runs again."
	for _, tc := range []struct {
		name, owner, access, fail, want string
		staged                          bool
	}{
		{"operator CronJob agent apply", "operator", "managed", "apply -k ",
			"kubehz: could not apply the CronJob agent (identity bootstrap). The apply can have applied part of the agent objects; an applied CronJob is suspended, so " + stop + " Fix the cause, then run 'lo kubehz deploy' again.", false},
		{"operator live agent apply", "operator", "managed", "live-agent/managed",
			"kubehz: could not apply the live agent. The apply can have applied part of the live agent objects. The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator), so " + stop + " Run 'lo kubehz deploy' again, or set spec.kubehz.agent back to cronjob and run it again.", true},
		{"cronjob live agent delete", "cronjob", "registered", "delete -k ",
			"kubehz: could not remove the live agent. The delete can have removed part of the live agent objects. The deploy did not apply the CronJob agent, so " + stop, false},
		{"cronjob sweep", "cronjob", "registered", "delete deployment -l",
			"kubehz: could not sweep live-agent Deployments by label. The live agent delete before it passed, and the sweep can have removed some Deployments. The deploy did not apply the CronJob agent, so " + stop, false},
		{"cronjob CronJob agent apply", "cronjob", "registered", "apply -k ",
			"kubehz: could not apply the CronJob agent. The live agent is gone. The apply can have applied part of the agent objects; an applied CronJob is suspended, so " + stop + " Fix the cause, then run 'lo kubehz deploy' again.", false},
	} {
		for _, onDisk := range []bool{true, false} {
			h := newHarness(t)
			work := renderInto(t, h, tc.owner, tc.access)
			if onDisk {
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
			}
			log := bindStageLogger(h, tc.fail)
			mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", tc.owner, tc.access))
			out := h.output()
			mustContain(t, out, tc.want)
			for _, old := range []string{"Nothing else was changed.", "nothing else was changed", "Only the namespace kubehz-system was applied.", "reporting NOTHING", "one producer"} {
				mustNotContain(t, out, old)
			}
			if got := indexOf(*log, bindApplyArgv) >= 0; got != (tc.staged && onDisk) {
				t.Fatalf("%s/%v: the Secret was staged: %v, want %v: %v", tc.name, onDisk, got, tc.staged && onDisk, *log)
			}
			if tc.owner == "operator" && onDisk {
				// The preflight ran before the CronJob agent apply.
				cron := "kubectl apply -k " + filepath.Join(work, "agent")
				assertBindProvedFirst(t, append(append([]string{}, (*log)[:3]...), cron), work)
			}
		}
	}
}

// In the operator direction, a preflight of the stage that fails stops the
// deploy BEFORE the CronJob agent apply: no agent changes, no Secret is
// staged, and the error says what was changed (the namespace, when its apply
// passed; what the namespace apply can have changed, when it failed) and
// what the kubeconfig needs.
//
// The subtest names hold no fail substring: t.TempDir puts the test name in
// the render path, and a fail substring there fails the namespace apply.
func TestDeployApplyFailedPreflightStopsBeforeTheCronJob(t *testing.T) {
	for _, tc := range []struct {
		name, fail, changed string
		creates, proofs     int
	}{
		{"server dry run", "--dry-run=server", "The deploy applied only the namespace kubehz-system.", 1, 1},
		{"dry-run create", "--dry-run=client", "The deploy applied only the namespace kubehz-system.", 1, 0},
		{"namespace apply", "namespace.yaml", "The namespace apply can have changed the namespace kubehz-system. The deploy applied no other object.", 0, 0},
	} {
		for _, s := range stagedBindSecrets {
			t.Run(tc.name+"/"+s.name, func(t *testing.T) {
				h := newHarness(t)
				work := renderInto(t, h, "operator", "managed")
				writeBindSecret(t, h, "acme.example.com", s.value)
				log := bindStageLogger(h, tc.fail)
				mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
				out := h.output()
				mustContain(t, out, "kubehz: the preflight of the bind secret stage failed (a server-side dry run of Secret kubehz-system/kubehz-agent-bind with the value from clusters/acme.example.com/.kubehz-bind), so the deploy stopped before the CronJob agent apply. "+tc.changed+" The kubeconfig needs patch on Secrets in kubehz-system, and create when the Secret is new. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. To deploy without the secret, remove the file and the Secret (kubectl -n kubehz-system delete secret kubehz-agent-bind --ignore-not-found), then claim the new row with the code from 'lo kubehz claim-code'.")
				for _, old := range []string{"Nothing else was changed.", "Only the namespace kubehz-system was applied.", "proof"} {
					mustNotContain(t, out, old)
				}
				mustNotContain(t, out, s.value)
				// The calls that ran, the failed one included (the runner
				// records failed calls too): the preflight stopped at its step.
				// After a failed namespace apply, no dry-run create and no
				// server dry run ran.
				if n := len(stdinsOf(h, "create secret generic")); n != tc.creates {
					t.Fatalf("want %d dry-run creates, got %d: %v", tc.creates, n, h.runner.calls)
				}
				if n := len(stdinsOf(h, "--dry-run=server")); n != tc.proofs {
					t.Fatalf("want %d server dry runs, got %d: %v", tc.proofs, n, h.runner.calls)
				}
				joined := strings.Join(*log, "\n")
				mustNotContain(t, joined, "apply -k ")
				mustNotContain(t, joined, bindApplyArgv)
				mustNotContain(t, joined, "get secret kubehz-agent ")
				mustNotContain(t, joined, "create job")
				mustNotContain(t, joined, "rollout status")
			})
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

// A first deploy finds the identity Secret empty: the apply of the agent
// manifests just created it. The deploy runs the CronJob's bootstrap once
// (it fills the Secret), waits for its Job to complete, removes the one-off
// job, and only then applies the live agent.
func TestDeployApplyBootstrapsTheIdentitySecret(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	// Empty before the bootstrap, filled once the one-off Job completed.
	completed := false
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "wait --for=condition=complete") {
			completed = true
			return false, nil
		}
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") && !completed {
			return true, nil
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

func TestDeployApplyIdentitySecretNeverFilledFails(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	h.env["KUBEHZ_IDENTITY_BOOTSTRAP_SECONDS"] = "10"
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") {
			return true, nil
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
	mustNotContain(t, joined, "delete job kubehz-heartbeat-bootstrap-")
	mustContain(t, h.output(), "A Job that failed stays until you delete it, or until the next 'lo kubehz deploy' deletes it.")
	mustContain(t, h.output(), "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again.")
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
	mustContain(t, h.output(), "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so heartbeats can stop until the deploy runs again. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again.")
	mustNotContain(t, h.output(), "reporting NOTHING")
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

// An absent identity Secret stops the deploy, also behind a warning line
// (a kubeconfig deprecation, say): the apply just before created it, and
// the agent cannot create it (its Role has no create), so a bootstrap
// cannot help.
func TestDeployApplyIdentitySecretNotFoundStops(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") {
			io.WriteString(c.Stderr, "Warning: the kubeconfig field exec.apiVersion v1alpha1 is deprecated\nError from server (NotFound): secrets \"kubehz-agent\" not found\n")
			return true, exitErr(1)
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "Secret kubehz-agent does not exist in kubehz-system, but the deploy applied it just before")
	mustContain(t, h.output(), "The agent cannot create it")
	mustContain(t, h.output(), "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again.")
	mustNotContain(t, h.output(), "reporting NOTHING")
	joined := strings.Join(*log, "\n")
	mustNotContain(t, joined, "create job")
	mustNotContain(t, joined, "apply -k "+filepath.Join(work, "live-agent", "managed"))
}

// A bootstrap Job that completes but leaves the Secret without an
// agent-token (a key-less fill, say) stops the deploy before the live
// agent, and the Job stays for its log. A warning line before the word
// agent-token does not hide it.
func TestDeployApplyIdentityBootstrapWithoutATokenFails(t *testing.T) {
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	log := kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") {
			io.WriteString(c.Stdout, "Warning: a deprecated field\nclaim-code\n")
			return true, nil
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "completed but left no agent-token in Secret kubehz-agent")
	joined := strings.Join(*log, "\n")
	mustContain(t, joined, "create job kubehz-heartbeat-bootstrap-")
	mustNotContain(t, joined, "delete job kubehz-heartbeat-bootstrap-")
	mustNotContain(t, joined, "apply -k "+filepath.Join(work, "live-agent", "managed"))

	// The word on a line of its own after a warning line counts.
	h.reset()
	log = kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") {
			io.WriteString(c.Stdout, "Warning: a deprecated field\nagent-token")
			return true, nil
		}
		return false, nil
	})
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustNotContain(t, strings.Join(*log, "\n"), "create job")
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
	mustContain(t, h.output(), "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and the live agent is not Ready, so heartbeats can stop until the deploy runs again.")
	mustNotContain(t, h.output(), "NOTHING")
}

// readyTimeout runs the operator apply with a rollout wait that times out.
// The pod read after it answers pods (or fails with podsErr), the events
// read answers events (or fails with eventsErr).
func readyTimeout(t *testing.T, pods string, podsErr error, events string, eventsErr error) *harness {
	t.Helper()
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	kubectlLogger(h, func(c execx.Cmd) (bool, error) {
		line := argvLine(c)
		switch {
		case strings.Contains(line, "rollout status"):
			return true, exitErr(1)
		case strings.Contains(line, "get pods -l app.kubernetes.io/name=kubehz-live-agent "):
			io.WriteString(c.Stdout, pods)
			return true, podsErr
		case strings.Contains(line, "get events "):
			io.WriteString(c.Stdout, events)
			return true, eventsErr
		}
		return false, nil
	})
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	mustContain(t, h.output(), "never became Ready")
	mustContain(t, h.output(), "(an image pull that fails or hangs, or an untolerated node taint, is the usual cause)")
	return h
}

const testAgentImage = "ghcr.io/kernpilot/kubehz-agent@sha256:474994f0a64acf179fa5d8524aac8ee963b347ae29407895c8a8f51ec6d064aa"

// A pull that started and never finished (a Pulling event, no Pulled event,
// both for the agent container) is named with its pod, node and image, and
// the hint says how to check the IPv6 path to the registry and how to work
// around it. A pod that is not in a pull state (no node yet) comes first and
// is skipped. A finished pull of another container does not count.
func TestDeployApplyNeverReadyNamesAStalledPull(t *testing.T) {
	pods := "kubehz-live-agent-6b7c9d8f5-q2w4r|||" + testAgentImage + "\n" +
		"kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|ContainerCreating|" + testAgentImage + "\n"
	events := "|Scheduled\nspec.containers{sidecar}|Pulling\nspec.containers{sidecar}|Pulled\nspec.containers{agent}|Pulling\n"
	h := readyTimeout(t, pods, nil, events, nil)
	want := "" +
		"  Pod kubehz-live-agent-6b7c9d8f5-x2k4q on node worker-1 started to pull its image " + testAgentImage + ", and the pull did not finish.\n" +
		"  A pull that hangs or times out can mean that the IPv6 path from the node to ghcr.io is broken.\n" +
		"  On that path, the connection opens but carries no data, and the pull does not try IPv4.\n" +
		"  To check, run these two commands on node worker-1. If only the IPv6 command times out, the IPv6 path is broken:\n" +
		"    curl -6 -sS -o /dev/null --max-time 15 https://ghcr.io/v2/\n" +
		"    curl -4 -sS -o /dev/null --max-time 15 https://ghcr.io/v2/\n" +
		"  ghcr.io sends the image layers from another host. Run the two commands against https://pkg-containers.githubusercontent.com/ too.\n" +
		"  To work around it, make the node reach ghcr.io over IPv4. Repair its IPv6 route, or run the node without IPv6.\n" +
		"  Then delete the pod, so that a new pod pulls the image again: kubectl -n kubehz-system delete pod kubehz-live-agent-6b7c9d8f5-x2k4q\n"
	if !strings.HasSuffix(h.errOut.String(), want) {
		t.Fatalf("stderr does not end with the pull hint:\n%s", h.errOut.String())
	}
	mustNotContain(t, h.output(), "q2w4r on node")
	// The two reads, argv for argv (the bash twin sends the same).
	mustContain(t, strings.Join(h.runner.lines(), "\n"),
		`kubectl -n kubehz-system get pods -l app.kubernetes.io/name=kubehz-live-agent --request-timeout=10s -o jsonpath={range .items[*]}{.metadata.name}|{.spec.nodeName}|{.status.containerStatuses[?(@.name=="agent")].state.waiting.reason}|{.spec.containers[?(@.name=="agent")].image}{"\n"}{end}`)
	mustContain(t, strings.Join(h.runner.lines(), "\n"),
		`kubectl -n kubehz-system get events --field-selector involvedObject.kind=Pod,involvedObject.name=kubehz-live-agent-6b7c9d8f5-x2k4q --request-timeout=10s -o jsonpath={range .items[*]}{.involvedObject.fieldPath}|{.reason}{"\n"}{end}`)
	if n := h.runner.countCalls("get events"); n != 1 {
		t.Fatalf("events reads: %d, want 1 (only for the pod in ContainerCreating)", n)
	}
}

// A pull that failed (ErrImagePull, ImagePullBackOff) is named from the
// waiting reason alone, without an events read. The registry comes from the
// pod's image, so a mirror with a port is the host the commands test.
func TestDeployApplyNeverReadyNamesAFailedPull(t *testing.T) {
	for _, reason := range []string{"ErrImagePull", "ImagePullBackOff"} {
		t.Run(reason, func(t *testing.T) {
			image := "registry.example.com:5000/kubehz-agent@sha256:474994f0"
			h := readyTimeout(t, "kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|"+reason+"|"+image+"\n", nil, "", nil)
			mustContain(t, h.errOut.String(), "  Pod kubehz-live-agent-6b7c9d8f5-x2k4q on node worker-1 cannot pull its image "+image+" ("+reason+"). Read why: kubectl -n kubehz-system describe pod kubehz-live-agent-6b7c9d8f5-x2k4q\n")
			mustContain(t, h.errOut.String(), "the IPv6 path from the node to registry.example.com:5000 is broken")
			mustContain(t, h.errOut.String(), "    curl -6 -sS -o /dev/null --max-time 15 https://registry.example.com:5000/v2/\n")
			// The layer host line belongs to ghcr.io only.
			mustNotContain(t, h.errOut.String(), "pkg-containers")
			if h.runner.anyCall("get events") {
				t.Fatal("a failed pull needs no events read")
			}
		})
	}
}

// No pull hint where the pod's state does not show a pull that failed or
// hangs, and none where the state cannot be read or does not have the
// expected shape: the hint names only what it read.
func TestDeployApplyNeverReadyWithoutAPullNamesNoPull(t *testing.T) {
	pod := "kubehz-live-agent-6b7c9d8f5-x2k4q"
	creating := pod + "|worker-1|ContainerCreating|" + testAgentImage + "\n"
	for name, tc := range map[string]struct {
		pods      string
		podsErr   error
		events    string
		eventsErr error
	}{
		"pull finished, volume wait": {pods: creating, events: "|Scheduled\nspec.containers{agent}|Pulling\nspec.containers{agent}|Pulled\n|FailedMount\n"},
		"no pull started":            {pods: creating, events: "|Scheduled\n|FailedMount\n"},
		"another container pulling":  {pods: creating, events: "|Scheduled\nspec.containers{sidecar}|Pulling\n"},
		"a reason without its field": {pods: creating, events: "Pulling\n"},
		"events unreadable":          {pods: creating, events: "spec.containers{agent}|Pulling\n", eventsErr: exitErr(1)},
		"pod read fails":             {pods: creating, podsErr: exitErr(1), events: "spec.containers{agent}|Pulling\n"},
		"not scheduled":              {pods: pod + "|||" + testAgentImage + "\n"},
		"container running":          {pods: pod + "|worker-1||" + testAgentImage + "\n"},
		"other waiting reason":       {pods: pod + "|worker-1|CrashLoopBackOff|" + testAgentImage + "\n"},
		"escape in the pod name":     {pods: pod + "\x1b[31m|worker-1|ImagePullBackOff|" + testAgentImage + "\n"},
		"escape in the image":        {pods: pod + "|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-agent\x1b[31m@sha256:474994f0\n"},
		"bidi override in the image": {pods: pod + "|worker-1|ImagePullBackOff|ghcr.io/kernpilot/\u202ekubehz-agent@sha256:474994f0\n"},
		"non-ASCII letter in image":  {pods: pod + "|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-ag\u00e9nt@sha256:474994f0\n"},
		"shell text in the image":    {pods: pod + "|worker-1|ImagePullBackOff|ghcr.io/x;$(id)\n"},
		"NUL in the image":           {pods: pod + "|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-agent\x00@sha256:474994f0\n"},
		"NUL in the events reason":   {pods: creating, events: "spec.containers{agent}|Pul\x00ling\n"},
		"a fifth field":              {pods: pod + "|worker-1|ImagePullBackOff|" + testAgentImage + "|x\n"},
		"no pods":                    {},
	} {
		t.Run(name, func(t *testing.T) {
			h := readyTimeout(t, tc.pods, tc.podsErr, tc.events, tc.eventsErr)
			mustNotContain(t, h.output(), "IPv6")
			mustNotContain(t, h.output(), "  Pod ")
		})
	}
}

// The image may be at most liveAgentImageMaxLen bytes: a line with a longer
// image gets no hint. The bash twin has the same limit as a separate check
// (a regex bound above 255 does not compile on every libc), and its bats
// test pins the same boundary.
func TestDeployApplyNeverReadyImageLengthLimit(t *testing.T) {
	pod := "kubehz-live-agent-6b7c9d8f5-x2k4q"
	prefix := "ghcr.io/kernpilot/kubehz-agent:"
	atMax := prefix + strings.Repeat("a", 511-len(prefix))
	if len(atMax) != 511 || liveAgentImageMaxLen != 511 {
		t.Fatalf("image %d bytes, limit %d: the test and the limit disagree", len(atMax), liveAgentImageMaxLen)
	}
	h := readyTimeout(t, pod+"|worker-1|ImagePullBackOff|"+atMax+"\n", nil, "", nil)
	mustContain(t, h.errOut.String(), "  Pod "+pod+" on node worker-1 cannot pull its image "+atMax+" (ImagePullBackOff).")
	h = readyTimeout(t, pod+"|worker-1|ImagePullBackOff|"+atMax+"a\n", nil, "", nil)
	mustNotContain(t, h.output(), "IPv6")
	mustNotContain(t, h.output(), "  Pod ")
}

// The hint reads the agent container of the pods that the Deployment
// selects. Both come from the vendored manifest: this test reads them from
// the embedded live-agent/base/deployment.yaml, so a re-vendor that renames
// the container or changes the selector fails here and does not silently
// turn the hint off.
func TestLiveAgentPullHintFollowsTheVendoredDeployment(t *testing.T) {
	raw, err := fs.ReadFile(Manifests(), "live-agent/base/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"selector"`
			Template struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
				Spec struct {
					Containers []struct {
						Name string `yaml:"name"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers: %d, want 1", len(d.Spec.Template.Spec.Containers))
	}
	container := d.Spec.Template.Spec.Containers[0].Name
	name := d.Spec.Selector.MatchLabels["app.kubernetes.io/name"]
	if name == "" || d.Spec.Template.Metadata.Labels["app.kubernetes.io/name"] != name {
		t.Fatalf("selector app.kubernetes.io/name %q does not select the pod template %v", name, d.Spec.Template.Metadata.Labels)
	}

	pod := "kubehz-live-agent-6b7c9d8f5-x2k4q"
	h := readyTimeout(t, pod+"|worker-1|ContainerCreating|"+testAgentImage+"\n", nil,
		"spec.containers{"+container+"}|Pulling\n", nil)
	mustContain(t, h.errOut.String(), "  Pod "+pod+" on node worker-1 started to pull its image")
	calls := strings.Join(h.runner.lines(), "\n")
	mustContain(t, calls, "get pods -l app.kubernetes.io/name="+name+" ")
	if n := strings.Count(calls, `@.name=="`+container+`"`); n != 2 {
		t.Fatalf("the pod read names container %q %d times, want 2:\n%s", container, n, calls)
	}
}

func TestImageRegistry(t *testing.T) {
	t.Parallel()
	for image, want := range map[string]string{
		testAgentImage: "ghcr.io",
		"registry.example.com:5000/agent@sha256:a": "registry.example.com:5000",
		"localhost/agent:1":                        "localhost",
		"mirror:5000/agent:1":                      "mirror:5000",
		"docker.io/library/busybox:1":              "registry-1.docker.io",
		"library/busybox:1":                        "registry-1.docker.io",
		"busybox@sha256:a":                         "registry-1.docker.io",
	} {
		if got := imageRegistry(image); got != want {
			t.Errorf("imageRegistry(%q) = %q, want %q", image, got, want)
		}
	}
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
	if len(*log) != 6 {
		t.Fatalf("calls: %v", *log)
	}
	mustContain(t, (*log)[0], "delete -k "+filepath.Join(work, "live-agent", "managed")+" --ignore-not-found=true")
	mustContain(t, (*log)[1], "delete deployment -l app.kubernetes.io/part-of=kubehz,app.kubernetes.io/component=live-view")
	mustContain(t, (*log)[2], "apply -k "+filepath.Join(work, "agent"))
	if (*log)[3] != agentJobsDeleteArgv || (*log)[4] != agentJobsPollArgv || (*log)[5] != agentResumeArgv {
		t.Fatalf("want the Job delete, the pod poll and the resume after the CronJob agent apply: %v", *log)
	}
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
		// Only the heartbeat probe fails; the poll for the pods of the
		// CronJob agent's Jobs (fail-hard) finds none.
		if strings.Contains(argvLine(c), "--field-selector=status.phase=Running") {
			io.WriteString(c.Stderr, "Error from server (Forbidden): pods is forbidden\n")
			return exitErr(1)
		}
		if strings.Contains(argvLine(c), "get pods") {
			return nil
		}
		log = append(log, argvLine(c))
		if strings.Contains(argvLine(c), "get secret kubehz-agent ") {
			io.WriteString(c.Stdout, "agent-token")
		}
		return nil
	}
	mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"), h.output())
	mustContain(t, h.output(), "could not check for an in-flight heartbeat pod")
	if log[2] != agentResumeArgv {
		t.Fatalf("the resume must come before the heartbeat probe: %v", log)
	}
	mustContain(t, log[3], "get secret kubehz-agent")
	mustContain(t, log[4], "apply -k "+filepath.Join(work, "live-agent", "managed"))
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
		mustOK(t, h.ctx.waitAgentJobsGone(t.Context()), h.output())
		if h.output() != "" || slept != 0 {
			t.Fatalf("agent Jobs wait tripped on a warning: %q slept=%d", h.output(), slept)
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
