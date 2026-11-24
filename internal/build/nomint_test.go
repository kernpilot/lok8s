package build

// nomint_test.go: with LOK8S_SECRETS_NO_MINT set, `lo build` (Artifacts)
// refuses a Secret plugin that does not have the no-mint run before the
// render (checkNoMintPlugin). The render is the stub kustomize of
// artifacts_test.go; the plugin is the logging stub of keysonly_test.go.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

const staleNoMintRefusal = "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the no-mint run (LOK8S_SECRETS_NO_MINT is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo\n"

// noMintProject is an exec-pipeline project with a Secret plugin stub
// (stubSecretPlugin, body) and a prior artifacts.yaml. TMPDIR is a
// directory of the project. It returns the paths, the domain dir, the
// probe log of the stub, the log that the stub kustomize writes when it
// runs and TMPDIR.
func noMintProject(t *testing.T, body string) (p *config.Paths, domainDir, probeLog, renderLog, tmp string) {
	t.Helper()
	p, domainDir = keysOnlyExecProject(t)
	stubRender(t, p, twoDocRender)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
	renderLog = filepath.Join(p.Base, "kustomize-keys.log")
	t.Setenv("KUSTOMIZE_STUB_KEYSLOG", renderLog)
	tmp = filepath.Join(p.Base, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	probeLog = stubSecretPlugin(t, p, body)
	return p, domainDir, probeLog, renderLog, tmp
}

func buildArtifacts(t *testing.T, p *config.Paths, noSecrets bool) (string, error) {
	t.Helper()
	var stderr bytes.Buffer
	err := Artifacts(t.Context(), Options{Paths: p, Domain: "d.dev", SplitOverride: "0", NoSecrets: noSecrets, Stderr: &stderr})
	return stderr.String(), err
}

// With the switch set to an on value or to an unknown value, a plugin
// that does not print "no-mint" is refused before the render: the plugin
// of the keys-only run ("dry-run\nkeys-only"), one that prints nothing,
// one that fails, one that prints the line but fails, one with a longer
// line. The refusal is one line that names the plugin path and the run.
// The render does not run, and artifacts.yaml stays. bats: "build::artifacts
// refuses a Secret plugin without the no-mint run".
func TestNoMintRefusesAPluginWithoutTheNoMintRun(t *testing.T) {
	for _, value := range []string{"1", "true", "yes"} {
		for _, body := range []string{
			"printf 'dry-run\\nkeys-only\\n'",
			"exit 0",
			"echo 'secret plugin: open spec \"--capabilities\": no such file' >&2; exit 1",
			"printf 'no-mint\\n'; exit 1",
			"printf 'no-mint-not\\nkeys-only\\n'",
		} {
			t.Run(value+": "+body, func(t *testing.T) {
				p, domainDir, probeLog, renderLog, tmp := noMintProject(t, body)
				t.Setenv(secret.NoMintEnv, value)
				stderr, err := buildArtifacts(t, p, false)
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("Artifacts = %v, want ErrHandled\n%s", err, stderr)
				}
				if stderr != staleNoMintRefusal {
					t.Errorf("stderr = %q\nwant %q", stderr, staleNoMintRefusal)
				}
				if _, err := os.Stat(probeLog); err != nil {
					t.Errorf("the probe did not run: %v", err)
				}
				if _, err := os.Stat(renderLog); err == nil {
					t.Error("the render ran")
				}
				if got, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); string(got) != "kind: Prior\n" {
					t.Errorf("artifacts.yaml = %q, want the prior content", got)
				}
				if left := dirEntries(t, tmp); len(left) != 0 {
					t.Errorf("the probe left %v in TMPDIR", left)
				}
			})
		}
	}
}

