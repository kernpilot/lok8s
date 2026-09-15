package cli

// lo lint — structure and spec validation. Go port of .lok8s/libs/lint
// main::lint; the checks live in internal/lint.

import (
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/lint"
)

func init() { registerPorted("lint", newLintCommand) }

func newLintCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	var notes bool
	var format string
	cmd := &cobra.Command{
		Use:          "lint",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         argshNoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// No positionals in the argsh spec (the domain rides --domain);
			// same message as the argsh parser, exit 1 (version/use precedent).
			setDebugFromVerbose(cmd)

			// Domain: the canonical precedence chain (--domain flag >
			// DOMAIN_NAME env > clusters/.active > lok8s.dev). Always
			// non-empty, so — exactly like the bash, whose main pre-sets the
			// lib's `domain` local — the "validate all domains" branch inside
			// the lib is unreachable through the CLI.
			domainFlag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(domainFlag, paths.Clusters, stderr)

			// Go-only: --format editor|github rewrites the finding lines
			// (internal/lint/format.go). The flag picks the format; under
			// GITHUB_ACTIONS=true with stdout off a terminal the default
			// becomes github (the environment chooses a rendering, never a
			// code path; text on a terminal, or on --format text).
			f, err := lintFormat(format, cmd.OutOrStdout())
			if err != nil {
				return argshErrorf(stderr, "invalid --format %q: text, editor or github", format)
			}
			// A finding without a path is charged to the file the linter
			// validates for the domain (cluster.lok8s.yaml, else deploy).
			fallback := filepath.Join("clusters", d, filepath.Base(lint.SpecFile(filepath.Join(paths.Clusters, d))))
			out := lint.NewFormatWriter(cmd.OutOrStdout(), f, fallback)
			findings := lint.NewFormatWriter(stderr, f, fallback)
			defer func() {
				for _, w := range []io.Writer{out, findings} {
					if fw, ok := w.(*lint.FormatWriter); ok {
						_ = fw.Flush()
					}
				}
			}()
			stderr = findings

			// Go-only: the spec.implementation block of lok8s.yaml (every
			// other command refuses to start on an invalid block; lint
			// reports it, and a missing routed tree, as a finding, and
			// unknown keys as warnings). A valid or absent block prints
			// nothing, so the output stays byte-identical to bash.
			l := &lint.Linter{Paths: paths, Out: out, ErrOut: stderr, Notes: notes,
				Implementation: func() ([]string, error) {
					r := newRouting(paths)
					return r.impl.Warnings, r.problem()
				}}
			if err := l.Run(d); err != nil {
				return ErrHandled
			}
			return nil
		},
	}
	// Go-only: the advisory for keys equal to a documented default
	// (internal/lint/defaults.go). Opt-in so every `check - lint` parity
	// case stays byte-identical (the bash lint prints no such line).
	cmd.Flags().BoolVar(&notes, "notes", false, "Also print a [note] per spec key that equals its documented default (advisory; the exit code is unchanged)")
	cmd.Flags().StringVar(&format, "format", "", "Finding format: text (default), editor (file:line: [level] message) or github (workflow commands, the default under GITHUB_ACTIONS off a terminal)")
	_ = cmd.RegisterFlagCompletionFunc("format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{lint.FormatText, lint.FormatEditor, lint.FormatGitHub}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// lintStdoutIsTerminal is the TTY seam of the github auto-select (tests
// swap it).
var lintStdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// lintFormat resolves --format: the flag when given. Without one, github
// under GITHUB_ACTIONS=true when out is the process stdout and that is
// not a terminal (a workflow step). A writer a caller supplied (a test
// buffer, the MCP server) never reads the environment. Else text.
func lintFormat(flag string, out io.Writer) (string, error) {
	switch flag {
	case lint.FormatText, lint.FormatEditor, lint.FormatGitHub:
		return flag, nil
	case "":
		if out == io.Writer(os.Stdout) && os.Getenv("GITHUB_ACTIONS") == "true" && !lintStdoutIsTerminal() {
			return lint.FormatGitHub, nil
		}
		return lint.FormatText, nil
	}
	return "", os.ErrInvalid
}
