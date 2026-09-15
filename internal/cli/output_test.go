package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
)

// runOut runs the tree and returns stdout, stderr and the error.
func runOut(t *testing.T, paths *config.Paths, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("DOMAIN_NAME", "")
	root := NewRoot(paths)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestUseOutputJSONAndYAML(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)

	out, errOut, err := runOut(t, paths, "use", "-o", "json")
	if err != nil || errOut != "" {
		t.Fatalf("lo use -o json: %v %q", err, errOut)
	}
	want := "{\n  \"active\": \"alpha.dev\",\n  \"domains\": [\n" +
		"    {\n      \"name\": \"alpha.dev\",\n      \"kind\": \"lo\"\n    },\n" +
		"    {\n      \"name\": \"beta.cloud\",\n      \"kind\": \"lo\"\n    },\n" +
		"    {\n      \"name\": \"gamma.app\",\n      \"kind\": \"deploy\",\n      \"clusterRef\": \"?\"\n    }\n  ]\n}\n"
	if out != want {
		t.Errorf("lo use -o json:\n--- got ---\n%s--- want ---\n%s", out, want)
	}

	out, _, err = runOut(t, paths, "use", "--output", "yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if yaml.Unmarshal([]byte(out), &doc) != nil || doc["active"] != "alpha.dev" || !strings.HasPrefix(out, "active: alpha.dev\ndomains:\n  - name: alpha.dev\n    kind: lo\n") {
		t.Errorf("lo use -o yaml:\n%s", out)
	}

	// text stays the default and unchanged.
	text, _, _ := runOut(t, paths, "use")
	if !strings.HasPrefix(text, "Active: alpha.dev\n\nAvailable domains:\n  alpha.dev (lo)\n") {
		t.Errorf("lo use text:\n%s", text)
	}
	// An unknown format is a parse error in the argsh shape.
	if _, errOut, err := runOut(t, paths, "use", "-o", "xml"); err == nil || !strings.Contains(errOut, `Error: invalid --output "xml": text, json or yaml`) {
		t.Errorf("lo use -o xml: err %v, stderr %q", err, errOut)
	}
}

func TestUseErrorShapeOffATerminalIsTheLegacyLine(t *testing.T) {
	paths := completionProject(t)
	_, errOut, err := runOut(t, paths, "use", "nosuch.dev")
	if err == nil || errOut != "[error] domain not found: clusters/nosuch.dev/ (no cluster.lok8s.yaml or deploy.lok8s.yaml)\n" {
		t.Errorf("lo use nosuch.dev piped: err %v, stderr %q", err, errOut)
	}
}

