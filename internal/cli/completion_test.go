package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
)

// completionProject writes a project with two cluster domains, one deploy
// domain, a services.yaml and one service directory.
func completionProject(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	clusters := filepath.Join(base, "clusters")
	for _, d := range []string{"alpha.dev", "beta.cloud"} {
		os.MkdirAll(filepath.Join(clusters, d), 0o755)
		os.WriteFile(filepath.Join(clusters, d, "cluster.lok8s.yaml"), []byte("kind: Lo\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(clusters, "gamma.app"), 0o755)
	os.WriteFile(filepath.Join(clusters, "gamma.app", "deploy.lok8s.yaml"), []byte("kind: Deploy\n"), 0o644)
	os.MkdirAll(filepath.Join(clusters, "stray"), 0o755) // no spec: not a domain
	os.MkdirAll(filepath.Join(clusters, ".hidden"), 0o755)
	os.WriteFile(filepath.Join(base, "services.yaml"), []byte("services:\n  api: {}\n  web: {}\n"), 0o644)
	os.MkdirAll(filepath.Join(base, "worker"), 0o755)
	os.WriteFile(filepath.Join(base, "worker", "lok8s.yaml"), []byte("kind: Service\n"), 0o644)
	os.MkdirAll(filepath.Join(base, "docs"), 0o755) // no service file
	return &config.Paths{Base: base, Clusters: clusters, Lok8s: filepath.Join(base, ".lok8s"), Bin: filepath.Join(base, ".bin")}
}

// complete runs cobra's hidden __complete command and returns the
// candidate lines (the trailing directive line dropped).
func complete(t *testing.T, paths *config.Paths, words ...string) []string {
	t.Helper()
	root := NewRoot(paths)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut) // cobra's "Completion ended with directive" debug line
	root.SetArgs(append([]string{cobra.ShellCompRequestCmd}, words...))
	if err := root.Execute(); err != nil {
		t.Fatalf("__complete %v: %v\n%s", words, err, errOut.String())
	}
	var values []string
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if l == "" || strings.HasPrefix(l, ":") {
			continue
		}
		values = append(values, l)
	}
	return values
}

func TestCompletionDomains(t *testing.T) {
	paths := completionProject(t)
	want := []string{"alpha.dev", "beta.cloud", "gamma.app"}

	for _, words := range [][]string{
		{"use", ""},
		{"audit", ""},
		{"recover", ""},
		{"--domain", ""},
		{"status", "--domain", ""},
		{"build", "--cluster-override", ""},
		{"drivers", "lo", "status", ""},
	} {
		got := complete(t, paths, words...)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("complete %v = %v, want %v", words, got, want)
		}
	}
	// A second positional on `lo use` gets nothing.
	if got := complete(t, paths, "use", "alpha.dev", ""); len(got) != 0 {
		t.Errorf("lo use <domain> <Tab> = %v, want nothing", got)
	}
	// The prefix filter is the shell's job: the binary lists every domain.
	if got := complete(t, paths, "use", "be"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lo use be<Tab> = %v, want %v", got, want)
	}
}

func TestCompletionAddonsAndAssets(t *testing.T) {
	paths := completionProject(t)
	got := complete(t, paths, "addons", "")
	if len(got) == 0 || !contains(got, "cilium") {
		t.Fatalf("lo addons <Tab> = %v, want the embedded addons (cilium among them)", got)
	}
	// An addon already on the line is not offered twice.
	if again := complete(t, paths, "addons", "cilium", ""); contains(again, "cilium") || len(again) != len(got)-1 {
		t.Errorf("lo addons cilium <Tab> = %v, want the list without cilium", again)
	}
	rels := complete(t, paths, "assets", "eject", "")
	if !contains(rels, "addons/cilium") || !contains(rels, "bash") || !contains(rels, "drivers/lo/cluster") {
		t.Errorf("lo assets eject <Tab> = %v, want the asset rels plus bash", rels)
	}
	if one := complete(t, paths, "assets", "update", "addons/cilium", ""); len(one) != 0 {
		t.Errorf("lo assets update <rel> <Tab> = %v, want nothing", one)
	}
}

func TestCompletionServices(t *testing.T) {
	paths := completionProject(t)
	got := complete(t, paths, "init", "service", "")
	if strings.Join(got, ",") != "api,web,worker" {
		t.Errorf("lo init service <Tab> = %v, want api,web,worker", got)
	}
}

func TestCompletionOutsideProjectIsEmptyNotAnError(t *testing.T) {
	base := t.TempDir()
	paths := &config.Paths{Base: base, Clusters: filepath.Join(base, "clusters")}
	if got := complete(t, paths, "use", ""); len(got) != 0 {
		t.Errorf("lo use <Tab> without clusters/ = %v, want nothing", got)
	}
	if got := complete(t, paths, "init", "service", ""); len(got) != 0 {
		t.Errorf("lo init service <Tab> in an empty dir = %v, want nothing", got)
	}
}
