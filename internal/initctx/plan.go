package initctx

// plan.go: the decision layer. It holds the defaults a situation
// suggests, the ordered actions a set of answers makes, and the `next`
// step a project state calls for. Each action carries the files it
// writes, the command it runs and the flag-twin command line a script
// runs instead. Nothing here writes the filesystem; the one read is the
// existence check for a cluster spec.

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// Answers is what the new-project screen holds (the defaults, or the
// details the user changed). Every field has a flag twin on `lo init
// project`, `git init`, `lo toolchain install` or `lo use`.
type Answers struct {
	// Dir is the project directory ("" = the situation's default: the
	// working directory, or the git root below which the user stands).
	Dir string
	// Name is metadata.name ("" = the directory name).
	Name string
	// Env is the environment file: mise, direnv or none ("" = the one the
	// directory already has, else mise).
	Env string
	// GitInit asks for `git init` (honoured only without a repository).
	GitInit bool
	// Domain and Driver describe the first cluster spec; "" = none.
	Domain, Driver string
	// Use makes Domain the active domain (`lo use`).
	Use bool
	// Toolchain runs `lo toolchain install` with Groups (nil = the
	// default groups).
	Toolchain bool
	Groups    []string
}

// ActionKind names one action of a Plan.
type ActionKind string

const (
	// ActionWriteProjectFiles — clusters/, lok8s.yaml, the .gitignore
	// entries and (with Env) the environment file, plus (with Domain) the
	// first cluster spec: one `lo init project` run.
	ActionWriteProjectFiles ActionKind = "project files"
	// ActionGitInit — `git init` in the project directory.
	ActionGitInit ActionKind = "git init"
	// ActionWriteClusterSpec — clusters/<domain>/cluster.lok8s.yaml into
	// an existing project.
	ActionWriteClusterSpec ActionKind = "cluster spec"
	// ActionToolchainInstall — `lo toolchain install` (network).
	ActionToolchainInstall ActionKind = "toolchain"
	// ActionUse — `lo use <domain>`.
	ActionUse ActionKind = "use"
	// ActionAddService — `lo init service <name>`.
	ActionAddService ActionKind = "service"
	// ActionAddTests — `lo init test`.
	ActionAddTests ActionKind = "tests"
	// ActionEjectBash — `lo assets eject bash`.
	ActionEjectBash ActionKind = "eject bash"
	// ActionSetImplementation — spec.implementation.default in lok8s.yaml.
	ActionSetImplementation ActionKind = "implementation"
)

// Action is one step of a Plan.
type Action struct {
	Kind ActionKind
	// Files lists what the step writes (relative to Plan.Dir); Run names
	// what it runs, in the words of the screen ("" = nothing).
	Files []string
	Run   string
	// Command is the flag-twin command line, run from Plan.Dir.
	Command string
	// Network is whether the step needs the network.
	Network bool
	// The parameters the executor hands to the functions behind Command.
	Name, Env, Domain, Driver, Service, Path, Implementation string
	Groups                                                   []string
}

// Plan is the ordered list of actions for one `lo init` run.
type Plan struct {
	// Dir is the project directory every action runs in (absolute).
	Dir string
	// Name is the project name the actions use.
	Name string
	// Force overwrites existing files (a verb's --force; a screen never
	// sets it).
	Force bool
	// Actions in execution order; empty = nothing to do.
	Actions []Action
}

// Commands lists the flag-twin command lines in order.
func (p Plan) Commands() []string {
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, a.Command)
	}
	return out
}

// StepError is a failed action of a plan: the command, the cause and the
// commands that did not run.
type StepError struct {
	Command string
	Err     error
	NotRun  []string
}

func (e *StepError) Error() string { return e.Command + ": " + e.Err.Error() }

func (e *StepError) Unwrap() error { return e.Err }

// Files lists every file the plan writes, in order.
func (p Plan) Files() []string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, a.Files...)
	}
	return out
}

