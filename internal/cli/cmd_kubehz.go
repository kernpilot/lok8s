package cli

// lo kubehz — kubehz platform integration. Go port of .lok8s/libs/kubehz/
// main (main::kubehz + the node and handover groups); the bodies live in
// internal/kubehz.
//
// `handover receive` gives -s to --snapshot (the bash spec), shadowing the
// global --cluster shorthand; it declares its own `cluster` flag so cobra
// does not merge the inherited one (the cmd_secrets precedent).

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kubehz"
)

func init() { registerPorted("kubehz", newKubehzCommand) }

// kubehzContext binds the library to the command's streams and paths.
func kubehzContext(cmd *cobra.Command, paths *config.Paths) *kubehz.Context {
	return &kubehz.Context{
		Paths:  paths,
		Runner: execx.NewRunner(paths),
		Out:    cmd.OutOrStdout(),
		ErrOut: cmd.ErrOrStderr(),
		IsTTY:  func() bool { return term.IsTerminal(int(os.Stdout.Fd())) },
	}
}

// kubehzRun maps the library's already-printed sentinel onto the cli one.
func kubehzRun(err error) error {
	if errors.Is(err, kubehz.ErrHandled) {
		return ErrHandled
	}
	return err
}

func newKubehzCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "kubehz",
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
		newKubehzRegister(paths),
		newKubehzDeregister(paths),
		newKubehzDeploy(paths),
		newKubehzStatus(paths),
		newKubehzJoin(paths),
		newKubehzNode(paths),
		newKubehzClaimCode(paths),
		newKubehzClaim(paths),
		newKubehzReEnroll(paths),
		newKubehzAssess(paths),
		newKubehzHandover(paths),
	)
	return cmd
}

// kubehzSimple builds a subcommand whose only input is the ambient domain.
func kubehzSimple(paths *config.Paths, use, alias, short string, spec commandSpec,
	run func(*kubehz.Context, *cobra.Command, string) error) *cobra.Command {
	c := &cobra.Command{
		Use:          use,
		Short:        short,
		Annotations:  spec.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := ambientMainEnv(cmd, paths)
			return kubehzRun(run(kubehzContext(cmd, paths), cmd, d))
		},
	}
	if alias != "" {
		c.Aliases = []string{alias}
	}
	return c
}

func newKubehzRegister(paths *config.Paths) *cobra.Command {
	return kubehzSimple(paths, "register", "r", "Register cluster with kubehz", commandSpec{destructive: true},
		func(c *kubehz.Context, cmd *cobra.Command, d string) error { return c.Register(cmd.Context(), d) })
}

func newKubehzDeregister(paths *config.Paths) *cobra.Command {
	return kubehzSimple(paths, "deregister", "d", "Remove cluster from kubehz", commandSpec{destructive: true},
		func(c *kubehz.Context, cmd *cobra.Command, d string) error { return c.Deregister(cmd.Context(), d) })
}

func newKubehzStatus(paths *config.Paths) *cobra.Command {
	return kubehzSimple(paths, "status", "s", "Check kubehz registration status", commandSpec{readonly: true},
		func(c *kubehz.Context, cmd *cobra.Command, d string) error { return c.Status(cmd.Context(), d) })
}

func newKubehzReEnroll(paths *config.Paths) *cobra.Command {
	return kubehzSimple(paths, "re-enroll", "", "Re-enroll a regenerated in-cluster agent token with the platform", commandSpec{destructive: true},
		func(c *kubehz.Context, cmd *cobra.Command, d string) error { return c.ReEnroll(cmd.Context(), d) })
}

func newKubehzAssess(paths *config.Paths) *cobra.Command {
	return kubehzSimple(paths, "assess", "a", "Show the platform assessment + handover feasibility", commandSpec{readonly: true},
		func(c *kubehz.Context, cmd *cobra.Command, d string) error { return c.Assess(cmd.Context(), d) })
}

func newKubehzDeploy(paths *config.Paths) *cobra.Command {
	c := kubehzSimple(paths, "deploy", "", "Deploy the in-cluster agent (spec.kubehz.agent)", commandSpec{destructive: true},
		func(c *kubehz.Context, cmd *cobra.Command, d string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return c.Deploy(cmd.Context(), d, dryRun)
		})
	c.Flags().Bool("dry-run", false, "Print the rendered manifests and apply nothing")
	return c
}

