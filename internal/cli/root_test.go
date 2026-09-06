package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

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
	for _, line := range strings.Split(text[start:], "\n") {
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

func TestCommandTreeMatchesArgshUsage(t *testing.T) {
	want := parseArgshUsage(t)
	got := map[string]commandSpec{}
	for _, s := range commandTree {
		got[s.use] = s
	}

	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("command %q exists in .lok8s/lo but not in the Go tree", name)
			continue
		}
		if len(w.aliases) > 0 && (len(g.aliases) == 0 || g.aliases[0] != w.aliases[0]) {
			t.Errorf("command %q: alias mismatch: bash %v, go %v", name, w.aliases, g.aliases)
		}
		if g.hidden != w.hidden {
			t.Errorf("command %q: hidden mismatch: bash %v, go %v", name, w.hidden, g.hidden)
		}
		if g.destructive != w.destructive || g.readonly != w.readonly || g.idempotent != w.idempotent {
			t.Errorf("command %q: annotation mismatch: bash {d:%v r:%v i:%v}, go {d:%v r:%v i:%v}",
				name, w.destructive, w.readonly, w.idempotent, g.destructive, g.readonly, g.idempotent)
		}
		if g.short != w.short {
			t.Errorf("command %q: short text drift:\n  bash: %s\n  go:   %s", name, w.short, g.short)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("command %q exists in the Go tree but not in .lok8s/lo", name)
		}
	}

	// The assembled root may carry more than commandTree: the Go-only
	// commands. Each one must be allowlisted in goOnlyCommands with a reason,
	// and must NOT exist in the bash tree (once it does, it belongs in
	// commandTree so the mirror stays one-to-one).
	goOnly := map[string]goOnlyCommand{}
	for _, g := range goOnlyCommands {
		if g.why == "" || g.build == nil {
			t.Errorf("goOnlyCommands[%q]: needs both a reason and a builder", g.name)
		}
		if _, inBash := want[g.name]; inBash {
			t.Errorf("Go-only command %q now exists in .lok8s/lo: move it into commandTree", g.name)
		}
		goOnly[g.name] = g
	}
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	for _, cmd := range root.Commands() {
		name := cmd.Name()
		if w, ok := want[name]; ok {
			// commandTree mirrors the usage array, but a builder may still
			// hard-code its own Short; the ASSEMBLED tree is what `lo --help`
			// prints, so it is compared too. (Flags, arg arity and long help
			// are outside this gate.)
			if cmd.Short != w.short {
				t.Errorf("command %q: assembled Short drifts from .lok8s/lo:\n  bash: %s\n  go:   %s", name, w.short, cmd.Short)
			}
			continue
		}
		if _, ok := goOnly[name]; ok {
			continue
		}
		t.Errorf("command %q is in the Go root but neither in .lok8s/lo nor allowlisted in goOnlyCommands", name)
	}
}

func TestNewRootRegistersEveryCommand(t *testing.T) {
	paths := &config.Paths{Base: t.TempDir()}
	root := NewRoot(paths)
	for _, g := range goOnlyCommands {
		cmd, _, err := root.Find([]string{g.name})
		if err != nil || cmd.Name() != g.name {
			t.Errorf("Go-only command %q not resolvable: %v", g.name, err)
		}
	}
	for _, spec := range commandTree {
		cmd, _, err := root.Find([]string{spec.use})
		if err != nil || cmd.Name() != spec.use {
			t.Errorf("command %q not resolvable: %v", spec.use, err)
		}
		for _, alias := range spec.aliases {
			cmd, _, err := root.Find([]string{alias})
			if err != nil || cmd.Name() != spec.use {
				t.Errorf("alias %q does not resolve to %q: %v", alias, spec.use, err)
			}
		}
	}
}

func TestShimEnvPreparesPathAndPluginHome(t *testing.T) {
	p := &config.Paths{
		Base:  "/proj",
		Bin:   "/proj/.bin",
		Lok8s: "/proj/.lok8s",
	}
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")

	env := shimEnv(p)
	var path, pluginHome string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
		if v, ok := strings.CutPrefix(kv, "KUSTOMIZE_PLUGIN_HOME="); ok {
			pluginHome = v
		}
	}
	if want := "/proj/.bin:/proj/.lok8s:/usr/bin"; path != want {
		t.Errorf("PATH = %q, want %q", path, want)
	}
	if want := filepath.Join("/proj", ".kustomize"); pluginHome != want {
		t.Errorf("KUSTOMIZE_PLUGIN_HOME = %q, want %q", pluginHome, want)
	}

	// Already-present entries are not duplicated.
	t.Setenv("PATH", "/proj/.bin:/usr/bin")
	env = shimEnv(p)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if strings.Count(v, "/proj/.bin") != 1 {
				t.Errorf("PATH duplicated .bin entry: %q", v)
			}
		}
	}
}
