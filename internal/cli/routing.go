package cli

// routing.go — which implementation runs a command: the Go code in this
// binary (the default) or the frozen bash tree in the project, as the
// project file lok8s.yaml says (spec.implementation, config.Implementation).
//
// NewRoot resolves the routing once and registers a shim command (verbatim
// argv, no cobra flag parse) for every routed name; `default: bash` shims
// the whole usage tree. The hook is the tree build, not a PersistentPreRun:
// a pre-run hook fires after cobra parsed the Go flags and would reject a
// flag only the bash side knows. Aliases keep working: cobra maps `lo r up`
// to `registry`, and os.Args reaches bash unchanged.
//
// The routed tree is ALWAYS <project>/<tree>/lo. The cache extract and a
// PATH_LOK8S checkout are never routed to: a customised lib lives in the
// project, reviewed with it. Go-only commands (assets, mcp, operator,
// toolchain, and init for its project|toolchain subcommands) cannot be
// listed.
//
// Nothing is read from the environment and nothing switches on the
// presence of a file inside the tree.

import (
	"github.com/spf13/cobra"

	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
)

// routing is the resolved implementation selection of one process.
type routing struct {
	// impl is the parsed block (defaults applied).
	impl config.Implementation
	// commands is the validated set of routed top-level names (empty when
	// all is set: the whole tree is routed).
	commands map[string]bool
	// all is `default: bash`.
	all bool
	// err is the validation error of the block, when any; no command is
	// routed then, and every command but the diagnostic ones refuses to
	// run (see routing.refuse). lint reports it as a finding instead.
	err error
	// treeErr is the routing precondition that failed: the block is valid
	// but the tree it routes to is missing from the project. Only a routed
	// command refuses on it (its shim prints the fix); the Go-only
	// commands and treeExempt run, so `lo assets eject bash` can create
	// the tree the message asks for.
	treeErr error
}

// treeExempt lists the usage-tree commands that fall back to Go while the
// routed tree is missing: the diagnostics (lint reports the missing tree
// as a finding, doctor as a warning) and init (its project|toolchain
// subcommands are Go-only).
var treeExempt = map[string]bool{"lint": true, "doctor": true, "init": true}

// refuseExempt lists the commands that run on an invalid block: the
// diagnostics, which report it themselves, and cobra's own help and
// completion, which run no lo code, the completion requests the shell
// scripts send (`__complete`, `__completeNoDesc`: a Tab must answer on a
// broken project too), and the bare root (`lo` alone: the orientation
// block or the help, which printed on an invalid block before the root
// had a RunE and still does).
var refuseExempt = map[string]bool{
	"lint": true, "doctor": true, "help": true, "completion": true, "lo": true,
	cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true,
}

// goOnlySubcommands lists the usage-tree commands that carry Go-only
// subcommands (registered by their command file, not by goOnlyCommands):
// routing the parent would take those away.
var goOnlySubcommands = map[string][]string{
	"init": {"cluster", "project", "toolchain"},
}

// sharedState names the routed commands whose state Go also writes on
// its own paths; doctor warns about them (a customised lib then drifts
// from what the Go side maintains).
//
// #nosec G101 -- command names and doctor hints; the "secrets" key is
// the `lo secrets` command, not a credential.
var sharedState = map[string]string{
	"registry":  "lo up manages the same registries with Go code",
	"image":     "lo up manages the same cache registry with Go code",
	"secrets":   "lo build reads the same secrets store with Go code",
	"use":       "every Go command reads the same clusters/.active",
	"kustomize": "lo build uses the same plugin home with Go code",
}

