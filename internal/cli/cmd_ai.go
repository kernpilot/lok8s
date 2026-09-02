package cli

// lo ai — manage the lok8s AI integration: the `lo chat` runtime and the
// agent skills (skills/*/SKILL.md), and how those skills reach assistants.
// Go port of .lok8s/libs/ai (main::ai); output is byte-identical.
//
// Skill-aware agents (claude) load skills natively from a skill dir, so `lo
// ai link` (sym)links the lok8s skills there. Agents without a skill system
// (and the local `lo chat` conductor) get them by instant injection instead.
// `lo ai check` reports the whole setup (bridge + runtime + skill wiring).

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/ui"
)

func init() { registerPorted("ai", newAiCommand) }

// runProcess runs a child to completion with inherited stdio and returns
// its exit code (bash: `"${PATH_LOK8S}/lo" chat --check || rc=$?`). Tests
// swap it to capture the argv.
var runProcess = func(bin string, argv, env []string) int {
	c := exec.Command(bin, argv[1:]...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if xe, ok := err.(*exec.ExitError); ok {
			return xe.ExitCode()
		}
		return 1
	}
	return 0
}

// exitProcess is os.Exit behind a seam for the rc passthroughs.
var exitProcess = os.Exit

// aiSkillsSrc is where the skills live — the source of truth.
func aiSkillsSrc(paths *config.Paths) string { return filepath.Join(paths.Base, "skills") }

// aiAgentSkillDir is the dir an assistant loads project skills from; false
// for agents with no native skill dir (they get instant injection).
func aiAgentSkillDir(paths *config.Paths, agent string) (string, bool) {
	if agent == "claude" {
		return filepath.Join(paths.Base, ".claude", "skills"), true
	}
	return "", false
}

func newAiCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "ai",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}

	link := &cobra.Command{
		Use:          "link [agent]",
		Short:        "Symlink (or --copy) the skills into an assistant skill dir",
		Annotations:  map[string]string{AnnotationIdempotent: "true"},
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			who := "claude"
			if len(args) > 0 {
				who = args[0]
			}
			copyMode, _ := cmd.Flags().GetCount("copy")
			return aiLink(paths, who, copyMode > 0, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	link.Flags().CountP("copy", "c", "Copy the skills instead of symlinking")

	cmd.AddCommand(
		&cobra.Command{
			Use:          "check",
			Short:        "Check the AI setup: lo mcp bridge, local runtime, skill wiring",
			Annotations:  map[string]string{AnnotationReadonly: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return aiCheck(paths, cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		},
		&cobra.Command{
			Use:          "skills",
			Short:        "List the agent skills and how each assistant gets them",
			Annotations:  map[string]string{AnnotationReadonly: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return aiSkills(paths, cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		},
		link,
		&cobra.Command{
			Use:          "unlink [agent]",
			Short:        "Remove skills previously linked into an assistant",
			Annotations:  map[string]string{AnnotationIdempotent: "true"},
			Args:         cobra.ArbitraryArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				who := "claude"
				if len(args) > 0 {
					who = args[0]
				}
				return aiUnlink(paths, who, cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		},
	)
	return cmd
}

// aiCheck runs the lochat runtime check (the same command line `lo chat
// --check` execs, as a child), then the skill wiring. The runtime's exit
// status is kept — a broken runtime should fail `lo ai check` — but the
// skill wiring is still shown.
func aiCheck(paths *config.Paths, out, stderr io.Writer) error {
	rc := 0
	bin, argv, err := chatCommandLine(paths, stderr)
	if err != nil {
		rc = 1
	} else {
		rc = runProcess(bin, append(argv, "--check"), shimEnv(paths))
	}
	fmt.Fprintln(out)
	if err := aiSkills(paths, out, stderr); err != nil {
		return err
	}
	if rc == 0 {
		return nil
	}
	if rc != 1 {
		exitProcess(rc)
	}
	return ErrHandled
}

// skillDirs lists the skill directories under src, in glob order (bash:
// for s in "${src}"/*/ with [[ -f "${s}SKILL.md" ]]).
func skillDirs(src string) []string {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(src, e.Name())
		if info, err := os.Stat(full); err != nil || !info.IsDir() {
			continue
		}
		if !fileExists(filepath.Join(full, "SKILL.md")) {
			continue
		}
		dirs = append(dirs, full)
	}
	// bash `*/` glob order: the trailing slash takes part in the comparison.
	sort.Slice(dirs, func(i, j int) bool { return dirs[i]+"/" < dirs[j]+"/" })
	return dirs
}

func aiSkills(paths *config.Paths, out, stderr io.Writer) error {
	src := aiSkillsSrc(paths)
	if !dirExists(src) {
		ui.Errorf(stderr, "no skills dir: %s", src)
		return ErrHandled
	}
	claudeDir, _ := aiAgentSkillDir(paths, "claude")
	fmt.Fprintf(out, "Agent skills — %s\n", src)
	for _, s := range skillDirs(src) {
		name := filepath.Base(s)
		target := filepath.Join(claudeDir, name)
		status := "lo chat: injected (not linked)"
		if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
			status = "claude: linked"
		} else if _, err := os.Stat(target); err == nil {
			status = "claude: copied"
		}
		fmt.Fprintf(out, "  %-24s %s\n", name, status)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Link them natively into Claude:  lo ai link claude")
	return nil
}

func aiLink(paths *config.Paths, who string, copyMode bool, out, stderr io.Writer) error {
	dst, ok := aiAgentSkillDir(paths, who)
	if !ok {
		ui.Errorf(stderr, "%s has no native skill dir — it gets skills by injection from `lo chat`, nothing to link.", who)
		return ErrHandled
	}
	src := aiSkillsSrc(paths)
	if !dirExists(src) {
		ui.Errorf(stderr, "no skills dir: %s", src)
		return ErrHandled
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	n := 0
	for _, s := range skillDirs(src) {
		target := filepath.Join(dst, filepath.Base(s))
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if copyMode {
			if err := copyDir(s, target); err != nil {
				return err
			}
		} else if err := os.Symlink(s, target); err != nil {
			return err
		}
		n++
	}
	mode := "symlink"
	if copyMode {
		mode = "copy"
	}
	fmt.Fprintf(out, "Linked %d skills into %s (%s).\n", n, dst, mode)
	fmt.Fprintf(out, "%s (run in this project) now loads the lok8s skills natively.\n", who)
	return nil
}

// aiUnlink iterates the DEST (not the source) so a skill removed/renamed in
// skills/ after linking still gets its stale entry cleaned: drop our
// symlinks (those resolving into skills/, even if now dangling) + copies
// whose name matches a current skill.
func aiUnlink(paths *config.Paths, who string, out, stderr io.Writer) error {
	dst, ok := aiAgentSkillDir(paths, who)
	if !ok {
		ui.Errorf(stderr, "%s: no skill dir", who)
		return ErrHandled
	}
	if !dirExists(dst) {
		fmt.Fprintf(out, "Nothing linked in %s.\n", dst)
		return nil
	}
	src := aiSkillsSrc(paths)
	entries, _ := os.ReadDir(dst)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		entry := filepath.Join(dst, e.Name())
		info, err := os.Lstat(entry)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(entry)
			if err == nil && strings.HasPrefix(link, src+"/") {
				_ = os.RemoveAll(entry)
				n++
			}
			continue
		}
		if dirExists(filepath.Join(src, e.Name())) {
			_ = os.RemoveAll(entry)
			n++
		}
	}
	fmt.Fprintf(out, "Unlinked %d skills from %s.\n", n, dst)
	return nil
}

// copyDir copies the directory src to dst (bash: cp -r src dst — symlinks
// preserved, modes kept).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
