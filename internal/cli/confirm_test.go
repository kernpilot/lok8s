package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	root.PersistentFlags().BoolP("remote", "r", false, "")
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

// runConfirm runs the guarded command with the terminal facts forced and
// the answer scripted; it returns stderr and the error.
func runConfirm(t *testing.T, root *cobra.Command, stdinTTY, stderrTTY bool, answer string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("DOMAIN_NAME", "") // ambientMainEnv exports it; each run resolves afresh
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	prevTTY, prevIn := confirmTerminals, confirmIn
	confirmTerminals = func() (bool, bool) { return stdinTTY, stderrTTY }
	confirmIn = strings.NewReader(answer)
	t.Cleanup(func() { confirmTerminals, confirmIn = prevTTY, prevIn })
	var errOut bytes.Buffer
	root.SetOut(&errOut)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return errOut.String(), err
}

// registrySet writes the domain's registry file and points the driver at
// it, the way a run does after its own init.
func registrySet(t *testing.T, paths *config.Paths, d, content string) {
	t.Helper()
	path := filepath.Join(paths.Clusters, d, ".registries.json")
	os.WriteFile(path, []byte(content), 0o644)
	t.Setenv("LOK8S_REGISTRY_JSON", path)
}

const projectSet = `{"shared":false,"tls":true,"project_network":"alpha","network":{"name":"alpha"},"registries":[{"name":"build","type":"build"},{"name":"cache","type":"cache"},{"name":"io-docker","type":"mirror"}]}`
const sharedSet = `{"shared":true,"tls":true,"project_network":"alpha","network":{"name":"lok8s-registries"},"registries":[{"name":"build","type":"build"},{"name":"cache","type":"cache"},{"name":"io-docker","type":"mirror"}]}`

func TestConfirmDownListsWhatDownRemoves(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	os.MkdirAll(filepath.Join(paths.Base, ".kubeconfig"), 0o755)
	os.WriteFile(filepath.Join(paths.Base, ".kubeconfig", "alpha.yaml"), []byte("{}"), 0o644)
	registrySet(t, paths, "alpha.dev", projectSet)

	root, ran := confirmRoot(t, paths, "down")
	out, err := runConfirm(t, root, true, true, "n\n", "down")
	if err == nil || *ran {
		t.Fatalf("declined lo down: err %v, ran %v", err, *ran)
	}
	// runDown: the kind cluster, the kubeconfig context, the project
	// registry containers (volumes stay); no TLS volume, no proxy.
	want := "lo down removes:\n" +
		"  kind cluster         alpha\n" +
		"  kubeconfig           .kubeconfig/alpha.yaml (the file stays, kind drops its context)\n" +
		"  registry containers  alpha-registry-build, alpha-registry-cache, alpha-registry-io-docker (volumes stay)\n" +
		"Continue? [y/N] aborted: nothing removed (answer y, or pass --yes)\n"
	if out != want {
		t.Errorf("prompt:\n--- got ---\n%s--- want ---\n%s", out, want)
	}

	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, true, true, "yes\n", "down"); err != nil || !*ran || !strings.Contains(out, "Continue? [y/N] ") {
		t.Errorf("lo down answered yes: err %v, ran %v, out %q", err, *ran, out)
	}
	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, true, true, "", "down", "--yes"); err != nil || !*ran || out != "" {
		t.Errorf("lo down --yes: err %v, ran %v, out %q", err, *ran, out)
	}
}

