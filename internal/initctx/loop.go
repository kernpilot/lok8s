package initctx

// loop.go: project mode. The card, the result of the last action under
// it, and one choice from what the state allows. An entry opens its
// action screen (screen.go). When the action is done, the loop reads the
// state again and prints the card again. Exit, Esc and Ctrl-C leave.
// Nothing is written except on a screen's Create, so an exit never
// leaves a half-written state. A failed step becomes a result row, not
// an exit. Each iteration prints the card and the list below the
// previous output; the forms draw inline and clear themselves.

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"

	"github.com/kernpilot/lok8s/internal/ui"
)

// Entry is one choice of the project list, with the verb a script runs
// instead (the twin).
type Entry struct {
	Key, Label, Twin string
}

// The keys of the project list.
const (
	EntryCluster        = "cluster"
	EntryService        = "service"
	EntryTests          = "tests"
	EntryToolchain      = "toolchain"
	EntryActive         = "active"
	EntryEject          = "eject"
	EntryImplementation = "implementation"
	EntryExit           = "exit"
)

// Entries lists what the project state allows, in the order of the list.
// Always: a cluster, a service, Exit. When there is no tests/: the test
// suite. When a pinned tool is missing or the pin file is unreadable:
// the toolchain. With several clusters, or with clusters and no valid
// active domain: the active domain. Without a bash tree: the eject; with
// one: the implementation switch.
func Entries(s State) []Entry {
	p := s.Project
	if p == nil {
		return nil
	}
	out := []Entry{
		{EntryCluster, "Add a cluster", "lo init cluster <domain>"},
		{EntryService, "Add a service", "lo init service <name>"},
	}
	if !p.Tests {
		out = append(out, Entry{EntryTests, "Add the test suite", "lo init test"})
	}
	switch {
	case !p.BYAML:
		out = append(out, Entry{EntryToolchain, "Install the toolchain", "lo toolchain install"})
	case p.BYAMLInvalid:
		out = append(out, Entry{EntryToolchain, "Install the toolchain (pin file unreadable)", "lo toolchain install"})
	case len(p.ToolsMissing) > 0:
		out = append(out, Entry{EntryToolchain, fmt.Sprintf("Install the toolchain (%d of %d missing)", len(p.ToolsMissing), p.Tools), "lo toolchain install"})
	}
	if len(p.Domains) > 1 || (len(p.Domains) > 0 && !p.ActiveValid()) {
		out = append(out, Entry{EntryActive, "Set the active domain", "lo use <domain>"})
	}
	if p.BashTree {
		other := "bash"
		if p.Implementation == "bash" {
			other = "go"
		}
		out = append(out, Entry{EntryImplementation, "Switch implementation (" + p.Implementation + " → " + other + ")", "lo init project --implementation " + other})
	} else {
		out = append(out, Entry{EntryEject, "Eject the bash tree", "lo assets eject bash"})
	}
	return append(out, Entry{EntryExit, "Exit", ""})
}

// WriteEntries prints the list as text (--plan): the labels, then the
// twins dim, then extra rows in the same columns.
func WriteEntries(w io.Writer, entries []Entry, extra ...row) {
	labels, twins := []string{}, []string{}
	for _, e := range entries {
		labels = append(labels, e.Label)
		if e.Twin != "" {
			twins = append(twins, e.Twin)
		}
	}
	rows := []row{{key: "actions", value: joined(labels)}, {key: "equivalent", value: joined(twins), dim: true}}
	writeRows(w, append(rows, extra...), false)
}

// Welcome is the one line mode 1 opens with: what `lo init` is about to
// set up here.
func Welcome(w io.Writer, s State) {
	what := "a lok8s project in this directory"
	if s.Situation() == SituationGitBelowRoot {
		what = "a lok8s project at the repository root"
	}
	line := "lo init sets up " + what + ": the project file, the first cluster spec, the toolchain."
	if p := s.Project; p != nil {
		// An existing project without a repository: what it lacks.
		parts := []string{"a git repository"}
		if len(p.Domains) == 0 {
			parts = append(parts, "the first cluster spec")
		}
		if p.ToolchainMissing() {
			parts = append(parts, "the toolchain")
		}
		line = "lo init completes the project " + projectName(s) + ": " + strings.Join(parts, ", ") + "."
	}
	fmt.Fprintf(w, "  %s\n", ui.For(w).Bold(line))
}

