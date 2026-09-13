package cli

import (
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

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
