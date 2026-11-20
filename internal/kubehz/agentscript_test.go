package kubehz

// agentscript_test.go: the heartbeat CronJob runs kubectl in its pod on the
// in-cluster config. A kubectl client-config flag (--request-timeout,
// --server, --kubeconfig, --context, --token, ...) makes kubectl skip that
// config and dial localhost:8080, so the call always fails in the pod. This
// was proven on prod with the same image, service account and labels. The
// marker read carried --request-timeout=5s, so it failed on every tick, and
// the 24h assessment gate, the claim nonce and the desired-state poll were
// off. A time bound is `timeout -k 2 5 kubectl ...`, outside kubectl.
//
// The tests read the embedded manifests. TestEmbeddedManifestsMatchBashTree
// pins them to .lok8s/libs/kubehz/manifests byte for byte, so the bash tree
// is checked too. tests/unit/kubehz_heartbeat_test.bats runs the script
// against a kubectl stub that refuses these flags the way the pod does.

import (
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// clientConfigFlags are the kubectl flags that override the client config
// (client-go clientcmd: the kubeconfig, context, cluster, auth info and
// timeout overrides). Each one makes kubectl ignore the in-cluster config.
// -n/--namespace is not here: it does not change the REST config.
var clientConfigFlags = []string{
	"--kubeconfig", "--context", "--cluster", "--user",
	"--server", "-s", "--token", "--username", "--password",
	"--certificate-authority", "--client-certificate", "--client-key",
	"--insecure-skip-tls-verify", "--tls-server-name",
	"--as", "--as-group", "--as-uid",
	"--request-timeout", "--disable-compression",
}

// kubectlCall is one kubectl invocation in a script.
type kubectlCall struct {
	line   int    // the 1-based script line where the logical line starts
	prefix string // the text before "kubectl" on the logical line
	args   []string
}

var kubectlWord = regexp.MustCompile(`(^|[\s($|;&])kubectl(\s|$)`)

// kubectlCalls finds every kubectl invocation in a POSIX sh script. Lines
// that end in a backslash are joined first, comment lines are skipped. The
// arguments end at an unquoted | ; ) or &, and a redirection is not an
// argument.
func kubectlCalls(script string) []kubectlCall {
	var calls []kubectlCall
	lines := strings.Split(script, "\n")
	for i := 0; i < len(lines); i++ {
		start := i
		logical := lines[i]
		for strings.HasSuffix(logical, `\`) && i+1 < len(lines) {
			i++
			logical = strings.TrimSuffix(logical, `\`) + " " + lines[i]
		}
		if strings.HasPrefix(strings.TrimSpace(logical), "#") {
			continue
		}
		for _, m := range kubectlWord.FindAllStringSubmatchIndex(logical, -1) {
			at := m[3] // the end of the leading group: "kubectl" starts here
			calls = append(calls, kubectlCall{
				line:   start + 1,
				prefix: logical[:at],
				args:   shellWords(logical[at+len("kubectl"):]),
			})
		}
	}
	return calls
}

// shellWords splits the text after a command name into its words, quote
// aware, up to the first unquoted | ; ) or &. A word with an unquoted < or
// > is a redirection and is dropped.
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	quote := byte(0)
	inWord, redirect := false, false
	flush := func() {
		if inWord && !redirect {
			words = append(words, cur.String())
		}
		cur.Reset()
		inWord, redirect = false, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			inWord = true
			cur.WriteByte(c)
		case c == '|' || c == ';' || c == ')' || c == '&':
			flush()
			return words
		case c == ' ' || c == '\t':
			flush()
		case c == '<' || c == '>':
			inWord, redirect = true, true
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()
	return words
}

// clientConfigFlag returns the client-config flag that arg sets, or "".
func clientConfigFlag(arg string) string {
	for _, f := range clientConfigFlags {
		if arg == f || strings.HasPrefix(arg, f+"=") {
			return f
		}
	}
	return ""
}

// containerScripts returns the command and args strings of every container
// and init container in the embedded manifests, keyed by file.
func containerScripts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(Manifests(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		raw, err := fs.ReadFile(Manifests(), path)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var doc any
			if dec.Decode(&doc) != nil {
				break
			}
			for _, s := range collectContainerStrings(doc) {
				out[path] += s + "\n"
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func collectContainerStrings(node any) []string {
	var out []string
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "containers" || k == "initContainers" {
				list, _ := v.([]any)
				for _, c := range list {
					cm, _ := c.(map[string]any)
					for _, field := range []string{"command", "args"} {
						items, _ := cm[field].([]any)
						for _, it := range items {
							if s, ok := it.(string); ok {
								out = append(out, s)
							}
						}
					}
				}
				continue
			}
			out = append(out, collectContainerStrings(v)...)
		}
	case []any:
		for _, v := range n {
			out = append(out, collectContainerStrings(v)...)
		}
	}
	return out
}

// heartbeatScript is the CronJob's `sh -c` script.
func heartbeatScript(t *testing.T) string {
	t.Helper()
	s := containerScripts(t)["agent/cronjob.yaml"]
	if !strings.Contains(s, "set -e") {
		t.Fatal("agent/cronjob.yaml: the heartbeat script was not found")
	}
	return s
}

func TestKubectlCallsSeesAFlagOnAContinuedLine(t *testing.T) {
	t.Parallel()
	script := "# kubectl get x --request-timeout=5s (a comment)\n" +
		"A=$(kubectl get pods -n kube-system \\\n  -o jsonpath='{.a|b}' --request-timeout=5s 2>/dev/null) || A=\"\"\n" +
		"timeout 5 kubectl get crd x >/dev/null 2>&1 && B=true\n" +
		"kubectl -s https://x get nodes | wc -l\n"
	calls := kubectlCalls(script)
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3: %+v", len(calls), calls)
	}
	if got := strings.Join(calls[0].args, " "); got != "get pods -n kube-system -o jsonpath='{.a|b}' --request-timeout=5s" {
		t.Errorf("call 1 args = %q", got)
	}
	if clientConfigFlag(calls[0].args[len(calls[0].args)-1]) != "--request-timeout" {
		t.Error("--request-timeout=5s is not seen as a client-config flag")
	}
	if !strings.HasSuffix(calls[1].prefix, "timeout 5 ") || strings.Join(calls[1].args, " ") != "get crd x" {
		t.Errorf("call 2 = %+v", calls[1])
	}
	if clientConfigFlag(calls[2].args[0]) != "-s" {
		t.Errorf("call 3: -s is not seen as a client-config flag: %+v", calls[2])
	}
	if clientConfigFlag("-n") != "" || clientConfigFlag("--namespace=x") != "" {
		t.Error("the namespace flag does not change the REST config")
	}
}

// TestAgentKubectlCallsCarryNoClientConfigFlag: no kubectl call in any
// container script of the embedded manifests sets a client-config flag.
func TestAgentKubectlCallsCarryNoClientConfigFlag(t *testing.T) {
	t.Parallel()
	total := 0
	for file, script := range containerScripts(t) {
		for _, c := range kubectlCalls(script) {
			total++
			for _, a := range c.args {
				if f := clientConfigFlag(a); f != "" {
					t.Errorf("%s: script line %d: kubectl %s sets %s. In the pod that skips the in-cluster config and dials localhost:8080; bound the call with `timeout -k 2 5 kubectl ...`",
						file, c.line, strings.Join(c.args, " "), f)
				}
			}
		}
	}
	// The heartbeat script alone has more than 30 calls. Fewer means the
	// scan reads nothing.
	if total < 30 {
		t.Fatalf("the scan found %d kubectl calls, want at least 30", total)
	}
}

// The bounded helper of the heartbeat script: it runs a call under
// `timeout -k 2 SECS` only when the call fits in the tick budget.
const boundedHelper = `bounded() {
  if ! budget_fits $(( $1 + 2 )); then
    echo "kubehz: tick budget: ${2} ${3} not started: it can take $(( $1 + 2 ))s, and the budget ends in $(( BUDGET_END - $(date -u +%s) ))s" >&4
    return 124
  fi
  BOUND_SECS="$1"
  shift
  timeout -k 2 "${BOUND_SECS}" "$@"
}
`

// TestAgentKubectlCallsAreBoundedByTimeout: every kubectl call of the
// heartbeat script is bounded in time, with `bounded 5` right before
// kubectl, and the node list with `bounded 30`: `timeout -k 2 5` (30), TERM
// after 5 s (30 s), then KILL 2 s later, and only when the call fits in the
// tick budget. The BusyBox applet and GNU coreutils both take that syntax.
// The one exception is the write-back of the send gate, which runs outside
// the budget under a plain `timeout -k 2 5`. The bound is never a kubectl
// flag (TestAgentKubectlCallsCarryNoClientConfigFlag). A call without the
// bound, the identity Secret calls, the marker writes and the nonce clear
// included, can hold the tick until activeDeadlineSeconds kills it.
func TestAgentKubectlCallsAreBoundedByTimeout(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	if !strings.Contains(script, boundedHelper) {
		t.Fatal("the script must define bounded(): the budget check, then `timeout -k 2 SECS`")
	}
	bound := regexp.MustCompile(`(^|[\s($|])(bounded|timeout -k 2) (\d+) $`)
	calls := kubectlCalls(script)
	marker, nodeList, plain := 0, 0, 0
	for _, c := range calls {
		joined := strings.Join(c.args, " ")
		want := "5"
		if joined == "get --raw /api/v1/nodes" {
			nodeList++
			want = "30"
		}
		if strings.HasPrefix(joined, "get configmap kubehz-agent-state ") && strings.Contains(joined, "{.metadata.annotations}") {
			marker++
		}
		m := bound.FindStringSubmatch(c.prefix)
		if m == nil {
			t.Errorf("script line %d: kubectl %s is not bounded by `bounded %s`: %q", c.line, joined, want, c.prefix)
			continue
		}
		if m[3] != want {
			t.Errorf("script line %d: kubectl %s runs under `%s %s`, want `bounded %s`", c.line, joined, m[2], m[3], want)
		}
		if m[2] == "timeout -k 2" {
			plain++
			if !slices.Contains(c.args, `"${WB_LAST}"`) {
				t.Errorf("script line %d: kubectl %s runs outside the tick budget: only the write-back may", c.line, joined)
			}
		}
	}
	if marker != 1 || nodeList != 1 || plain != 1 {
		t.Errorf("%d marker reads, %d node lists and %d calls outside the budget found, want 1, 1 and 1", marker, nodeList, plain)
	}
	// The heartbeat script has more than 30 kubectl calls. Fewer means the
	// scan reads nothing.
	if len(calls) < 30 {
		t.Fatalf("the scan found %d kubectl calls, want at least 30", len(calls))
	}
}

// TestAgentCurlCallsAreBounded: each curl call of the heartbeat script runs
// under `bounded`: the agent-register POST and the desired-state poll with
// 10 s, each heartbeat POST with 15 s. curl has no time limit of its own
// here, so a call without the bound can hold the tick until
// activeDeadlineSeconds kills it, and the send gate is not put back.
func TestAgentCurlCallsAreBounded(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	curlWord := regexp.MustCompile(`(^|[\s($|;&])curl(\s|$)`)
	bound := regexp.MustCompile(`(^|[\s($|])bounded (\d+) $`)
	got := map[string]int{}
	for i, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range curlWord.FindAllStringSubmatchIndex(line, -1) {
			at := m[3]
			rest := line[at:]
			// "curl" inside a quoted log line is not a call.
			if strings.Count(line[:at], `"`)%2 == 1 {
				continue
			}
			b := bound.FindStringSubmatch(line[:at])
			if b == nil {
				t.Errorf("script line %d: %s is not bounded", i+1, strings.TrimSpace(rest))
				continue
			}
			switch {
			case strings.Contains(rest, "/agent-register"):
				got["register "+b[2]]++
			case strings.Contains(rest, "/heartbeat"):
				got["heartbeat "+b[2]]++
			case strings.Contains(rest, `-K - "$@"`):
				got["desired "+b[2]]++
			default:
				t.Errorf("script line %d: an unknown curl call: %s", i+1, strings.TrimSpace(rest))
			}
		}
	}
	want := map[string]int{"register 10": 1, "heartbeat 15": 2, "desired 10": 1}
	if !maps.Equal(got, want) {
		t.Errorf("the curl calls and their bounds: got %v, want %v", got, want)
	}
}

// budgetNumber reads one integer of the heartbeat script: the first match
// of re (one group).
func budgetNumber(t *testing.T, script, re string) int {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("the script has no match for %s", re)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAgentTickBudgetEndsBeforeTheJobDeadline: the numbers of the tick
// budget. The script ends at the latest when the budget ends plus the one
// write-back outside it (7 s), and that must leave time for the pod to
// start before activeDeadlineSeconds kills it. The reads before the
// heartbeat leave room for one heartbeat POST, the probes for two (the
// POST and its plain retry). The bounds alone do not fit: with every call
// at its bound, a tick with an assessment takes far longer than the
// deadline, which is why the budget exists.
func TestAgentTickBudgetEndsBeforeTheJobDeadline(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	raw, err := fs.ReadFile(Manifests(), "agent/cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cj struct {
		Spec struct {
			JobTemplate struct {
				Spec struct {
					ActiveDeadlineSeconds int `yaml:"activeDeadlineSeconds"`
				} `yaml:"spec"`
			} `yaml:"jobTemplate"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &cj); err != nil {
		t.Fatal(err)
	}
	deadline := cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds
	tick := budgetNumber(t, script, `\n\s*TICK_END=\$\(\( \$\(date -u \+%s\) \+ (\d+) \)\)\n`)
	readReserve := budgetNumber(t, script, `\n\s*READ_END=\$\(\( TICK_END - (\d+) \)\)\n`)
	probeReserve := budgetNumber(t, script, `\n\s*PROBE_END=\$\(\( TICK_END - (\d+) \)\)\n`)
	const kill, writeBack, podStart = 2, 5 + 2, 20
	heartbeat := budgetNumber(t, script, `bounded (\d+) curl -q -fsSL -X POST "\$\{KUBEHZ_API_URL\}/api/clusters/\$\{CLUSTER_ID\}/heartbeat"`) + kill
	if end := tick + writeBack; end+podStart > deadline {
		t.Errorf("the script can run %d s (budget %d s + write-back %d s); with %d s for the pod to start that passes activeDeadlineSeconds %d", end, tick, writeBack, podStart, deadline)
	}
	if readReserve < heartbeat {
		t.Errorf("READ_END leaves %d s, want room for one heartbeat POST (%d s)", readReserve, heartbeat)
	}
	if probeReserve < 2*heartbeat {
		t.Errorf("PROBE_END leaves %d s, want room for the POST and its retry (%d s)", probeReserve, 2*heartbeat)
	}
	// The sum of the bounds of a tick with an assessment: every kubectl call
	// at 5 s + 2 s (the node list at 30 s + 2 s), every curl call at its own.
	sum := 0
	for _, c := range kubectlCalls(script) {
		if strings.Join(c.args, " ") == "get --raw /api/v1/nodes" {
			sum += 30 + kill
		} else {
			sum += 5 + kill
		}
	}
	sum += 2*heartbeat + 2*(10+kill)
	if sum <= deadline {
		t.Errorf("the bounds sum to %d s, not more than activeDeadlineSeconds %d: the budget is no longer needed, or the scan reads too little", sum, deadline)
	}
	// The budget switches: the gate and the probes on PROBE_END, the
	// heartbeat and later on TICK_END, after the probes.
	gate := strings.Index(script, `BUDGET_END="${PROBE_END}"`)
	probesDone := strings.Index(script, `[ -n "${ASSESSMENT}" ] || assess_write_back`)
	full := strings.Index(script, `BUDGET_END="${TICK_END}"`)
	post := strings.Index(script, `bounded 15 curl -q -fsSL -X POST`)
	start := strings.Index(script, `BUDGET_END="${READ_END}"`)
	if start < 0 || gate < start || probesDone < gate || full < probesDone || post < full {
		t.Errorf("the budget order must be READ_END, then PROBE_END for the gate and the probes, then TICK_END before the heartbeat POST: %d %d %d %d %d", start, gate, probesDone, full, post)
	}
}

// loopHead matches the head of a sh loop up to its `do`, also after a pipe
// (`... | while read -r x; do`). loopDone matches a `done` later on the same
// line (a one-line loop). loopEnd matches a `done` that starts a line.
var (
	loopHead = regexp.MustCompile(`(?:^|[|;&(])\s*(for\s+\w+\s+in\s+([^;]*)|while\s[^;]*|until\s[^;]*);\s*do\b`)
	loopDone = regexp.MustCompile(`;\s*done\b`)
	loopEnd  = regexp.MustCompile(`^\s*done\b`)
)

// runtimeLoops returns the 1-based [first, last] line ranges of the loops in
// a POSIX sh script whose iteration count is known only at run time: every
// while or until loop, and every for loop whose word list expands something.
// A for loop over literal words is fixed. A fixed loop nested in a
// runtime-sized loop is part of its range.
func runtimeLoops(script string) [][2]int {
	var out [][2]int
	lines := strings.Split(script, "\n")
	for i, l := range lines {
		m := loopHead.FindStringSubmatch(l)
		if m == nil || (strings.HasPrefix(m[1], "for") && !strings.ContainsAny(m[2], "$`")) {
			continue
		}
		depth := 0
		for j := i; j < len(lines); j++ {
			switch {
			case loopHead.MatchString(lines[j]) && loopDone.MatchString(lines[j]):
				// A one-line loop opens and closes on this line.
			case loopHead.MatchString(lines[j]):
				depth++
			case loopEnd.MatchString(lines[j]):
				depth--
			}
			if depth == 0 {
				out = append(out, [2]int{i + 1, j + 1})
				break
			}
		}
	}
	return out
}

func TestRuntimeLoopsFindsOnlyRuntimeSizedLoops(t *testing.T) {
	t.Parallel()
	script := "for comp in a b; do\n  kubectl get pods\ndone\n" +
		"for node in ${NODES}; do\n  kubectl get node \"${node}\"\ndone\n" +
		"printf x | while read -r n; do\n  :\ndone\n" +
		"for n in ${NODES}; do kubectl get nodes \"${n}\"; done\n" +
		"for c in a b; do kubectl get pods; done\n"
	got := runtimeLoops(script)
	want := [][2]int{{4, 6}, {7, 9}, {10, 10}}
	if !slices.Equal(got, want) {
		t.Fatalf("runtimeLoops = %v, want %v", got, want)
	}
}

// TestHeartbeatKubectlCallsDoNotGrowWithTheCluster: a kubectl call
// takes ~1.5 s on prod. The heartbeat made 3 calls per node, so at ~18 nodes
// the tick ran past activeDeadlineSeconds (120 s) and the job was killed
// before the POST. The node list must be one call, no call may read a
// single node, and no kubectl call may sit in a runtime-sized loop.
func TestHeartbeatKubectlCallsDoNotGrowWithTheCluster(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	loops := runtimeLoops(script)
	nodeLists := 0
	for _, c := range kubectlCalls(script) {
		joined := strings.Join(c.args, " ")
		if joined == "get --raw /api/v1/nodes" {
			nodeLists++
		}
		if readsOneNode(c.args) {
			t.Errorf("script line %d: kubectl %s reads one node; a call per node grows with the cluster", c.line, joined)
		}
		for _, l := range loops {
			if c.line >= l[0] && c.line <= l[1] {
				t.Errorf("script line %d: kubectl %s runs in the loop at lines %d-%d; the call count grows with the cluster", c.line, joined, l[0], l[1])
			}
		}
	}
	if nodeLists != 1 {
		t.Errorf("%d `kubectl get --raw /api/v1/nodes` calls found, want 1", nodeLists)
	}
}

// readsOneNode reports a kubectl call that reads a single node: `get node
// X`, `get nodes X` (a name, not a flag) or `get --raw /api/v1/nodes/X`.
func readsOneNode(args []string) bool {
	if len(args) < 3 || args[0] != "get" {
		return false
	}
	switch args[1] {
	case "node", "nodes":
		return !strings.HasPrefix(args[2], "-")
	case "--raw":
		return strings.HasPrefix(args[2], "/api/v1/nodes/")
	}
	return false
}

func TestReadsOneNode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		call string
		want bool
	}{
		{"get node cp-1", true},
		{`get nodes "${n}"`, true},
		{"get --raw /api/v1/nodes/cp-1", true},
		{"get --raw /api/v1/nodes", false},
		{"get nodes -o json", false},
		{"get nodes -l node-role.kubernetes.io/control-plane -o name", false},
		{"get pods -n kube-system", false},
	} {
		if got := readsOneNode(strings.Fields(tc.call)); got != tc.want {
			t.Errorf("readsOneNode(%q) = %v, want %v", tc.call, got, tc.want)
		}
	}
}
