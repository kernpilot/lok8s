package initctx

// plan.go — the decision function: State + Answers → the ordered actions
// the cli executes, each with the flag-twin command line a CI user can
// run instead. Pure: nothing here touches the filesystem.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// Answers is what the wizard asked (or the defaults `--plan` assumes).
// Every field has a flag twin on an existing subcommand; Decide prints it.
type Answers struct {
	// Dir is the project directory for a new project ("" = the situation's
	// default: the working directory, or the git root below which the
	// user stands). Ignored inside an existing project (its root).
	Dir string
	// Name is metadata.name ("" = the directory name).
	Name string
	// Env is the environment file: mise, direnv or none ("" = the one the
	// directory already has, else mise). Inside an existing project it
	// only matters when none exists yet.
	Env string
	// GitInit asks for `git init` (honoured only without a repository).
	GitInit bool
	// Domain and Driver describe the first (or an added) cluster spec;
	// "" = none.
	Domain, Driver string
	// Use makes Domain the active domain (`lo use`).
	Use bool
	// Toolchain runs `lo toolchain install` with Groups (nil = the
	// default groups).
	Toolchain bool
	Groups    []string
	// Implementation switches spec.implementation.default ("" = keep;
	// "bash" ejects the tree first when the project has none).
	Implementation string
	// Service adds a service (`lo init service <name>`); Tests the
	// Playwright suite; Register adds the service directory the user
	// stands in to services.yaml.
	Service  string
	Tests    bool
	Register bool
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
	// ActionWriteEnvFile — the environment file into an existing project.
	ActionWriteEnvFile ActionKind = "environment file"
	// ActionWriteClusterSpec — clusters/<domain>/cluster.lok8s.yaml into
	// an existing project.
	ActionWriteClusterSpec ActionKind = "cluster spec"
	// ActionEjectBash — `lo assets eject bash`.
	ActionEjectBash ActionKind = "eject bash"
	// ActionSetImplementation — spec.implementation.default in lok8s.yaml.
	ActionSetImplementation ActionKind = "implementation"
	// ActionToolchainInstall — `lo toolchain install` (network).
	ActionToolchainInstall ActionKind = "toolchain"
	// ActionUse — `lo use <domain>`.
	ActionUse ActionKind = "use"
	// ActionAddService — `lo init service <name>`.
	ActionAddService ActionKind = "service"
	// ActionAddTests — `lo init test`.
	ActionAddTests ActionKind = "tests"
	// ActionRegisterService — the service directory into services.yaml.
	ActionRegisterService ActionKind = "register service"
)

// Action is one step of a Plan.
type Action struct {
	Kind ActionKind
	// Summary says what the step writes or runs, in one line.
	Summary string
	// Command is the flag-twin command line, run from Plan.Dir.
	Command string
	// Network is whether the step needs the network.
	Network bool
	// The parameters the executor hands to the functions behind Command.
	Name, Env, Domain, Driver, Service, ServicePath, Implementation string
	Groups                                                          []string
}

// Plan is the ordered list of actions for one `lo init` run.
type Plan struct {
	Situation Situation
	// Dir is the project directory every action runs in (absolute).
	Dir string
	// Name is the project name the actions use.
	Name string
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

// Network reports whether any action needs the network.
func (p Plan) Network() bool {
	for _, a := range p.Actions {
		if a.Network {
			return true
		}
	}
	return false
}

// DefaultAnswers are what `--plan` assumes without a conversation: a new
// project gets its files where the situation suggests, `git init` when
// git exists and there is no repository, the environment file the
// directory already has (else mise) and the toolchain; an existing
// project gets nothing (the card, and the menu as hints).
func DefaultAnswers(s State) Answers {
	switch s.Situation() {
	case SituationEmptyDir, SituationGitBelowRoot, SituationBareDir:
		return Answers{
			Dir:       DefaultDir(s),
			Env:       DefaultEnv(s, DefaultDir(s)),
			GitInit:   s.Git.Available && s.Git.Root == "",
			Toolchain: true,
		}
	}
	return Answers{}
}

// DefaultDir is the project directory a new project defaults to: the git
// root when the user stands below it, else the working directory.
func DefaultDir(s State) string {
	if s.Situation() == SituationGitBelowRoot {
		return s.Git.Root
	}
	return s.Cwd
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

// Decide turns the state and the answers into the plan. Answers that do
// not apply to the situation are ignored (a `git init` inside a
// repository, a registration outside a service directory).
func Decide(s State, a Answers) Plan {
	sit := s.Situation()
	switch sit {
	case SituationProjectRoot, SituationInsideProject:
		return existingProject(s, a)
	}
	return newProject(s, a)
}

// newProject is the empty, bare and git-below-root conversations.
func newProject(s State, a Answers) Plan {
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
		name = filepath.Base(dir)
	}
	env := a.Env
	if env == "" {
		env = DefaultEnv(s, dir)
	}
	p := Plan{Situation: s.Situation(), Dir: dir, Name: name}

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
		files = append(files, "clusters/"+a.Domain+"/cluster.lok8s.yaml ("+action.Driver+")")
		cmd += " --cluster " + a.Domain + " --driver " + action.Driver
	}
	action.Summary = strings.Join(files, ", ")
	action.Command = cmd
	p.Actions = append(p.Actions, action)

	if a.GitInit && s.Git.Available && s.Git.Root == "" {
		p.Actions = append(p.Actions, Action{Kind: ActionGitInit, Summary: "a git repository in " + shortDir(s.Cwd, dir), Command: "git init"})
	}
	p.Actions = append(p.Actions, toolchainActions(a)...)
	if a.Domain != "" && a.Use {
		p.Actions = append(p.Actions, useAction(a.Domain))
	}
	return p
}