// newRouting reads and validates spec.implementation for the project.
func newRouting(paths *config.Paths) routing {
	impl, err := config.LoadImplementation(paths.Base)
	r := routing{impl: impl}
	if err != nil {
		r.err = err
		return r
	}
	r.all = impl.Default == config.ImplBash
	r.commands = map[string]bool{}
	for _, name := range impl.Commands {
		if err := validateRoutedName(name); err != nil {
			r.err = err
			return r
		}
		if r.commands[name] {
			r.err = implError("spec.implementation.bash.commands: %q is listed twice.", name)
			return r
		}
		r.commands[name] = true
	}
	if !impl.Routes() {
		return r
	}
	// A routed command needs the tree in the project; the cache extract
	// is never routed to.
	entry := filepath.Join(impl.TreeDir, "lo")
	if info, err := os.Stat(entry); err != nil || info.IsDir() {
		fix := "remove spec.implementation.bash.commands"
		if r.all {
			fix = "set spec.implementation.default: go"
		}
		r.treeErr = config.ImplementationError(fmt.Sprintf("implementation bash: the tree %s is missing. Run \"lo assets eject bash\", or %s.", entry, fix))
		return r
	}
	// The entrypoint itself must resolve inside the project too: a tree
	// directory that passes the escape rule may still hold a `lo` symlink
	// to a checkout elsewhere (bash derives PATH_BASE from BASH_SOURCE,
	// not from the resolved file, but the code it runs would be foreign).
	if resolved, ok := resolvesInside(paths.Base, entry); !ok {
		r.err = implError("spec.implementation.bash.tree: the entrypoint %s resolves to %s, outside the project.", filepath.Join(impl.Tree, "lo"), resolved)
	}
	return r
}

// resolvesInside reports whether path, symlinks resolved, lies under the
// resolved base; it returns the resolved path either way ("" when it
// cannot be resolved, which counts as outside).
func resolvesInside(base, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return resolved, false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return resolved, false
	}
	return resolved, true
}

// implError is a spec.implementation validation error in the loader's
// format (config.ImplementationError).
func implError(format string, a ...any) error {
	return config.ImplementationError(config.ProjectFile + ": " + fmt.Sprintf(format, a...))
}

// validateRoutedName checks one spec.implementation.bash.commands entry:
// a top-level usage-tree name that has a bash implementation. Aliases,
// Go-only commands and unknown names are errors.
func validateRoutedName(name string) error {
	for _, g := range goOnlyCommands {
		if name == g.name {
			return implError("%q has no bash implementation.", name)
		}
	}
	for _, spec := range commandTree {
		if name == spec.use {
			if subs := goOnlySubcommands[name]; len(subs) > 0 {
				return implError("%q has no bash implementation for %q.", name, name+" "+strings.Join(subs, "|"))
			}
			return nil
		}
		if slices.Contains(spec.aliases, name) {
			return implError("spec.implementation.bash.commands: %q is an alias. Use the command name %q.", name, spec.use)
		}
	}
	return implError("spec.implementation.bash.commands: unknown command %q. Use top-level command names from \"lo --help\".", name)
}

// routed reports whether the top-level command name runs through the
// bash tree.
func (r routing) routed(name string) bool {
	if r.err != nil || (r.treeErr != nil && treeExempt[name]) {
		return false
	}
	return r.all || r.commands[name]
}

// problem is the first thing wrong with the routing: the block's
// validation error, else the missing tree, else nil. lint reports it as
// a finding, doctor as a warning.
func (r routing) problem() error {
	if r.err != nil {
		return r.err
	}
	return r.treeErr
}

// active reports whether anything is routed (the doctor lines depend on
// it; unset keeps doctor byte-identical to the bash implementation).
func (r routing) active() bool {
	return r.err == nil && r.impl.Routes()
}

// routedNames lists the explicitly routed commands in file order.
func (r routing) routedNames() []string {
	if r.err != nil {
		return nil
	}
	return r.impl.Commands
}

// refuse returns the block's validation error for every command but the
// refuseExempt set: lint reports it as a finding of its own
// (lint.Linter.Implementation) and doctor as a warning, so a broken block
// is diagnosable through the commands made for it. The missing-tree
// precondition (treeErr) is not a refusal here: the routed shim reports
// it when it runs.
func (r routing) refuse(name string) error {
	if r.err == nil || refuseExempt[name] {
		return nil
	}
	return r.err
}