// A plugin that prints "no-mint" renders. The probe runs in a new
// directory in TMPDIR (lok8s-no-mint-*) with the sandbox of the keys-only
// probe, and the check keeps the home that it checked for kustomize. The
// Artifacts run gives kustomize that home, also when the environment
// names another home after the check: the fake Runner sets
// KUSTOMIZE_PLUGIN_HOME after the probe, so a render that picks the home
// again would give kustomize the other one.
func TestNoMintAcceptsAPluginWithTheNoMintRun(t *testing.T) {
	p, domainDir, probeLog, renderLog, tmp := noMintProject(t, "printf 'dry-run\\nkeys-only\\nno-mint\\n'")
	t.Setenv(secret.NoMintEnv, "1")
	checked := filepath.Join(p.Base, ".kustomize")
	t.Setenv(config.KustomizePluginHomeEnv, checked)
	other := filepath.Join(p.Base, "other-home")
	envlog := filepath.Join(p.Base, "kustomize-env.log")
	t.Setenv("KUSTOMIZE_STUB_ENVLOG", envlog)
	var stderr bytes.Buffer
	o := Options{Paths: p, Domain: "d.dev", SplitOverride: "0", Stderr: &stderr,
		Runner: homeSwitchingRunner{execx.NewRunner(p), other}}
	if err := Artifacts(t.Context(), o); err != nil {
		t.Fatalf("Artifacts: %v\n%s", err, stderr.String())
	}
	if home := os.Getenv(config.KustomizePluginHomeEnv); home != other {
		t.Fatalf("the probe did not run through the fake Runner: KUSTOMIZE_PLUGIN_HOME = %q, want %q", home, other)
	}
	if raw, err := os.ReadFile(envlog); err != nil {
		t.Errorf("the render did not log its plugin home: %v", err)
	} else if got := strings.TrimSpace(string(raw)); got != checked {
		t.Errorf("kustomize got KUSTOMIZE_PLUGIN_HOME %q, want the checked home %q", got, checked)
	}
	if _, err := os.Stat(renderLog); err != nil {
		t.Errorf("the render did not run: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); !strings.Contains(string(got), "name: a") {
		t.Errorf("artifacts.yaml = %q, want the render", got)
	}
	raw, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("the probe did not run: %v", err)
	}
	got := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	if cwd := got["cwd"]; filepath.Dir(cwd) != tmp || !strings.HasPrefix(filepath.Base(cwd), "lok8s-no-mint-") {
		t.Errorf("the probe ran in %q, want a new lok8s-no-mint-* directory in TMPDIR %s", cwd, tmp)
	}
	for k, want := range map[string]string{
		"args":        "--capabilities",
		"cwd-entries": "",
		"secrets":     filepath.Join(got["cwd"], "store"),
		"disable":     "1",
		"keys-only":   "0",
		"stdin":       "",
	} {
		if got[k] != want {
			t.Errorf("probe %s = %q, want %q", k, got[k], want)
		}
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Errorf("the probe left %v in TMPDIR", left)
	}

	t.Setenv(config.KustomizePluginHomeEnv, checked)
	direct := Options{Paths: p, Domain: "d.dev"}
	if err := checkNoMintPlugin(t.Context(), &direct, &bytes.Buffer{}); err != nil {
		t.Fatalf("checkNoMintPlugin: %v", err)
	}
	if direct.pluginHome != checked {
		t.Errorf("the check kept the home %q, want %q", direct.pluginHome, checked)
	}
}

// homeSwitchingRunner runs each command with the real Runner. After the
// probe of the Secret plugin, it sets KUSTOMIZE_PLUGIN_HOME to home in the
// process environment (the test registers the variable with t.Setenv
// first, so the test restores it).
type homeSwitchingRunner struct {
	execx.Runner
	home string
}

func (r homeSwitchingRunner) Run(ctx context.Context, c execx.Cmd) error {
	err := r.Runner.Run(ctx, c)
	if filepath.Base(c.Name) == "Secret" && slices.Equal(c.Args, []string{"--capabilities"}) {
		_ = os.Setenv(config.KustomizePluginHomeEnv, r.home)
	}
	return err
}

// With the switch off (unset, or an off value), a build that reads the
// store and a --no-secrets build with the switch on do not probe the
// plugin: a plugin without the no-mint run renders as before.
func TestNoMintOffOrNoSecretsDoesNotProbe(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		set         bool
		noSecrets   bool
	}{
		{"unset", "", false, false},
		{"empty", "", true, false},
		{"0", "0", true, false},
		{"false", "false", true, false},
		{"FALSE with spaces", " FALSE ", true, false},
		{"--no-secrets", "1", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, probeLog, renderLog, _ := noMintProject(t, "printf 'dry-run\\nkeys-only\\n'")
			t.Setenv(secret.NoMintEnv, tc.value)
			if !tc.set {
				if err := os.Unsetenv(secret.NoMintEnv); err != nil {
					t.Fatal(err)
				}
			}
			if stderr, err := buildArtifacts(t, p, tc.noSecrets); err != nil {
				t.Fatalf("Artifacts: %v\n%s", err, stderr)
			}
			if _, err := os.Stat(probeLog); err == nil {
				t.Error("the plugin was probed")
			}
			if _, err := os.Stat(renderLog); err != nil {
				t.Errorf("the render did not run: %v", err)
			}
		})
	}
}

