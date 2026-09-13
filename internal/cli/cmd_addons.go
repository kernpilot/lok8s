package cli

// lo addons — list and inspect framework-shipped bootstrap addons. Go port
// of .lok8s/libs/addons (main::addons); the list/show/detail logic lives in
// internal/addons next to the render pipeline it shares the tree with.
// Read-only, cluster-free; output is byte-identical.

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/bootstrap"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
)

func init() { registerPorted("addons", newAddonsCommand) }

func addonsRun(err error) error {
	if errors.Is(err, addons.ErrHandled) {
		return ErrHandled
	}
	return err
}

func newAddonsCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "addons [addon...]",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			setDebugFromVerbose(cmd)
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			flag, _ := cmd.Flags().GetString("domain")
			d := domain.Resolve(flag, paths.Clusters, stderr)

			// --origin (Go-only): the eject-model column — builtin · local ·
			// local (modified) · local-only. Opt-in so the default table
			// stays byte-identical to the frozen implementation.
			origin, _ := cmd.Flags().GetBool("origin")
			show, detailFn, list := addons.Show, addons.Detail, addons.List
			if origin {
				show, detailFn, list = addons.ShowOrigin, addons.DetailOrigin, addons.ListOrigin
			}

			// Named addons win over --detail (bash: is::set addons first).
			if len(args) > 0 {
				for i, name := range args {
					if err := addonsRun(show(paths, d, name, out, stderr)); err != nil {
						return err
					}
					if i < len(args)-1 {
						fmt.Fprint(out, "\n---\n")
					}
				}
				return nil
			}
			if detail, _ := cmd.Flags().GetCount("detail"); detail > 0 {
				return addonsRun(detailFn(paths, d, out, stderr, bootstrapEntries(paths)))
			}
			return addonsRun(list(paths, d, out, stderr))
		},
	}
	// argsh 'detail|:+' is a counting flag.
	cmd.Flags().Count("detail", "Inventory the addons THIS cluster deploys (spec.bootstrap) + category + how to configure")
	cmd.Flags().Bool("origin", false, "Add the ORIGIN column: builtin (served from the binary) · local · local (modified) · local-only (see: lo assets)")
	return cmd
}

// bootstrapEntries is the addons.EntryResolver over the real bootstrap
// parser (bash: bootstrap::_resolve_entries 2>/dev/null, then _parse_entry
// per entry with `|| continue`).
func bootstrapEntries(paths *config.Paths) addons.EntryResolver {
	return func(spec, kind, d string) []addons.Resolved {
		raw, _ := bootstrap.ResolveEntries(spec, kind)
		var out []addons.Resolved
		for _, r := range raw {
			if r == "" {
				continue
			}
			e, err := bootstrap.ParseEntry(paths, io.Discard, d, r)
			if err != nil {
				continue
			}
			out = append(out, addons.Resolved{Name: e.Name, Dir: e.Dir, Builtin: e.Builtin})
		}
		return out
	}
}