func newKubehzJoin(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "join <node>",
		Aliases:      []string{"j"},
		Short:        "Mint a node join ticket (hosting: shared)",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(1, 1, "node"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			printToken, _ := cmd.Flags().GetBool("print-token")
			d := ambientMainEnv(cmd, paths)
			return kubehzRun(kubehzContext(cmd, paths).Join(cmd.Context(), d, args[0], printToken))
		},
	}
	// Go-only (deviation D21): the ticket travels inside the join script;
	// the terminal shows it only on request.
	c.Flags().Bool("print-token", false, "Also print the plaintext ticket when a join script was written")
	return c
}

func newKubehzClaimCode(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "claim-code",
		Aliases:      []string{"c"},
		Short:        "Print the one-time claim code to paste into the dashboard",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Reads the LIVE cluster via the ambient kubeconfig (the global
			// --domain parses but does not re-point it — `lo use` first).
			ambientMainEnv(cmd, paths)
			return kubehzRun(kubehzContext(cmd, paths).ClaimCode(cmd.Context()))
		},
	}
}

func newKubehzClaim(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "claim --nonce <khzn_…>",
		Short:        "Place a dashboard-minted claim nonce for the agent to echo (mode 3)",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			flag, _ := cmd.Flags().GetString("nonce")
			kc := kubehzContext(cmd, paths)
			// Flag as given, `-` = stdin, else KUBEHZ_CLAIM_NONCE (D22) — the
			// refusal below is the bash one, so it stays in front of the
			// run header ambientMainEnv prints.
			nonce, err := kc.ClaimNonce(flag, cmd.InOrStdin())
			if err != nil {
				return kubehzRun(err)
			}
			if nonce == "" {
				return argshErrorf(cmd.ErrOrStderr(), "missing required flag: nonce")
			}
			ambientMainEnv(cmd, paths)
			return kubehzRun(kc.Claim(cmd.Context(), nonce))
		},
	}
	c.Flags().StringP("nonce", "n", "", "Claim-challenge nonce minted in the dashboard (khzn_…); '-' reads it from stdin, or set KUBEHZ_CLAIM_NONCE")
	return c
}

// ── node: machines you bring to a hosted control plane ────

func newKubehzNode(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "node",
		Aliases:      []string{"n"},
		Short:        "Nodes you bring to a hosted control plane (join/remove/status)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newKubehzNodeJoin(paths), newKubehzNodeRemove(paths), newKubehzNodeStatus(paths))
	return cmd
}

// nodeOpts reads the node flags, refusing the inherited --cluster/-s first
// (node::reject_global_cluster_flag — the value would parse silently and
// be ignored otherwise; the target here is --cluster-id).
func nodeOpts(c *kubehz.Context, cmd *cobra.Command) (kubehz.NodeOpts, error) {
	if f := cmd.Flags().Lookup("cluster"); f != nil && f.Changed {
		return kubehz.NodeOpts{}, c.RejectGlobalClusterFlag()
	}
	var o kubehz.NodeOpts
	o.ClusterID, _ = cmd.Flags().GetString("cluster-id")
	o.Pool, _ = cmd.Flags().GetString("pool")
	o.Name, _ = cmd.Flags().GetString("name")
	o.NodeIP, _ = cmd.Flags().GetString("node-ip")
	o.KubeletVersion, _ = cmd.Flags().GetString("kubelet-version")
	o.PrintOnly, _ = cmd.Flags().GetBool("print-only")
	return o, nil
}

func newKubehzNodeJoin(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "join",
		Aliases:      []string{"j"},
		Short:        "Join THIS machine to a hosted cluster (runs kubeadm join)",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc := kubehzContext(cmd, paths)
			o, err := nodeOpts(kc, cmd)
			if err != nil {
				return kubehzRun(err)
			}
			d := ambientMainEnv(cmd, paths)
			return kubehzRun(kc.NodeJoin(cmd.Context(), d, o))
		},
	}
	f := c.Flags()
	f.StringP("cluster-id", "c", "", "Cluster id (cl-xxxxxxxx); default: the cluster of the active domain")
	f.StringP("pool", "p", "", "Static pool to join; default: the one pool every node of the cluster is in")
	f.StringP("name", "n", "", "Node name; default: the short hostname of this machine")
	f.String("node-ip", "", "Address other nodes reach this machine on (needed behind NAT)")
	f.String("kubelet-version", "", "Kubelet version to declare; default: read from this machine")
	f.Bool("print-only", false, "Print the join command and run nothing")
	return c
}

