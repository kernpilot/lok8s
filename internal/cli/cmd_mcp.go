package cli

// lo mcp — the native MCP server over the lo command tree.
//
// Built on github.com/njayp/ophis (pinned, see go.mod). ophis walks a cobra
// tree, turns every runnable leaf into an MCP tool named `<root>_<path…>` —
// the exact scheme the argsh `lo mcp` builtin used (lo_secrets_encrypt,
// lo_kubehz_join, lo_registry_up, …) — and executes a call by spawning THIS
// binary with the reconstructed argv. Shim commands therefore work unchanged:
// the subprocess is `lo registry up …`, which the shim hands to the bash
// implementation verbatim.
//
// The server never walks the real root: it projects newUsageTree (the
// argsh-mirrored tree) plus the shim dispatchers' leaves (shimLeaves) and
// stamps the MCP hint annotations from the lok8s markers (mcpAnnotate). The
// Go-only commands — this one included — are never tools.
//
// Exposure policy, enforced structurally (a command that is not exposed is
// not registered with the server, so it cannot be called by name either):
//   - readonly commands are exposed by default;
//   - mutating, non-destructive commands (build, use, secrets encrypt, …)
//     need --allow-mutating;
//   - destructive commands (up, down, deploy, destroy, …) need
//     --allow-destructive, which implies --allow-mutating;
//   - a command without any marker (own or inherited) counts as mutating —
//     it is not known to be safe;
//   - flags that carry a credential (token, secret, password, key, nonce, …)
//     are never exposed; --force and --force-recreate only with
//     --allow-destructive; --verbose never (ophis renders a count flag as
//     `--verbose N`, which cobra would take as a stray positional).
//
// LO_MCP_ALLOW=mutating|destructive is the env form of the opt-in, for the
// editor configs `lo mcp <editor> enable --env LO_MCP_ALLOW=…` writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/njayp/ophis"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kernpilot/lok8s/internal/config"
)

// Exposure tiers, ordered by the opt-in they need.
const (
	mcpTierReadonly = iota
	mcpTierMutating
	mcpTierDestructive
)

// mcpAllowEnv is the environment form of the exposure opt-in.
const mcpAllowEnv = "LO_MCP_ALLOW"

// mcpExposure is the resolved opt-in state of one server run.
type mcpExposure struct {
	mutating    bool
	destructive bool
}

// resolve folds LO_MCP_ALLOW into the flag state (flags win) and applies
// "destructive implies mutating".
func (x *mcpExposure) resolve() error {
	switch v := os.Getenv(mcpAllowEnv); v {
	case "":
	case "mutating":
		x.mutating = true
	case "destructive":
		x.destructive = true
	default:
		return fmt.Errorf("%s=%q: want \"mutating\" or \"destructive\"", mcpAllowEnv, v)
	}
	if x.destructive {
		x.mutating = true
	}
	return nil
}

func (x mcpExposure) allows(tier int) bool {
	switch tier {
	case mcpTierReadonly:
		return true
	case mcpTierMutating:
		return x.mutating || x.destructive
	default:
		return x.destructive
	}
}

// mcpSensitiveFlagWords are the words (hyphen-separated segments of a flag
// name) that mark a flag as credential-bearing. `--ssh-key` and `--nonce`
// match; `--cluster-id` does not.
var mcpSensitiveFlagWords = map[string]bool{
	"token": true, "secret": true, "secrets": true,
	"password": true, "passwd": true,
	"credential": true, "credentials": true,
	"nonce": true, "key": true, "apikey": true, "pat": true,
}

// mcpDestructiveOnlyFlags are exposed only with --allow-destructive.
var mcpDestructiveOnlyFlags = map[string]bool{"force": true, "force-recreate": true}

// mcpNeverFlags are never exposed: `verbose` is a count flag (see the file
// comment); `help` is cobra's own.
var mcpNeverFlags = map[string]bool{"verbose": true, "help": true}

func mcpSensitiveFlag(name string) bool {
	for _, word := range strings.Split(strings.ToLower(name), "-") {
		if mcpSensitiveFlagWords[word] {
			return true
		}
	}
	return false
}

func (x mcpExposure) allowsFlag(name string) bool {
	if mcpNeverFlags[name] || mcpSensitiveFlag(name) {
		return false
	}
	if mcpDestructiveOnlyFlags[name] {
		return x.destructive
	}
	return true
}

