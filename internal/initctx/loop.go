package initctx

// loop.go — project mode: the card, the result of the last action under
// it, and one choice from what the state allows. An entry opens its
// action screen (screen.go); when the action is done the state is read
// again and the card is printed again. Exit and Ctrl-C leave. Nothing is
// written except on a screen's Create, so leaving never leaves a
// half-written state. The rendering is a clean print per iteration: the
// card and the list are appended, the forms draw inline and clear
// themselves.

import (
	"errors"
	"fmt"
	"io"

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

// Entries lists what the project state allows, in the order the list
// shows it: a cluster, a service, the test suite when there is none,
// the toolchain when a pinned tool is missing, the active domain with
// several clusters, the bash tree when absent or the implementation
// switch when present, then Exit.
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
	case len(p.ToolsMissing) > 0:
		out = append(out, Entry{EntryToolchain, fmt.Sprintf("Install the toolchain (%d of %d missing)", len(p.ToolsMissing), p.Tools), "lo toolchain install"})
	}
	if len(p.Domains) > 1 {
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
func WriteEntries(w io.Writer, entries []Entry, paint ui.Paint, extra ...row) {
	labels, twins := []string{}, []string{}
	for _, e := range entries {
		labels = append(labels, e.Label)
		if e.Twin != "" {
			twins = append(twins, e.Twin)
		}
	}
	rows := []row{{key: "actions", value: joined(labels)}, {key: "equivalent", value: joined(twins), dim: true}}
	writeRows(w, append(rows, extra...), paint, false)
}

// Welcome is the one line mode 1 opens with: what `lo init` is about to
// set up here.
func Welcome(w io.Writer, s State, paint ui.Paint) {
	what := "a lok8s project in this directory"
	if s.Situation() == SituationGitBelowRoot {
		what = "a lok8s project at the repository root"
	}
	fmt.Fprintf(w, "  %s\n", paint.Bold("lo init sets up "+what+": the project file, the first cluster spec, the toolchain."))
}

// Loop is project mode. Detect reads the state again after an action;
// Execute runs a plan (the cli's executor over the subcommands' own
// functions).
type Loop struct {
	Out     io.Writer
	IO      IO
	Detect  func() (State, error)
	Execute func(Plan) error
}

// Run prints the card and the list until Exit or Ctrl-C. first is the
// plan that ran before the loop (the bootstrap; nil = none): its result
// goes under the first card. A cancelled action returns to the list; a
// failed action ends the run with its error (the executor printed what
// was not run).
func (l Loop) Run(s State, first *Plan) error {
	paint := ui.Paint(s.Terminal.StdoutTTY)
	var result *row
	if first != nil {
		verb, what := first.Result()
		result = &row{key: verb, value: what}
	}
	for {
		if result != nil {
			WriteCard(l.Out, s, *result)
		} else {
			WriteCard(l.Out, s)
		}
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
			if errors.Is(err, ErrAborted) {
				return nil
			}
			return err
		}
		if choice == EntryExit {
			WriteNext(l.Out, Next(s, false), paint)
			return nil
		}
		plan, err := Run(l.Out, l.IO, paint, l.actionScreen(s, choice))
		switch {
		case errors.Is(err, ErrCancelled):
			result = &row{key: "cancelled", value: "nothing written"}
			continue
		case errors.Is(err, ErrAborted), errors.Is(err, ErrIncomplete):
			// Left, or nothing more to read: leave, nothing written.
			return nil
		case err != nil:
			return err
		}
		if err := l.Execute(plan); err != nil {
			return err
		}
		verb, what := plan.Result()
		result = &row{key: verb, value: what}
		if s, err = l.Detect(); err != nil {
			return err
		}
	}
}

// actionScreen is the screen builder behind one list entry.
func (l Loop) actionScreen(s State, key string) func() Screen {
	p := s.Project
	switch key {
	case EntryCluster:
		in := &ClusterInput{Driver: "lo", Active: p.Active == "" || len(p.Domains) == 0}
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
	WriteEntries(w, Entries(s), ui.Paint(s.Terminal.StdoutTTY), row{key: "next", value: Next(s, false)})
}
