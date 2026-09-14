package initctx

// screen.go: the one screen every write goes through. The screen has
// the card's two-column layout. It shows the values the plan uses, the
// files it writes (`writes`), the commands it runs (`runs`) and, dim and
// last, the flag-twin command lines (`equivalent`). Then it asks one
// thing: Create, Change details, or Cancel. Change details turns the
// values into fields, one per row. The help under a field says what the
// value is and gives an example; it never names a flag. Then the screen
// appears again. --plan prints the rows alone.
//
// The screens: the bootstrap (bare `lo init` without a project or
// without a repository, every value prefilled), a cluster (`lo init
// cluster`), a service (`lo init service`), the test suite (`lo init
// test`), the toolchain, the active domain, the bash tree and the
// implementation switch (project mode). A value given on the command
// line is a fixed row, never a field.

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/huh/v2"

	"github.com/kernpilot/lok8s/internal/scaffold"
	"github.com/kernpilot/lok8s/internal/toolchain"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Screen is what the user sees before a write.
type Screen struct {
	// Title heads the screen ("New project").
	Title string
	// Values are the rows above the writes/runs lines.
	Values []row
	// Plan is what Create executes.
	Plan Plan
	// Details builds the editable fields, bound to the values (nil = no
	// details: every value came from the command line).
	Details func() []huh.Field
	// Incomplete is whether a value the screen needs is still empty: the
	// details open before the screen is shown, and --plan refuses with
	// Missing.
	Incomplete bool
	// Missing is the error line for an incomplete screen off the details
	// (--plan, or a verb off a terminal).
	Missing string
}

// The error lines of a verb without its value, on a terminal and off.
const (
	MissingDomain = "lo init cluster: give a domain: lo init cluster <domain>"
	MissingName   = "lo init service: give a name: lo init service <name>"
)

// rows are the screen's rows: the values, then writes, runs, equivalent.
func (sc Screen) rows() []row {
	rows := slices.Clone(sc.Values)
	rows = append(rows, row{key: "writes", value: joined(sc.Plan.Files())})
	if runs := sc.Plan.Runs(); len(runs) > 0 {
		rows = append(rows, row{key: "runs", value: joined(runs)})
	}
	return append(rows, row{key: "equivalent", value: joined(sc.Plan.Commands()), dim: true})
}

// WriteScreen prints the screen as text: the title, then the rows.
func WriteScreen(w io.Writer, sc Screen, paint ui.Paint) {
	fmt.Fprintf(w, "  %s\n", paint.Bold(sc.Title))
	writeRows(w, sc.rows(), paint, false)
}

// The closing choice of a screen.
const (
	choiceCreate = "create"
	choiceChange = "change"
	choiceCancel = "cancel"
)

// Run shows the screen and asks. Create returns the plan. Change details
// runs the fields and shows the screen again. Cancel returns
// ErrCancelled; Esc and Ctrl-C return ErrAborted. build makes the screen
// from the current values, so the next render shows a changed detail.
// The screen goes to out, the forms to tio.
func Run(out io.Writer, tio IO, paint ui.Paint, build func() Screen) (Plan, error) {
	sc := build()
	if sc.Incomplete && sc.Details != nil {
		if err := run(huh.NewForm(huh.NewGroup(sc.Details()...).Title(sc.Title)), tio); err != nil {
			return Plan{}, err
		}
		sc = build()
	}
	if sc.Incomplete {
		return Plan{}, ErrIncomplete
	}
	for {
		WriteScreen(out, sc, paint)
		fmt.Fprintln(out)
		opts := []huh.Option[string]{huh.NewOption("Create", choiceCreate)}
		if sc.Details != nil {
			opts = append(opts, huh.NewOption("Change details", choiceChange))
		}
		opts = append(opts, huh.NewOption("Cancel", choiceCancel))
		choice := choiceCreate
		if err := run(huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Options(opts...).Value(&choice),
		)), tio); err != nil {
			return Plan{}, err
		}
		switch choice {
		case choiceCreate:
			return sc.Plan, nil
		case choiceCancel:
			return Plan{}, ErrCancelled
		}
		if err := run(huh.NewForm(huh.NewGroup(sc.Details()...).Title(sc.Title)), tio); err != nil {
			return Plan{}, err
		}
		sc = build()
	}
}

// prompt is one field's words: the title, the help under it (what the
// value is, an example) and the placeholder of an empty input. None of
// them names a flag; the flag twins live on the screen's equivalent row.
// TestPromptsCarryNoFlag holds that line.
type prompt struct {
	title, help, placeholder string
}