// TestConfirmGateIsStdinAndStderr: the prompt asks when stdin and stderr
// are terminals (`lo down | cat` still asks), not when either is a pipe,
// and no environment variable silences it.
func TestConfirmGateIsStdinAndStderr(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	registrySet(t, paths, "alpha.dev", projectSet)

	// stdin and stderr terminals, stdout piped (lo down | cat): asks.
	root, ran := confirmRoot(t, paths, "down")
	if out, _ := runConfirm(t, root, true, true, "n\n", "down"); *ran || !strings.HasPrefix(out, "lo down removes:") {
		t.Errorf("stdin+stderr terminals: ran %v, out %q", *ran, out)
	}
	// stdin piped (lo down < /dev/null, a script): no prompt, runs.
	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, false, true, "n\n", "down"); err != nil || !*ran || out != "" {
		t.Errorf("stdin piped: err %v, ran %v, out %q", err, *ran, out)
	}
	// stderr piped (2>log): no prompt, runs.
	root, ran = confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, true, false, "n\n", "down"); err != nil || !*ran || out != "" {
		t.Errorf("stderr piped: err %v, ran %v, out %q", err, *ran, out)
	}
	// CI and LOK8S_NONINTERACTIVE do not silence the prompt.
	t.Setenv("CI", "true")
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	root, ran = confirmRoot(t, paths, "down")
	if out, _ := runConfirm(t, root, true, true, "n\n", "down"); *ran || !strings.HasPrefix(out, "lo down removes:") {
		t.Errorf("CI=true on terminals: ran %v, out %q (an env must not silence a safety prompt)", *ran, out)
	}
}

// TestConfirmInterruptDuringThePrompt: Ctrl-C while the prompt waits ends
// the run with nothing removed and nothing printed after the prompt.
func TestConfirmInterruptDuringThePrompt(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	registrySet(t, paths, "alpha.dev", projectSet)
	t.Setenv("DOMAIN_NAME", "")
	root, ran := confirmRoot(t, paths, "down")
	pr, pw := io.Pipe() // never written: the read blocks like a terminal
	t.Cleanup(func() { pw.Close() })
	prevTTY, prevIn := confirmTerminals, confirmIn
	confirmTerminals = func() (bool, bool) { return true, true }
	confirmIn = pr
	t.Cleanup(func() { confirmTerminals, confirmIn = prevTTY, prevIn })
	ctx, cancel := context.WithCancel(context.Background())
	var errOut bytes.Buffer
	root.SetErr(&errOut)
	root.SetArgs([]string{"down"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrHandled) || *ran {
		t.Errorf("interrupted prompt: err %v, ran %v", err, *ran)
	}
	if !strings.HasSuffix(errOut.String(), "Continue? [y/N] ") {
		t.Errorf("something printed after the interrupted prompt:\n%s", errOut.String())
	}
}

func TestConfirmSkipsTheCloudDriversAndMalformedSpecs(t *testing.T) {
	paths := completionProject(t)
	// beta.cloud is a KubeOne cluster: its dispatch has its own gate.
	os.WriteFile(filepath.Join(paths.Clusters, "beta.cloud", "cluster.lok8s.yaml"), []byte("kind: KubeOne\nmetadata:\n  name: beta\n"), 0o644)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("beta.cloud\n"), 0o644)
	for _, cmd := range []string{"down", "clean", "destroy"} {
		root, ran := confirmRoot(t, paths, cmd)
		if out, err := runConfirm(t, root, true, true, "n\n", cmd); err != nil || !*ran || out != "" {
			t.Errorf("lo %s on a KubeOne domain: err %v, ran %v, out %q (want no prompt)", cmd, err, *ran, out)
		}
	}
	// A malformed .kind: the command refuses on its own, no prompt first.
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	os.WriteFile(filepath.Join(paths.Clusters, "alpha.dev", "cluster.lok8s.yaml"), []byte("kind: 'lo; rm -rf /'\nmetadata:\n  name: alpha\n"), 0o644)
	root, ran := confirmRoot(t, paths, "down")
	if out, err := runConfirm(t, root, true, true, "n\n", "down"); err != nil || !*ran || out != "" {
		t.Errorf("lo down on a malformed .kind: err %v, ran %v, out %q (want no prompt, the command refuses)", err, *ran, out)
	}
	// destroy --remote on lo: the driver's infrastructure gate asks, not us.
	os.WriteFile(filepath.Join(paths.Clusters, "alpha.dev", "cluster.lok8s.yaml"), []byte("kind: Lo\nmetadata:\n  name: alpha\n"), 0o644)
	root, ran = confirmRoot(t, paths, "destroy")
	if out, err := runConfirm(t, root, true, true, "n\n", "destroy", "--remote"); err != nil || !*ran || out != "" {
		t.Errorf("lo destroy --remote: err %v, ran %v, out %q (want the driver's gate only)", err, *ran, out)
	}
}