// Runs lists every command the plan runs, in the screen's words.
func (p Plan) Runs() []string {
	var out []string
	for _, a := range p.Actions {
		if a.Run != "" {
			out = append(out, a.Run)
		}
	}
	return out
}

// Network reports whether any action needs the network.
func (p Plan) Network() bool {
	for _, a := range p.Actions {
		if a.Network {
			return true
		}
	}
	return false
}

// Bootstrap reports whether the state calls for the bootstrap screen
// (mode 1): no project here, or a project without a git repository
// (git installed, no repository). Everything else is project mode.
func Bootstrap(s State) bool {
	return s.Project == nil || (s.Git.Available && s.Git.Root == "")
}

// DefaultAnswers are the bootstrap defaults the screen shows first. For
// a new project: the files where the situation suggests; the name from
// the directory; the first cluster `<name>.dev` on the lo driver, made
// active; the environment file the directory has, else mise; the
// toolchain; `git init` when git exists and there is no repository. For
// a project without a repository: its name and root; a first cluster
// only when it has none; the toolchain only when a pin is missing; `git
// init`.
func DefaultAnswers(s State) Answers {
	dir := DefaultDir(s)
	a := Answers{
		Dir:       dir,
		Name:      DefaultName(dir),
		Driver:    "lo",
		Env:       DefaultEnv(s, dir),
		GitInit:   s.Git.Available && s.Git.Root == "",
		Toolchain: true,
	}
	if p := s.Project; p != nil {
		a.Name = projectName(s)
		a.Toolchain = p.ToolchainMissing()
		if len(p.Domains) > 0 {
			return a
		}
	}
	a.Domain, a.Use = a.Name+".dev", true
	return a
}

// DefaultDir is the project directory the bootstrap defaults to: the
// project's root inside one, the git root when the user stands below it,
// else the working directory.
func DefaultDir(s State) string {
	if s.Project != nil {
		return s.Project.Root
	}
	if s.Situation() == SituationGitBelowRoot {
		return s.Git.Root
	}
	return s.Cwd
}

var (
	nameBad   = regexp.MustCompile(`[^a-z0-9._-]+`)
	nameStart = regexp.MustCompile(`^[^a-z0-9]+`)
)

// DefaultName is the project name a directory suggests: its base name,
// lowercased, every run of other characters one `-`, "project" when
// nothing is left.
func DefaultName(dir string) string {
	name := strings.ToLower(filepath.Base(dir))
	name = nameBad.ReplaceAllString(name, "-")
	name = nameStart.ReplaceAllString(name, "")
	name = strings.TrimRight(name, "-")
	if name == "" {
		return "project"
	}
	return name
}

// DefaultEnv is the environment file a directory suggests: the one it
// already has, else mise.
func DefaultEnv(s State, dir string) string {
	if s.Project != nil && s.Project.EnvFile != "" {
		return s.Project.EnvFile
	}
	if env := existingEnv(dir); env != "" {
		return env
	}
	return "mise"
}

// Decide turns the state and the answers into the new-project plan: the
// project files (with the first cluster spec), `git init`, the toolchain,
// `lo use`. A `git init` inside a repository is ignored.
func Decide(s State, a Answers) Plan {
	dir := a.Dir
	if dir == "" {
		dir = DefaultDir(s)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.Cwd, dir)
	}
	dir = filepath.Clean(dir)
	name := a.Name
	if name == "" {
		name = DefaultName(dir)
	}
	env := a.Env
	if env == "" {
		env = DefaultEnv(s, dir)
	}
	p := Plan{Dir: dir, Name: name}

	// The files `lo init project` writes; inside an existing project only
	// the ones it lacks (the scaffold keeps every existing file).
	files := []string{}
	proj := s.Project
	if proj == nil || !proj.Clusters {
		files = append(files, "clusters/")
	}
	if proj == nil || !proj.ProjectFile {
		files = append(files, "lok8s.yaml")
	}
	files = append(files, ".gitignore entries")
	cmd := "lo init project " + name + " --env " + env
	if rel := relDir(s.Cwd, dir); rel != "" {
		cmd += " --path " + rel
	}
	if proj == nil || proj.EnvFile == "" {
		switch env {
		case "mise":
			files = append(files, "mise.toml")
		case "direnv":
			files = append(files, ".envrc")
		}
	}
	action := Action{Kind: ActionWriteProjectFiles, Name: name, Env: env}
	if a.Domain != "" {
		action.Domain, action.Driver = a.Domain, driverOr(a.Driver)
		files = append(files, clusterSpecFile(dir, a.Domain))
		cmd += " --cluster " + a.Domain + " --driver " + action.Driver
	}
	action.Files = files
	action.Command = cmd
	p.Actions = append(p.Actions, action)

	if a.GitInit && s.Git.Available && s.Git.Root == "" {
		p.Actions = append(p.Actions, Action{Kind: ActionGitInit, Run: "git init", Command: "git init"})
	}
	p.Actions = append(p.Actions, toolchainActions(a)...)
	if a.Domain != "" && a.Use {
		p.Actions = append(p.Actions, useAction(a.Domain))
	}
	return p
}

