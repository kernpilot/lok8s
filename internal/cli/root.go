// Package cli builds the lo command tree.
package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
)

// ErrHandled marks an error whose message was already printed in the bash
// implementation's own format ([error] … on stderr). The caller exits
// non-zero without printing anything further.
var ErrHandled = errors.New("handled")

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
		why:   "the eject model (internal/assets): list/show/eject/diff/update the framework assets embedded in the binary; bash reads .lok8s/** from disk and has no embedded copy to compare against",
		build: newAssetsCommand,
	},
}

// NewRoot builds the full lo command tree: the usage-mirrored tree plus the
// Go-only commands.
func NewRoot(paths *config.Paths) *cobra.Command {
	root := newUsageTree(paths)
	for _, g := range goOnlyCommands {
		root.AddCommand(g.build(paths))
	}
	return root
}

// newUsageTree builds the part of the tree that mirrors the argsh usage
// array one-to-one. Commands without a Go implementation yet are registered
// as passthroughs to the argsh implementation via Shim. The MCP server
// projects THIS tree (never the Go-only additions) into tools.
func newUsageTree(paths *config.Paths) *cobra.Command {
	// `lo --version` names the build too: "lo version 0.3.0 (core)" /
	// "(full)". `lo version` (the command) stays byte-identical to the bash
	// implementation (parity-test diffs it).
	root := &cobra.Command{
		Use:           "lo",
		Short:         "lok8s - local dev orchestration",
		Version:       version + " (" + render.Variant() + ")",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The eject policy is process-wide (every consumer of an embedded
		// asset reads it); set it once before any command body runs.
		// Shim commands disable flag parsing, so for them only the
		// environment form (LO_ASSETS_EJECT=never) applies — and they read
		// .lok8s from disk anyway.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			noEject, _ := cmd.Flags().GetBool("no-eject")
			assets.Configure(noEject)
		},
	}

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

	root.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Cluster lifecycle:"},
		&cobra.Group{ID: groupConfigure, Title: "Configure & inspect:"},
		&cobra.Group{ID: groupIntegrations, Title: "Integrations:"},
		&cobra.Group{ID: groupComponents, Title: "Components:"},
	)

	for _, spec := range commandTree {
		if build, ok := portedCommands[spec.use]; ok {
			root.AddCommand(build(paths, spec))
			continue
		}
		root.AddCommand(newShimCommand(paths, spec))
	}
	return root
}

// newShimCommand registers a command whose implementation still lives in the
// argsh tree. Flag parsing is disabled and the ORIGINAL argv is passed
// through, so alias spelling, flag order, and everything else reach the bash
// parser exactly as typed.
func newShimCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
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
			return Shim(paths, os.Args[1:])
		},
	}
}
