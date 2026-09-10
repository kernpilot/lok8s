package cli

// lo kustomize — kustomize plugin build pipeline (Go).
// Port of .lok8s/libs/kustomize: builds, tests, cleans, and lists lok8s's
// Go-based kustomize *exec* plugins (the secrets.lok8s.dev/v1/Secret
// generator, plus any project-local plugins under ./kustomize/).

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("kustomize", newKustomizeCommand) }

func newKustomizeCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          spec.use,
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		// An unknown subcommand is a parse error in argsh (`Invalid
		// command: x`, rc 2); without a RunE cobra printed the group help
		// and exited 0. Same message, rc 1 (D1) — the `ai` group's shape.
		RunE: argshGroupRunE,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "build", Aliases: []string{"b"},
			Short:        "Compile all kustomize plugin binaries into .kustomize/",
			Annotations:  map[string]string{AnnotationIdempotent: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return kustomizeBuild(cmd.Context(), execx.NewRunner(paths), paths)
			},
		},
		&cobra.Command{
			Use: "test", Aliases: []string{"t"},
			Short:        "Run plugin unit + integration tests",
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return kustomizeTest(cmd.Context(), execx.NewRunner(paths), paths)
			},
		},
		&cobra.Command{
			Use:          "clean",
			Short:        "Remove built plugin binaries",
			Annotations:  map[string]string{AnnotationDestructive: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return kustomizeClean(cmd.Context(), execx.NewRunner(paths), paths)
			},
		},
		&cobra.Command{
			Use: "list", Aliases: []string{"l"},
			Short:        "List discoverable plugins under .kustomize/",
			Annotations:  map[string]string{AnnotationReadonly: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE:         func(cmd *cobra.Command, _ []string) error { return kustomizeList(paths, cmd) },
		},
	)
	return cmd
}

// kustomizeSources returns the plugin source dirs: the lok8s FRAMEWORK
// plugins (shipped with the framework, sibling of .lok8s) plus the project's
// own kustomize/ if present. Build output installs into the PROJECT's plugin
// home (${PATH_BASE}/.kustomize) via the Makefile's overridable BIN_ROOT, so
// a fresh project gets the framework plugins without carrying the Go source.
func kustomizeSources(paths *config.Paths) []string {
	var sources []string
	if fw := filepath.Join(filepath.Dir(paths.Lok8s), "kustomize"); fsutil.DirExists(fw) {
		sources = append(sources, fw)
	}
	if own := filepath.Join(paths.Base, "kustomize"); own != "" && fsutil.DirExists(own) && !contains(sources, own) {
		sources = append(sources, own)
	}
	return sources
}

func kustomizeBuild(ctx context.Context, r execx.Runner, paths *config.Paths) error {
	if _, err := exec.LookPath("go"); err != nil {
		ui.Error("go is not installed (run 'b install go' or use goenv)")
		return ErrHandled
	}
	sources := kustomizeSources(paths)
	if len(sources) == 0 {
		ui.Error("no kustomize plugin sources (lok8s/kustomize or %s/kustomize)", paths.Base)
		return ErrHandled
	}
	for _, s := range sources {
		ui.Debug("kustomize: building %s -> %s/.kustomize", s, paths.Base)
		if err := runMake(ctx, r, s, []string{"BIN_ROOT=" + filepath.Join(paths.Base, ".kustomize")}, "build"); err != nil {
			return ErrHandled
		}
	}
	return nil
}

func kustomizeTest(ctx context.Context, r execx.Runner, paths *config.Paths) error {
	if _, err := exec.LookPath("go"); err != nil {
		ui.Error("go is not installed")
		return ErrHandled
	}
	failed := false
	for _, s := range kustomizeSources(paths) {
		if err := runMake(ctx, r, s, nil, "test"); err != nil {
			failed = true
		}
	}
	if failed {
		return ErrHandled
	}
	return nil
}

func kustomizeClean(ctx context.Context, r execx.Runner, paths *config.Paths) error {
	for _, s := range kustomizeSources(paths) {
		// Best-effort, like the bash `|| true`.
		_ = runMake(ctx, r, s, []string{"BIN_ROOT=" + filepath.Join(paths.Base, ".kustomize")}, "clean")
	}
	return nil
}

func kustomizeList(paths *config.Paths, cmd *cobra.Command) error {
	root := filepath.Join(paths.Base, ".kustomize")
	if !fsutil.DirExists(root) {
		ui.Warn("No .kustomize/ directory found")
		return nil
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Discoverable kustomize plugins under %s:\n", root)
	// find -maxdepth 4 -type f -perm -u+x -printf '  %P\n'. WalkDir visits
	// lexically, so the listing is stable (bash find used directory order).
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		depth := countSeparators(rel) + 1
		if d.IsDir() {
			if depth >= 4 {
				return fs.SkipDir
			}
			return nil
		}
		if depth > 4 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.Mode()&0o100 == 0 || !info.Mode().IsRegular() {
			return nil
		}
		fmt.Fprintf(out, "  %s\n", rel)
		return nil
	})
	return nil
}

func runMake(ctx context.Context, r execx.Runner, dir string, env []string, target string) error {
	return r.Run(ctx, execx.Cmd{
		Name: "make", Args: []string{target}, Dir: dir, Env: env,
		Stdout: os.Stdout, Stderr: os.Stderr,
	})
}

func contains(list []string, v string) bool {
	return slices.Contains(list, v)
}

func countSeparators(rel string) int {
	n := 0
	for _, r := range rel {
		if r == filepath.Separator {
			n++
		}
	}
	return n
}