// existingProject is the project-root and inside-a-project conversations.
func existingProject(s State, a Answers) Plan {
	proj := s.Project
	name := proj.Name
	if name == "" {
		name = filepath.Base(proj.Root)
	}
	p := Plan{Situation: s.Situation(), Dir: proj.Root, Name: name}

	if a.Env != "" && a.Env != "none" && proj.EnvFile == "" {
		file := "mise.toml"
		if a.Env == "direnv" {
			file = ".envrc"
		}
		p.Actions = append(p.Actions, Action{Kind: ActionWriteEnvFile, Env: a.Env, Name: name,
			Summary: file, Command: "lo init project --env " + a.Env})
	}
	if a.Domain != "" {
		driver := driverOr(a.Driver)
		p.Actions = append(p.Actions, Action{Kind: ActionWriteClusterSpec, Domain: a.Domain, Driver: driver, Name: name,
			Summary: "clusters/" + a.Domain + "/cluster.lok8s.yaml (" + driver + ")",
			Command: "lo init project --env none --cluster " + a.Domain + " --driver " + driver})
	}
	if a.Implementation != "" && a.Implementation != proj.Implementation {
		if a.Implementation == "bash" && !proj.BashTree {
			p.Actions = append(p.Actions, Action{Kind: ActionEjectBash,
				Summary: ".lok8s/ (the bash implementation, plus every data asset the project lacks)",
				Command: "lo assets eject bash"})
		}
		p.Actions = append(p.Actions, Action{Kind: ActionSetImplementation, Implementation: a.Implementation, Name: name,
			Summary: "lok8s.yaml: spec.implementation.default: " + a.Implementation,
			Command: `yq -i '.spec.implementation.default = "` + a.Implementation + `"' lok8s.yaml`})
	}
	p.Actions = append(p.Actions, toolchainActions(a)...)
	if a.Domain != "" && a.Use {
		p.Actions = append(p.Actions, useAction(a.Domain))
	}
	if a.Service != "" {
		p.Actions = append(p.Actions, Action{Kind: ActionAddService, Service: a.Service, ServicePath: "./" + a.Service,
			Summary: a.Service + "/lok8s.yaml, services.yaml entry, Tiltfile",
			Command: "lo init service " + a.Service})
	}
	if a.Tests {
		p.Actions = append(p.Actions, Action{Kind: ActionAddTests, Summary: "tests/ (the Playwright suite)", Command: "lo init test"})
	}
	if a.Register && s.ServiceDir && !proj.AtRoot {
		svc := s.ServiceName()
		path := "./" + filepath.ToSlash(relDir(proj.Root, s.Cwd))
		p.Actions = append(p.Actions, Action{Kind: ActionRegisterService, Service: svc, ServicePath: path,
			Summary: "services.yaml: services." + svc + ".path = " + path + ", Tiltfile",
			Command: "lo init service " + svc + " --path " + path})
	}
	return p
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
		Summary: ".bin/b.yaml, b and the pinned toolchain into .bin/ (groups " + strings.Join(groups, ",") + "; network)",
		Command: "lo toolchain install --groups " + strings.Join(groups, ",")}}
}

func useAction(domain string) Action {
	return Action{Kind: ActionUse, Domain: domain, Summary: "clusters/.active = " + domain, Command: "lo use " + domain}
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

// shortDir is dir as the summary names it: "." for cwd, else relative.
func shortDir(cwd, dir string) string {
	if rel := relDir(cwd, dir); rel != "" {
		return rel
	}
	return "."
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

// Option is one entry of the "what to add" menu in an existing project,
// with the flag twin the card prints as a hint.
type Option struct {
	Key     string
	Label   string
	Command string
}

// Options lists the menu for an existing project: what the situation
// allows, in the order the wizard offers it.
func Options(s State) []Option {
	p := s.Project
	if p == nil {
		return nil
	}
	var out []Option
	if s.ServiceDir && !p.AtRoot {
		svc := s.ServiceName()
		path := "./" + filepath.ToSlash(relDir(p.Root, s.Cwd))
		out = append(out, Option{Key: "register", Label: "Add this service (" + svc + ") to services.yaml", Command: "lo init service " + svc + " --path " + path})
	}
	out = append(out,
		Option{Key: "cluster", Label: "Add a cluster spec", Command: "lo init project --env none --cluster <domain> --driver <driver>"},
		Option{Key: "service", Label: "Add a service", Command: "lo init service <name>"},
	)
	if !p.Tests {
		out = append(out, Option{Key: "tests", Label: "Add the Playwright test suite", Command: "lo init test"})
	}
	if p.EnvFile == "" {
		out = append(out, Option{Key: "env", Label: "Add an environment file (mise.toml or .envrc)", Command: "lo init project --env mise|direnv"})
	}
	toolchainLabel := "Install the pinned toolchain (network)"
	if p.BYAML && len(p.ToolsMissing) == 0 {
		toolchainLabel = "Reinstall the pinned toolchain (network)"
	}
	out = append(out, Option{Key: "toolchain", Label: toolchainLabel, Command: "lo toolchain install --groups " + strings.Join(toolchain.DefaultGroups, ",")})
	other := "bash"
	if p.Implementation == "bash" {
		other = "go"
	}
	out = append(out, Option{Key: "implementation", Label: fmt.Sprintf("Switch the implementation to %s (now %s)", other, p.Implementation),
		Command: `yq -i '.spec.implementation.default = "` + other + `"' lok8s.yaml`})
	return out
}
