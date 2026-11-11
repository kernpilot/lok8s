package cli

// cmd_kubehz_agent.go — `lo kubehz space …` and `lo kubehz cluster …`, the
// kubehz api calls of an agent key. The library is internal/kubehz/agent.go,
// and the bash twin is .lok8s/libs/kubehz/agent. They are plain commands:
// `lo mcp` turns them into tools by their markers, like every other leaf.
//   - list and get are readonly, the default tier.
//   - create is mutating: it adds a space and changes nothing that exists.
//   - delete and lease are destructive: a lease can bring the delete closer.
//   - kubeconfig is credential output, so no MCP server offers it.
//
// Every leaf takes -o text|json|yaml. The required flags come first, in the
// argsh shape (`Error: missing required flag: <name>`), as the `:!` flags
// of the bash twin do.

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/kubehz"
)

// agentReport is what an agent command prints: the text form here, json
// and yaml through writeOutput.
type agentReport interface{ WriteText(io.Writer) }

// agentLeaf builds one leaf. It checks the positional arguments, the
// required string flags and -o, then runs the command and prints the report
// in the chosen format.
func agentLeaf(paths *config.Paths, use, short string, spec commandSpec, positional, required []string,
	run func(kc *kubehz.Context, cmd *cobra.Command, args []string) (agentReport, error)) *cobra.Command {
	var format func() (string, error)
	c := &cobra.Command{
		Use:          use,
		Short:        short,
		Annotations:  spec.annotations(),
		Args:         agentArgs(positional...),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range required {
				if v, _ := cmd.Flags().GetString(name); v == "" {
					return argshErrorf(cmd.ErrOrStderr(), "missing required flag: %s", name)
				}
			}
			f, err := format()
			if err != nil {
				return err
			}
			r, err := run(kubehzContext(cmd, paths), cmd, args)
			if err != nil {
				return kubehzRun(err)
			}
			if f == outputText {
				r.WriteText(cmd.OutOrStdout())
				return nil
			}
			return writeOutput(cmd.OutOrStdout(), f, r)
		},
	}
	format = addOutputFlag(c)
	return c
}

// agentArgs takes exactly the named positional arguments, with the argsh
// parse errors (`missing required argument: id`, `too many arguments: x`).
func agentArgs(names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < len(names) {
			return argshErrorf(cmd.ErrOrStderr(), "missing required argument: %s", names[len(args)])
		}
		if len(args) > len(names) {
			return argshErrorf(cmd.ErrOrStderr(), "too many arguments: %s", args[len(names)])
		}
		return nil
	}
}

func newKubehzSpace(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "space",
		Short:        "Spaces through the kubehz api (agent tools)",
		SilenceUsage: true,
		RunE:         argshGroupRunE,
	}
	id := []string{"id"}

	list := agentLeaf(paths, "list", "List the spaces the credential reaches", commandSpec{readonly: true}, nil, nil,
		func(kc *kubehz.Context, cmd *cobra.Command, _ []string) (agentReport, error) {
			return kc.SpaceList(cmd.Context())
		})
	get := agentLeaf(paths, "get <id>", "Show one space", commandSpec{readonly: true}, id, nil,
		func(kc *kubehz.Context, cmd *cobra.Command, args []string) (agentReport, error) {
			return kc.SpaceGet(cmd.Context(), args[0])
		})

	create := agentLeaf(paths, "create --name <name> --slug <slug>", "Create a space (an agent key sets a lease)", commandSpec{},
		nil, []string{"name", "slug"},
		func(kc *kubehz.Context, cmd *cobra.Command, _ []string) (agentReport, error) {
			f := cmd.Flags()
			var o kubehz.SpaceCreateOptions
			o.Name, _ = f.GetString("name")
			o.Slug, _ = f.GetString("slug")
			o.Nodes, _ = f.GetString("nodes")
			o.Namespaces, _ = f.GetString("namespaces")
			o.ObjectCapKiB, _ = f.GetString("object-cap-kib")
			o.Region, _ = f.GetString("region")
			o.LeaseHours, _ = f.GetString("lease-hours")
			return kc.SpaceCreate(cmd.Context(), o)
		})
	cf := create.Flags()
	cf.String("name", "", "Display name of the space")
	cf.String("slug", "", "DNS label of the space, unique on the platform (the namespace name)")
	// Strings, not ints: lo checks the shape itself, with the same
	// refusal text as the bash twin.
	cf.String("nodes", "", "Node limit; default: the platform's")
	cf.String("namespaces", "", "Namespace limit; default: the platform's")
	cf.String("object-cap-kib", "", "Size cap of one Secret or ConfigMap in KiB; default: the platform's")
	cf.String("region", "", "Region to place the space in; default: the platform's choice")
	cf.String("lease-hours", "", "Hours until the platform deletes the space (1 to 720); an agent key gets 2 without it")

	del := agentLeaf(paths, "delete <id>", "Delete a space", commandSpec{destructive: true}, id, nil,
		func(kc *kubehz.Context, cmd *cobra.Command, args []string) (agentReport, error) {
			return kc.SpaceDelete(cmd.Context(), args[0])
		})

	cmd.AddCommand(list, get, create, del,
		agentLeaseLeaf(paths, "Set the hours until the platform deletes a space", (*kubehz.Context).SpaceLease),
		agentKubeconfigLeaf(paths, "Write the agent kubeconfig of a space to a file", (*kubehz.Context).SpaceKubeconfig))
	return cmd
}

