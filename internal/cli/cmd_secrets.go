package cli

// lo secrets — secret cache management with SOPS/age encryption.
// Go port of .lok8s/libs/secrets; output is byte-identical (see
// internal/secrets for the operations).
//
// `set` and `env` disable cobra flag parsing and hand-parse argsh-style: the
// bash spec gives -s to --namespace inside those subcommands, shadowing the
// global --cluster shorthand — cobra cannot express a shadowed inherited
// shorthand (the merged flag set panics on the redefinition), the hand parser
// can.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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

// argshErrorf prints a parse error in the argsh entrypoint's format. (The
// argsh implementation exits 2 on parse errors; the Go binary exits 1 — the
// message is what scripts and humans match on.)
func argshErrorf(errOut io.Writer, format string, a ...any) error {
	fmt.Fprintf(errOut, "Error: "+format+"\n\n  Run \"lo -h\" for more information.\n", a...)
	return ErrHandled
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

// ── set / env: argsh-style hand parsing ──────────────────

// setEnvFlags is the parse result for the hand-parsed subcommands. The
// grammar is the subcommand's own spec plus everything it inherits in bash
// (the parent's domain flag and main's global flags) — first-match-wins on
// shorthands, which is how -s means --namespace here while it means --cluster
// everywhere else.
type setEnvFlags struct {
	name, namespace, domain, cluster string
	encrypt                          bool
	help                             bool
	positionals                      []string
}

// parseSetEnv hand-parses argv for `secrets set` (withEncrypt) and
// `secrets env`. Unknown flags produce the argsh error shape.
func parseSetEnv(errOut io.Writer, args []string, withEncrypt bool) (*setEnvFlags, error) {
	p := &setEnvFlags{namespace: "default"}
	// value-taking global flags that are accepted (and inherited) in bash but
	// have no effect at this level; their values must still be consumed.
	valueFlag := func(long string) bool {
		switch long {
		case "name", "namespace", "domain", "kubernetes", "cluster", "config", "domain-sans":
			return true
		}
		return false
	}
	setValue := func(long, val string) {
		switch long {
		case "name":
			p.name = val
		case "namespace":
			p.namespace = val
		case "domain":
			p.domain = val
		case "cluster":
			p.cluster = val
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-" || !strings.HasPrefix(a, "-"):
			p.positionals = append(p.positionals, a)
		case strings.HasPrefix(a, "--"):
			long, val, hasVal := strings.Cut(a[2:], "=")
			switch {
			case valueFlag(long):
				if !hasVal {
					i++
					if i >= len(args) {
						return nil, argshErrorf(errOut, "flag needs an argument: %s", a)
					}
					val = args[i]
				}
				setValue(long, val)
			case withEncrypt && (long == "encrypt" || long == "enc"):
				p.encrypt = true
			case long == "verbose" || long == "force" || long == "force-recreate" || long == "remote":
				// consumed, no effect at subcommand level (matches bash:
				// main's DEBUG/FORCE exports happen before dispatch)
			case long == "help":
				p.help = true
			default:
				return nil, argshErrorf(errOut, "unknown flag: %s", a)
			}
		default:
			if len(a) != 2 {
				return nil, argshErrorf(errOut, "unknown flag: %s", a)
			}
			switch a[1] {
			case 'n', 's':
				long := "name"
				if a[1] == 's' {
					long = "namespace"
				}
				i++
				if i >= len(args) {
					return nil, argshErrorf(errOut, "flag needs an argument: %s", a)
				}
				setValue(long, args[i])
			case 'e':
				if !withEncrypt {
					return nil, argshErrorf(errOut, "unknown flag: %s", a)
				}
				p.encrypt = true
			case 'v', 'f', 'r':
				// global booleans, consumed (see above)
			case 'h':
				p.help = true
			default:
				return nil, argshErrorf(errOut, "unknown flag: %s", a)
			}
		}
	}
	return p, nil
}

func newSecretsSet(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:                "set [--name N] [--namespace NS] [--encrypt] <key> [value]",
		Aliases:            []string{"s"},
		Short:              "Write a literal value into the secret cache",
		Annotations:        commandSpec{destructive: true}.annotations(),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := parseSetEnv(cmd.ErrOrStderr(), args, true)
			if err != nil {
				return err
			}
			if p.help {
				return cmd.Help()
			}
			if len(p.positionals) < 1 {
				return argshErrorf(cmd.ErrOrStderr(), "missing required argument: key")
			}
			if len(p.positionals) > 2 {
				return argshErrorf(cmd.ErrOrStderr(), "unexpected argument: %s", p.positionals[2])
			}
			key := p.positionals[0]
			value := ""
			if len(p.positionals) == 2 {
				value = p.positionals[1]
			}
			ctx := secretsContext(cmd, paths, p.domain, p.cluster)
			return secretsRun(ctx.Set(p.name, p.namespace, key, value, p.encrypt))
		},
	}
}

func newSecretsEnv(paths *config.Paths) *cobra.Command {
	return &cobra.Command{
		Use:                "env --name N [--namespace NS]",
		Short:              "Emit export KEY=value lines for a cached secret",
		Annotations:        commandSpec{readonly: true}.annotations(),
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := parseSetEnv(cmd.ErrOrStderr(), args, false)
			if err != nil {
				return err
			}
			if p.help {
				return cmd.Help()
			}
			if len(p.positionals) > 0 {
				return argshErrorf(cmd.ErrOrStderr(), "unexpected argument: %s", p.positionals[0])
			}
			ctx := secretsContext(cmd, paths, p.domain, p.cluster)
			return secretsRun(ctx.Env(p.name, p.namespace))
		},
	}
}
