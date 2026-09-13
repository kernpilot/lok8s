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
// project, reviewed with it. Go-only commands (assets, mcp, operator, and
// init for its project|toolchain subcommands) cannot be listed.
//
// Nothing is read from the environment and nothing switches on the
// presence of a file inside the tree.

import (
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
	// routed then, and every command but lint refuses to run (see
	// routing.refuse). lint reports it as a finding instead.
	err error
}

// goOnlySubcommands lists the usage-tree commands that carry Go-only
// subcommands (registered by their command file, not by goOnlyCommands):
// routing the parent would take those away.
var goOnlySubcommands = map[string][]string{
	"init": {"project", "toolchain"},
}

// sharedState names the routed commands whose state Go also writes on
// its own paths; doctor warns about them (a customised lib then drifts
// from what the Go side maintains).
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
	if info, err := os.Stat(filepath.Join(impl.TreeDir, "lo")); err != nil || info.IsDir() {
		fix := "remove spec.implementation.bash.commands"
		if r.all {
			fix = "set spec.implementation.default: go"
		}
		r.err = config.ImplementationError(fmt.Sprintf("implementation bash: the tree %s is missing. Run \"lo assets eject bash\", or %s.", filepath.Join(impl.TreeDir, "lo"), fix))
	}
	return r
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
	if r.err != nil {
		return false
	}
	return r.all || r.commands[name]
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

// refuse returns the block's validation error for every command but lint,
// which reports it as a finding of its own (lint.Linter.Implementation)
// so a broken block is diagnosable through the command made for it.
func (r routing) refuse(name string) error {
	if r.err == nil || name == "lint" {
		return nil
	}
	return r.err
}
