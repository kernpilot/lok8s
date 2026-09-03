package cli

// lo version — print lok8s + toolchain versions.
// Go port of .lok8s/libs/version. The bash line is gone (this is not bash);
// everything else is format-identical: `%-11s %s` per tool, best-effort
// version extraction, "present" when unparsable.

import (
	"fmt"
	"os/exec"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

// versionTools lists the external tools reported, in bash's order, with the
// extraction each one used (nil regex = first line verbatim).
var versionTools = []struct {
	name string
	args []string
	re   *regexp.Regexp
}{
	{"argsh", []string{"--version"}, nil},
	{"yq", []string{"--version"}, regexp.MustCompile(`v?[0-9]+\.[0-9]+\.[0-9]+`)},
	{"kustomize", []string{"version"}, regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+`)},
	{"kubectl", []string{"version", "--client"}, regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+`)},
	{"kind", []string{"--version"}, regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)},
	{"tilt", []string{"version"}, regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+`)},
	{"docker", []string{"--version"}, regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)},
}

func init() { registerPorted("version", newVersionCommand) }

func newVersionCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	return &cobra.Command{
		Use:          spec.use,
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-11s %s\n", "lok8s", lok8sVersion(paths))
			for _, tool := range versionTools {
				path, ok := execx.Look(paths, tool.name)
				if !ok {
					continue
				}
				fmt.Fprintf(out, "%-11s %s\n", tool.name, toolVersion(path, tool.args, tool.re))
			}
			return nil
		},
	}
}

// lok8sVersion is the binary's version: ldflags-stamped, else the embedded
// VERSION file. The bash implementation read .lok8s/VERSION from disk; the
// binary no longer does (the frozen tree and the embedded copy are held
// identical by the assets drift test, so the two implementations agree).
func lok8sVersion(*config.Paths) string {
	return assets.Version()
}

// toolVersion runs the tool and extracts a best-effort version string,
// "present" when unparsable (bash: version::_of).
func toolVersion(path string, args []string, re *regexp.Regexp) string {
	out, err := exec.Command(path, args...).Output()
	if err != nil && len(out) == 0 {
		return "present"
	}
	text := string(out)
	if re == nil {
		if line, _, found := cutLine(text); found {
			text = line
		}
		if text == "" {
			return "present"
		}
		return text
	}
	if m := re.FindString(text); m != "" {
		return m
	}
	return "present"
}

// cutLine returns the first line of s.
func cutLine(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", s != ""
}
