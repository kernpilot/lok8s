package cli

// lo image — manage the local cache registry. Go port of .lok8s/libs/image
// (main::image); the operations live in internal/image.

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/image"
)

func init() { registerPorted("image", newImageCommand) }

// imageContext binds an image.Context to the command's streams. The
// domain default chain is the lib's own `${domain:-${DOMAIN_NAME:-lok8s.dev}}`
// — ambientMainEnv has already exported the resolved DOMAIN_NAME.
func imageContext(cmd *cobra.Command, paths *config.Paths, d string) *image.Context {
	return &image.Context{
		Paths:  paths,
		Runner: execx.NewRunner(paths),
		Out:    cmd.OutOrStdout(),
		ErrOut: cmd.ErrOrStderr(),
		Domain: d,
	}
}

// imageRun maps the image package's already-printed sentinel onto the cli one.
func imageRun(err error) error {
	if err == image.ErrHandled {
		return ErrHandled
	}
	return err
}

func newImageCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "image",
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
	cmd.AddCommand(
		newImageCache(paths),
		newImageList(paths),
		newImageClean(paths),
	)
	return cmd
}

func newImageCache(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "cache [service]",
		Aliases:      []string{"c"},
		Short:        "Pre-pull image(s) into the local cache registry",
		Annotations:  commandSpec{destructive: true, idempotent: true}.annotations(),
		Args:         secretsArgs(0, 1, "service"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			service := ""
			if len(args) > 0 {
				service = args[0]
			}
			// The subcommand's own `force|f:+` local SHADOWED the global in
			// bash — only a flag on this invocation forces (the cobra local
			// flag below shadows the persistent one identically).
			force, _ := cmd.Flags().GetBool("force")
			all, _ := cmd.Flags().GetBool("all")
			return imageRun(imageContext(cmd, paths, d).Cache(cmd.Context(), service, force, all))
		},
	}
	c.Flags().BoolP("force", "f", false, "Force re-pull even if image exists in cache")
	c.Flags().BoolP("all", "a", false, "Process every service in the active cache queue")
	return c
}

func newImageList(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Aliases:      []string{"l"},
		Short:        "List images currently in the cache registry",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			rc, err := imageContext(cmd, paths, d).List(cmd.Context())
			if err != nil {
				return imageRun(err)
			}
			if rc != 0 {
				// bash: the function's status is the curl pipeline's (e.g.
				// curl's 7 on connection-refused) — pass it through.
				os.Exit(rc)
			}
			return nil
		},
	}
}

func newImageClean(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "clean",
		Aliases:      []string{"x"},
		Short:        "Drop all images from the cache registry",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			// bash: network="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-lok8s}" —
			// ambientMainEnv already layered spec.network.name > env > lok8s.
			return imageRun(imageContext(cmd, paths, d).Clean(cmd.Context(), os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK")))
		},
	}
}
