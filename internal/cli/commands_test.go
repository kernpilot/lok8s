package cli

// commands_test.go is the tree-drift gate: commandTree must mirror the
// argsh usage array in .lok8s/lo, and the assembled root may add only the
// allowlisted Go-only commands.

import (
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

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
			// prints, so it is compared too — top-level commands only.
			// (Subgroup leaves, flags, arg arity and long help are outside
			// this gate.)
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
