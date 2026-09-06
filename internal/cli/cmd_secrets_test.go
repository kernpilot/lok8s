package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

// secretsProject scaffolds a minimal project with one domain that has its own
// store, and returns the paths.
func secretsProject(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	t.Setenv("DOMAIN_NAME", "")
	os.Unsetenv("DOMAIN_NAME")
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	os.Unsetenv("LOK8S_CLUSTER_NAME")
	if err := os.MkdirAll(filepath.Join(base, "clusters", "alpha.dev", "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
}

func runSecrets(t *testing.T, paths *config.Paths, args ...string) (string, string, error) {
	t.Helper()
	root := NewRoot(paths)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

// TestSecretsDomainFlagBothPositions pins the contract that both
// `lo secrets --domain d path` and `lo secrets path --domain d` parse.
func TestSecretsDomainFlagBothPositions(t *testing.T) {
	paths := secretsProject(t)
	want := paths.Clusters + "/alpha.dev/secrets\n"
	for _, args := range [][]string{
		{"secrets", "--domain", "alpha.dev", "path"},
		{"secrets", "path", "--domain", "alpha.dev"},
		{"--domain", "alpha.dev", "secrets", "path"},
	} {
		out, _, err := runSecrets(t, paths, args...)
		if err != nil || out != want {
			t.Errorf("%v → out=%q err=%v (want %q)", args, out, err, want)
		}
	}
	// No domain in context → the terminal default (lok8s.dev) has no store
	// dir, so the flat store resolves.
	out, _, err := runSecrets(t, paths, "secrets", "path")
	if err != nil || out != paths.Base+"/.secrets\n" {
		t.Errorf("flat: out=%q err=%v", out, err)
	}
}

// TestSecretsSetHandParser pins the argsh-grammar hand parsing of `set`: -s
// means --namespace here (it means --cluster everywhere else), --enc/-e alias
// --encrypt, --name=x wins over an earlier -n, and --domain routes the write
// into the per-domain store.
func TestSecretsSetHandParser(t *testing.T) {
	paths := secretsProject(t)

	if _, _, err := runSecrets(t, paths, "secrets", "set", "-n", "app", "-s", "myns", "K1", "v1"); err != nil {
		t.Fatalf("set -s: %v", err)
	}
	if raw, err := os.ReadFile(paths.Base + "/.secrets/Secret.app.myns.K1"); err != nil || string(raw) != "v1" {
		t.Errorf("-s namespace: %q %v", raw, err)
	}

	if _, _, err := runSecrets(t, paths, "secrets", "set", "-n", "app", "--name=x2", "K2", "v2"); err != nil {
		t.Fatalf("--name=: %v", err)
	}
	if !fileExists(paths.Base + "/.secrets/Secret.x2.default.K2") {
		t.Error("--name=x2 did not win over -n app")
	}

	if _, _, err := runSecrets(t, paths, "secrets", "set", "--domain", "alpha.dev", "-n", "app", "K3", "v3"); err != nil {
		t.Fatalf("--domain: %v", err)
	}
	if !fileExists(paths.Clusters + "/alpha.dev/secrets/Secret.app.default.K3") {
		t.Error("--domain write landed in the wrong store")
	}

	// --enc/-e parse as the encrypt flag: without .sops.yaml the encrypt gate
	// fails, but the plaintext cache was written with key/value in the right
	// positions (an unknown flag would have shifted them).
	for _, flag := range []string{"--encrypt", "--enc", "-e"} {
		os.RemoveAll(paths.Base + "/.secrets")
		_, errStr, err := runSecrets(t, paths, "secrets", "set", "-n", "app", flag, "K4", "v4")
		if err == nil || !strings.Contains(errStr, "No .sops.yaml found — run: lo secrets init") {
			t.Errorf("%s: err=%v stderr=%q", flag, err, errStr)
		}
		if raw, rerr := os.ReadFile(paths.Base + "/.secrets/Secret.app.default.K4"); rerr != nil || string(raw) != "v4" {
			t.Errorf("%s: cache %q %v", flag, raw, rerr)
		}
		if fileExists(paths.Base + "/.secrets/Secret.app.default.K4.enc") {
			t.Errorf("%s: .enc produced without config", flag)
		}
	}

	// Missing key positional → argsh-shaped parse error.
	_, errStr, err := runSecrets(t, paths, "secrets", "set", "-n", "app")
	if err == nil || !strings.Contains(errStr, "Error: missing required argument: key") {
		t.Errorf("missing key: err=%v stderr=%q", err, errStr)
	}

	// Unknown flag → argsh-shaped error.
	_, errStr, err = runSecrets(t, paths, "secrets", "set", "--bogus", "K", "v")
	if err == nil || !strings.Contains(errStr, "Error: unknown flag: --bogus") {
		t.Errorf("unknown flag: err=%v stderr=%q", err, errStr)
	}
}

// TestSecretsSubcommandAnnotations mirrors the argsh @markers for the secrets
// subtree (MCP tool filtering + confirmation gating read these).
func TestSecretsSubcommandAnnotations(t *testing.T) {
	paths := secretsProject(t)
	root := NewRoot(paths)
	want := map[string]string{
		"init":    AnnotationIdempotent,
		"add-key": AnnotationDestructive,
		"set":     AnnotationDestructive,
		"encrypt": AnnotationIdempotent,
		"decrypt": AnnotationIdempotent,
		"allow":   AnnotationDestructive,
		"list":    AnnotationReadonly,
		"print":   AnnotationReadonly,
		"env":     AnnotationReadonly,
		"path":    AnnotationReadonly,
	}
	aliases := map[string]string{
		"init": "i", "set": "s", "encrypt": "e", "decrypt": "d",
		"allow": "a", "list": "l", "print": "p",
	}
	for name, annotation := range want {
		cmd, _, err := root.Find([]string{"secrets", name})
		if err != nil || cmd.Name() != name {
			t.Errorf("secrets %s not resolvable: %v", name, err)
			continue
		}
		if cmd.Annotations[annotation] != "true" {
			t.Errorf("secrets %s: missing annotation %s (have %v)", name, annotation, cmd.Annotations)
		}
		if alias, ok := aliases[name]; ok {
			acmd, _, err := root.Find([]string{"secrets", alias})
			if err != nil || acmd.Name() != name {
				t.Errorf("secrets alias %s → %s not resolvable: %v", alias, name, err)
			}
		}
	}
}

// `secrets set` / `env`: -s is --namespace (the bash spec), -e/--enc are
// --encrypt; the global --cluster is reachable by its long name only.
func TestSecretsSetEnvFlagsBindLikeTheBashSpec(t *testing.T) {
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	set, _, err := root.Find([]string{"secrets", "set"})
	if err != nil {
		t.Fatal(err)
	}
	if err := set.ParseFlags([]string{"-n", "app", "-s", "myns", "--enc", "--cluster", "c1", "--domain", "a.dev"}); err != nil {
		t.Fatal(err)
	}
	f := set.Flags()
	name, _ := f.GetString("name")
	ns, _ := f.GetString("namespace")
	enc, _ := f.GetBool("enc")
	encrypt, _ := f.GetBool("encrypt")
	cluster, _ := f.GetString("cluster")
	if name != "app" || ns != "myns" || !enc || encrypt || cluster != "c1" {
		t.Fatalf("name=%q ns=%q enc=%v encrypt=%v cluster=%q", name, ns, enc, encrypt, cluster)
	}
	env, _, err := root.Find([]string{"secrets", "env"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.ParseFlags([]string{"--name", "app", "-s", "other"}); err != nil {
		t.Fatal(err)
	}
	if ns, _ := env.Flags().GetString("namespace"); ns != "other" {
		t.Fatalf("env -s bound to %q, want namespace", ns)
	}
	if env.Flags().Lookup("encrypt") != nil {
		t.Fatal("env must not take --encrypt")
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"secrets", "set", "-n"}, "missing value for flag: name"},
		{[]string{"secrets", "set", "-x", "k"}, "unknown flag: -x"},
		{[]string{"secrets", "set", "--nope", "k"}, "unknown flag: --nope"},
		{[]string{"secrets", "set"}, "missing required argument: key"},
		{[]string{"secrets", "env", "extra"}, "unexpected argument: extra"},
		{[]string{"env", "services", "--nope"}, "unknown flag: --nope"},
	} {
		_, stderr, err := runLo(t, NewRoot(&config.Paths{Base: t.TempDir()}), tc.args...)
		if err == nil || !strings.Contains(stderr, "Error: "+tc.want) {
			t.Errorf("%v: err=%v stderr=%q", tc.args, err, stderr)
		}
	}
}
