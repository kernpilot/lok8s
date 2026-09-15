package cli

// lo toolchain — the consumer toolchain via b (Go-only, no twin in the
// frozen tree; WP9 moved it out of `lo init`, which now writes project
// files only).
//
//	lo toolchain install [--path] [--groups] [--dry-run]
//	lo toolchain doctor
//
// install writes .bin/b.yaml from the pinned template (internal/toolchain),
// never overwriting an existing one (diff + instructions instead), appends
// the .gitignore entries, installs b into .bin/ from its pinned,
// checksum-verified release tarball, and runs `.bin/b install`. doctor is
// the pinned-tools section of `lo doctor` on its own. `lo init toolchain`
// stays for one release as a hidden alias of install.

import (
	"context"
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
	"github.com/kernpilot/lok8s/internal/ui"
)

// toolchainTemplate renders the b.yaml for this binary: its own version
// pins the Secret plugin asset, its variant is named in the header, its
// plugin home is where the `file:` lines put the exec plugins.
func toolchainTemplate(paths *config.Paths, name string, groups []string) (string, error) {
	return toolchain.Template(toolchain.TemplateOptions{
		Name:          name,
		LoVersion:     assets.Version(),
		Variant:       render.Variant(),
		Groups:        groups,
		PluginFileDir: pluginFileDir(paths),
	})
}

// pluginFileDir is the plugin home (config.KustomizePluginHome) as a path b
// resolves from .bin, for the `file:` lines of a generated b.yaml: relative
// to .bin when the home is inside the project (`../.kustomize` for the
// default, byte-identical to the template before v0.4.1), the absolute path
// otherwise. So `lo toolchain install` installs where the render and
// `lo doctor` look, with or without an exported home.
func pluginFileDir(paths *config.Paths) string {
	home := config.KustomizePluginHome(paths)
	if strings.HasPrefix(home, paths.Base+string(filepath.Separator)) {
		if rel, err := filepath.Rel(paths.Bin, home); err == nil {
			return rel
		}
	}
	return home
}

func newToolchainCommand(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "toolchain",
		Short:        "Install and verify the pinned project toolchain via b",
		GroupID:      groupConfigure,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newToolchainInstallCommand(paths, "install", false), newToolchainDoctorCommand(paths))
	return cmd
}

// toolchainInstallLong is the install help; it names the b release and the
// pins, so it is built at registration time.
func toolchainInstallLong() string {
	return `Provision a project's toolchain with b (github.com/fentas/b):

  1. .bin/b.yaml from the template pinned to this lo (kustomize ` + toolchain.KustomizeCLI + `,
     khelm v` + toolchain.KhelmVersion + `, the secrets.lok8s.dev Secret plugin at this lo's version,
     plus kubectl; kind/tilt/mkcert for --groups local; kubeone/hcloud for cloud;
     argsh/yq/jq/envsubst/sops/ssh-to-age for bash, the runtime of the frozen
     bash implementation and the provider plugins, carried commented out).
     The bash tree itself is embedded in the binary.
     An existing b.yaml is never overwritten — a diff is printed instead.
  2. .gitignore entries for .bin/ (b.yaml + b.lock stay committed).
  3. b itself into .bin/b: the pinned release v` + toolchain.BRelease.Version + ` tarball, downloaded over
     https to a temp file and verified against its published SHA-256 before
     anything is extracted (no curl | sh). GITHUB_TOKEN is passed through if set.
  4. .bin/b install — every binary in b.yaml lands in .bin/ (plugins under .kustomize/).

--dry-run prints each step without touching the tree or the network.`
}

// deprecatedInitToolchain is the one-line hint the hidden `lo init
// toolchain` alias prints on stderr before it runs `lo toolchain install`.
const deprecatedInitToolchain = "lo init toolchain is deprecated and goes away next release. Use: lo toolchain install"