func newKubehzCluster(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "cluster",
		Short:        "Clusters through the kubehz api (agent tools)",
		SilenceUsage: true,
		RunE:         argshGroupRunE,
	}
	list := agentLeaf(paths, "list", "List the clusters the credential reaches", commandSpec{readonly: true}, nil, nil,
		func(kc *kubehz.Context, cmd *cobra.Command, _ []string) (agentReport, error) {
			return kc.ClusterList(cmd.Context())
		})
	get := agentLeaf(paths, "get <id>", "Show one cluster", commandSpec{readonly: true}, []string{"id"}, nil,
		func(kc *kubehz.Context, cmd *cobra.Command, args []string) (agentReport, error) {
			return kc.ClusterGet(cmd.Context(), args[0])
		})
	cmd.AddCommand(list, get,
		agentLeaseLeaf(paths, "Set the hours until the platform deletes a hosted cluster", (*kubehz.Context).ClusterLease),
		agentKubeconfigLeaf(paths, "Write the agent kubeconfig of a hosted cluster to a file", (*kubehz.Context).ClusterKubeconfig))
	return cmd
}

// agentLeaseLeaf is `lease <id> --hours <n>`: destructive, because a
// shorter lease brings the platform's delete closer.
func agentLeaseLeaf(paths *config.Paths, short string,
	lease func(kc *kubehz.Context, ctx context.Context, id, hours string) (*kubehz.LeaseResult, error)) *cobra.Command {
	c := agentLeaf(paths, "lease <id> --hours <n>", short, commandSpec{destructive: true}, []string{"id"}, []string{"hours"},
		func(kc *kubehz.Context, cmd *cobra.Command, args []string) (agentReport, error) {
			hours, _ := cmd.Flags().GetString("hours")
			return lease(kc, cmd.Context(), args[0], hours)
		})
	c.Flags().String("hours", "", "Hours from now until the platform deletes it (1 to 720)")
	return c
}

// agentKubeconfigLeaf is `kubeconfig <id> --file <path>`. The file holds no
// secret: its exec stanza runs `lo kubehz token`. Still the command counts
// as credential output, so `lo mcp` never offers it and `lo chat` denies
// it. A file that exists needs the global --force.
func agentKubeconfigLeaf(paths *config.Paths, short string,
	download func(kc *kubehz.Context, ctx context.Context, id, file string, force bool) (*kubehz.KubeconfigResult, error)) *cobra.Command {
	c := agentLeaf(paths, "kubeconfig <id> --file <path>", short, commandSpec{credentialOutput: true}, []string{"id"}, []string{"file"},
		func(kc *kubehz.Context, cmd *cobra.Command, args []string) (agentReport, error) {
			file, _ := cmd.Flags().GetString("file")
			force, _ := cmd.Flags().GetBool("force")
			return download(kc, cmd.Context(), args[0], file, force)
		})
	c.Flags().String("file", "", "Write the kubeconfig to this file (mode 0600) and print the path; a file that exists needs --force")
	return c
}
