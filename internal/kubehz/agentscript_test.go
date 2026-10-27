package kubehz

// agentscript_test.go: B423. The heartbeat CronJob runs kubectl in its pod
// on the in-cluster config. A kubectl client-config flag (--request-timeout,
// --server, --kubeconfig, --context, --token, ...) makes kubectl skip that
// config and dial localhost:8080, so the call always fails in the pod. This
// was proven on prod with the same image, service account and labels. The
// marker read carried --request-timeout=5s, so it failed on every tick, and
// the 24h assessment gate, the claim nonce and the desired-state poll were
// off. A time bound is `timeout 5 kubectl ...`, outside kubectl.
//
// The tests read the embedded manifests. TestEmbeddedManifestsMatchBashTree
// pins them to .lok8s/libs/kubehz/manifests byte for byte, so the bash tree
// is checked too. tests/unit/kubehz_heartbeat_test.bats runs the script
// against a kubectl stub that refuses these flags the way the pod does.

import (
	"io/fs"
	"regexp"
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
// container script of the embedded manifests sets a client-config flag
// (B423).
func TestAgentKubectlCallsCarryNoClientConfigFlag(t *testing.T) {
	t.Parallel()
	total := 0
	for file, script := range containerScripts(t) {
		for _, c := range kubectlCalls(script) {
			total++
			for _, a := range c.args {
				if f := clientConfigFlag(a); f != "" {
					t.Errorf("%s: script line %d: kubectl %s sets %s. In the pod that skips the in-cluster config and dials localhost:8080; bound the call with `timeout 5 kubectl ...` (B423)",
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

// TestAgentProbesAreBoundedByTimeout: the marker read and every assessment
// probe stay bounded in time, with `timeout <seconds>` (the syntax that
// busybox and coreutils both accept), not with a kubectl flag (B423).
func TestAgentProbesAreBoundedByTimeout(t *testing.T) {
	t.Parallel()
	script := heartbeatScript(t)
	bounded := regexp.MustCompile(`(^|[\s($])timeout 5 $`)

	marker := 0
	for _, c := range kubectlCalls(script) {
		joined := strings.Join(c.args, " ")
		if strings.HasPrefix(joined, "get configmap kubehz-agent-config ") && strings.Contains(joined, "{.metadata.annotations}") {
			marker++
			if !bounded.MatchString(c.prefix) {
				t.Errorf("script line %d: the marker read is not bounded by `timeout 5`: %q kubectl %s", c.line, c.prefix, joined)
			}
		}
	}
	if marker != 1 {
		t.Errorf("%d marker reads found, want 1", marker)
	}

	begin := strings.Index(script, `if [ -n "${ASSESS_NOW}" ]; then`)
	end := strings.Index(script, "ASSESSMENT=$(jq ")
	if begin < 0 || end < begin {
		t.Fatalf("the assessment block was not found (begin %d, end %d)", begin, end)
	}
	probes := kubectlCalls(script[begin:end])
	for _, c := range probes {
		if !bounded.MatchString(c.prefix) {
			t.Errorf("assessment probe is not bounded by `timeout 5`: %q kubectl %s", c.prefix, strings.Join(c.args, " "))
		}
	}
	if len(probes) < 12 {
		t.Errorf("%d assessment probes found, want at least 12", len(probes))
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
	if len(got) != len(want) {
		t.Fatalf("runtimeLoops = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runtimeLoops = %v, want %v", got, want)
		}
	}
}

// TestHeartbeatKubectlCallsDoNotGrowWithTheCluster (B448): a kubectl call
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
			t.Errorf("script line %d: kubectl %s reads one node; a call per node grows with the cluster (B448)", c.line, joined)
		}
		for _, l := range loops {
			if c.line >= l[0] && c.line <= l[1] {
				t.Errorf("script line %d: kubectl %s runs in the loop at lines %d-%d; the call count grows with the cluster (B448)", c.line, joined, l[0], l[1])
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
	switch {
	case args[1] == "node" || args[1] == "nodes":
		return !strings.HasPrefix(args[2], "-")
	case args[1] == "--raw":
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