// newToolchainInstallCommand builds `lo toolchain install`, or, with
// deprecated set, the hidden `lo init toolchain` alias (same flags, same
// run; the hint on stderr first).
func newToolchainInstallCommand(paths *config.Paths, use string, deprecated bool) *cobra.Command {
	var dir, groupsFlag string
	var dryRun bool
	cmd := &cobra.Command{
		Use:          use,
		Short:        "Install the pinned toolchain via b",
		Long:         toolchainInstallLong(),
		Args:         cobra.NoArgs,
		Hidden:       deprecated,
		SilenceUsage: true,
		// Mutating, never destructive: it adds files and binaries under the
		// project and never removes or overwrites (b.yaml is kept). The MCP
		// projection reads the marker: the mutating tier, idempotent.
		Annotations: map[string]string{AnnotationIdempotent: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			setDebugFromVerbose(cmd)
			if deprecated {
				fmt.Fprintln(cmd.ErrOrStderr(), deprecatedInitToolchain)
			}
			groups, err := toolchain.NormalizeGroups(strings.Split(groupsFlag, ","))
			if err != nil {
				return err
			}
			base := dir
			if base == "" {
				// The project the USER STANDS IN, never the ambient one: with
				// PATH_BASE exported (direnv/mise shells) paths.Base is
				// whatever project that variable points at, and a toolchain
				// installed into the wrong tree is worse than a stray
				// mise.toml. Kept while PATH_BASE keeps first place in
				// config.ResolvePaths (WP9 step 1, parked).
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				base = config.FindProjectRoot(cwd)
			}
			return runToolchainInstall(cmd.Context(), base, groups, dryRun, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVarP(&dir, "path", "p", "", "Project directory (default: the nearest project root above the working directory, else the working directory)")
	cmd.Flags().StringVar(&groupsFlag, "groups", strings.Join(toolchain.DefaultGroups, ","), "Groups to activate (core,local,cloud,bash; core is implied)")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "Print what would be written, downloaded and run; touch nothing")
	return cmd
}

// projectName is the project's metadata.name from <base>/lok8s.yaml when
// present (what `lo init project <name>` wrote), else the directory name —
// so a re-run renders the same header and an unchanged file is reported
// as matching, not diffed.
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

func runToolchainInstall(ctx context.Context, base string, groups []string, dryRun bool, out, stderr io.Writer) error {
	bin := filepath.Join(base, ".bin")
	name := projectName(base)
	fmt.Fprintf(out, "lo toolchain install — %s (lo %s, %s; groups: %s)\n", base, assets.Version(), render.Variant(), strings.Join(groups, ","))
	content, err := toolchainTemplate(projectPaths(base), name, groups)
	if err != nil {
		return err
	}
	if err := scaffold.WriteBYAML(bin, content, dryRun, out); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out, "would ensure .gitignore entries in %s\n", filepath.Join(base, ".gitignore"))
	} else if err := scaffold.EnsureGitignore(base, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "Toolchain (b → .bin/):")
	if err := toolchain.Bootstrap(ctx, toolchain.BootstrapOptions{
		Base: base, Bin: bin, Out: out, Stderr: stderr, DryRun: dryRun,
	}); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintln(out, "dry run — nothing was written, downloaded or run")
		return nil
	}
	fmt.Fprintln(out, "Done.")
	ui.Next(out, "toolchain doctor", "verifies b, kustomize, khelm and the Secret plugin against the pins")
	return nil
}

// newToolchainDoctorCommand builds `lo toolchain doctor`: the pinned-tools
// section of `lo doctor` on its own, with no marker gate. Exit 1 when a
// tool this build execs is missing. Unlike install it takes the project
// of the current shell (paths: an exported PATH_BASE wins, like `lo
// doctor`); the two agree once WP9 step 1 lands.
func newToolchainDoctorCommand(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Verify the b-managed toolchain against the pins",
		Long: `Print the toolchain section of lo doctor on its own: .bin/b, kustomize, the
khelm ChartRenderer and the secrets.lok8s.dev Secret plugin, each against its
pin. No marker and no flag gate it. Exit 1 when a tool this build execs is
missing (lo core); lo-full only warns about the render tools.

It uses the project of the current shell (an exported PATH_BASE wins, else the
nearest project above the working directory), like lo doctor. lo toolchain
install resolves from the working directory only.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Annotations:  map[string]string{AnnotationReadonly: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			setDebugFromVerbose(cmd)
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			path := childPATH(paths, bashTreeForPATH(paths).Dir)
			// Go-only, so the title prints in both modes (bold on a
			// terminal). doctorToolchain opens with the blank line and the
			// section header it prints inside lo doctor.
			ui.Title(out, "=== toolchain doctor ===")
			if !doctorToolchain(cmd.Context(), out, paths, config.KustomizePluginHome(paths), path) {
				ui.ErrorTo(stderr, "toolchain doctor: a pinned tool is missing (see ✗ above)")
				ui.Next(stderr, "toolchain install", "installs the pins of this lo build")
				return ErrHandled
			}
			return nil
		},
	}
}
