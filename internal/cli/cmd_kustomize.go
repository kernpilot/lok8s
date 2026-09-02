package cli

// lo kustomize — kustomize plugin build pipeline (Go).
// Port of .lok8s/libs/kustomize: builds, tests, cleans, and lists lok8s's
// Go-based kustomize *exec* plugins (the secrets.lok8s.dev/v1/Secret
// generator, plus any project-local plugins under ./kustomize/).

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("kustomize", newKustomizeCommand) }

func newKustomizeCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          spec.use,
		Aliases:      spec.aliases,
		Short:        "Manage kustomize plugins (Go-based exec generators)",
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "build", Aliases: []string{"b"},
			Short:        "Compile all kustomize plugin binaries into .kustomize/",
			Annotations:  map[string]string{AnnotationIdempotent: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE:         func(cmd *cobra.Command, _ []string) error { return kustomizeBuild(paths) },
		},
		&cobra.Command{
			Use: "test", Aliases: []string{"t"},
			Short:        "Run plugin unit + integration tests",
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE:         func(cmd *cobra.Command, _ []string) error { return kustomizeTest(paths) },
		},
		&cobra.Command{
			Use:          "clean",
			Short:        "Remove built plugin binaries",
			Annotations:  map[string]string{AnnotationDestructive: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE:         func(cmd *cobra.Command, _ []string) error { return kustomizeClean(paths) },
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
	if fw := filepath.Join(filepath.Dir(paths.Lok8s), "kustomize"); dirExists(fw) {
		sources = append(sources, fw)
	}
	if own := filepath.Join(paths.Base, "kustomize"); own != "" && dirExists(own) && !contains(sources, own) {
		sources = append(sources, own)
	}
	return sources
}

func kustomizeBuild(paths *config.Paths) error {
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
		if err := runMake(s, []string{"BIN_ROOT=" + filepath.Join(paths.Base, ".kustomize")}, "build"); err != nil {
			return ErrHandled
		}
	}
	return nil
}

func kustomizeTest(paths *config.Paths) error {
	if _, err := exec.LookPath("go"); err != nil {
		ui.Error("go is not installed")
		return ErrHandled
	}
	failed := false
	for _, s := range kustomizeSources(paths) {
		if err := runMake(s, nil, "test"); err != nil {
			failed = true
		}
	}
	if failed {
		return ErrHandled
	}
	return nil
}

func kustomizeClean(paths *config.Paths) error {
	for _, s := range kustomizeSources(paths) {
		// Best-effort, like the bash `|| true`.
		_ = runMake(s, []string{"BIN_ROOT=" + filepath.Join(paths.Base, ".kustomize")}, "clean")
	}
	return nil
}

func kustomizeList(paths *config.Paths, cmd *cobra.Command) error {
	root := filepath.Join(paths.Base, ".kustomize")
	if !dirExists(root) {
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

func runMake(dir string, env []string, target string) error {
	c := exec.Command("make", target)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
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