// mcpMarks returns the lok8s marker annotations that apply to cmd: its own
// when it carries any, else the nearest annotated ancestor's (a dispatcher's
// markers cover the subcommands that declare none). nil when nothing in the
// chain is annotated.
func mcpMarks(cmd *cobra.Command) map[string]string {
	for c := cmd; c != nil; c = c.Parent() {
		a := c.Annotations
		if a[AnnotationDestructive] == "true" || a[AnnotationReadonly] == "true" || a[AnnotationIdempotent] == "true" {
			return a
		}
	}
	return nil
}

// mcpTier classifies a command for the exposure gate.
func mcpTier(cmd *cobra.Command) int {
	marks := mcpMarks(cmd)
	switch {
	case marks[AnnotationDestructive] == "true":
		return mcpTierDestructive
	case marks[AnnotationReadonly] == "true":
		return mcpTierReadonly
	default:
		return mcpTierMutating
	}
}

// mcpAnnotate stamps the MCP hint keys ophis reads onto every command of the
// tree, derived from the lok8s markers (own or inherited). Clients get the
// same readOnlyHint / destructiveHint / idempotentHint the argsh server sent.
// destructiveHint is left unset for a command without any marker: the MCP
// default for "unknown" is destructive, and that is the honest answer.
func mcpAnnotate(cmd *cobra.Command) {
	marks := mcpMarks(cmd)
	a := map[string]string{}
	maps.Copy(a, cmd.Annotations)
	if marks[AnnotationReadonly] == "true" {
		a[ophis.AnnotationReadOnly] = "true"
	}
	if marks[AnnotationIdempotent] == "true" {
		a[ophis.AnnotationIdempotent] = "true"
	}
	switch {
	case mcpTier(cmd) == mcpTierDestructive:
		a[ophis.AnnotationDestructive] = "true"
	case marks != nil:
		a[ophis.AnnotationDestructive] = "false"
	}
	cmd.Annotations = a
	for _, sub := range cmd.Commands() {
		mcpAnnotate(sub)
	}
}

// shimLeaves lists the subcommands of the shim dispatchers — commands whose
// implementation still lives in bash and which the Go tree carries as a
// single passthrough. The MCP projection adds them as shim leaves so the
// tools keep the leaf names the argsh server exposed (lo_registry_up, …)
// instead of collapsing into one lo_registry tool. Entries are verbatim from
// the dispatcher's usage array (markers included — bash declares none for
// registry). TestMcpShimLeavesMatchArgshUsage guards the drift and
// TestMcpShimLeavesAreNotPorted fails once the dispatcher gets a Go port.
var shimLeaves = map[string][]commandSpec{}

// newMCPTree builds the tree the MCP server projects into tools: the
// usage-mirrored tree, the shim dispatchers' leaves, and the MCP hints.
func newMCPTree(paths *config.Paths) *cobra.Command {
	root := newUsageTree(paths)
	for _, cmd := range root.Commands() {
		for _, spec := range shimLeaves[cmd.Name()] {
			cmd.AddCommand(newShimCommand(paths, spec))
		}
	}
	mcpAnnotate(root)
	return root
}

// mcpHiddenSubtree reports whether cmd or any ancestor is hidden. ophis
// only checks the leaf's own Hidden flag; the framework-internal dispatchers
// (hooks, env, k8s, crds) are hidden at the top and their leaves are not.
func mcpHiddenSubtree(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Hidden {
			return true
		}
	}
	return false
}

// mcpSelectors is the single ophis selector: leaves only (dispatchers are
// traversed, not exposed — as in the argsh server), outside hidden subtrees,
// gated by tier, with the flag policy applied to local and inherited flags
// alike.
func mcpSelectors(x mcpExposure) []ophis.Selector {
	flagOK := func(f *pflag.Flag) bool { return x.allowsFlag(f.Name) }
	return []ophis.Selector{{
		CmdSelector: func(cmd *cobra.Command) bool {
			return !cmd.HasSubCommands() && !mcpHiddenSubtree(cmd) && x.allows(mcpTier(cmd))
		},
		LocalFlagSelector:     flagOK,
		InheritedFlagSelector: flagOK,
	}}
}