// The in-process render serves the plugin from this binary: there is no
// plugin file to check.
func TestNoMintInProcessDoesNotProbe(t *testing.T) {
	p, _, probeLog, _, _ := noMintProject(t, "printf 'dry-run\\nkeys-only\\n'")
	t.Setenv(render.ModeEnv, string(render.ModeInProcess))
	if mode, err := render.CurrentMode(); err != nil || mode != render.ModeInProcess {
		t.Skipf("no in-process render in this binary (%v, %v)", mode, err)
	}
	t.Setenv(secret.NoMintEnv, "1")
	o := Options{Paths: p, Domain: "d.dev"}
	if err := checkNoMintPlugin(t.Context(), &o, &bytes.Buffer{}); err != nil {
		t.Fatalf("checkNoMintPlugin: %v", err)
	}
	if _, err := os.Stat(probeLog); err == nil {
		t.Error("the plugin was probed")
	}
}

// An absent plugin home, a home without the Secret executable, a probe
// that cannot make its directory and a probe that a signal stops each
// have their own line, and the render does not run. kustomize takes the
// first plugin home that exists, so with an absent home it would load a
// plugin that lo did not check (the old plugin in XDG_CONFIG_HOME would
// write a canary).
func TestNoMintCheckFailures(t *testing.T) {
	const plugin = ".kustomize/secrets.lok8s.dev/v1/secret/Secret"
	for _, tc := range []struct {
		name, want string
		prepare    func(t *testing.T, p *config.Paths)
	}{
		{"absent home",
			"[error] lo build: LOK8S_SECRETS_NO_MINT is set, but the kustomize plugin home .kustomize does not exist, so kustomize would load plugins from other directories that lo does not check: run 'lo toolchain install' to install the plugin of this lo\n",
			func(t *testing.T, p *config.Paths) {
				if err := os.RemoveAll(filepath.Join(p.Base, ".kustomize")); err != nil {
					t.Fatal(err)
				}
				xdg := filepath.Join(p.Base, "xdg")
				old := filepath.Join(xdg, "kustomize", "plugin", "secrets.lok8s.dev", "v1", "secret", "Secret")
				writeFileT(t, old, "#!/bin/sh\ntouch '"+filepath.Join(p.Base, "old-plugin-ran")+"'\necho no-mint\n")
				if err := os.Chmod(old, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_CONFIG_HOME", xdg)
			}},
		{"no Secret executable",
			"[error] lo build: LOK8S_SECRETS_NO_MINT is set, but the Secret plugin " + plugin + " does not exist, so kustomize could load another form of the plugin (a Go plugin, Secret.so) that lo does not check: run 'lo toolchain install' to install the plugin of this lo\n",
			func(t *testing.T, p *config.Paths) {
				dir := filepath.Join(p.Base, ".kustomize", "secrets.lok8s.dev", "v1", "secret")
				if err := os.Remove(filepath.Join(dir, "Secret")); err != nil {
					t.Fatal(err)
				}
				writeFileT(t, filepath.Join(dir, "Secret.so"), "a Go plugin\n")
			}},
		{"no temporary directory",
			"[error] lo build: cannot make a temporary directory for the check of the Secret plugin " + plugin + ": ",
			func(t *testing.T, p *config.Paths) { t.Setenv("TMPDIR", filepath.Join(p.Base, "no-such-dir")) }},
		{"stopped by a signal",
			"[error] lo build: the check of the Secret plugin " + plugin + " was interrupted\n",
			func(t *testing.T, p *config.Paths) { stubSecretPlugin(t, p, "kill -TERM $$") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, domainDir, _, renderLog, _ := noMintProject(t, "printf 'no-mint\\n'")
			tc.prepare(t, p)
			t.Setenv(secret.NoMintEnv, "1")
			stderr, err := buildArtifacts(t, p, false)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("Artifacts = %v, want ErrHandled\n%s", err, stderr)
			}
			if !strings.HasPrefix(stderr, tc.want) || strings.Count(stderr, "\n") != 1 {
				t.Errorf("stderr = %q\nwant one line with the prefix %q", stderr, tc.want)
			}
			for _, f := range []string{renderLog, filepath.Join(p.Base, "old-plugin-ran")} {
				if _, err := os.Stat(f); err == nil {
					t.Errorf("%s exists: the render or the old plugin ran", f)
				}
			}
			if got, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); string(got) != "kind: Prior\n" {
				t.Errorf("artifacts.yaml = %q, want the prior content", got)
			}
		})
	}
}

