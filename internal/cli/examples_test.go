package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
)

// TestEveryVisibleCommandHasExample keeps commandExamples complete in both
// directions: every visible command carries an Example block in the one
// shape (lines of `  lo …`), and every key in the map names a command
// that exists (a renamed command cannot leave a stale example behind).
func TestEveryVisibleCommandHasExample(t *testing.T) {
	root := NewRoot(&config.Paths{Base: t.TempDir()})

	seen := map[string]bool{}
	walkVisibleCommands(root, func(cmd *cobra.Command) {
		path := commandPathBelowRoot(cmd)
		seen[path] = true
		if cmd.Example == "" {
			t.Errorf("lo %s: no Example block (add it to commandExamples)", path)
			return
		}
		lines := strings.Split(cmd.Example, "\n")
		if len(lines) > 3 {
			t.Errorf("lo %s: %d example lines, the shape is one to three", path, len(lines))
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
				t.Errorf("lo %s: example line %q must start with exactly two spaces", path, line)
			}
			if !strings.Contains(line, "lo ") {
				t.Errorf("lo %s: example line %q does not show a lo command", path, line)
			}
			if strings.HasSuffix(line, ".") {
				t.Errorf("lo %s: example line %q ends with a period", path, line)
			}
		}
	})

	for path := range commandExamples {
		if _, ok := seen[path]; !ok {
			if findByPath(root, path) == nil {
				t.Errorf("commandExamples[%q]: no such command", path)
			}
		}
	}
}

// TestShortDescriptionsShape audits the one-line Short of every visible
// Go-only command (the ported top-level ones mirror .lok8s/lo verbatim and
// are gated by TestCommandTreeMatchesArgshUsage): no trailing period, at
// most 60 characters.
func TestShortDescriptionsShape(t *testing.T) {
	root := NewRoot(&config.Paths{Base: t.TempDir()})
	goOnly := map[string]bool{}
	for _, g := range goOnlyCommands {
		goOnly[g.name] = true
	}
	walkVisibleCommands(root, func(cmd *cobra.Command) {
		if !goOnly[topLevelName(cmd)] {
			return
		}
		path := commandPathBelowRoot(cmd)
		if strings.HasSuffix(cmd.Short, ".") {
			t.Errorf("lo %s: Short ends with a period: %q", path, cmd.Short)
		}
		if n := len([]rune(cmd.Short)); n > 60 {
			t.Errorf("lo %s: Short is %d characters, the limit is 60: %q", path, n, cmd.Short)
		}
	})
}
