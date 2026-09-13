package initctx

// card.go — the state card and the plan summary, plain text: what `lo
// init` saw, what it would write, and the commands a CI user runs
// instead. The cli appends the doctor sections to the card inside a
// project.

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// WriteCard prints the state card.
func WriteCard(w io.Writer, s State) {
	fmt.Fprintf(w, "lo init — %s\n", s.Cwd)
	fmt.Fprintf(w, "  situation: %s\n", s.Situation())
	fmt.Fprintf(w, "  git: %s\n", gitLine(s))
	if s.Project == nil {
		switch {
		case s.Empty:
			fmt.Fprintln(w, "  directory: empty")
		case s.ServiceDir:
			fmt.Fprintf(w, "  directory: %d entries, a service directory (lok8s.yaml), no project above\n", s.Entries)
		default:
			fmt.Fprintf(w, "  directory: %d entries, no project marker (clusters/ or a kind: Project lok8s.yaml) here or above\n", s.Entries)
		}
		return
	}
	p := s.Project
	where := "you are at the root"
	switch {
	case s.ServiceDir && !p.AtRoot:
		where = "service directory " + s.ServiceName() + " (" + relOrDot(p.Root, s.Cwd) + ")"
	case !p.AtRoot:
		where = relOrDot(p.Root, s.Cwd) + " below the root"
	}
	if s.Git.Submodule && s.Git.Root != p.Root {
		where += ", a submodule under the umbrella project"
	}
	name := p.Name
	if name == "" {
		name = filepath.Base(p.Root) + " (no lok8s.yaml; marked by clusters/)"
	}
	fmt.Fprintf(w, "  project: %s at %s; %s\n", name, p.Root, where)
	fmt.Fprintf(w, "  clusters: %s\n", clustersLine(p))
	env := "none (lo init project --env mise|direnv)"
	switch p.EnvFile {
	case "mise":
		env = "mise.toml"
	case "direnv":
		env = ".envrc"
	}
	fmt.Fprintf(w, "  environment file: %s\n", env)
	fmt.Fprintf(w, "  toolchain: %s\n", toolchainLine(p))
	tree := ".lok8s/lo absent (lo assets eject bash)"
	if p.BashTree {
		tree = ".lok8s/lo present"
	}
	impl := p.Implementation
	if p.ImplementationErr != "" {
		impl = "invalid (" + p.ImplementationErr + ")"
	}
	fmt.Fprintf(w, "  implementation: %s; bash tree %s\n", impl, tree)
	extras := []string{}
	if p.Services {
		extras = append(extras, "services.yaml")
	}
	if p.Tests {
		extras = append(extras, "tests/")
	}
	if len(extras) > 0 {
		fmt.Fprintf(w, "  also: %s\n", strings.Join(extras, ", "))
	}
}

func gitLine(s State) string {
	switch {
	case !s.Git.Available:
		return "not installed"
	case s.Git.Root == "":
		return "not a repository"
	}
	line := "repository at " + s.Git.Root
	if !s.Git.AtRoot {
		line = "repository at " + s.Git.Root + " (you are " + relOrDot(s.Git.Root, s.Cwd) + " below it)"
	}
	if s.Git.Submodule {
		line += ", a submodule or worktree"
	}
	if s.Git.Dirty {
		line += ", uncommitted changes"
	} else {
		line += ", clean"
	}
	return line
}

func clustersLine(p *Project) string {
	if !p.Clusters {
		return "no clusters/ directory"
	}
	if len(p.Domains) == 0 {
		return "clusters/ is empty (lo init project --cluster <domain>)"
	}
	names := make([]string, 0, len(p.Domains))
	for _, d := range p.Domains {
		names = append(names, d.Name+" ("+d.Kind+")")
	}
	line := strings.Join(names, ", ")
	if p.Active != "" {
		return line + "; active " + p.Active
	}
	return line + "; no active domain (lo use <domain>)"
}

func toolchainLine(p *Project) string {
	switch {
	case !p.BYAML:
		return "no .bin/b.yaml (lo toolchain install)"
	case len(p.ToolsMissing) == 0 && p.BYAMLMarker:
		return ".bin/b.yaml (lo toolchain install), every pinned tool present"
	case len(p.ToolsMissing) == 0:
		return ".bin/b.yaml (not written by lo toolchain install), every pinned tool present"
	}
	return ".bin/b.yaml present; missing: " + strings.Join(p.ToolsMissing, ", ") + " (lo toolchain install)"
}

// relOrDot is path relative to base, "." when equal.
func relOrDot(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "" {
		return path
	}
	return rel
}

// WriteSummary prints the plan: the steps, then the equivalent commands
// (run from Plan.Dir; a `cd` line names it when it is not cwd).
func WriteSummary(w io.Writer, p Plan, cwd string) {
	if len(p.Actions) == 0 {
		fmt.Fprintln(w, "Nothing to do.")
		return
	}
	fmt.Fprintf(w, "Plan (%s):\n", p.Situation)
	for i, a := range p.Actions {
		fmt.Fprintf(w, "  %d. %s: %s\n", i+1, a.Kind, a.Summary)
	}
	fmt.Fprintln(w, "Equivalent commands:")
	if rel := relDir(cwd, p.Dir); rel != "" {
		fmt.Fprintf(w, "  cd %s\n", rel)
	}
	for _, c := range p.Commands() {
		fmt.Fprintf(w, "  %s\n", c)
	}
}

// WriteOptions prints the menu of an existing project as hints (the
// `--plan` output when there is nothing to do).
func WriteOptions(w io.Writer, s State) {
	opts := Options(s)
	if len(opts) == 0 {
		return
	}
	fmt.Fprintln(w, "Available (lo init on a terminal asks; or run the command):")
	for _, o := range opts {
		fmt.Fprintf(w, "  %-52s %s\n", o.Label, o.Command)
	}
}
