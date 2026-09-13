package initctx

// form.go — the huh forms of the wizard, one group per step. The layer is
// thin: State in, Answers out; Decide and the cli's executor do the rest.
// accessible selects huh's line-driven mode (the tests script it over an
// io.Reader; a terminal gets the interactive forms). The base16 theme,
// no custom styling.

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/huh/v2"

	"github.com/kernpilot/lok8s/internal/scaffold"
)

// ErrAborted is returned when the user leaves the form (Ctrl-C, Esc).
var ErrAborted = errors.New("lo init: aborted")

// IO is where a form reads and writes.
type IO struct {
	In  io.Reader
	Out io.Writer
	// Accessible runs the fields as line prompts instead of the
	// interactive forms (off a TTY; the tests).
	Accessible bool
}

// Ask runs the conversation for s and returns the answers. Nothing is
// written: the caller decides (Decide), prints the summary and asks
// Confirm before it executes anything.
func Ask(s State, tio IO) (Answers, error) {
	switch s.Situation() {
	case SituationProjectRoot, SituationInsideProject:
		return askExisting(s, tio)
	}
	return askNew(s, tio)
}

// Confirm is the summary step's question; network says whether a step
// needs the network (the question mentions it).
func Confirm(tio IO, network bool) (bool, error) {
	title := "Write these files and run these commands?"
	if network {
		title = "Write these files and run these commands (the toolchain step uses the network)?"
	}
	ok := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&ok),
	))
	if err := run(form, tio); err != nil {
		return false, err
	}
	return ok, nil
}

// run applies the shared options and maps huh's abort.
func run(form *huh.Form, tio IO) error {
	form = form.WithTheme(huh.ThemeFunc(huh.ThemeBase16)).WithAccessible(tio.Accessible)
	if tio.In != nil {
		form = form.WithInput(tio.In)
	}
	if tio.Out != nil {
		form = form.WithOutput(tio.Out)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrAborted
		}
		return err
	}
	return nil
}

// envOptions are the --env values with a label each.
func envOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("mise (mise.toml)", "mise"),
		huh.NewOption("direnv (.envrc)", "direnv"),
		huh.NewOption("none (CI, or a shell of your own)", "none"),
	}
}

// driverOptions are scaffold.Drivers as select options.
func driverOptions() []huh.Option[string] {
	out := make([]huh.Option[string], 0, len(scaffold.Drivers))
	for _, d := range scaffold.Drivers {
		out = append(out, huh.NewOption(d.Label, d.Name))
	}
	return out
}

// groupOptions are the optional toolchain groups (core is implied).
func groupOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("local: kind, tilt, mkcert (the Lo driver)", "local").Selected(true),
		huh.NewOption("cloud: kubeone, hcloud", "cloud"),
		huh.NewOption("bash: argsh, yq, jq, envsubst, sops, ssh-to-age (the bash implementation, the provider plugins)", "bash"),
	}
}

// validateDomainOrEmpty accepts an empty answer (no cluster yet).
func validateDomainOrEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return scaffold.ValidateDomain(strings.TrimSpace(v))
}

// validateName is scaffold.ValidateName with the message returned rather
// than printed.
func validateName(v string) error {
	if err := scaffold.ValidateName(strings.TrimSpace(v), io.Discard); err != nil {
		return fmt.Errorf("must match ^[a-z0-9][a-z0-9._-]*$")
	}
	return nil
}