// Result is one row under the card: what the last action did (`added`,
// `installed`, `failed`, ...).
type Result struct {
	Key, Value string
}

// ResultOf is the result row of an executed plan.
func ResultOf(p Plan) Result {
	verb, what := p.Result()
	return Result{verb, what}
}

// FailureRows are the rows of a failed step: the command with its
// reason, and the commands that did not run.
func FailureRows(e *StepError) []Result {
	rows := []Result{{"failed", e.Command + " · " + e.Err.Error()}}
	if len(e.NotRun) > 0 {
		rows = append(rows, Result{"not run", joined(e.NotRun)})
	}
	return rows
}

// Loop is project mode. Detect reads the state again after an action;
// Execute runs a plan (the cli's executor over the subcommands' own
// functions) and returns a *StepError for a failed step.
type Loop struct {
	Out     io.Writer
	IO      IO
	Detect  func() (State, error)
	Execute func(Plan) error
}

// Run prints the card and the list until Exit, Esc or Ctrl-C. first are
// the rows of what ran before the loop (the bootstrap; nil = none): they
// go under the first card. A cancelled action returns to the list; a
// failed step returns to the list with the failed and not-run rows;
// Ctrl-C returns ErrAborted (the cli maps it to rc 130).
func (l Loop) Run(s State, first []Result) error {
	results := first
	for {
		WriteCard(l.Out, s, resultRows(results)...)
		fmt.Fprintln(l.Out)
		entries := Entries(s)
		opts := make([]huh.Option[string], 0, len(entries))
		for _, e := range entries {
			opts = append(opts, huh.NewOption(e.Label, e.Key))
		}
		// The cursor starts on the first entry.
		choice := entries[0].Key
		if err := run(huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		)), l.IO); err != nil {
			if errors.Is(err, ErrCancelled) {
				return nil
			}
			return err
		}
		if choice == EntryExit {
			WriteNext(l.Out, Next(s, false))
			return nil
		}
		plan, err := Run(l.Out, l.IO, l.actionScreen(s, choice))
		switch {
		case errors.Is(err, ErrCancelled):
			results = []Result{{"cancelled", "nothing written"}}
			continue
		case errors.Is(err, ErrIncomplete):
			// The input ended at a prompt (accessible mode): nothing
			// written, nothing more to ask.
			return nil
		case err != nil:
			return err
		}
		var step *StepError
		switch err := l.Execute(plan); {
		case errors.As(err, &step):
			results = FailureRows(step)
		case err != nil:
			return err
		default:
			results = []Result{ResultOf(plan)}
		}
		if s, err = l.Detect(); err != nil {
			return err
		}
	}
}

// resultRows converts the results to card rows.
func resultRows(results []Result) []row {
	rows := make([]row, 0, len(results))
	for _, r := range results {
		rows = append(rows, row{key: r.Key, value: r.Value, warn: r.Key == "failed"})
	}
	return rows
}

// actionScreen is the screen builder behind one list entry.
func (l Loop) actionScreen(s State, key string) func() Screen {
	p := s.Project
	switch key {
	case EntryCluster:
		// A new cluster becomes the active domain when the project has
		// no valid one.
		in := &ClusterInput{Driver: "lo", Active: !p.ActiveValid()}
		return func() Screen { return ClusterScreen(p.Root, in) }
	case EntryService:
		in := &ServiceInput{}
		return func() Screen { return ServiceScreen(p.Root, in) }
	case EntryTests:
		in := &TestsInput{}
		return func() Screen { return TestsScreen(p.Root, in) }
	case EntryToolchain:
		var groups []string
		return func() Screen { return ToolchainScreen(p.Root, &groups) }
	case EntryActive:
		dom := ""
		return func() Screen { return ActiveScreen(s, &dom) }
	case EntryEject:
		return func() Screen { return EjectScreen(s) }
	}
	return func() Screen { return ImplementationScreen(s) }
}

// WritePlan is `--plan` in project mode: the card, then the list.
func WritePlan(w io.Writer, s State) {
	WriteCard(w, s)
	fmt.Fprintln(w)
	WriteEntries(w, Entries(s), row{key: "next", value: Next(s, false)})
}