// checkKeysOnlyPlugin and checkNoMintPlugin share the probe run
// (probeCapability). The keys-only check still asks for its own line: a
// plugin with the keys-only run passes, one with only the no-mint run is
// refused, and a probe that a signal stops is reported as interrupted.
// This test needs no yq, so it runs where the keys-only render tests skip.
func TestKeysOnlyCheckAsksForItsOwnLine(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"keys-only", "printf 'dry-run\\nkeys-only\\n'", ""},
		{"no-mint only", "printf 'dry-run\\nno-mint\\n'", "does not have the keys-only run"},
		{"stopped by a signal", "kill -TERM $$", "lo build --keys-only: the check of the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret was interrupted\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := keysOnlyExecProject(t)
			t.Setenv("TMPDIR", t.TempDir())
			stubSecretPlugin(t, p, tc.body)
			o := Options{Paths: p, Domain: "d.dev"}
			var stderr bytes.Buffer
			err := checkKeysOnlyPlugin(t.Context(), &o, &stderr)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkKeysOnlyPlugin: %v\n%s", err, stderr.String())
				}
				return
			}
			if !errors.Is(err, ErrHandled) || !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("checkKeysOnlyPlugin = %v, stderr %q; want ErrHandled and %q", err, stderr.String(), tc.want)
			}
		})
	}
}

// A relative KUSTOMIZE_PLUGIN_HOME resolves from the working directory of
// lo. Both checks probe the plugin of that home and return it, or keep it
// for kustomize, as an absolute path: kustomize runs an exec plugin from
// the kustomization directory, where a relative home names another
// plugin. bats: "build::_abs_plugin_home gives the path that filepath.Abs
// gives".
func TestPluginChecksResolveARelativeHome(t *testing.T) {
	p, _, probeLog, _, _ := noMintProject(t, "printf 'dry-run\\nkeys-only\\nno-mint\\n'")
	t.Setenv(secret.NoMintEnv, "1")
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	t.Chdir(p.Base)
	t.Setenv(config.KustomizePluginHomeEnv, ".kustomize")
	want := filepath.Join(p.Base, ".kustomize")
	var stderr bytes.Buffer
	home, err := CheckNoMintPlugin(t.Context(), p, nil, &stderr, "lo build")
	if err != nil {
		t.Fatalf("CheckNoMintPlugin: %v\n%s", err, stderr.String())
	}
	if home != want {
		t.Errorf("no-mint home = %q, want %q", home, want)
	}
	o := Options{Paths: p}
	if err := checkKeysOnlyPlugin(t.Context(), &o, &stderr); err != nil {
		t.Fatalf("checkKeysOnlyPlugin: %v\n%s", err, stderr.String())
	}
	if o.pluginHome != want {
		t.Errorf("keys-only home = %q, want %q", o.pluginHome, want)
	}
	raw, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("the probes did not run the plugin: %v", err)
	}
	if n := strings.Count(string(raw), "args=--capabilities\n"); n != 2 {
		t.Errorf("probes = %d, want 2\n%s", n, raw)
	}

	// A relative home that does not exist resolves too, and the check
	// refuses it by its path in the project.
	t.Setenv(config.KustomizePluginHomeEnv, "no-such-home")
	stderr.Reset()
	if _, err := CheckNoMintPlugin(t.Context(), p, nil, &stderr, "lo build"); !errors.Is(err, ErrHandled) {
		t.Fatalf("CheckNoMintPlugin with a missing home = %v, want ErrHandled", err)
	}
	wantErr := "[error] lo build: LOK8S_SECRETS_NO_MINT is set, but the kustomize plugin home no-such-home does not exist, so kustomize would load plugins from other directories that lo does not check: run 'lo toolchain install' to install the plugin of this lo\n"
	if stderr.String() != wantErr {
		t.Errorf("stderr = %q\nwant %q", stderr.String(), wantErr)
	}
}

