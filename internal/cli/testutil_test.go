package cli

// testutil_test.go holds the fixtures the command tests share: the repo
// root, the synthetic project, the argsh usage-array parser and the
// one-shot command runner. Every command test runs against a temp dir over
// a fake runner. Nothing here reaches a cluster, Tilt or a registry.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func repoRootDir(t *testing.T) string {
	t.Helper()
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	if _, err := os.Stat(filepath.Join(root, ".lok8s", "lo")); err != nil {
		t.Skip("repo checkout not available")
	}
	return root
}

// synthProject is a synthetic lok8s project: its own .lok8s (empty unless
// the test links the framework tree), clusters/, .bin/.
func synthProject(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	os.MkdirAll(p.Bin, 0o755)
	os.MkdirAll(p.Lok8s, 0o755)
	os.MkdirAll(p.Clusters, 0o755)
	return p
}

// runLo executes the Go command tree with the given argv (no subprocess).
func runLo(t *testing.T, root *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// orchestrateProject is a synthetic project with the routing-axis domains
// and the exec/exit seams faked for the test's lifetime.
func orchestrateProject(t *testing.T) (*config.Paths, *scriptRunner, *[]int) {
	t.Helper()
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "alpha.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: alpha\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "beta.cloud", "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata:\n  name: beta\nspec:\n  kubernetes:\n    version: \"1.31.0\"\n  bootstrap:\n    - name: cilium\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "gamma.app", "deploy.lok8s.yaml"), "kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "nokind.dev", "cluster.lok8s.yaml"), "metadata:\n  name: prod\n")

	r := &scriptRunner{handler: func(c execx.Cmd) error {
		t.Errorf("unexpected exec under test: %s %v", c.Name, c.Args)
		return errors.New("no exec under test")
	}}
	prevRunner := newRunner
	newRunner = func(*config.Paths) execx.Runner { return r }
	exits := captureExits(t)
	t.Cleanup(func() { newRunner = prevRunner })

	t.Setenv("DOMAIN_NAME", "")
	os.Unsetenv("DOMAIN_NAME")
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	t.Setenv("LOK8S_REGISTRY_JSON", "")
	os.Unsetenv("LOK8S_REGISTRY_JSON")
	return p, r, exits
}

// usageEntry matches one argsh usage line: 'name|alias@marker...'  'Short text'
var usageEntry = regexp.MustCompile(`^\s*'(#?)([a-z0-9-]+)(\|([a-z0-9-]+))?((?:@[a-z]+)*)'\s+'(.*)'\s*$`)

// parseArgshUsage extracts the top-level command list from the argsh
// entrypoint's usage array, so the Go tree cannot silently drift from the
// bash tree while both exist.
func parseArgshUsage(t *testing.T) map[string]commandSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".lok8s", "lo"))
	if err != nil {
		t.Fatalf("reading argsh entrypoint: %v", err)
	}
	text := string(raw)
	start := strings.Index(text, "local -a usage=(")
	if start < 0 {
		t.Fatal("usage array not found in .lok8s/lo")
	}

	specs := map[string]commandSpec{}
	for line := range strings.SplitSeq(text[start:], "\n") {
		if strings.TrimSpace(line) == ")" {
			break
		}
		m := usageEntry.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		spec := commandSpec{
			use:         m[2],
			hidden:      m[1] == "#",
			short:       m[6],
			destructive: strings.Contains(m[5], "@destructive"),
			readonly:    strings.Contains(m[5], "@readonly"),
			idempotent:  strings.Contains(m[5], "@idempotent"),
		}
		if m[4] != "" {
			spec.aliases = []string{m[4]}
		}
		specs[spec.use] = spec
	}
	if len(specs) < 20 {
		t.Fatalf("parsed only %d usage entries — parser or usage array broke", len(specs))
	}
	return specs
}
