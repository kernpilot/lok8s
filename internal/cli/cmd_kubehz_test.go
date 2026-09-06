package cli

// cmd_kubehz_test.go — the `lo kubehz` subtree cannot drift from the argsh
// usage arrays (main::kubehz, kubehz::node, kubehz::handover) while both
// exist: names, aliases, @destructive/@readonly markers and short text are
// read from the bash and compared. Plus the hand parser for `handover
// receive` and the cluster-free command paths.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
)

// parseUsageArray extracts the usage entries of one bash function.
func parseUsageArray(t *testing.T, file, fn string) map[string]commandSpec {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, fn+"() {")
	if start < 0 {
		t.Fatalf("%s not found in %s", fn, file)
	}
	text = text[start:]
	start = strings.Index(text, "local -a usage=(")
	if start < 0 {
		t.Fatalf("usage array not found for %s", fn)
	}
	specs := map[string]commandSpec{}
	for _, line := range strings.Split(text[start:], "\n") {
		if strings.TrimSpace(line) == ")" {
			break
		}
		m := usageEntry.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		spec := commandSpec{use: m[2], short: m[6],
			destructive: strings.Contains(m[5], "@destructive"), readonly: strings.Contains(m[5], "@readonly")}
		if m[4] != "" {
			spec.aliases = []string{m[4]}
		}
		specs[spec.use] = spec
	}
	if len(specs) == 0 {
		t.Fatalf("no entries parsed for %s", fn)
	}
	return specs
}

func assertTreeMatches(t *testing.T, parent *cobra.Command, want map[string]commandSpec) {
	t.Helper()
	got := map[string]*cobra.Command{}
	for _, c := range parent.Commands() {
		got[c.Name()] = c
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s %q exists in bash but not in Go", parent.Name(), name)
			continue
		}
		if len(w.aliases) > 0 && (len(g.Aliases) == 0 || g.Aliases[0] != w.aliases[0]) {
			t.Errorf("%s %q: alias bash %v go %v", parent.Name(), name, w.aliases, g.Aliases)
		}
		if g.Short != w.short {
			t.Errorf("%s %q: short drift:\n  bash: %s\n  go:   %s", parent.Name(), name, w.short, g.Short)
		}
		if (g.Annotations[AnnotationDestructive] == "true") != w.destructive || (g.Annotations[AnnotationReadonly] == "true") != w.readonly {
			t.Errorf("%s %q: annotation mismatch bash {d:%v r:%v} go %v", parent.Name(), name, w.destructive, w.readonly, g.Annotations)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s %q exists in Go but not in bash", parent.Name(), name)
		}
	}
}

func TestKubehzTreeMatchesArgshUsage(t *testing.T) {
	lib := filepath.Join("..", "..", ".lok8s", "libs", "kubehz")
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	kh, _, err := root.Find([]string{"kubehz"})
	if err != nil {
		t.Fatal(err)
	}
	assertTreeMatches(t, kh, parseUsageArray(t, filepath.Join(lib, "main"), "main::kubehz"))
	node, _, _ := root.Find([]string{"kubehz", "node"})
	assertTreeMatches(t, node, parseUsageArray(t, filepath.Join(lib, "node"), "kubehz::node"))
	ho, _, _ := root.Find([]string{"kubehz", "handover"})
	assertTreeMatches(t, ho, parseUsageArray(t, filepath.Join(lib, "handover"), "kubehz::handover"))
	for _, path := range [][]string{{"kh", "s"}, {"kubehz", "n", "j"}, {"kubehz", "h", "r"}, {"kubehz", "c"}} {
		if _, _, err := root.Find(path); err != nil {
			t.Errorf("alias path %v: %v", path, err)
		}
	}
}