// absPluginHome gives the path that filepath.Abs gives, for each row of
// testdata/abs_plugin_home.tsv: a relative home gets the working directory
// before it, and the path is cleaned by its text. The bats twin reads the
// same table: "build::_abs_plugin_home gives the path that filepath.Abs
// gives".
func TestAbsPluginHomeMatchesFilepathAbs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "abs_plugin_home.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	p := testPaths(t)
	root := t.TempDir()
	cwd := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	rows := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		in, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("row %q has no tab", line)
		}
		want = strings.ReplaceAll(want, "@ROOT@", root)
		if got, err := filepath.Abs(in); err != nil || got != want {
			t.Errorf("filepath.Abs(%q) = %q, %v; want %q", in, got, err, want)
		}
		t.Setenv(config.KustomizePluginHomeEnv, in)
		if got, err := absPluginHome(p); err != nil || got != want {
			t.Errorf("absPluginHome with %q = %q, %v; want %q", in, got, err, want)
		}
		rows++
	}
	if rows < 15 {
		t.Errorf("the table has %d rows, want 15 or more", rows)
	}
}

// execPluginKustomize is a stub kustomize that runs the Secret plugin the
// way kustomize runs an exec plugin. It joins the plugin path by its text
// (kustomize: filepath.Join, which cleans repeated slashes, "." and ".."
// elements; here: awk), then runs the plugin from the kustomization
// directory (the last argument). A relative home thus resolves from that
// directory, and a ".." after a symlinked directory names the directory
// next to the link. It prints the render of stubRender.
const execPluginKustomize = `#!/bin/sh
for a in "$@"; do dir=$a; done
plugin=$(awk -v p="$KUSTOMIZE_PLUGIN_HOME/secrets.lok8s.dev/v1/secret/Secret" 'BEGIN {
	n = split(p, a, "/"); abs = (substr(p, 1, 1) == "/"); k = 0
	for (i = 1; i <= n; i++) {
		s = a[i]
		if (s == "" || s == ".") continue
		if (s == "..") {
			if (k > 0 && o[k] != "..") { k--; continue }
			if (abs) continue
		}
		o[++k] = s
	}
	r = abs ? "/" : ""
	for (i = 1; i <= k; i++) r = r (i > 1 ? "/" : "") o[i]
	if (r == "") r = "."
	print r
}')
(cd "$dir" && "$plugin" </dev/null >/dev/null 2>&1)
cat "$KUSTOMIZE_STUB_OUTPUT"
`

