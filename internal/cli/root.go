// Package cli builds the lo command tree.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr). The caller exits
// non-zero without printing anything further.
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// argshErrorf prints a parse error in the argsh entrypoint's format. (The
// argsh implementation exits 2 on parse errors; the Go binary exits 1 — the
// message is what scripts and humans match on.)
func argshErrorf(errOut io.Writer, format string, a ...any) error {
	fmt.Fprintf(errOut, "Error: "+format+"\n\n  Run \"lo -h\" for more information.\n", a...)
	return ErrHandled
}

// argshNoArgs is the Args validator of a command whose argsh spec declares
// no positional. A stray positional is a parse error there (rc 2, this
// message). Same message here, exit 1.
func argshNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[0])
	}
	return nil
}

// argshGroupRunE is the RunE of a command group. An unknown subcommand is
// a parse error in argsh (`Invalid command: x`, rc 2); without a RunE
// cobra printed the group help and exited 0. Same message, exit 1. A bare
// group prints its help.
func argshGroupRunE(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
	}
	return cmd.Help()
}

// setDebugFromVerbose exports DEBUG=1 for -v/--verbose, like the argsh
// entrypoint (the debug() lines depend on it).
func setDebugFromVerbose(cmd *cobra.Command) {
	v, _ := cmd.Flags().GetCount("verbose")
	setDebug(v > 0)
}

// setDebug exports DEBUG=1 when verbose is set.
func setDebug(verbose bool) {
	if verbose {
		os.Setenv("DEBUG", "1")
	}
}

// portedCommands maps command names to their Go implementations. Anything
// absent here still shims to the argsh implementation. Entries register
// themselves via registerPorted from each command file's init(), so adding a
// port never touches this file.
var portedCommands = map[string]func(*config.Paths, commandSpec) *cobra.Command{}

func registerPorted(name string, build func(*config.Paths, commandSpec) *cobra.Command) {
	if _, dup := portedCommands[name]; dup {
		panic("duplicate ported command: " + name)
	}
	portedCommands[name] = build
}

// goOnlyCommand is a command that exists ONLY in the Go binary. It has no
// entry in the argsh usage array, so it lives outside commandTree (which
// mirrors that array verbatim) and TestCommandTreeMatchesArgshUsage
// allowlists it by name instead of flagging it as drift. Every entry states
// why the bash tree has no twin.
type goOnlyCommand struct {
	name  string
	why   string
	build func(*config.Paths) *cobra.Command
}

// goOnlyCommands is the additive, Go-only part of the tree. Keep it short:
// anything that CAN mirror a usage entry belongs in commandTree instead.
var goOnlyCommands = []goOnlyCommand{
	{
		name:  "mcp",
		why:   "native MCP server (ophis); bash `lo mcp` is an argsh.so builtin dispatched implicitly, not a usage entry",
		build: newMcpCommand,
	},
	{
		name:  "operator",
		why:   "shell-operator hook bodies (internal/operator); the operator/hooks/*.sh shims exec `lo operator <hook>` — shell-operator discovers hooks by path, there was never a usage entry",
		build: newOperatorCommand,
	},
	{
		name:  "assets",
		why:   "the eject model (internal/assets): list/eject/diff/update the framework assets embedded in the binary; bash reads .lok8s/** from disk and has no embedded copy to compare against",
		build: newAssetsCommand,
	},
	{
		name:  "toolchain",
		why:   "the consumer toolchain via b (internal/toolchain): install the pins this binary was built against, verify them; the bash tree is installed by the b profile and has no such step",
		build: newToolchainCommand,
	},
}

// NewRoot builds the full lo command tree: the usage-mirrored tree plus the
// Go-only commands. The project's spec.implementation (routing.go) is
// read once here: a routed command is registered as a shim to the bash
// tree in the project, `default: bash` shims the whole usage tree, and
// the Go-only commands stay Go.
func NewRoot(paths *config.Paths) *cobra.Command {
	root := newUsageTree(paths, newRouting(paths))
	for _, g := range goOnlyCommands {
		root.AddCommand(g.build(paths))
	}
	// Help text and shell completion are decorations over the assembled
	// tree (examples.go, completion.go): neither touches a command's output.
	applyExamples(root)
	installCompletions(root, paths)
	return root
}

