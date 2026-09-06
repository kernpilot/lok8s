package cli

// lo init toolchain — the consumer toolchain via b (Go-only, no twin in
// .lok8s/libs/init). Writes .bin/b.yaml from the pinned template
// (internal/toolchain), never overwriting an existing one (diff +
// instructions instead), appends the .gitignore entries, installs b into
// .bin/ from its pinned, checksum-verified release tarball, and runs
// `.bin/b install`. `lo init project` runs the same steps unless
// --no-toolchain.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/scaffold"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// toolchainTemplate renders the b.yaml for this binary: its own version
// pins the Secret plugin asset, its variant is named in the header.
func toolchainTemplate(name string, groups []string) string {
	return toolchain.Template(toolchain.TemplateOptions{
		Name:      name,
		LoVersion: assets.Version(),
		Variant:   render.Variant(),
		Groups:    groups,
	})
}

func newInitToolchainCommand(paths *config.Paths) *cobra.Command {
	var dir, groupsFlag string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "toolchain",
		Short: "Install the pinned toolchain via b (.bin/b.yaml + b + b install)",
		Long: `Provision a project's toolchain with b (github.com/fentas/b):

  1. .bin/b.yaml from the template pinned to this lo (kustomize ` + toolchain.KustomizeCLI + `,
     khelm v` + toolchain.KhelmVersion + `, the secrets.lok8s.dev Secret plugin at this lo's version,
     plus kubectl; kind/tilt/mkcert for --groups local; kubeone/hcloud for cloud).
     An existing b.yaml is never overwritten — a diff is printed instead.
  2. .gitignore entries for .bin/ (b.yaml + b.lock stay committed).
  3. b itself into .bin/b: the pinned release v` + toolchain.BRelease.Version + ` tarball, downloaded over
     https to a temp file and verified against its published SHA-256 before
     anything is extracted (no curl | sh). GITHUB_TOKEN is passed through if set.
  4. .bin/b install — every binary in b.yaml lands in .bin/ (plugins under .kustomize/).

--dry-run prints each step without touching the tree or the network.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			setDebugFromVerbose(cmd)
			groups, err := toolchain.NormalizeGroups(strings.Split(groupsFlag, ","))
			if err != nil {
				return err
			}
			base := dir
			if base == "" {
				base = paths.Base
			}
			return runInitToolchain(cmd, base, groups, dryRun, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVarP(&dir, "path", "p", "", "Project directory (default: the current project root)")
	cmd.Flags().StringVar(&groupsFlag, "groups", strings.Join(toolchain.DefaultGroups, ","), "Groups to activate (core,local,cloud; core is implied)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "Print what would be written, downloaded and run; touch nothing")
	return cmd
}

// projectName is the project's metadata.name from <base>/lok8s.yaml when
// present (what `lo init project <name>` wrote), else the directory name —
// so a standalone re-run renders the same header and an unchanged file is
// reported as matching, not diffed.
func projectName(base string) string {
	raw, err := os.ReadFile(filepath.Join(base, "lok8s.yaml"))
	if err == nil {
		var doc struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if yaml.Unmarshal(raw, &doc) == nil && doc.Metadata.Name != "" {
			return doc.Metadata.Name
		}
	}
	return filepath.Base(base)
}

func runInitToolchain(cmd *cobra.Command, base string, groups []string, dryRun bool, out, stderr io.Writer) error {
	bin := filepath.Join(base, ".bin")
	name := projectName(base)
	fmt.Fprintf(out, "lo init toolchain — %s (lo %s, %s; groups: %s)\n", base, assets.Version(), render.Variant(), strings.Join(groups, ","))
	if err := scaffold.WriteBYAML(bin, toolchainTemplate(name, groups), dryRun, out); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out, "would ensure .gitignore entries in %s\n", filepath.Join(base, ".gitignore"))
	} else if err := scaffold.EnsureGitignore(base, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "Toolchain (b → .bin/):")
	if err := toolchain.Bootstrap(cmd.Context(), toolchain.BootstrapOptions{
		Base: base, Bin: bin, Out: out, Stderr: stderr, DryRun: dryRun,
	}); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintln(out, "dry run — nothing was written, downloaded or run")
		return nil
	}
	fmt.Fprintln(out, "Done. Next: lo doctor   # verifies b, kustomize, khelm and the Secret plugin against the pins")
	return nil
}