// The receive flags are real cobra flags now (help, completion and the MCP
// schema see them); -s must still mean --snapshot, not the global --cluster.
func TestHandoverReceiveFlagsBindLikeTheBashSpec(t *testing.T) {
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	cmd, _, err := root.Find([]string{"kubehz", "handover", "receive"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"--bundle", "/b", "-s", "/snap", "--single-node", "-f", "--domain=x.dev", "-v"}); err != nil {
		t.Fatal(err)
	}
	f := cmd.Flags()
	bundle, _ := f.GetString("bundle")
	snapshot, _ := f.GetString("snapshot")
	single, _ := f.GetBool("single-node")
	force, _ := f.GetBool("force")
	domain, _ := f.GetString("domain")
	verbose, _ := f.GetCount("verbose")
	if bundle != "/b" || snapshot != "/snap" || !single || !force || domain != "x.dev" || verbose != 1 {
		t.Fatalf("bundle=%q snapshot=%q single=%v force=%v domain=%q verbose=%d", bundle, snapshot, single, force, domain, verbose)
	}
	// -s means --snapshot here, shadowing the global --cluster shorthand.
	if cluster, _ := f.GetString("cluster"); cluster != "" {
		t.Fatal("-s must not bind to --cluster")
	}
	// Parse failures keep the argsh error shape.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"kubehz", "handover", "receive", "--bundle"}, "flag needs an argument: --bundle"},
		{[]string{"kubehz", "handover", "receive", "--nope"}, "unknown flag: --nope"},
		{[]string{"kubehz", "handover", "receive", "-b"}, "missing value for flag: bundle"},
		{[]string{"kubehz", "handover", "receive"}, "missing required flag: bundle"},
	} {
		_, stderr, err := runLo(t, NewRoot(&config.Paths{Base: t.TempDir()}), tc.args...)
		if err == nil || !strings.Contains(stderr, "Error: "+tc.want) {
			t.Errorf("%v: err=%v stderr=%q", tc.args, err, stderr)
		}
	}
}

// runKubehzLo executes the root against a synthetic project and returns
// stdout, stderr and the error.
func runKubehzLo(t *testing.T, base string, args ...string) (string, string, error) {
	t.Helper()
	paths := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	root := NewRoot(paths)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestKubehzCommandsClusterFreePaths(t *testing.T) {
	base := t.TempDir()
	t.Setenv("DOMAIN_NAME", "")
	t.Setenv("KUBEHZ_TOKEN", "")
	_ = os.MkdirAll(filepath.Join(base, "clusters", "alpha.dev"), 0o755)
	_ = os.WriteFile(filepath.Join(base, "clusters", "alpha.dev", "cluster.lok8s.yaml"), []byte("kind: Lo\nmetadata:\n  name: alpha\n"), 0o644)
	_ = os.WriteFile(filepath.Join(base, "clusters", ".active"), []byte("alpha.dev\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(base, "clusters", "shared.dev"), 0o755)
	_ = os.WriteFile(filepath.Join(base, "clusters", "shared.dev", "cluster.lok8s.yaml"), []byte("kind: Kubehz\nspec:\n  kubehz:\n    hosting: shared\n    apiUrl: https://api.kubehz.example\n"), 0o644)

	out, _, err := runKubehzLo(t, base, "kubehz", "status")
	if err != nil || out != "Domain:  alpha.dev\nHosting: self\nAccess:  none\nAPI URL: <not set>\nStatus:  not registered (access: none)\n" {
		t.Fatalf("status: %v\n%s", err, out)
	}
	_, errOut, err := runKubehzLo(t, base, "kubehz", "register")
	if err != ErrHandled || !strings.Contains(errOut, "spec.kubehz.access is 'none' — nothing to register") {
		t.Fatalf("register: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "join")
	if err != ErrHandled || !strings.Contains(errOut, "Error: missing required argument: node") {
		t.Fatalf("join: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "join", "n1", "--domain", "shared.dev")
	if err != ErrHandled || !strings.Contains(errOut, "KUBEHZ_TOKEN is required to mint a join ticket") {
		t.Fatalf("join shared: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "claim")
	if err != ErrHandled || !strings.Contains(errOut, "Error: missing required flag: nonce") {
		t.Fatalf("claim: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "node", "join", "--cluster", "x")
	if err != ErrHandled || !strings.Contains(errOut, "--cluster/-s names the kind cluster") {
		t.Fatalf("node join --cluster: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "node", "join", "-scl-x")
	if err != ErrHandled || !strings.Contains(errOut, "--cluster-id cl-xxxxxxxx") {
		t.Fatalf("node join -scl-x: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "node", "remove")
	if err != ErrHandled || !strings.Contains(errOut, "Error: missing required flag: name") {
		t.Fatalf("node remove: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "handover", "receive")
	if err != ErrHandled || !strings.Contains(errOut, "Error: missing required flag: bundle") {
		t.Fatalf("receive: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "handover", "preseed", "--bundle", "/nonexistent")
	if err != ErrHandled || !strings.Contains(errOut, "Error: missing required flag: node") {
		t.Fatalf("preseed: %v %s", err, errOut)
	}
	_, errOut, err = runKubehzLo(t, base, "kubehz", "bogus")
	if err != ErrHandled || !strings.Contains(errOut, "Error: Invalid command: bogus") {
		t.Fatalf("bogus: %v %s", err, errOut)
	}
}