// TestConfirmDestroyAndCleanListTheDeletion: the prompts carry every name
// the teardown removes: the registry containers and volumes, the TLS
// certificate volume, the proxy container.
func TestConfirmDestroyAndCleanListTheDeletion(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	registrySet(t, paths, "alpha.dev", projectSet)

	root, _ := confirmRoot(t, paths, "destroy")
	out, _ := runConfirm(t, root, true, true, "n\n", "destroy")
	want := "lo destroy removes:\n" +
		"  kind cluster                     alpha\n" +
		"  registry containers and volumes  alpha-registry-build, alpha-registry-cache, alpha-registry-io-docker\n" +
		"  TLS certificate volume           alpha-registry-tls\n" +
		"  proxy container                  alpha-proxy\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("lo destroy prompt:\n--- got ---\n%s--- want prefix ---\n%s", out, want)
	}

	root, _ = confirmRoot(t, paths, "clean")
	out, _ = runConfirm(t, root, true, true, "n\n", "clean")
	for _, line := range []string{"lo clean removes:\n", "  kind cluster                     alpha\n", "  docker volumes                   every volume named alpha-*\n", "  registry containers and volumes  alpha-registry-build, alpha-registry-cache, alpha-registry-io-docker\n", "  TLS certificate volume           alpha-registry-tls\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("lo clean prompt lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "(volumes stay)") || strings.Count(out, "registry containers") != 1 {
		t.Errorf("lo clean prompt must carry one registry line:\n%s", out)
	}
	// --all: runClean prunes after the teardown and stops; no volumes, no
	// registry set.
	root, _ = confirmRoot(t, paths, "clean")
	out, _ = runConfirm(t, root, true, true, "n\n", "clean", "--all")
	if !strings.Contains(out, "system prune -f") || strings.Contains(out, "docker volumes") || strings.Contains(out, "TLS certificate") {
		t.Errorf("lo clean --all prompt must list the prune only after the teardown:\n%s", out)
	}
}