// askNew is the welcome conversation (empty, bare, git below root).
func askNew(s State, tio IO) (Answers, error) {
	defaultDir := DefaultDir(s)
	dir := shortDir(s.Cwd, defaultDir)
	name := filepath.Base(defaultDir)
	env := DefaultEnv(s, defaultDir)
	gitInit := true
	domain := ""
	driver := "lo"
	toolchain := true
	groups := []string{"local"}

	dirDesc := "The working directory (.) or a subdirectory; the project files go there."
	if s.Situation() == SituationGitBelowRoot {
		dirDesc = "You stand below the git root; the project usually lives at the root."
	}
	project := []huh.Field{
		huh.NewInput().Title("Project directory").Description(dirDesc).Value(&dir).
			Validate(func(v string) error {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("a directory is required (. for here)")
				}
				return nil
			}),
		huh.NewInput().Title("Project name").Description("metadata.name of lok8s.yaml (lo init project <name>).").Value(&name).Validate(validateName),
		huh.NewSelect[string]().Title("Environment file").Description("Puts .bin on PATH; nothing else (lo init project --env).").Options(envOptions()...).Value(&env),
	}
	if s.Git.Available && s.Git.Root == "" {
		project = append(project, huh.NewConfirm().Title("Run git init?").Value(&gitInit))
	}
	groupsOf := []*huh.Group{
		huh.NewGroup(project...).Title("Project"),
		huh.NewGroup(
			huh.NewInput().Title("First cluster domain").Description("clusters/<domain>/cluster.lok8s.yaml; leave empty to add one later (lo init project --cluster).").Value(&domain).Validate(validateDomainOrEmpty),
			huh.NewSelect[string]().Title("Driver").Description("lo init project --driver").Options(driverOptions()...).Value(&driver),
		).Title("First cluster"),
		huh.NewGroup(
			huh.NewConfirm().Title("Install the pinned toolchain now?").Description("lo toolchain install: .bin/b.yaml, b and the tools into .bin/ (network).").Value(&toolchain),
			huh.NewMultiSelect[string]().Title("Toolchain groups").Description("core is always on (lo toolchain install --groups).").Options(groupOptions()...).Value(&groups),
		).Title("Toolchain"),
	}
	if err := run(huh.NewForm(groupsOf...), tio); err != nil {
		return Answers{}, err
	}
	a := Answers{
		Dir:       strings.TrimSpace(dir),
		Name:      strings.TrimSpace(name),
		Env:       env,
		GitInit:   gitInit,
		Domain:    strings.TrimSpace(domain),
		Driver:    driver,
		Use:       strings.TrimSpace(domain) != "",
		Toolchain: toolchain,
		Groups:    withCore(groups),
	}
	if a.Dir == "." {
		a.Dir = s.Cwd
	}
	return a, nil
}

// askExisting is the project conversation: the menu, then the details of
// what was chosen.
func askExisting(s State, tio IO) (Answers, error) {
	opts := Options(s)
	menu := make([]huh.Option[string], 0, len(opts))
	for _, o := range opts {
		menu = append(menu, huh.NewOption(o.Label, o.Key))
	}
	var chosen []string
	if err := run(huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("What do you want to add?").Description("Nothing selected: the card only.").Options(menu...).Value(&chosen),
	).Title("Project")), tio); err != nil {
		return Answers{}, err
	}
	a := Answers{}
	has := func(key string) bool { return slices.Contains(chosen, key) }
	a.Tests = has("tests")
	a.Register = has("register")
	if has("implementation") {
		a.Implementation = "bash"
		if s.Project.Implementation == "bash" {
			a.Implementation = "go"
		}
	}

	domain, driver, service, env := "", "lo", "", "mise"
	use := s.Project.Active == ""
	groups := []string{"local"}
	var details []*huh.Group
	if has("cluster") {
		details = append(details, huh.NewGroup(
			huh.NewInput().Title("Cluster domain").Description("clusters/<domain>/cluster.lok8s.yaml (lo init project --cluster).").Value(&domain).
				Validate(func(v string) error { return scaffold.ValidateDomain(strings.TrimSpace(v)) }),
			huh.NewSelect[string]().Title("Driver").Description("lo init project --driver").Options(driverOptions()...).Value(&driver),
			huh.NewConfirm().Title("Make it the active domain?").Description("lo use <domain>").Value(&use),
		).Title("Cluster"))
	}
	if has("service") {
		details = append(details, huh.NewGroup(
			huh.NewInput().Title("Service name").Description("./<name>/lok8s.yaml, registered in services.yaml (lo init service <name>).").Value(&service).Validate(validateName),
		).Title("Service"))
	}
	if has("env") {
		details = append(details, huh.NewGroup(
			huh.NewSelect[string]().Title("Environment file").Description("lo init project --env").Options(envOptions()[:2]...).Value(&env),
		).Title("Environment"))
	}
	if has("toolchain") {
		details = append(details, huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Toolchain groups").Description("core is always on (lo toolchain install --groups).").Options(groupOptions()...).Value(&groups),
		).Title("Toolchain"))
	}
	if len(details) > 0 {
		if err := run(huh.NewForm(details...), tio); err != nil {
			return Answers{}, err
		}
	}
	if has("cluster") {
		a.Domain, a.Driver, a.Use = strings.TrimSpace(domain), driver, use
	}
	if has("service") {
		a.Service = strings.TrimSpace(service)
	}
	if has("env") {
		a.Env = env
	}
	if has("toolchain") {
		a.Toolchain, a.Groups = true, withCore(groups)
	}
	return a, nil
}

// withCore prepends the implied core group, in the template's order.
func withCore(groups []string) []string {
	out := []string{"core"}
	for _, g := range []string{"local", "cloud", "bash"} {
		if slices.Contains(groups, g) {
			out = append(out, g)
		}
	}
	return out
}
