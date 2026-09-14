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

// confirmRoot builds a root with one guarded command whose body records
// that it ran, so the guard is tested without docker behind it.
func confirmRoot(t *testing.T, paths *config.Paths, path string) (*cobra.Command, *bool) {
	t.Helper()
	ran := false
	root := &cobra.Command{Use: "lo", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("domain", "", "")
	root.PersistentFlags().StringP("cluster", "s", "", "")
	parent := root
	parts := strings.Fields(path)
	for i, name := range parts {
		c := &cobra.Command{Use: name, SilenceUsage: true}
		if i == len(parts)-1 {
			c.RunE = func(*cobra.Command, []string) error { ran = true; return nil }
			c.Flags().Bool("all", false, "")
			c.Flags().BoolP("shared", "S", false, "")
		}
		parent.AddCommand(c)
		parent = c
	}
	installConfirmations(root, paths)
	return root, &ran
}

func runConfirm(t *testing.T, root *cobra.Command, tty bool, answer string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("DOMAIN_NAME", "") // ambientMainEnv exports it; each run resolves afresh
	prevTTY, prevIn := confirmIsTerminal, confirmIn
	confirmIsTerminal = func() bool { return tty }
	confirmIn = strings.NewReader(answer)
	t.Cleanup(func() { confirmIsTerminal, confirmIn = prevTTY, prevIn })
	var errOut bytes.Buffer
	root.SetOut(&errOut)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return errOut.String(), err
}

func TestConfirmDownListsObjectsAndAsks(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	os.MkdirAll(filepath.Join(paths.Base, ".kubeconfig"), 0o755)
	os.WriteFile(filepath.Join(paths.Base, ".kubeconfig", "alpha.yaml"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(paths.Clusters, "alpha.dev", ".registries.json"), []byte(`{"shared":false,"project_network":"alpha","registries":[{"name":"build","type":"build"},{"name":"io-docker","type":"mirror"}]}`), 0o644)
	t.Setenv("DOMAIN_NAME", "")
	t.Setenv("LOK8S_CLUSTER_NAME", "")

	root, ran := confirmRoot(t, paths, "down")
	out, err := runConfirm(t, root, true, "n\n", "down")
	if err == nil || *ran {
		t.Fatalf("declined lo down: err %v, ran %v", err, *ran)
	}
	want := "lo down removes:\n" +
		"  kind cluster         alpha\n" +
		"  kubeconfig           .kubeconfig/alpha.yaml\n" +
		"  registry containers  alpha-registry-build, alpha-registry-io-docker (volumes stay)\n" +
		"Continue? [y/N] aborted: nothing removed (answer y, or pass --yes)\n"
	if out != want {
		t.Errorf("prompt:\n--- got ---\n%s--- want ---\n%s", out, want)
	}

	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, true, "yes\n", "down"); err != nil || !*ran || !strings.Contains(out, "Continue? [y/N] ") {
		t.Errorf("lo down answered yes: err %v, ran %v, out %q", err, *ran, out)
	}
	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, true, "", "down", "--yes"); err != nil || !*ran || out != "" {
		t.Errorf("lo down --yes: err %v, ran %v, out %q", err, *ran, out)
	}
	// Piped: no prompt, nothing printed, the command runs as before.
	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, false, "", "down"); err != nil || !*ran || out != "" {
		t.Errorf("lo down piped: err %v, ran %v, out %q", err, *ran, out)
	}
}

func TestConfirmSkipsTheCloudDriversAndListsClean(t *testing.T) {
	paths := completionProject(t)
	t.Setenv("DOMAIN_NAME", "")
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	// beta.cloud is a KubeOne cluster: its dispatch has its own gate.
	os.WriteFile(filepath.Join(paths.Clusters, "beta.cloud", "cluster.lok8s.yaml"), []byte("kind: KubeOne\nmetadata:\n  name: beta\n"), 0o644)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("beta.cloud\n"), 0o644)
	for _, cmd := range []string{"down", "destroy"} {
		root, ran := confirmRoot(t, paths, cmd)
		if out, err := runConfirm(t, root, true, "n\n", cmd); err != nil || !*ran || out != "" {
			t.Errorf("lo %s on a KubeOne domain: err %v, ran %v, out %q (want no prompt)", cmd, err, *ran, out)
		}
	}

	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	root, _ := confirmRoot(t, paths, "clean")
	out, _ := runConfirm(t, root, true, "n\n", "clean", "--all")
	for _, line := range []string{"lo clean removes:\n", "  kind cluster    alpha\n", "  docker volumes  every volume named alpha-*\n", "  docker          system prune -f"} {
		if !strings.Contains(out, line) {
			t.Errorf("lo clean --all prompt lacks %q:\n%s", line, out)
		}
	}
	root, _ = confirmRoot(t, paths, "destroy")
	if out, _ := runConfirm(t, root, true, "n\n", "destroy"); !strings.HasPrefix(out, "lo destroy removes:\n  kind cluster  alpha\n") {
		t.Errorf("lo destroy prompt:\n%s", out)
	}
}

func TestConfirmRegistryAndImageClean(t *testing.T) {
	paths := completionProject(t)
	t.Setenv("DOMAIN_NAME", "")
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	os.WriteFile(filepath.Join(paths.Clusters, "alpha.dev", ".registries.json"), []byte(`{"shared":true,"project_network":"alpha","network":{"name":"lok8s-registries"},"registries":[{"name":"build","type":"build"},{"name":"cache","type":"cache"},{"name":"io-docker","type":"mirror"}]}`), 0o644)

	root, _ := confirmRoot(t, paths, "registry clean")
	out, _ := runConfirm(t, root, true, "n\n", "registry", "clean", "--shared")
	want := "lo registry clean removes:\n" +
		"  registry containers and volumes  alpha-registry-build, alpha-registry-cache, lok8s-registry-io-docker\n" +
		"  docker network                   lok8s-registries\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("lo registry clean --shared prompt:\n--- got ---\n%s--- want prefix ---\n%s", out, want)
	}
	root, _ = confirmRoot(t, paths, "registry clean")
	if out, _ := runConfirm(t, root, true, "n\n", "registry", "clean"); strings.Contains(out, "io-docker") || strings.Contains(out, "docker network") {
		t.Errorf("lo registry clean without --shared names the shared mirrors:\n%s", out)
	}
	root, _ = confirmRoot(t, paths, "image clean")
	if out, _ := runConfirm(t, root, true, "n\n", "image", "clean"); !strings.HasPrefix(out, "lo image clean removes:\n  docker volume  alpha-registry-cache\n") {
		t.Errorf("lo image clean prompt:\n%s", out)
	}
}

func TestConfirmIsOnEveryDestructiveCommand(t *testing.T) {
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	for _, path := range []string{"down", "clean", "destroy", "registry clean", "image clean"} {
		cmd := findByPath(root, path)
		if cmd == nil || cmd.Flags().Lookup("yes") == nil {
			t.Errorf("lo %s: no --yes flag (the guard is not installed)", path)
		}
	}
	for _, path := range []string{"up", "registry down", "image list", "secrets encrypt"} {
		if cmd := findByPath(root, path); cmd != nil && cmd.Flags().Lookup("yes") != nil {
			t.Errorf("lo %s: has --yes but is not in the destructive set", path)
		}
	}
}