// mcpDefaultEnv is what `lo mcp <editor> enable` records for the launched
// server: the shim-prepared PATH (toolchain + framework dirs) and the project
// base, because editors start MCP servers with a minimal environment and an
// unrelated working directory.
func mcpDefaultEnv(paths *config.Paths) map[string]string {
	env := map[string]string{"PATH_BASE": paths.Base}
	for _, kv := range shimEnv(paths) {
		if k, v, _ := strings.Cut(kv, "="); k == "PATH" {
			env[k] = v
		}
	}
	return env
}

// mcpExportEnv gives the server process — and so every tool subprocess — the
// environment the shim prepares (PATH, KUSTOMIZE_PLUGIN_HOME).
func mcpExportEnv(paths *config.Paths) {
	for _, kv := range shimEnv(paths) {
		if k, v, _ := strings.Cut(kv, "="); k == "PATH" || k == "KUSTOMIZE_PLUGIN_HOME" {
			os.Setenv(k, v)
		}
	}
}

func mcpConfig(paths *config.Paths, x mcpExposure, transport mcp.Transport, quiet bool) *ophis.Config {
	cfg := &ophis.Config{
		Selectors:  mcpSelectors(x),
		Transport:  transport,
		DefaultEnv: mcpDefaultEnv(paths),
	}
	if quiet {
		cfg.SloggerOptions = &slog.HandlerOptions{Level: slog.LevelError}
		cfg.ServerOptions = &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	return cfg
}

// mcpRun executes `mcp <args…>` of a fresh ophis command attached to the
// projection tree, so ophis walks exactly that tree. A nil transport means
// stdio (ophis's default).
func mcpRun(ctx context.Context, paths *config.Paths, x mcpExposure, transport mcp.Transport, quiet bool, out, errOut io.Writer, args ...string) error {
	tree := newMCPTree(paths)
	tree.AddCommand(ophis.Command(mcpConfig(paths, x, transport, quiet)))
	tree.SetOut(out)
	tree.SetErr(errOut)
	tree.SetArgs(append([]string{"mcp"}, args...))
	return tree.ExecuteContext(ctx)
}

// mcpListTools returns the tools a server with exposure x advertises, by
// running one over an in-memory transport and asking it. This is the exact
// list — the same code path a client sees — not a re-implementation.
func mcpListTools(ctx context.Context, paths *config.Paths, x mcpExposure) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	served := make(chan error, 1)
	go func() {
		served <- mcpRun(ctx, paths, x, serverT, true, io.Discard, io.Discard, "start")
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "lo-mcp-tools", Version: version}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to the in-memory server: %w", err)
	}
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			session.Close()
			return nil, err
		}
		tools = append(tools, tool)
	}
	session.Close()
	cancel()
	if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

func mcpExposureFlags(cmd *cobra.Command, x *mcpExposure) {
	f := cmd.Flags()
	f.BoolVar(&x.mutating, "allow-mutating", false,
		"Also expose mutating, non-destructive commands (build, use, secrets encrypt, …)")
	f.BoolVar(&x.destructive, "allow-destructive", false,
		"Also expose destructive commands (up, down, deploy, destroy, …) and --force/--force-recreate; implies --allow-mutating")
}

const mcpLong = `Serve the lo commands to AI agents and editors as MCP tools.

Every leaf command becomes one tool named lo_<path with underscores>:
lo_status, lo_secrets_encrypt, lo_kubehz_join, lo_registry_up, ...
Dispatchers (secrets, tilt, kubehz, ...) are traversed, not exposed.
A tool call runs the same lo binary as a subprocess and returns its
stdout, stderr and exit code.

Exposure policy — what an agent can call:

  default              readonly commands only (status, lint, audit,
                       kubeconfig, secrets list, tilt status, ...)
  --allow-mutating     + mutating, non-destructive commands
                       (build, use, init, secrets encrypt, ...)
  --allow-destructive  + destructive commands (up, down, deploy, destroy,
                       tilt down, kubehz deregister, ...) and the --force
                       flags; implies --allow-mutating

A command without a marker counts as mutating. Flags that carry a
credential (token, secret, password, key, nonce, ...) are never exposed.
A command that is not exposed is not registered, so it cannot be called.
LO_MCP_ALLOW=mutating|destructive is the environment form of the opt-in,
for editor configs (flags win).

Transports: 'start' serves stdio (what editors launch); 'serve' serves
streamable HTTP on loopback — it has no authentication, keep it there.
'tools' prints the tool list a server would advertise.

Editor setup writes the launch command, the toolchain PATH and PATH_BASE
into the editor's MCP config:

  lo mcp claude enable          # Claude Desktop
  lo mcp vscode enable          # VS Code (Copilot agent mode)
  lo mcp cursor enable
  lo mcp claude enable --env LO_MCP_ALLOW=destructive`