func newKubehzNodeRemove(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "remove --name <node>",
		Aliases:      []string{"r"},
		Short:        "Remove one node from a hosted cluster and free its slot",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc := kubehzContext(cmd, paths)
			o, err := nodeOpts(kc, cmd)
			if err != nil {
				return kubehzRun(err)
			}
			if o.Name == "" {
				return argshErrorf(cmd.ErrOrStderr(), "missing required flag: name")
			}
			d := ambientMainEnv(cmd, paths)
			return kubehzRun(kc.NodeRemove(cmd.Context(), d, o))
		},
	}
	f := c.Flags()
	f.StringP("name", "n", "", "Node name to remove")
	f.StringP("cluster-id", "c", "", "Cluster id (cl-xxxxxxxx); default: the cluster of the active domain")
	return c
}

func newKubehzNodeStatus(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "status",
		Aliases:      []string{"s"},
		Short:        "List the nodes you brought to a hosted cluster",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc := kubehzContext(cmd, paths)
			o, err := nodeOpts(kc, cmd)
			if err != nil {
				return kubehzRun(err)
			}
			d := ambientMainEnv(cmd, paths)
			return kubehzRun(kc.NodeStatus(cmd.Context(), d, o))
		},
	}
	c.Flags().StringP("cluster-id", "c", "", "Cluster id (cl-xxxxxxxx); default: the cluster of the active domain")
	return c
}

// ── handover: the eject target side ──────────────────────

func newKubehzHandover(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "handover",
		Aliases:      []string{"h"},
		Short:        "Control-plane handover (receive/preseed on the eject target)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "Invalid command: %s", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newKubehzHandoverReceive(paths), newKubehzHandoverPreseed(paths))
	return cmd
}

func newKubehzHandoverReceive(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "receive --bundle <path> [--snapshot <file>] [--single-node] [--force]",
		Aliases:      []string{"r"},
		Short:        "Restore an exported control plane onto THIS node (kubeadm path)",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			bundle, _ := f.GetString("bundle")
			snapshot, _ := f.GetString("snapshot")
			singleNode, _ := f.GetBool("single-node")
			force, _ := f.GetBool("force")
			if bundle == "" {
				return argshErrorf(cmd.ErrOrStderr(), "missing required flag: bundle")
			}
			ambientMainEnv(cmd, paths)
			return kubehzRun(kubehzContext(cmd, paths).HandoverReceive(cmd.Context(), kubehz.ReceiveOpts{
				Bundle: bundle, Snapshot: snapshot, SingleNode: singleNode, Force: force,
			}))
		},
	}
	f := c.Flags()
	f.StringP("bundle", "b", "", "Exported control-plane bundle (tar or directory)")
	f.StringP("snapshot", "s", "", "etcd snapshot file to restore (default: the bundle's snapshot-location)")
	f.Bool("single-node", false, "Restore as a single control-plane node")
	// -s is --snapshot here; the local `cluster` shadows the global by name.
	f.String("cluster", "", "Cluster name to manage")
	return argshFlagErrors(c)
}

func newKubehzHandoverPreseed(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "preseed --bundle <path> --node <ip>",
		Aliases:      []string{"p"},
		Short:        "Pre-seed exported PKI onto a kubeone node before kubeone apply",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bundle, _ := cmd.Flags().GetString("bundle")
			node, _ := cmd.Flags().GetString("node")
			if bundle == "" {
				return argshErrorf(cmd.ErrOrStderr(), "missing required flag: bundle")
			}
			if node == "" {
				return argshErrorf(cmd.ErrOrStderr(), "missing required flag: node")
			}
			user, _ := cmd.Flags().GetString("user")
			port, _ := cmd.Flags().GetInt("port")
			sshKey, _ := cmd.Flags().GetString("ssh-key")
			ambientMainEnv(cmd, paths)
			return kubehzRun(kubehzContext(cmd, paths).HandoverPreseed(cmd.Context(), kubehz.PreseedOpts{
				Bundle: bundle, Node: node, User: user, Port: port, SSHKey: sshKey,
			}))
		},
	}
	f := c.Flags()
	f.StringP("bundle", "b", "", "Export bundle: a directory or .tar.gz with the contract keys")
	f.StringP("node", "n", "", "Target node address (kubeone node0) to place the PKI on")
	f.StringP("user", "u", "root", "SSH user")
	f.IntP("port", "p", 22, "SSH port")
	f.StringP("ssh-key", "i", "", "SSH private key file")
	return c
}
