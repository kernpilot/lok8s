// Package cli builds the lo command tree.
package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
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

// NewRoot builds the full lo command tree. Commands without a Go
// implementation yet are registered as passthroughs to the argsh
// implementation via Shim.
func NewRoot(paths *config.Paths) *cobra.Command {
	root := &cobra.Command{
		Use:           "lo",
		Short:         "lok8s - local dev orchestration",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
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