// newMcpCommand builds the `lo mcp` group: the lo-owned start/serve/tools
// (they carry the exposure flags) plus ophis's editor config commands.
func newMcpCommand(paths *config.Paths) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "mcp",
		Short:        "Serve the lo commands as MCP tools (stdio or HTTP)",
		Long:         mcpLong,
		GroupID:      groupIntegrations,
		Annotations:  map[string]string{AnnotationReadonly: "true"},
		SilenceUsage: true,
	}
	cmd.AddCommand(newMcpStart(paths), newMcpServe(paths), newMcpTools(paths))
	for _, sub := range ophis.Command(mcpConfig(paths, mcpExposure{}, nil, false)).Commands() {
		switch sub.Name() {
		case "claude", "vscode", "cursor":
			cmd.AddCommand(sub)
		}
	}
	return cmd
}

func newMcpStart(paths *config.Paths) *cobra.Command {
	var x mcpExposure
	var logLevel string
	c := &cobra.Command{
		Use:          "start",
		Short:        "Serve over stdio (what editors and agents launch)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := x.resolve(); err != nil {
				return err
			}
			mcpExportEnv(paths)
			return mcpRun(cmd.Context(), paths, x, nil, false, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				"start", "--log-level", logLevel)
		},
	}
	mcpExposureFlags(c, &x)
	c.Flags().StringVar(&logLevel, "log-level", "info", "Server log level on stderr (debug, info, warn, error)")
	return c
}

func newMcpServe(paths *config.Paths) *cobra.Command {
	var x mcpExposure
	var logLevel, host string
	var port int
	c := &cobra.Command{
		Use:          "serve",
		Aliases:      []string{"stream"},
		Short:        "Serve over streamable HTTP (loopback by default; no authentication)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := x.resolve(); err != nil {
				return err
			}
			if host != "127.0.0.1" && host != "localhost" && host != "::1" {
				fmt.Fprintf(cmd.ErrOrStderr(), "[warn] serving MCP without authentication on %s:%d — every client that reaches it can run the exposed tools\n", host, port)
			}
			mcpExportEnv(paths)
			return mcpRun(cmd.Context(), paths, x, nil, false, cmd.OutOrStdout(), cmd.ErrOrStderr(),
				"stream", "--host", host, "--port", strconv.Itoa(port), "--log-level", logLevel)
		},
	}
	mcpExposureFlags(c, &x)
	c.Flags().StringVar(&host, "host", "127.0.0.1", "Address to listen on")
	c.Flags().IntVar(&port, "port", 8080, "Port to listen on")
	c.Flags().StringVar(&logLevel, "log-level", "info", "Server log level on stderr (debug, info, warn, error)")
	return c
}

func newMcpTools(paths *config.Paths) *cobra.Command {
	var x mcpExposure
	var asJSON bool
	c := &cobra.Command{
		Use:          "tools",
		Short:        "Print the tools a server would expose (name, tier, description)",
		Args:         cobra.NoArgs,
		Annotations:  map[string]string{AnnotationReadonly: "true"},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := x.resolve(); err != nil {
				return err
			}
			tools, err := mcpListTools(cmd.Context(), paths, x)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(tools)
			}
			for _, tool := range tools {
				desc, _, _ := strings.Cut(tool.Description, "\n")
				fmt.Fprintf(out, "%-32s %-12s %s\n", tool.Name, mcpHintLabel(tool.Annotations), desc)
			}
			return nil
		},
	}
	mcpExposureFlags(c, &x)
	c.Flags().BoolVar(&asJSON, "json", false, "Print the full tool definitions (schemas, annotations) as JSON")
	return c
}

// mcpHintLabel renders a tool's MCP hints as the lok8s tier word.
func mcpHintLabel(a *mcp.ToolAnnotations) string {
	switch {
	case a == nil:
		return "mutating"
	case a.ReadOnlyHint:
		return "readonly"
	case a.DestructiveHint != nil && *a.DestructiveHint:
		return "destructive"
	default:
		return "mutating"
	}
}