func TestConfirmRegistryAndImageClean(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	registrySet(t, paths, "alpha.dev", sharedSet)

	root, _ := confirmRoot(t, paths, "registry clean")
	out, _ := runConfirm(t, root, true, true, "n\n", "registry", "clean", "--shared")
	want := "lo registry clean removes:\n" +
		"  registry containers and volumes       alpha-registry-build, alpha-registry-cache\n" +
		"  TLS certificate volume                alpha-registry-tls\n" +
		"  shared mirror containers and volumes  lok8s-registry-io-docker\n" +
		"  docker network                        lok8s-registries\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("lo registry clean --shared prompt:\n--- got ---\n%s--- want prefix ---\n%s", out, want)
	}
	root, _ = confirmRoot(t, paths, "registry clean")
	if out, _ := runConfirm(t, root, true, true, "n\n", "registry", "clean"); strings.Contains(out, "io-docker") || strings.Contains(out, "docker network") || !strings.Contains(out, "alpha-registry-tls") {
		t.Errorf("lo registry clean without --shared:\n%s", out)
	}
	// --shared on a set that is NOT shared: RegistryClean still removes
	// the shared-network name of every mirror and the file's network, so
	// the prompt lists them from the same branch.
	registrySet(t, paths, "alpha.dev", projectSet)
	root, _ = confirmRoot(t, paths, "registry clean")
	out, _ = runConfirm(t, root, true, true, "n\n", "registry", "clean", "--shared")
	want = "lo registry clean removes:\n" +
		"  registry containers and volumes       alpha-registry-build, alpha-registry-cache, alpha-registry-io-docker\n" +
		"  TLS certificate volume                alpha-registry-tls\n" +
		"  shared mirror containers and volumes  lok8s-registry-io-docker\n" +
		"  docker network                        alpha\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("lo registry clean --shared on a non-shared set:\n--- got ---\n%s--- want prefix ---\n%s", out, want)
	}
	// A non-Lo domain: the command's driver gate refuses, no prompt first.
	os.WriteFile(filepath.Join(paths.Clusters, "beta.cloud", "cluster.lok8s.yaml"), []byte("kind: KubeOne\nmetadata:\n  name: beta\n"), 0o644)
	root, ran := confirmRoot(t, paths, "registry clean")
	if out, _ := runConfirm(t, root, true, true, "n\n", "registry", "clean", "--domain", "beta.cloud"); !*ran || out != "" {
		t.Errorf("lo registry clean on a KubeOne domain: ran %v, out %q (want no prompt)", *ran, out)
	}

	// image clean names the cache registry on the network the command
	// reads (spec.network.name through ambientMainEnv, else the default).
	root, _ = confirmRoot(t, paths, "image clean")
	if out, _ := runConfirm(t, root, true, true, "n\n", "image", "clean"); !strings.HasPrefix(out, "lo image clean removes:\n  cache registry container and volume  lok8s-registry-cache\n") {
		t.Errorf("lo image clean prompt:\n%s", out)
	}
	os.WriteFile(filepath.Join(paths.Clusters, "alpha.dev", "cluster.lok8s.yaml"), []byte("kind: Lo\nmetadata:\n  name: alpha\nspec:\n  network:\n    name: alphanet\n"), 0o644)
	root, _ = confirmRoot(t, paths, "image clean")
	if out, _ := runConfirm(t, root, true, true, "n\n", "image", "clean"); !strings.Contains(out, "alphanet-registry-cache") {
		t.Errorf("lo image clean prompt must follow spec.network.name:\n%s", out)
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

// TestConfirmPlanWritesNothing: composing a plan reads the registry file
// a run left behind and never generates one. With the file present its
// bytes are unchanged after a "no"; without it the prompt says so and no
// file appears. No LOK8S_REGISTRY_JSON here: the path is the domain's.
func TestConfirmPlanWritesNothing(t *testing.T) {
	paths := completionProject(t)
	t.Setenv("LOK8S_REGISTRY_JSON", "")
	os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte("alpha.dev\n"), 0o644)
	file := filepath.Join(paths.Clusters, "alpha.dev", ".registries.json")
	os.WriteFile(file, []byte(projectSet), 0o644)
	before, _ := os.ReadFile(file)

	root, ran := confirmRoot(t, paths, "destroy")
	out, _ := runConfirm(t, root, true, true, "n\n", "destroy")
	if *ran || !strings.Contains(out, "alpha-registry-tls") {
		t.Errorf("destroy prompt from the file: ran %v, out:\n%s", *ran, out)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("the prompt rewrote %s", file)
	}

	os.Remove(file)
	root, ran = confirmRoot(t, paths, "destroy")
	out, _ = runConfirm(t, root, true, true, "n\n", "destroy")
	if *ran || !strings.Contains(out, "the set named by the spec (no clusters/alpha.dev/.registries.json yet)\n") {
		t.Errorf("destroy prompt without the file:\n%s", out)
	}
	if _, err := os.Stat(file); err == nil {
		t.Errorf("the prompt generated %s", file)
	}
}

// TestImageCleanRefusesANonLoDomain: the cache registry is a Lo-driver
// feature; on another driver the command refuses before any removal,
// and the prompt has nothing to ask.
func TestImageCleanRefusesANonLoDomain(t *testing.T) {
	paths := completionProject(t)
	os.WriteFile(filepath.Join(paths.Clusters, "beta.cloud", "cluster.lok8s.yaml"), []byte("kind: KubeOne\nmetadata:\n  name: beta\n"), 0o644)
	t.Setenv("LOK8S_REGISTRY_IP_CACHE", "")
	_, errOut, err := runOut(t, paths, "image", "clean", "--domain", "beta.cloud")
	if err == nil || !strings.Contains(errOut, "domain 'beta.cloud' uses the 'kubeone' driver") {
		t.Errorf("lo image clean on a KubeOne domain: err %v, stderr %q (want the driver refusal, no removal)", err, errOut)
	}
}
