package cli

// lo secrets — secret cache management with SOPS/age encryption.
// Go port of .lok8s/libs/secrets; output is byte-identical (see
// internal/secrets for the operations).
//
// `set` and `env` give -s to --namespace (the bash spec), which shadows the
// global --cluster shorthand. cobra panics when a merged flag set redefines a
// shorthand, so the two declare their own `cluster` flag (same name, no
// shorthand): the inherited one is then skipped by name and -s is free. The
// local flag is what secretsContext reads.

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/secrets"
)

func init() { registerPorted("secrets", newSecretsCommand) }

// defaultSSHKey resolves the LOK8S_SSH_KEY default chain
// (bash: : "${LOK8S_SSH_KEY:=${HOME}/.ssh/id_ed25519}").
func defaultSSHKey() string {
	if v := os.Getenv("LOK8S_SSH_KEY"); v != "" {
		return v
	}
	return os.Getenv("HOME") + "/.ssh/id_ed25519"
}

// secretsRun maps the secrets package's already-printed sentinel onto the cli
// one.
func secretsRun(err error) error {
	if errors.Is(err, secrets.ErrPrinted) {
		return ErrHandled
	}
	return err
}

// secretsContext resolves the domain (canonical chain: explicit flag >
// DOMAIN_NAME env > clusters/.active > lok8s.dev) and the entrypoint-derived
// KUBECONFIG, mirroring what the bash main does before dispatching. explicit
// carries a --domain value the caller parsed itself ("" = consult the
// inherited cobra flag).
func secretsContext(cmd *cobra.Command, paths *config.Paths, explicit, explicitCluster string) *secrets.Context {
	if explicit == "" {
		if f := cmd.Flags().Lookup("domain"); f != nil && f.Changed {
			explicit = f.Value.String()
		}
	}
	dom := domain.Resolve(explicit, paths.Clusters, cmd.ErrOrStderr())

	// KUBECONFIG the way the bash entrypoint exports it: cluster name =
	// spec metadata.name > --cluster flag > LOK8S_CLUSTER_NAME > "local".
	cluster := os.Getenv("LOK8S_CLUSTER_NAME")
	if cluster == "" {
		cluster = "local"
	}
	if explicitCluster == "" {
		if f := cmd.Flags().Lookup("cluster"); f != nil && f.Changed {
			explicitCluster = f.Value.String()
		}
	}
	if explicitCluster != "" {
		cluster = explicitCluster
	}
	if name := specClusterName(paths.Clusters + "/" + dom + "/cluster.lok8s.yaml"); name != "" {
		cluster = name
	}

	return &secrets.Context{
		Paths:      paths,
		Domain:     dom,
		Out:        cmd.OutOrStdout(),
		ErrOut:     cmd.ErrOrStderr(),
		Stdin:      cmd.InOrStdin(),
		StdinIsTTY: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		ReadPassword: func() (string, error) {
			raw, err := term.ReadPassword(int(os.Stdin.Fd()))
			return string(raw), err
		},
		Kubeconfig: paths.Base + "/.kubeconfig/" + cluster + ".yaml",
	}
}

// specClusterName reads .metadata.name from a cluster spec, "" when missing
// or unreadable (bash: yq -r '.metadata.name // ""').
func specClusterName(specPath string) string {
	var doc struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	raw, err := os.ReadFile(specPath)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Metadata.Name
}

// exactArgs returns an argsh-shaped arity validator: at least min positionals
// (naming the first missing one) and at most max.
func secretsArgs(min, max int, names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < min {
			return argshErrorf(cmd.ErrOrStderr(), "missing required argument: %s", names[len(args)])
		}
		if len(args) > max {
			return argshErrorf(cmd.ErrOrStderr(), "unexpected argument: %s", args[max])
		}
		return nil
	}
}

func newSecretsCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "secrets",
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
		newSecretsInit(paths),
		newSecretsAddKey(paths),
		newSecretsSet(paths),
		newSecretsEncrypt(paths),
		newSecretsDecrypt(paths),
		newSecretsAllow(paths),
		newSecretsList(paths),
		newSecretsPrint(paths),
		newSecretsEnv(paths),
		newSecretsPath(paths),
	)
	return cmd
}

func newSecretsInit(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "init",
		Aliases:      []string{"i"},
		Short:        "Initialize SOPS/age encryption from SSH key",
		Annotations:  commandSpec{idempotent: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sshKey, _ := cmd.Flags().GetString("ssh-key")
			if sshKey == "" {
				sshKey = defaultSSHKey()
			}
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.Init(sshKey))
		},
	}
	c.Flags().StringP("ssh-key", "k", "", "Path to SSH public key")
	return c
}