var prompts = map[string]prompt{
	"name":        {"Name", "The project name, written to lok8s.yaml. Example: shop.", "shop"},
	"directory":   {"Directory", "Where the project files go. Example: . for this directory.", "."},
	"domain":      {"Domain", "The cluster's DNS name. Its files go under clusters/<domain>/.", "kubehz.dev"},
	"driver":      {"Driver", "", ""},
	"environment": {"Environment file", "The file that puts .bin on PATH in this directory.", ""},
	"toolchain":   {"Toolchain", "The pinned tools under .bin: kubectl, kustomize, kind, tilt and the render plugins. The install uses the network.", ""},
	"git":         {"Git", "A new repository in the project directory.", ""},
	"active":      {"Active domain", "The domain every command uses when none is named.", ""},
	"service":     {"Name", "The service directory and its lok8s.yaml.", "api"},
	"path":        {"Path", "The service directory, relative to the project. Example: ./services/api.", ""},
	"tests":       {"Path", "The directory of the test suite. Example: tests.", "tests"},
	"groups":      {"Groups", "core (kubectl, kustomize, the render plugins) is always installed. Select the rest.", ""},
}

// inputField is one text row of the details. An empty answer keeps the
// value the row had: huh's accessible prompt validates the raw line
// before it falls back to the default, so the validator lets an empty
// line through while the row has a value. A required row (required is
// the error text) with no value re-asks on an empty answer; on a
// terminal huh keeps the field focused with the error, in accessible
// mode the prompt repeats. Only the end of the input leaves it empty
// (ErrIncomplete from Run).
func inputField(key string, v *string, required string, validate func(string) error) huh.Field {
	p := prompts[key]
	f := huh.NewInput().Title(p.title).Description(p.help).Placeholder(p.placeholder).Value(v)
	return f.Validate(func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			if required != "" && strings.TrimSpace(*v) == "" {
				return errors.New(required)
			}
			return nil
		}
		if validate != nil {
			return validate(s)
		}
		return nil
	})
}

// selectField is one choice row of the details.
func selectField[T comparable](key string, v *T, opts ...huh.Option[T]) huh.Field {
	p := prompts[key]
	return huh.NewSelect[T]().Title(p.title).Description(p.help).Options(opts...).Value(v)
}

// boolOptions labels a yes/no choice.
func boolOptions(yes, no string) []huh.Option[bool] {
	return []huh.Option[bool]{huh.NewOption(yes, true), huh.NewOption(no, false)}
}

// envOptions are the environment files.
func envOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("mise.toml", "mise"),
		huh.NewOption(".envrc", "direnv"),
		huh.NewOption("none", "none"),
	}
}

// driverOptions are scaffold.Drivers with their one-line descriptions.
func driverOptions() []huh.Option[string] {
	out := make([]huh.Option[string], 0, len(scaffold.Drivers))
	for _, d := range scaffold.Drivers {
		out = append(out, huh.NewOption(d.Label, d.Name))
	}
	return out
}

// driverValue is a driver on a row: "lo · kind on local Docker (dev
// clusters)"; an unknown name as given.
func driverValue(name string) string {
	for _, d := range scaffold.Drivers {
		if d.Name == name {
			return strings.Replace(d.Label, ": ", " · ", 1)
		}
	}
	return name
}

// envValue is an environment file on a row.
func envValue(env string) string {
	switch env {
	case "mise":
		return "mise.toml"
	case "direnv":
		return ".envrc"
	}
	return "none"
}

// validateDir accepts a directory `lo init` can continue in: this one,
// one below it, or one above it (the repository root). Detect walks up
// from the working directory, so a project elsewhere would not be found
// after Create.
func validateDir(cwd, v string) error {
	dir := filepath.Clean(filepath.Join(cwd, v))
	if filepath.IsAbs(v) {
		dir = filepath.Clean(v)
	}
	if dirReachable(cwd, dir) {
		return nil
	}
	return fmt.Errorf("%q is not this directory, one below it, or one above it", v)
}

// dirReachable reports whether dir is cwd, an ancestor of cwd, or below
// cwd.
func dirReachable(cwd, dir string) bool {
	up, err := filepath.Rel(dir, cwd)
	if err == nil && up != ".." && !strings.HasPrefix(up, ".."+string(filepath.Separator)) {
		return true
	}
	down, err := filepath.Rel(cwd, dir)
	return err == nil && down != ".." && !strings.HasPrefix(down, ".."+string(filepath.Separator))
}

// validateDomainOrEmpty accepts an empty answer (no cluster yet).
func validateDomainOrEmpty(v string) error {
	if v == "" {
		return nil
	}
	return scaffold.ValidateDomain(v)
}