// ClusterPlan is `lo init cluster`: the spec, then `lo use` when active.
func ClusterPlan(dir, dom, driver string, active bool) Plan {
	driver = driverOr(driver)
	cmd := "lo init cluster " + dom + " --driver " + driver
	if !active {
		cmd += " --no-active"
	}
	p := Plan{Dir: dir, Actions: []Action{{Kind: ActionWriteClusterSpec, Domain: dom, Driver: driver,
		Files: []string{clusterSpecFile(dir, dom)}, Command: cmd}}}
	if active {
		p.Actions = append(p.Actions, useAction(dom))
	}
	return p
}

// ServicePlan is `lo init service`: the service file, its catalog entry
// and the Tiltfile. path "" = ./<name>.
func ServicePlan(dir, name, path string) Plan {
	cmd := "lo init service " + name
	if path != "" {
		cmd += " --path " + path
	}
	where := path
	if where == "" {
		where = "./" + name
	}
	return Plan{Dir: dir, Actions: []Action{{Kind: ActionAddService, Service: name, Path: path,
		Files: []string{strings.TrimPrefix(where, "./") + "/lok8s.yaml", "services.yaml entry", "Tiltfile"}, Command: cmd}}}
}

// TestsPlan is `lo init test`: the Playwright suite. path "" = tests/.
func TestsPlan(dir, path string) Plan {
	cmd := "lo init test"
	if path != "" {
		cmd += " --path " + path
	}
	where := path
	if where == "" {
		where = "tests"
	}
	return Plan{Dir: dir, Actions: []Action{{Kind: ActionAddTests, Path: path,
		Files: []string{strings.TrimSuffix(strings.TrimPrefix(where, "./"), "/") + "/ (the Playwright suite)"}, Command: cmd}}}
}

// ToolchainPlan is `lo toolchain install` for the project at dir.
func ToolchainPlan(dir string, groups []string) Plan {
	return Plan{Dir: dir, Actions: toolchainActions(Answers{Toolchain: true, Groups: groups})}
}

// UsePlan is `lo use <domain>` for the project at dir.
func UsePlan(dir, dom string) Plan {
	return Plan{Dir: dir, Actions: []Action{useAction(dom)}}
}

// EjectPlan is `lo assets eject bash` for the project at dir.
func EjectPlan(dir string) Plan {
	return Plan{Dir: dir, Actions: []Action{{Kind: ActionEjectBash,
		Files:   []string{".lok8s/ (the bash implementation, plus every data asset the project lacks)"},
		Command: "lo assets eject bash"}}}
}

// ImplementationPlan sets spec.implementation.default for the project
// name at dir.
func ImplementationPlan(dir, name, impl string) Plan {
	return Plan{Dir: dir, Name: name, Actions: []Action{{Kind: ActionSetImplementation, Name: name, Implementation: impl,
		Files:   []string{"lok8s.yaml (spec.implementation.default: " + impl + ")"},
		Command: "lo init project --env none --implementation " + impl}}}
}

