package cli

// lo crds — operator CRD generation from the schema source (hidden; the CI
// drift gate). Go port of .lok8s/libs/crds (main::crds); the render lives in
// internal/crds and is byte-identical to the bash yq render.

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/crds"
)

func init() { registerPorted("crds", newCrdsCommand) }

func newCrdsCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "crds",
		Aliases:      spec.aliases,
		Short:        spec.short,
		Hidden:       spec.hidden,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "generate", Aliases: []string{"g"},
			Short:        "Generate operator/crds/*.yaml from schema/*.schema.yaml",
			Annotations:  map[string]string{AnnotationIdempotent: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return crds.Generate(crds.NewLayout(paths), cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use: "check", Aliases: []string{"c"},
			Short:        "Fail if any generated CRD is stale (drift gate)",
			Annotations:  map[string]string{AnnotationReadonly: "true"},
			Args:         cobra.NoArgs,
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				err := crds.Check(crds.NewLayout(paths), cmd.OutOrStdout(), cmd.ErrOrStderr())
				if errors.Is(err, crds.ErrStale) {
					return ErrHandled
				}
				return err
			},
		},
	)
	return cmd
}
