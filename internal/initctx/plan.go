package initctx

// plan.go — the decision layer: the defaults a situation suggests, the
// ordered actions a set of answers makes (each with the files it writes,
// the command it runs and the flag-twin command line a script runs
// instead), and the `next` step a project state calls for. Pure: nothing
// here touches the filesystem beyond one existence check for a cluster
// spec that already exists.

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

// DefaultAnswers are the new-project defaults the screen shows first: the
// files where the situation suggests, the name from the directory, the
// first cluster `<name>.dev` on the lo driver and made active, the
// environment file the directory already has (else mise), the toolchain,
// and `git init` when git exists and there is no repository.
func DefaultAnswers(s State) Answers {
	dir := DefaultDir(s)
	name := DefaultName(dir)
	return Answers{
		Dir:       dir,
		Name:      name,
		Domain:    name + ".dev",
		Driver:    "lo",
		Use:       true,
		Env:       DefaultEnv(s, dir),
		GitInit:   s.Git.Available && s.Git.Root == "",
		Toolchain: true,
	}
}

// DefaultDir is the project directory a new project defaults to: the git
// root when the user stands below it, else the working directory.
func DefaultDir(s State) string {
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

	files := []string{"clusters/", "lok8s.yaml", ".gitignore entries"}
	cmd := "lo init project " + name + " --env " + env
	if rel := relDir(s.Cwd, dir); rel != "" {
		cmd += " --path " + rel
	}
	switch env {
	case "mise":
		files = append(files, "mise.toml")
	case "direnv":
		files = append(files, ".envrc")
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

// Next is the step a project state calls for, in order of need: the
// toolchain when a pinned tool is missing, a cluster spec when there is
// none, the active domain when none is set, the assets diff on drift,
// `lo up` while the active domain has no kubeconfig yet, else `lo
// status`. Outside a project: `lo init`.
func Next(s State, drift bool) string {
	p := s.Project
	if p == nil {
		return "lo init"
	}
	active := false
	for _, d := range p.Domains {
		if d.Name == p.Active {
			active = true
		}
	}
	switch {
	case !p.BYAML || len(p.ToolsMissing) > 0:
		return "lo toolchain install"
	case len(p.Domains) == 0:
		return "lo init cluster"
	case !active:
		return "lo use <domain>"
	case drift:
		return "lo assets diff"
	case !p.Kubeconfig:
		return "lo up"
	}
	return "lo status"
}
