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
	"io"
	"os"
	"slices"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	if errors.Is(err, secrets.ErrHandled) {
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

// secretsArgs returns an argsh-shaped arity validator, shared by every leaf
// that mirrors an argsh `:args` spec: at least minArgs positionals (naming
// the first missing one) and at most maxArgs.
func secretsArgs(minArgs, maxArgs int, names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < minArgs {
			return argshErrorf(cmd.ErrOrStderr(), "missing required argument: %s", names[len(args)])
		}
		if len(args) > maxArgs {
			return argshErrorf(cmd.ErrOrStderr(), "unexpected argument: %s", args[maxArgs])
		}
		return nil
	}
}

// keyArgCommand guards a command whose argument is a key (add-key,
// remove-key): a pasted private key must not reach any message. Before RunE
// runs, cobra prints a flag-parse error and secretsArgs an extra argument,
// both with the value. So the Args check refuses a positional or a flag value
// of that shape first, and the flag-error func does not print an error that
// holds one. GuardPrivateKeyArgv runs the same check on the raw argv, before
// cobra parses anything.
func keyArgCommand(c *cobra.Command) *cobra.Command {
	arity := secretsArgs(1, 1, "key")
	c.Args = func(cmd *cobra.Command, args []string) error {
		private := false
		for _, a := range args {
			private = private || secrets.LooksLikePrivateKey(a)
		}
		cmd.Flags().Visit(func(f *pflag.Flag) {
			private = private || secrets.LooksLikePrivateKey(f.Value.String())
		})
		if private {
			secrets.RefusePrivateKey(cmd.ErrOrStderr())
			return ErrHandled
		}
		return arity(cmd, args)
	}
	c.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if secrets.LooksLikePrivateKey(err.Error()) {
			secrets.RefusePrivateKey(cmd.ErrOrStderr())
			return ErrHandled
		}
		return argshErrorf(cmd.ErrOrStderr(), "%s", argshFlagError(cmd, err.Error()))
	})
	return c
}

// GuardPrivateKeyArgv refuses a `lo secrets add-key` or `remove-key` call
// whose raw argv holds an argument with the shape of a private key, flag
// values and words that look like flags included. main runs it before
// Execute: cobra's flag parser and the argument checks print what they
// reject. It prints the refusal (never the argument) and reports whether it
// refused. Any other command is left to cobra.
func GuardPrivateKeyArgv(root *cobra.Command, argv []string, errOut io.Writer) bool {
	cmd, _, err := root.Find(argv)
	if err != nil || cmd.Parent() == nil || cmd.Parent().Name() != "secrets" {
		return false
	}
	if cmd.Name() != "add-key" && cmd.Name() != "remove-key" {
		return false
	}
	if slices.ContainsFunc(argv, secrets.LooksLikePrivateKey) {
		secrets.RefusePrivateKey(errOut)
		return true
	}
	return false
}

func newSecretsCommand(paths *config.Paths, spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "secrets",
		Aliases:      spec.aliases,
		Short:        spec.short,
		GroupID:      spec.group,
		Annotations:  spec.annotations(),
		SilenceUsage: true,
		RunE:         argshGroupRunE,
	}

	cmd.AddCommand(
		newSecretsInit(paths),
		newSecretsAddKey(paths),
		newSecretsRemoveKey(paths),
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
	return keyArgCommand(c)
}

func newSecretsRemoveKey(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:   "remove-key <key>",
		Short: "Remove an age recipient and re-key the store",
		Long: `Remove an age recipient from every creation rule in .sops.yaml, then
re-encrypt every managed Secret.*.enc file of the selected stores from its
plaintext, each with a new data key: the labels are not trusted to choose
the files. Without --all that is the store of the current domain; with
--all, every domain store. The flat store is not re-keyed. A warning names
each other store that still lists the recipient. A recipient that is in no
rule and on no .enc label is a warning, not a refusal: the command then
re-keys every managed file and changes no rule, so a mistyped key re-keys
every file.

Before the first write, the command refuses:
  - a key argument that looks like a private key (it is not printed),
  - a YAML tag under creation_rules other than !!str, !!seq or !!map, or a
    mapping key in .sops.yaml that is not a string,
  - a recipient outside a single-quoted age: list (an unquoted list, an
    escaped string, a YAML list, key_groups, an alias or a merge key),
  - the last recipient of a rule,
  - a store path or an .enc file that it cannot stat or read (a dangling
    symlink included), or a directory in a store that it cannot read,
  - another .enc file in a store: a name that ends in .enc, at any depth,
    that is not a Secret.*.enc file at the top of the store (the re-key
    does not rewrite it; re-key or remove it by hand),
  - an .enc file whose sops metadata holds a member twice (re-encrypt it
    with sops from its plaintext),
  - an .enc file whose sops metadata does not parse or lists no age
    recipient (the command reads the recipients from that metadata),
  - a managed .enc file that is newer than its plaintext (run
    'lo secrets decrypt' first),
  - an .enc file that matches no creation rule, a rule with a recipient that
    is not age, or a rule with an age recipient that does not parse,
  - without --skip-orphans, an .enc file with no decrypted twin: the re-key
    cannot rewrite that file, so it keeps the removed recipient,
  - a managed .enc file that holds other bytes than its decrypted twin, one
    that your age identity cannot decrypt, or a twin that it cannot read:
    the command decrypts each file in memory to compare it with its twin,
    and the re-key encrypts the twin.

The command holds the lock .sops.yaml.lock for the whole run, and waits for
it LO_SECRETS_LOCK_WAIT seconds (60 by default). If the re-key stops after
the .sops.yaml write, run the same command again to resume it. At the end
it prints the number of creation rules that changed, of .enc files whose
labels listed the recipient, and of .enc files that it re-keyed. Commit the
.enc files together with .sops.yaml.

A removal does not revoke what the key already read. Its holder can still
decrypt the older .enc files in the git history, and keeps every value it
decrypted before. Rotate those secrets after the removal.`,
		Annotations:  commandSpec{destructive: true}.annotations(),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			skipOrphans, _ := cmd.Flags().GetBool("skip-orphans")
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.RemoveKey(args[0], all, skipOrphans))
		},
	}
	c.Flags().BoolP("all", "a", false, "Re-key every domain store, not just the current one")
	c.Flags().Bool("skip-orphans", false, "Proceed even if some .enc files have no decrypted twin (they keep the removed recipient)")
	return keyArgCommand(c)
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
		Annotations:  commandSpec{readonly: true, credentialOutput: true}.annotations(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			onlyOne, _ := cmd.Flags().GetBool("only-one")
			copyFlag, _ := cmd.Flags().GetBool("copy")
			ctx := secretsContext(cmd, paths, "", "")
			return secretsRun(ctx.Print(cmd.Context(), args, onlyOne, copyFlag))
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
			dir, err := secretsContext(cmd, paths, "", "").ResolveStore()
			if err != nil {
				return secretsRun(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), dir)
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
			return secretsRun(ctx.Set(cmd.Context(), name, namespace, args[0], value, encrypt || enc))
		},
	}
	setEnvFlags(c, true)
	return argshFlagErrors(c)
}

func newSecretsEnv(paths *config.Paths) *cobra.Command {
	c := &cobra.Command{
		Use:          "env --name N [--namespace NS]",
		Short:        "Emit export KEY=value lines for a cached secret",
		Annotations:  commandSpec{readonly: true, credentialOutput: true}.annotations(),
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