func newSecretsAddKey(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "add-key <key>",
		Short:        "Add an age recipient and re-key the store",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(1, 1, "key"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			skipOrphans, _ := cmd.Flags().GetBool("skip-orphans")
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.AddKey(args[0], all, skipOrphans))
		},
	}
	c.Flags().BoolP("all", "a", false, "Re-key every domain store, not just the current one")
	c.Flags().Bool("skip-orphans", false, "Proceed even if some .enc files have no decrypted twin")
	return c
}

func newSecretsEncrypt(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "encrypt",
		Aliases:      []string{"e"},
		Short:        "Encrypt plaintext cache files for commit",
		Annotations:  commandSpec{idempotent: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.Encrypt(name))
		},
	}
	c.Flags().StringP("name", "n", "", "Encrypt only this Secret's cache files (metadata.name); default is the whole store")
	return c
}

func newSecretsDecrypt(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "decrypt",
		Aliases:      []string{"d"},
		Short:        "Decrypt .enc files into plaintext cache",
		Annotations:  commandSpec{idempotent: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sshKey, _ := cmd.Flags().GetString("ssh-key")
			if sshKey == "" {
				sshKey = defaultSSHKey()
			}
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.Decrypt(sshKey))
		},
	}
	c.Flags().StringP("ssh-key", "k", "", "Path to SSH private key")
	return c
}

func newSecretsAllow(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "allow",
		Aliases:      []string{"a"},
		Short:        "Approve bash: generators (re-compute .sha + .bash-allow)",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return secretsRun(secretsContext(cmd, paths, "", "").Allow())
		},
	}
}

func newSecretsList(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Aliases:      []string{"l"},
		Short:        "List secrets",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return secretsRun(secretsContext(cmd, paths, "", "").List())
		},
	}
}

func newSecretsPrint(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "print [pattern...]",
		Aliases:      []string{"p"},
		Short:        "Print secret(s)",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			onlyOne, _ := cmd.Flags().GetBool("only-one")
			copyFlag, _ := cmd.Flags().GetBool("copy")
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.Print(args, onlyOne, copyFlag))
		},
	}
	c.Flags().BoolP("only-one", "o", false, "Only print one secret (error if multiple matches)")
	c.Flags().BoolP("copy", "c", false, "Copy secret(s) to clipboard (--only-one is implied)")
	return c
}

func newSecretsPath(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:          "path",
		Short:        "Resolve the secrets path for the current context",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), secretsContext(cmd, paths, "", "").StorePath())
			return nil
		},
	}
}

// ── set / env ─────────────────────────────────────────────

// setEnvFlags declares the flag spec `set` and `env` share. -s is
// --namespace here; the local `cluster` keeps cobra from merging the
// inherited global (same name), so the shorthand is not redefined.
func setEnvFlags(c *cobra.Command, withEncrypt bool) {
	f := c.Flags()
	f.StringP("name", "n", "", "Secret name")
	f.StringP("namespace", "s", "default", "Namespace")
	f.String("cluster", "", "Cluster name to manage")
	if withEncrypt {
		f.BoolP("encrypt", "e", false, "SOPS-encrypt this one file after writing (needs .sops.yaml)")
		f.Bool("enc", false, "Alias of --encrypt")
		_ = f.MarkHidden("enc")
	}
}

func newSecretsSet(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "set [--name N] [--namespace NS] [--encrypt] <key> [value]",
		Aliases:      []string{"s"},
		Short:        "Write a literal value into the secret cache",
		Annotations:  commandSpec{destructive: true}.annotations(),
		Args:         secretsArgs(1, 2, "key"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			name, _ := f.GetString("name")
			namespace, _ := f.GetString("namespace")
			cluster, _ := f.GetString("cluster")
			encrypt, _ := f.GetBool("encrypt")
			enc, _ := f.GetBool("enc")
			value := ""
			if len(args) == 2 {
				value = args[1]
			}
			ctx := secretsContext(cmd, paths, "", cluster)
			return secretsRun(ctx.Set(name, namespace, args[0], value, encrypt || enc))
		},
	}
	setEnvFlags(c, true)
	return argshFlagErrors(c)
}

func newSecretsEnv(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "env --name N [--namespace NS]",
		Short:        "Emit export KEY=value lines for a cached secret",
		Annotations:  commandSpec{readonly: true}.annotations(),
		Args:         secretsArgs(0, 0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			name, _ := f.GetString("name")
			namespace, _ := f.GetString("namespace")
			cluster, _ := f.GetString("cluster")
			ctx := secretsContext(cmd, paths, "", cluster)
			return secretsRun(ctx.Env(name, namespace))
		},
	}
	setEnvFlags(c, false)
	return argshFlagErrors(c)
}
