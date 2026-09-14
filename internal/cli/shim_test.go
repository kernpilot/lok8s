package cli

// shim_test.go covers the env the bash shim inherits.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/ui"
)

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestShimEnvPreparesPathAndPluginHome(t *testing.T) {
	p := &config.Paths{
		Base:  "/proj",
		Bin:   "/proj/.bin",
		Lok8s: "/proj/.lok8s",
	}
	local := assets.Tree{Dir: p.Lok8s, Source: assets.TreeProject}
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	t.Setenv("PATH_BASE", "")
	os.Unsetenv("PATH_BASE")
	t.Setenv("PATH_LOK8S", "")
	os.Unsetenv("PATH_LOK8S")

	env := shimEnv(p, local)
	path, _ := envValue(env, "PATH")
	pluginHome, _ := envValue(env, "KUSTOMIZE_PLUGIN_HOME")
	if want := "/proj/.bin:/proj/.lok8s:/usr/bin"; path != want {
		t.Errorf("PATH = %q, want %q", path, want)
	}
	if want := filepath.Join("/proj", ".kustomize"); pluginHome != want {
		t.Errorf("KUSTOMIZE_PLUGIN_HOME = %q, want %q", pluginHome, want)
	}
	// A local tree: no PATH_* exported (the entrypoint derives them).
	for _, key := range []string{"PATH_BASE", "PATH_LOK8S"} {
		if v, ok := envValue(env, key); ok {
			t.Errorf("%s=%q exported for a local tree", key, v)
		}
	}

	// Already-present entries are not duplicated.
	t.Setenv("PATH", "/proj/.bin:/usr/bin")
	env = shimEnv(p, local)
	if v, _ := envValue(env, "PATH"); strings.Count(v, "/proj/.bin") != 1 {
		t.Errorf("PATH duplicated .bin entry: %q", v)
	}

	// The cache: the tree cannot derive the project from its location, so
	// PATH_BASE and PATH_LOK8S are set and the tree leads PATH.
	t.Setenv("PATH", "/usr/bin")
	cache := assets.Tree{Dir: "/home/u/.cache/lok8s/1.0.0/lok8s", Source: assets.TreeCache}
	env = shimEnv(p, cache)
	if v, _ := envValue(env, "PATH_BASE"); v != "/proj" {
		t.Errorf("PATH_BASE = %q", v)
	}
	if v, _ := envValue(env, "PATH_LOK8S"); v != cache.Dir {
		t.Errorf("PATH_LOK8S = %q", v)
	}
	if v, _ := envValue(env, "PATH"); !strings.HasPrefix(v, "/proj/.bin:"+cache.Dir+":") {
		t.Errorf("PATH = %q", v)
	}
	// The project's .lok8s while PATH_LOK8S points elsewhere: PATH_LOK8S
	// is corrected, PATH_BASE stays derived.
	q := *p
	q.Lok8s = "/elsewhere"
	env = shimEnv(&q, local)
	if v, _ := envValue(env, "PATH_LOK8S"); v != p.Lok8s {
		t.Errorf("PATH_LOK8S = %q", v)
	}
	if _, ok := envValue(env, "PATH_BASE"); ok {
		t.Error("PATH_BASE exported for a project tree")
	}
}

// Shim resolves the tree through assets.BashTree: a project without a
// tree gets the embedded copy from the cache, and the exec'd script is
// that tree's lo.
func TestShimResolvesTheCacheWithoutACheckout(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv(assets.EnvCacheHome, cacheRoot)
	p := synthProject(t)
	tree, err := assets.BashTree(p)
	if err != nil || tree.Source != assets.TreeCache {
		t.Fatalf("%+v %v", tree, err)
	}
	if !strings.HasPrefix(tree.Dir, cacheRoot) {
		t.Fatalf("tree outside the cache: %s", tree.Dir)
	}
	if info, err := os.Stat(filepath.Join(tree.Dir, "lo")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("cache lo: %v %v", info, err)
	}
	env := shimEnv(p, tree)
	if v, _ := envValue(env, "PATH_LOK8S"); v != tree.Dir {
		t.Errorf("PATH_LOK8S = %q", v)
	}
	if v, _ := envValue(env, "PATH_BASE"); v != p.Base {
		t.Errorf("PATH_BASE = %q", v)
	}
	// Nothing landed in the project.
	if _, err := os.Stat(filepath.Join(p.Lok8s, "lo")); err == nil {
		t.Fatal("the shim ejected the tree into the project")
	}
}

// stripNoColor takes the flag in every form and position and leaves the
// rest of argv, and everything after `--`, as it is.
func TestStripNoColor(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
		on   bool
	}{
		{[]string{"status"}, []string{"status"}, false},
		{[]string{"--no-color", "status"}, []string{"status"}, true},
		{[]string{"status", "--no-color"}, []string{"status"}, true},
		{[]string{"status", "--no-color=true", "--domain", "x"}, []string{"status", "--domain", "x"}, true},
		{[]string{"status", "--no-color=false"}, []string{"status"}, false},
		{[]string{"--no-color", "secrets", "set", "k", "--", "--no-color"}, []string{"secrets", "set", "k", "--", "--no-color"}, true},
		{[]string{"status", "--no-colors"}, []string{"status", "--no-colors"}, false},
	}
	for _, c := range cases {
		got, on := stripNoColor(c.in)
		if strings.Join(got, " ") != strings.Join(c.want, " ") || on != c.on {
			t.Errorf("stripNoColor(%q) = %q, %v; want %q, %v", c.in, got, on, c.want, c.on)
		}
	}
}

// A routed command never hands --no-color to the bash tree: the flag is
// stripped from argv (before or after the subcommand) and reaches the
// child as NO_COLOR=1; without the flag the environment is left alone.
func TestShimNoColorReachesTheTreeAsEnv(t *testing.T) {
	os.Unsetenv("NO_COLOR")
	t.Cleanup(func() { ui.SetNoColor(false); os.Unsetenv("NO_COLOR") })
	for _, c := range []struct {
		args    []string
		wantArg string
		wantEnv bool
	}{
		{[]string{"status", "--no-color"}, "status", true},
		{[]string{"--no-color", "status"}, "status", true},
		{[]string{"status", "--no-color=true", "--domain", "x.dev"}, "status --domain x.dev", true},
		{[]string{"status", "--no-color=false"}, "status", false},
		{[]string{"status"}, "status", false},
	} {
		ui.SetNoColor(false)
		os.Unsetenv("NO_COLOR")
		p := routedProject(t, "    default: bash\n", true)
		recs := recordShim(t)
		setOSArgs(t, c.args...)
		if _, _, err := runLo(t, NewRoot(p), c.args...); err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if len(*recs) != 1 {
			t.Fatalf("%q: %d execs", c.args, len(*recs))
		}
		rec := (*recs)[0]
		if got := strings.Join(rec.argv[2:], " "); got != c.wantArg {
			t.Errorf("%q: argv = %q, want %q", c.args, got, c.wantArg)
		}
		if v, ok := envValue(rec.env, "NO_COLOR"); ok != c.wantEnv || (ok && v != "1") {
			t.Errorf("%q: NO_COLOR in env = %q, %v; want present=%v", c.args, v, ok, c.wantEnv)
		}
	}
}
