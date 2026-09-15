package initctx

// card.go: the state card of a project, and the row layout the screens
// share.
//
// The card has the two-column layout of the run header (`lo up`): a
// lowercase key, two spaces past the longest key, the values joined with
// ` · `. The first key is the project name. The card uses one path form:
// the working directory as the user typed it. It prints a path in two
// cases only: the position below the project root, and a repository
// root elsewhere. A row carries facts only; a row the user can act on
// starts with `!`, and the command lives in the list's `equivalent` row
// and in `--plan`. The style is the writer's (internal/ui): colour on
// a terminal that allows it, plain in a pipe.

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kernpilot/lok8s/internal/ui"
)

// row is one line of the card or a screen.
type row struct {
	key, value string
	// warn marks a row the user can act on (the `!` marker); dim mutes
	// the value (the equivalent command lines).
	warn, dim bool
}

// WriteCard prints the state card of s.Project (nothing outside a
// project); extra rows (the result of the last action) follow it in the
// same columns.
func WriteCard(w io.Writer, s State, extra ...row) {
	if s.Project == nil {
		return
	}
	writeRows(w, append(projectRows(s), extra...), true)
}

// WriteNext prints the `next` line: the step the state calls for.
func WriteNext(w io.Writer, next string) {
	writeRows(w, []row{{key: "next", value: next}}, false)
}

// writeRows prints rows in the two-column layout. heading makes the
// first key bold (the name); the other keys are dim. The style is the
// writer's (internal/ui: a terminal that allows colour, else plain).
func writeRows(w io.Writer, rows []row, heading bool) {
	st := ui.For(w)
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.key))
	}
	for i, r := range rows {
		mark := "  "
		if r.warn {
			mark = st.Warn("!") + " "
		}
		key := st.Dim(r.key)
		if heading && i == 0 {
			key = st.Bold(r.key)
		}
		value := r.value
		if r.dim {
			value = st.Dim(value)
		}
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(r.key)+2)
		fmt.Fprintf(w, "%s%s%s%s\n", mark, key, pad, value)
	}
}

// projectRows is the card.
func projectRows(s State) []row {
	p := s.Project
	name := projectName(s)
	first := []string{"project"}
	if !p.ProjectFile {
		first = append(first, "no lok8s.yaml")
	}
	warn := false
	if p.ImplementationErr != "" {
		first = append(first, "invalid implementation ("+p.ImplementationErr+")")
		warn = true
	} else {
		first = append(first, p.Implementation)
	}
	gitAtRoot := s.Git.Root == "" || samePath(s.Git.Root, p.Root)
	if gitAtRoot {
		first = append(first, gitValues(s.Git)...)
	}
	rows := []row{{key: name, value: joined(first), warn: warn}}
	if !p.AtRoot {
		dir := []string{relPath(p.Root, s.Cwd)}
		if s.ServiceDir {
			dir = append(dir, "service directory")
		}
		rows = append(rows, row{key: "directory", value: joined(dir)})
	}
	if !gitAtRoot {
		repo := relPath(p.Root, s.Git.Root)
		if strings.HasPrefix(repo, "..") {
			repo = relPath(s.Cwd, s.Git.Root)
		}
		rows = append(rows, row{key: "repository", value: joined(append([]string{repo}, gitValues(s.Git)...))})
	}
	clusters, warnClusters := clustersValue(p)
	rows = append(rows, row{key: "clusters", value: clusters, warn: warnClusters})
	tools, warnTools := toolchainValue(p)
	rows = append(rows, row{key: "toolchain", value: tools, warn: warnTools})
	env, warnEnv := environmentValue(p)
	rows = append(rows, row{key: "environment", value: env, warn: warnEnv})
	if p.Services {
		rows = append(rows, row{key: "services", value: "services.yaml"})
	}
	if p.Tests {
		rows = append(rows, row{key: "tests", value: "tests/"})
	}
	return rows
}

// gitValues is the repository segment: the branch with the uncommitted
// count, or why there is none.
func gitValues(g Git) []string {
	switch {
	case !g.Available:
		return []string{"no git"}
	case g.Root == "":
		return []string{"no git repository"}
	}
	branch := g.Branch
	if branch == "" {
		branch = "detached HEAD"
	}
	if g.Uncommitted > 0 {
		branch += fmt.Sprintf(", %d uncommitted", g.Uncommitted)
	}
	out := []string{branch}
	if g.Submodule {
		out = append(out, "submodule or worktree")
	}
	return out
}

// clustersValue lists the domains grouped by driver, the active one
// first; the row warns without a cluster or without a valid active
// domain.
func clustersValue(p *Project) (string, bool) {
	switch {
	case !p.Clusters:
		return "no clusters/ directory", true
	case len(p.Domains) == 0:
		return "none", true
	}
	var (
		parts  []string
		order  []string
		byKind = map[string][]string{}
	)
	for _, d := range p.Domains {
		if d.Name == p.Active {
			parts = append(parts, d.Name+" ("+driverLabel(d.Kind)+", active)")
			continue
		}
		if _, ok := byKind[d.Kind]; !ok {
			order = append(order, d.Kind)
		}
		byKind[d.Kind] = append(byKind[d.Kind], d.Name)
	}
	for _, k := range order {
		parts = append(parts, strings.Join(byKind[k], ", ")+" ("+driverLabel(k)+")")
	}
	switch {
	case p.Active == "":
		return joined(append(parts, "none active")), true
	case !p.ActiveValid():
		return joined(append(parts, "active "+p.Active+" has no spec")), true
	}
	return joined(parts), false
}

// driverLabel names a driver as the card shows it: the lo driver is a
// kind cluster; an unreadable spec says so.
func driverLabel(kind string) string {
	switch kind {
	case "lo":
		return "kind"
	case "?":
		return "unreadable spec"
	}
	return kind
}

// toolchainValue counts the pinned tools; the row warns without a pin
// file, with an unreadable one, or with a missing tool.
func toolchainValue(p *Project) (string, bool) {
	switch {
	case !p.BYAML:
		return "none", true
	case p.BYAMLInvalid:
		return ".bin/b.yaml unreadable", true
	case len(p.ToolsMissing) == 0:
		return fmt.Sprintf(".bin/b.yaml · %s present", plural(p.Tools, "tool")), false
	}
	return fmt.Sprintf(".bin/b.yaml · %d of %d missing", len(p.ToolsMissing), p.Tools), true
}

// environmentValue is the environment file and the bash tree; the row
// warns without a file, or with a bash implementation and no tree.
func environmentValue(p *Project) (string, bool) {
	var parts []string
	warn := false
	switch p.EnvFile {
	case "mise":
		parts = append(parts, "mise.toml")
	case "direnv":
		parts = append(parts, ".envrc")
	default:
		parts = append(parts, "none")
		warn = true
	}
	switch {
	case p.BashTree:
		parts = append(parts, "bash tree present")
	case p.Implementation == "bash":
		parts = append(parts, "bash tree missing")
		warn = true
	}
	return joined(parts), warn
}

func joined(parts []string) string { return strings.Join(parts, " · ") }

// projectName is the name the card uses: metadata.name, else the root
// directory's name.
func projectName(s State) string {
	if s.Project == nil {
		return filepath.Base(s.Cwd)
	}
	if s.Project.Name != "" {
		return s.Project.Name
	}
	return filepath.Base(s.Project.Root)
}

// plural is "1 tool", "3 tools", "1 entry", "3 entries".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if stem, ok := strings.CutSuffix(noun, "y"); ok {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// relPath is path relative to base; the path itself when the two cannot
// be related.
func relPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "" {
		return path
	}
	return rel
}