// Result is the one-line outcome of an executed plan, as the project
// loop shows it under the card: a verb and what changed.
func (p Plan) Result() (verb, what string) {
	if len(p.Actions) == 0 {
		return "done", "nothing"
	}
	a := p.Actions[0]
	switch a.Kind {
	case ActionWriteProjectFiles:
		parts := []string{p.Name}
		if a.Domain != "" {
			parts = append(parts, "clusters/"+a.Domain)
		}
		git := false
		for _, b := range p.Actions[1:] {
			switch b.Kind {
			case ActionToolchainInstall:
				parts = append(parts, "toolchain "+plural(toolchain.Count(b.Groups), "tool"))
			case ActionGitInit:
				git = true
			}
		}
		if git {
			parts = append(parts, "git initialised")
		}
		return "created", joined(parts)
	case ActionWriteClusterSpec:
		what = a.Files[0]
		if len(p.Actions) > 1 && p.Actions[1].Kind == ActionUse {
			what += " · active"
		}
		return "added", what
	case ActionAddService, ActionAddTests:
		return "added", joined(a.Files)
	case ActionToolchainInstall:
		return "installed", joined(a.Files)
	case ActionUse:
		return "active", a.Domain
	case ActionEjectBash:
		return "ejected", ".lok8s/"
	case ActionSetImplementation:
		return "set", "implementation " + a.Implementation
	}
	return "created", p.Name
}

// clusterSpecFile names the spec a plan writes, or keeps when it already
// exists (the executor never overwrites it).
func clusterSpecFile(root, dom string) string {
	rel := "clusters/" + dom + "/cluster.lok8s.yaml"
	if fsutil.FileExists(filepath.Join(root, "clusters", dom, "cluster.lok8s.yaml")) {
		return rel + " (exists, kept)"
	}
	return rel
}

func toolchainActions(a Answers) []Action {
	if !a.Toolchain {
		return nil
	}
	groups := a.Groups
	if len(groups) == 0 {
		groups = toolchain.DefaultGroups
	}
	return []Action{{Kind: ActionToolchainInstall, Groups: groups, Network: true,
		Files: []string{".bin/b.yaml", ".bin/ (the pinned tools)"},
		Run:   "lo toolchain install (network)", Command: "lo toolchain install --groups " + strings.Join(groups, ",")}}
}

func useAction(domain string) Action {
	return Action{Kind: ActionUse, Domain: domain, Files: []string{"clusters/.active"}, Run: "lo use " + domain, Command: "lo use " + domain}
}

// driverOr defaults the driver to lo.
func driverOr(d string) string {
	if d == "" {
		return "lo"
	}
	return d
}

// relDir is dir relative to cwd ("" when equal; the absolute path when
// dir lies outside cwd).
func relDir(cwd, dir string) string {
	rel, err := filepath.Rel(cwd, dir)
	if err != nil || rel == "." {
		return ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return dir
	}
	return rel
}

// existingEnv is the environment file dir already has ("" = none).
func existingEnv(dir string) string {
	switch {
	case fsutil.FileExists(filepath.Join(dir, "mise.toml")):
		return "mise"
	case fsutil.FileExists(filepath.Join(dir, ".envrc")):
		return "direnv"
	}
	return ""
}

// Next is the step a project state calls for. The first that applies
// wins: the toolchain when a pinned tool is missing; a cluster spec when
// there is none; the active domain when none is set; the assets diff on
// drift; `lo up` while the active domain has no kubeconfig; else `lo
// status`. Outside a project: `lo init`.
func Next(s State, drift bool) string {
	p := s.Project
	if p == nil {
		return "lo init"
	}
	switch {
	case p.ToolchainMissing():
		return "lo toolchain install"
	case len(p.Domains) == 0:
		return "lo init cluster"
	case !p.ActiveValid():
		return "lo use <domain>"
	case drift:
		return "lo assets diff"
	case !p.Kubeconfig:
		return "lo up"
	}
	return "lo status"
}