// labelPlugin writes a Secret plugin into home that appends "probe <label>"
// or "run <label>" to log and prints caps for --capabilities.
func labelPlugin(t *testing.T, home, label, caps, log string) {
	t.Helper()
	path := filepath.Join(home, "secrets.lok8s.dev", "v1", "secret", "Secret")
	writeFileT(t, path, "#!/bin/sh\n"+
		"if [ \"$1\" = --capabilities ]; then echo 'probe "+label+"' >> '"+log+"'; printf '"+caps+"'; exit 0; fi\n"+
		"echo 'run "+label+"' >> '"+log+"'\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// With a relative KUSTOMIZE_PLUGIN_HOME, kustomize runs the plugin that lo
// probed. Two plugins record their runs: the one in the plugin home of
// the working directory (it has the no-mint run), and an older one at the
// same relative path under the domain directory, which kustomize would run
// with the relative home. lo build gives kustomize the absolute home, so
// the probed plugin runs and the older one does not. bats:
// "k8s::infrastructure and k8s::platform with no-mint and a relative plugin
// home run the probed plugin".
func TestNoMintRelativeHomeRunsTheProbedPlugin(t *testing.T) {
	p, domainDir := artifactsProject(t)
	stubRender(t, p, twoDocRender)
	if err := os.WriteFile(filepath.Join(p.Bin, "kustomize"), []byte(execPluginKustomize), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(p.Base, "plugin-runs.log")
	labelPlugin(t, filepath.Join(p.Base, ".kustomize"), "project", "dry-run\\nkeys-only\\nno-mint\\n", log)
	labelPlugin(t, filepath.Join(domainDir, ".kustomize"), "domain-dir", "dry-run\\nkeys-only\\n", log)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(secret.NoMintEnv, "1")
	t.Chdir(p.Base)
	t.Setenv(config.KustomizePluginHomeEnv, ".kustomize")
	stderr, err := buildArtifacts(t, p, false)
	if err != nil {
		t.Fatalf("Artifacts: %v\n%s", err, stderr)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), "probe project\nrun project\n"; got != want {
		t.Errorf("plugin runs = %q, want %q", got, want)
	}
}

// With a symlinked working directory, kustomize joins the plugin path by
// its text: <link>/../plugins is the plugins directory next to the link,
// not the one next to the target of the link. lo resolves the home the
// same way (filepath.Abs cleans by the text), so it probes the plugin that
// kustomize runs. With an older plugin next to the link, lo build refuses
// it, also when the plugin next to the target has the no-mint run. bats:
// "k8s::infrastructure and k8s::platform with no-mint, a symlinked working
// directory and ../plugins probe the plugin that runs".
func TestNoMintSymlinkedWorkingDirectoryProbesThePluginThatRuns(t *testing.T) {
	const noMint, older = "dry-run\\nkeys-only\\nno-mint\\n", "dry-run\\nkeys-only\\n"
	for _, tc := range []struct {
		name, nextToLink, nextToTarget, runs string
		refused                              bool
	}{
		{"the plugin next to the link has the no-mint run", noMint, older, "probe next-to-link\nrun next-to-link\n", false},
		{"the plugin next to the link is older", older, noMint, "probe next-to-link\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := artifactsProject(t)
			stubRender(t, p, twoDocRender)
			if err := os.WriteFile(filepath.Join(p.Bin, "kustomize"), []byte(execPluginKustomize), 0o755); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			target := filepath.Join(root, "srv", "project")
			link := filepath.Join(root, "work", "link")
			for _, dir := range []string{target, filepath.Dir(link)} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(root, "plugin-runs.log")
			labelPlugin(t, filepath.Join(root, "work", "plugins"), "next-to-link", tc.nextToLink, log)
			labelPlugin(t, filepath.Join(root, "srv", "plugins"), "next-to-target", tc.nextToTarget, log)
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv(secret.NoMintEnv, "1")
			t.Chdir(link)
			t.Setenv(config.KustomizePluginHomeEnv, "../plugins")
			stderr, err := buildArtifacts(t, p, false)
			if tc.refused {
				if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "does not have the no-mint run") {
					t.Fatalf("Artifacts = %v, want ErrHandled and the no-mint refusal\n%s", err, stderr)
				}
			} else if err != nil {
				t.Fatalf("Artifacts: %v\n%s", err, stderr)
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(raw); got != tc.runs {
				t.Errorf("plugin runs = %q, want %q", got, tc.runs)
			}
		})
	}
}

// Without a project there is no plugin home to check. With the switch on,
// the check refuses with one line that names the caller. With the switch
// off, it does not check and needs no project.
func TestNoMintCheckWithoutAProject(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	t.Setenv(secret.NoMintEnv, "1")
	var stderr bytes.Buffer
	home, err := CheckNoMintPlugin(t.Context(), nil, nil, &stderr, "lo k8s")
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("CheckNoMintPlugin = %v, want ErrHandled\n%s", err, stderr.String())
	}
	if home != "" {
		t.Errorf("home = %q, want none", home)
	}
	want := "[error] lo k8s: LOK8S_SECRETS_NO_MINT is set, but there is no project to find the kustomize plugin home in, so lo cannot check the Secret plugin\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q\nwant %q", stderr.String(), want)
	}

	t.Setenv(secret.NoMintEnv, "0")
	stderr.Reset()
	if home, err := CheckNoMintPlugin(t.Context(), nil, nil, &stderr, "lo k8s"); err != nil || home != "" || stderr.Len() != 0 {
		t.Errorf("switch off: home %q, err %v, stderr %q; want none", home, err, stderr.String())
	}
}