func TestVersionOutputJSON(t *testing.T) {
	paths := &config.Paths{Base: t.TempDir()}
	out, _, err := runOut(t, paths, "version", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Lok8s string `json:"lok8s"`
		Build string `json:"build"`
		Tools []struct {
			Name, Version, Path string
		} `json:"tools"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil || doc.Lok8s == "" || (doc.Build != "core" && doc.Build != "full") || doc.Tools == nil {
		t.Errorf("lo version -o json:\n%s", out)
	}
}

func TestAddonsOutputJSON(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	out, errOut, err := runOut(t, paths, "addons", "-o", "json")
	if err != nil {
		t.Fatalf("lo addons -o json: %v %q", err, errOut)
	}
	var entries []struct {
		Name, Type, Version, Origin, Path string
	}
	if json.Unmarshal([]byte(out), &entries) != nil || len(entries) == 0 {
		t.Fatalf("lo addons -o json:\n%s", out)
	}
	found := false
	for _, e := range entries {
		if e.Name == "cilium" {
			found = e.Origin == "builtin" && e.Type != "" && e.Path != ""
		}
	}
	if !found {
		t.Errorf("lo addons -o json: no builtin cilium row:\n%s", out)
	}
	if _, errOut, err := runOut(t, paths, "addons", "cilium", "-o", "json"); err == nil || !strings.Contains(errOut, "--output json applies to the list") {
		t.Errorf("lo addons cilium -o json: err %v, stderr %q", err, errOut)
	}
}

func TestAssetsListOutputYAML(t *testing.T) {
	paths := completionProject(t)
	out, _, err := runOut(t, paths, "assets", "list", "-o", "yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Lo     string           `yaml:"lo"`
		Assets []map[string]any `yaml:"assets"`
	}
	if yaml.Unmarshal([]byte(out), &doc) != nil || doc.Lo == "" || len(doc.Assets) == 0 || doc.Assets[0]["rel"] == nil {
		t.Errorf("lo assets list -o yaml:\n%s", out)
	}
	jsonOut, _, _ := runOut(t, paths, "assets", "list", "-o", "json")
	legacy, _, _ := runOut(t, paths, "assets", "list", "--json")
	if jsonOut != legacy {
		t.Errorf("-o json and --json differ:\n%s\n---\n%s", jsonOut, legacy)
	}
}

func TestStatusOutputJSON(t *testing.T) {
	paths := completionProject(t)
	os.MkdirAll(filepath.Join(paths.Clusters, "alpha.dev", "targets", "networking"), 0o755)
	os.WriteFile(filepath.Join(paths.Clusters, "alpha.dev", "artifacts.yaml"), []byte("---\n"), 0o644)
	os.WriteFile(filepath.Join(paths.Base, ".tilt.pid"), []byte("4242\n"), 0o644)
	deps := statusDeps{
		paths: paths,
		dispatchStatus: func(_ context.Context, out io.Writer, d string) error {
			io.WriteString(out, "\033[1mkind cluster alpha\033[0m: running\n")
			return nil
		},
		hasKubectl: func() bool { return false },
		pidAlive:   func(pid string) bool { return pid == "4242" },
	}
	r := gatherStatus(context.Background(), deps, "alpha.dev")
	raw, _ := json.Marshal(r)
	want := `{"domain":"alpha.dev","driver":"lo","cluster":["kind cluster alpha: running"],"nodes":[],"inventory":null,"targets":["networking"],"artifactsBuilt":true,"tilt":{"running":true,"pid":"4242"}}`
	if string(raw) != want {
		t.Errorf("status report:\n--- got ---\n%s\n--- want ---\n%s", raw, want)
	}
	nodes := parseNodes(`{"items":[{"metadata":{"name":"n1","labels":{"node-role.kubernetes.io/control-plane":""}},"status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.1"}],"nodeInfo":{"kubeletVersion":"v1.31.0"}}}]}`)
	if len(nodes) != 1 || !nodes[0].Ready || nodes[0].Roles != "control-plane" || nodes[0].InternalIP != "10.0.0.1" || nodes[0].KubeletVersion != "v1.31.0" {
		t.Errorf("parseNodes: %+v", nodes)
	}
}

func TestDoctorCollector(t *testing.T) {
	c := &doctorCollector{}
	io.WriteString(c, "=== lok8s doctor ===\n\n--- runtime ---\n  \033[32m✓\033[0m bash 5.2\n--- tools ---\n  \033[31m✗\033[0m kind: missing (b install)\n  \033[33m!\033[0m tilt: 0.33 (older)\n  PATH_BASE=/p\n\ndoctor: missing required prerequisites (see ✗ above)\n")
	raw, _ := json.Marshal(c.sections)
	want := `[{"name":"runtime","checks":[{"status":"ok","message":"bash 5.2"}]},{"name":"tools","checks":[{"status":"bad","message":"kind: missing (b install)"},{"status":"warn","message":"tilt: 0.33 (older)"},{"status":"info","message":"PATH_BASE=/p"}]}]`
	if string(raw) != want {
		t.Errorf("doctor sections:\n--- got ---\n%s\n--- want ---\n%s", raw, want)
	}
}

func TestParseRegistryStatus(t *testing.T) {
	text := "✓ build      [project] alpha-registry-build → https://10.125.125.101:5000 · 3 repos\n" +
		"⚠ cache      [project] alpha-registry-cache → https://10.125.125.102:5000 · running, unreachable\n" +
		"✗ io-docker  [shared] lok8s-registry-io-docker → https://10.125.200.2:5000 · not running\n" +
		"      some/repo\n"
	got, _ := json.Marshal(parseRegistryStatus(text))
	want := `[{"name":"build","scope":"project","container":"alpha-registry-build","endpoint":"https://10.125.125.101:5000","running":true,"reachable":true,"state":"3 repos"},` +
		`{"name":"cache","scope":"project","container":"alpha-registry-cache","endpoint":"https://10.125.125.102:5000","running":true,"reachable":false,"state":"running, unreachable"},` +
		`{"name":"io-docker","scope":"shared","container":"lok8s-registry-io-docker","endpoint":"https://10.125.200.2:5000","running":false,"reachable":false,"state":"not running"}]`
	if string(got) != want {
		t.Errorf("registry status:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestOutputFlagIsOnTheReportingCommands(t *testing.T) {
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	for _, path := range []string{"use", "status", "addons", "assets list", "registry status", "version", "doctor"} {
		cmd := findByPath(root, path)
		if cmd == nil || cmd.Flags().ShorthandLookup("o") == nil {
			t.Errorf("lo %s: no -o flag", path)
		}
	}
}

// TestUseWithADomainRefusesOutput: -o applies to the listing; with a
// domain it is an error, like `lo addons <name> -o`.
func TestUseWithADomainRefusesOutput(t *testing.T) {
	paths := completionProject(t)
	_, errOut, err := runOut(t, paths, "use", "alpha.dev", "-o", "json")
	if err == nil || !strings.Contains(errOut, "Error: --output json applies to the listing: lo use -o json") {
		t.Errorf("lo use alpha.dev -o json: err %v, stderr %q", err, errOut)
	}
	if raw, _ := os.ReadFile(filepath.Join(paths.Clusters, ".active")); len(raw) != 0 {
		t.Errorf("the refused run set the active domain: %q", raw)
	}
}

// TestAssetsListJSONFlagIsOutputJSON: --json is -o json; with a
// conflicting -o value it is an error.
func TestAssetsListJSONFlagIsOutputJSON(t *testing.T) {
	paths := completionProject(t)
	a, _, _ := runOut(t, paths, "assets", "list", "--json")
	b, _, _ := runOut(t, paths, "assets", "list", "--json", "-o", "json")
	if a == "" || a != b {
		t.Errorf("--json and --json -o json differ:\n%s\n---\n%s", a, b)
	}
	_, errOut, err := runOut(t, paths, "assets", "list", "--json", "-o", "yaml")
	if err == nil || !strings.Contains(errOut, "Error: lo assets list: --json conflicts with --output yaml") {
		t.Errorf("--json -o yaml: err %v, stderr %q", err, errOut)
	}
}

// TestWriteOutputKeepsAngleBrackets: the JSON writer does not HTML-escape
// (a < stays a <), like the inventory and registry writers.
func TestWriteOutputKeepsAngleBrackets(t *testing.T) {
	var out bytes.Buffer
	if err := writeOutput(&out, outputJSON, map[string]string{"note": "<b> & </b>"}); err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"note\": \"<b> & </b>\"\n}\n"; out.String() != want {
		t.Errorf("json = %q, want %q", out.String(), want)
	}
}