// validateName is scaffold.ValidateName with the message returned rather
// than printed.
func validateName(v string) error {
	if err := scaffold.ValidateName(v, io.Discard); err != nil {
		return fmt.Errorf("%q must match ^[a-z0-9][a-z0-9._-]*$", v)
	}
	return nil
}

// NewProjectScreen is the new-project screen for the answers a: the
// values, the plan, the details bound to a.
func NewProjectScreen(s State, a *Answers) Screen {
	plan := Decide(s, *a)
	values := []row{{key: "name", value: plan.Name}}
	if rel := relDir(s.Cwd, plan.Dir); rel != "" {
		values = append(values, row{key: "directory", value: relPath(s.Cwd, plan.Dir)})
	}
	dom := "none"
	if a.Domain != "" {
		dom = a.Domain
	}
	tc := "skip"
	if a.Toolchain {
		tc = fmt.Sprintf("install now · %s · network", plural(toolchain.Count(a.Groups), "tool"))
	}
	values = append(values,
		row{key: "domain", value: dom},
		row{key: "driver", value: driverValue(driverOr(a.Driver))},
		row{key: "environment", value: envValue(a.Env)},
		row{key: "toolchain", value: tc},
	)
	askGit := s.Git.Available && s.Git.Root == ""
	if askGit {
		git := "skip"
		if a.GitInit {
			git = "initialise a repository"
		}
		values = append(values, row{key: "git", value: git})
	}
	details := func() []huh.Field {
		fields := []huh.Field{inputField("name", &a.Name, "give a name", validateName)}
		if s.Situation() == SituationGitBelowRoot {
			fields = append(fields, inputField("directory", &a.Dir, "give a directory (. for this one)", func(v string) error { return validateDir(s.Cwd, v) }))
		}
		fields = append(fields,
			inputField("domain", &a.Domain, "", validateDomainOrEmpty),
			selectField("driver", &a.Driver, driverOptions()...),
			selectField("environment", &a.Env, envOptions()...),
			selectField("toolchain", &a.Toolchain, boolOptions("install now", "skip")...),
		)
		if askGit {
			fields = append(fields, selectField("git", &a.GitInit, boolOptions("initialise a repository", "skip")...))
		}
		return fields
	}
	return Screen{Title: "New project", Values: values, Plan: plan, Details: details}
}

// NewProject is the new-project flow: the screen with the defaults, the
// details on request, the plan on Create.
func NewProject(s State, out io.Writer, tio IO) (Plan, error) {
	a := DefaultAnswers(s)
	return Run(out, tio, ui.Paint(s.Terminal.StdoutTTY), func() Screen {
		a.Name, a.Domain, a.Dir = strings.TrimSpace(a.Name), strings.TrimSpace(a.Domain), strings.TrimSpace(a.Dir)
		return NewProjectScreen(s, &a)
	})
}

// ClusterInput is `lo init cluster`'s values; a *Given flag marks a value
// from the command line (a fixed row).
type ClusterInput struct {
	Domain, Driver string
	Active         bool
	// Force is the verb's --force: an existing spec is replaced.
	Force       bool
	DomainGiven bool
	DriverGiven bool
	ActiveGiven bool
}

// ClusterScreen is the cluster screen for the project at dir.
func ClusterScreen(dir string, in *ClusterInput) Screen {
	in.Domain = strings.TrimSpace(in.Domain)
	plan := ClusterPlan(dir, in.Domain, in.Driver, in.Active, in.Force)
	active := "no"
	if in.Active {
		active = "yes (lo use " + in.Domain + ")"
	}
	sc := Screen{Title: "New cluster", Plan: plan, Incomplete: in.Domain == "", Missing: MissingDomain,
		Values: []row{{key: "domain", value: in.Domain}, {key: "driver", value: driverValue(driverOr(in.Driver))}, {key: "active", value: active}}}
	if in.DomainGiven && in.DriverGiven && in.ActiveGiven {
		return sc
	}
	sc.Details = func() []huh.Field {
		var fields []huh.Field
		if !in.DomainGiven {
			fields = append(fields, inputField("domain", &in.Domain, "give a domain", scaffold.ValidateDomain))
		}
		if !in.DriverGiven {
			fields = append(fields, selectField("driver", &in.Driver, driverOptions()...))
		}
		if !in.ActiveGiven {
			fields = append(fields, selectField("active", &in.Active, boolOptions("yes", "no")...))
		}
		return fields
	}
	return sc
}

// ServiceInput is `lo init service`'s values.
type ServiceInput struct {
	Name, Path string
	NameGiven  bool
	PathGiven  bool
}