// newUsageTree builds the part of the tree that mirrors the argsh usage
// array one-to-one. A command the routing sends to bash is registered as a
// passthrough to the argsh implementation (newShimCommand); the rest get
// their Go port. The MCP server projects THIS tree (never the Go-only
// additions) into tools, built without routing so the tool names and
// schemas never depend on the project file.
func newUsageTree(paths *config.Paths, r routing) *cobra.Command {
	// `lo --version` names the build too: "lo version 0.3.0 (core)" /
	// "(full)". `lo version` (the command) stays byte-identical to the bash
	// implementation (parity-test diffs it).
	root := &cobra.Command{
		Use:           "lo",
		Short:         "lok8s - local dev orchestration",
		Version:       assets.Version() + " (" + render.Variant() + ")",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The eject policy is process-wide (every consumer of an embedded
		// asset reads it); set it once before any command body runs.
		// Shim commands disable flag parsing, so for them only the
		// environment form (LO_ASSETS_EJECT=never) applies — and they read
		// .lok8s from disk anyway. An invalid spec.implementation block
		// stops every command here (lint and doctor excepted: they report
		// it themselves; help and completion run no lo code), printed the
		// way main prints a startup error.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			noEject, _ := cmd.Flags().GetBool("no-eject")
			assets.Configure(noEject)
			noColor, _ := cmd.Flags().GetBool("no-color")
			ui.SetNoColor(noColor)
			if err := r.refuse(topLevelName(cmd)); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "lo: %v\n", err)
				return ErrHandled
			}
			return nil
		},
		// The root is runnable, so an unknown command is its own parse
		// error. (cobra's legacyArgs runs only on a root without Args.)
		// The error prints cobra's message, the "Did you mean" block and
		// the `Run "lo -h"` hint. A bare `lo` prints the help.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return unknownCommand(cmd, args[0])
		},
		// SuggestionsFor reads the distance as set (cobra defaults it only
		// on its own legacyArgs path).
		SuggestionsMinimumDistance: 2,
	}
	// Every flag-parse error, on every command, prints in the argsh shape
	// (cobra walks up to the root for the handler).
	argshFlagErrors(root)

	// Global flags, verbatim from the argsh entrypoint. Shim commands disable
	// cobra flag parsing and pass argv through untouched, so these exist for
	// `lo --domain x <cmd>` acceptance and for ported commands to read.
	pf := root.PersistentFlags()
	pf.CountP("verbose", "v", "Enable verbose logging")
	pf.BoolP("force", "f", false, "Force operation without prompts (also recreates immutable/terminating conflicts)")
	pf.Bool("force-recreate", false, "On apply, recreate objects blocked by an immutable field or a stuck Terminating finalizer")
	pf.BoolP("remote", "r", false, "Provision on remote VM (uses spec.provider + spec.remote)")
	pf.String("kubernetes", "", "Kubernetes version to use")
	pf.StringP("cluster", "s", "", "Cluster name to manage")
	pf.String("config", "", "Kind config to use")
	pf.String("domain", "", "Domain to use")
	pf.String("domain-sans", "", "Domain sans to use")
	// Go-only: the eject model's opt-out (env form: LO_ASSETS_EJECT=never).
	pf.Bool("no-eject", false, "Never write embedded framework assets into the project (.lok8s/…); serve them from a temp dir instead")
	// Go-only: colour off on a terminal (env form: NO_COLOR, no-color.org).
	// Piped output never carries colour.
	pf.Bool("no-color", false, "No ANSI colour on the terminal (env form: NO_COLOR)")

	root.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Cluster lifecycle:"},
		&cobra.Group{ID: groupConfigure, Title: "Configure & inspect:"},
		&cobra.Group{ID: groupIntegrations, Title: "Integrations:"},
		&cobra.Group{ID: groupComponents, Title: "Components:"},
	)

	for _, spec := range commandTree {
		var c *cobra.Command
		if build, ok := portedCommands[spec.use]; ok && !r.routed(spec.use) {
			c = build(paths, spec)
		} else {
			c = newShimCommand(paths, spec, r.impl.TreeDir, r.treeErr)
		}
		c.SuggestFor = suggestFor[spec.use]
		root.AddCommand(c)
	}
	return root
}

// suggestFor maps words from other tools (docker, kubectl, git) to the
// lo command they mean. cobra's "Did you mean" offers these next to the
// edit-distance matches.
var suggestFor = map[string][]string{
	"up":         {"start", "create"},
	"down":       {"stop"},
	"destroy":    {"delete", "rm", "remove"},
	"status":     {"ps", "info", "health"},
	"use":        {"switch", "select", "context"},
	"build":      {"render"},
	"deploy":     {"apply"},
	"lint":       {"validate", "check"},
	"doctor":     {"diagnose", "preflight"},
	"secrets":    {"secret"},
	"addons":     {"addon"},
	"drivers":    {"driver"},
	"registry":   {"registries"},
	"kubeconfig": {"kubecfg"},
}

// unknownCommand is the root's parse error for a word that is no command:
// cobra's message and its "Did you mean this?" block (the edit distance,
// a prefix, and the SuggestFor words), then the argsh hint.
func unknownCommand(root *cobra.Command, name string) error {
	msg := fmt.Sprintf("unknown command %q for %q", name, root.CommandPath())
	if suggestions := root.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return argshErrorf(root.ErrOrStderr(), "%s", msg)
}

// topLevelName is the name of cmd's top-level ancestor (`lo completion
// bash` is "completion", `lo secrets list` is "secrets"): the name the
// routing and its exemptions are keyed by.
func topLevelName(cmd *cobra.Command) string {
	c := cmd
	for c.HasParent() && c.Parent().HasParent() {
		c = c.Parent()
	}
	return c.Name()
}

// newShimCommand registers a command that runs in the argsh tree at
// treeDir (the project's tree: routing never targets the cache). Flag
// parsing is disabled and the ORIGINAL argv is passed through, so alias
// spelling, flag order, and everything else reach the bash parser exactly
// as typed. With treeErr set (the tree is missing) the command refuses
// with that message instead of exec'ing.
func newShimCommand(paths *config.Paths, spec commandSpec, treeDir string, treeErr error) *cobra.Command {
	return &cobra.Command{
		Use:                spec.use,
		Aliases:            spec.aliases,
		Short:              spec.short,
		GroupID:            spec.group,
		Hidden:             spec.hidden,
		Annotations:        spec.annotations(),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if treeErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "lo: %v\n", treeErr)
				return ErrHandled
			}
			tree := assets.Tree{Dir: treeDir, Source: assets.TreeProject}
			return execShim(paths, tree, os.Args[1:])
		},
	}
}
