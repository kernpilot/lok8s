package cli

// lo version — print lok8s + toolchain versions.
// Go port of .lok8s/libs/version. The bash line is gone (this is not bash);
// everything else is format-identical: `%-11s %s` per tool, best-effort
// version extraction, "present" when unparsable.

import (
	"context"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
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
	var format func() (string, error)
	cmd := &cobra.Command{
		Use:          spec.use,
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := format()
			if err != nil {
				return err
			}
			report := versionReport(cmd.Context(), paths)
			if f != outputText {
				return writeOutput(cmd.OutOrStdout(), f, report)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-11s %s\n", "lok8s", report.Lok8s)
			for _, t := range report.Tools {
				fmt.Fprintf(out, "%-11s %s\n", t.Name, t.Version)
			}
			return nil
		},
	}
	format = addOutputFlag(cmd)
	return cmd
}

// versionInfo is `lo version -o json|yaml`.
type versionInfo struct {
	Lok8s string        `json:"lok8s" yaml:"lok8s"`
	Build string        `json:"build" yaml:"build"`
	Tools []versionTool `json:"tools" yaml:"tools"`
}

type versionTool struct {
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
	Path    string `json:"path" yaml:"path"`
}

// versionReport gathers the lines the text form prints: lok8s, then every
// tool on PATH in bash's order with its best-effort version.
func versionReport(ctx context.Context, paths *config.Paths) versionInfo {
	v := versionInfo{Lok8s: lok8sVersion(paths), Build: render.Variant(), Tools: []versionTool{}}
	r := execx.NewRunner(paths)
	for _, tool := range versionTools {
		path, ok := execx.Look(paths, tool.name)
		if !ok {
			continue
		}
		v.Tools = append(v.Tools, versionTool{Name: tool.name, Version: toolVersion(ctx, r, path, tool.args, tool.re), Path: path})
	}
	return v
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
func toolVersion(ctx context.Context, r execx.Runner, path string, args []string, re *regexp.Regexp) string {
	out, err := execx.Output(ctx, r, execx.Cmd{Name: path, Args: args})
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