// ServiceScreen is the service screen for the project at dir.
func ServiceScreen(dir string, in *ServiceInput) Screen {
	in.Name, in.Path = strings.TrimSpace(in.Name), strings.TrimSpace(in.Path)
	plan := ServicePlan(dir, in.Name, in.Path)
	path := in.Path
	if path == "" {
		path = "./" + in.Name
	}
	sc := Screen{Title: "New service", Plan: plan, Incomplete: in.Name == "", Missing: MissingName,
		Values: []row{{key: "name", value: in.Name}, {key: "path", value: path}}}
	if in.NameGiven && in.PathGiven {
		return sc
	}
	sc.Details = func() []huh.Field {
		var fields []huh.Field
		if !in.NameGiven {
			fields = append(fields, inputField("service", &in.Name, "give a name", validateName))
		}
		if !in.PathGiven {
			fields = append(fields, inputField("path", &in.Path, "", nil))
		}
		return fields
	}
	return sc
}

// ToolchainScreen is the toolchain install screen for the project at
// dir: the groups are the details.
func ToolchainScreen(dir string, groups *[]string) Screen {
	sel := toolchain.DefaultGroups
	if len(*groups) > 0 {
		sel = withCore(*groups)
	}
	return Screen{Title: "Toolchain", Plan: ToolchainPlan(dir, sel),
		Values: []row{{key: "groups", value: strings.Join(sel, " · ")}, {key: "tools", value: plural(toolchain.Count(sel), "tool")}},
		Details: func() []huh.Field {
			p := prompts["groups"]
			return []huh.Field{huh.NewMultiSelect[string]().Title(p.title).Description(p.help).Options(groupOptions(sel)...).Value(groups)}
		}}
}

// withCore prepends the implied core group, in the template's order.
func withCore(groups []string) []string {
	out := []string{toolchain.GroupCore}
	for _, g := range []string{toolchain.GroupLocal, toolchain.GroupCloud, toolchain.GroupBash} {
		if slices.Contains(groups, g) {
			out = append(out, g)
		}
	}
	return out
}

// groupOptions are the optional toolchain groups, the selected ones
// checked (core is implied).
func groupOptions(selected []string) []huh.Option[string] {
	var out []huh.Option[string]
	for _, g := range []struct{ label, name string }{
		{"local: kind, tilt, mkcert", toolchain.GroupLocal},
		{"cloud: kubeone, hcloud", toolchain.GroupCloud},
		{"bash: argsh, yq, jq, envsubst, sops, ssh-to-age", toolchain.GroupBash},
	} {
		out = append(out, huh.NewOption(g.label, g.name).Selected(slices.Contains(selected, g.name)))
	}
	return out
}

// ActiveScreen is the active-domain screen: the domain is chosen first.
func ActiveScreen(s State, dom *string) Screen {
	p := s.Project
	sc := Screen{Title: "Active domain", Plan: UsePlan(p.Root, *dom), Incomplete: *dom == "", Missing: "lo use: give a domain: lo use <domain>",
		Values: []row{{key: "domain", value: *dom}}}
	sc.Details = func() []huh.Field {
		opts := make([]huh.Option[string], 0, len(p.Domains))
		for _, d := range p.Domains {
			label := d.Name + " (" + driverLabel(d.Kind) + ")"
			opts = append(opts, huh.NewOption(label, d.Name).Selected(d.Name == p.Active))
		}
		return []huh.Field{selectField("active", dom, opts...)}
	}
	return sc
}

// EjectScreen is the bash tree screen: nothing to change.
func EjectScreen(s State) Screen {
	return Screen{Title: "Bash tree", Plan: EjectPlan(s.Project.Root)}
}

// ImplementationScreen switches spec.implementation.default to the other
// implementation: nothing to change.
func ImplementationScreen(s State) Screen {
	p := s.Project
	other := "bash"
	if p.Implementation == "bash" {
		other = "go"
	}
	return Screen{Title: "Implementation", Plan: ImplementationPlan(p.Root, projectName(s), other),
		Values: []row{{key: "implementation", value: other + " (now " + p.Implementation + ")"}}}
}

// TestsInput is `lo init test`'s values.
type TestsInput struct {
	Path      string
	PathGiven bool
}

// TestsScreen is the test-suite screen for the project at dir.
func TestsScreen(dir string, in *TestsInput) Screen {
	in.Path = strings.TrimSpace(in.Path)
	path := in.Path
	if path == "" {
		path = "tests"
	}
	sc := Screen{Title: "Test suite", Plan: TestsPlan(dir, in.Path), Values: []row{{key: "path", value: path}}}
	if in.PathGiven {
		return sc
	}
	sc.Details = func() []huh.Field { return []huh.Field{inputField("tests", &in.Path, "", nil)} }
	return sc
}
