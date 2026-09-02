package cli

// lo use — set or show the active domain (clusters/.active).
// Go port of .lok8s/libs/use; output is byte-identical.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("use", newUseCommand) }

func newUseCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "use [domain]",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The target comes from a positional or an EXPLICIT --domain flag.
			// Ambient resolution (env/.active) must NOT set it: a bare
			// `lo use` under an exported DOMAIN_NAME would silently rewrite
			// .active from the environment.
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			if target == "" {
				if f := cmd.Flags().Lookup("domain"); f != nil && f.Changed && f.Value.String() != "" {
					target = f.Value.String()
				}
			}
			if target != "" {
				return useSetActive(paths, target, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return useShow(paths, cmd.OutOrStdout())
		},
	}
	return cmd
}

// useSetActive validates and persists the active domain (bash:
// use::_set_active). Rejects names that fail the character allowlist
// (path-traversal guard) or that don't resolve to a real domain directory.
func useSetActive(paths *config.Paths, target string, out, errOut io.Writer) error {
	if !domain.NameRe.MatchString(target) {
		ui.Errorf(errOut, "invalid domain name: %s", target)
		return ErrHandled
	}
	base := filepath.Join(paths.Clusters, target)
	if !fileExists(filepath.Join(base, "cluster.lok8s.yaml")) && !fileExists(filepath.Join(base, "deploy.lok8s.yaml")) {
		ui.Errorf(errOut, "domain not found: clusters/%s/ (no cluster.lok8s.yaml or deploy.lok8s.yaml)", target)
		return ErrHandled
	}
	if err := os.MkdirAll(paths.Clusters, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(paths.Clusters, ".active"), []byte(target+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Active domain: %s\n", target)
	return nil
}

// useShow prints the active domain plus every domain under clusters/, each
// with what it is (bash: use::_show).
func useShow(paths *config.Paths, out io.Writer) error {
	if raw, err := os.ReadFile(filepath.Join(paths.Clusters, ".active")); err == nil {
		fmt.Fprintf(out, "Active: %s\n", trimTrailingNewline(string(raw)))
	} else {
		fmt.Fprintln(out, "No active domain set.")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Available domains:")
	for _, specPath := range sortedGlob(filepath.Join(paths.Clusters, "*", "cluster.lok8s.yaml")) {
		d := filepath.Base(filepath.Dir(specPath))
		k, err := domain.SpecDriver(specPath, "?")
		if err != nil {
			k = "?"
		}
		fmt.Fprintf(out, "  %s (%s)\n", d, k)
	}
	for _, specPath := range sortedGlob(filepath.Join(paths.Clusters, "*", "deploy.lok8s.yaml")) {
		d := filepath.Base(filepath.Dir(specPath))
		fmt.Fprintf(out, "  %s (Deploy -> %s)\n", d, deployClusterRef(specPath))
	}
	return nil
}

// deployClusterRef reads .spec.clusterRef.domain from a deploy spec, "?" when
// missing or unreadable (bash: yq -r '.spec.clusterRef.domain // "?"').
func deployClusterRef(specPath string) string {
	var doc struct {
		Spec struct {
			ClusterRef struct {
				Domain string `yaml:"domain"`
			} `yaml:"clusterRef"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(specPath)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil || doc.Spec.ClusterRef.Domain == "" {
		return "?"
	}
	return doc.Spec.ClusterRef.Domain
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// sortedGlob matches bash's alphabetically-sorted glob expansion.
func sortedGlob(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	return matches
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
