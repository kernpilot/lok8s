// Package initctx reads the room for a bare `lo init`: where the user
// stands (a project root, a subdirectory, a service directory, a
// submodule under an umbrella project, an empty or a bare directory),
// what git says about it, what the project already has, and whether a
// terminal is attached. Detect is the read side; Decide turns the state
// and the wizard's answers into the ordered list of actions the cli
// executes through the existing subcommands; Ask is the huh form layer.
//
// Everything here is pure over the filesystem except the two git reads,
// which go through the execx.Runner seam so the tests script them.
package initctx

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
)

// Situation is one of the five conversations `lo init` can have.
type Situation int

const (
	// SituationUnknown is the zero value; Detect never returns it.
	SituationUnknown Situation = iota
	// SituationEmptyDir — an empty directory (a `.git` entry does not
	// count) and no project above: the welcome conversation.
	SituationEmptyDir
	// SituationGitBelowRoot — inside a git repository, below its root,
	// and no project above: the wizard suggests the git root.
	SituationGitBelowRoot
	// SituationBareDir — a non-empty directory, no project above, and
	// either no git or cwd is the git root: the wizard says what it sees
	// and offers here or a subdirectory.
	SituationBareDir
	// SituationProjectRoot — cwd is a project root: the status card, then
	// what to add.
	SituationProjectRoot
	// SituationInsideProject — cwd is inside a project (a subdirectory, a
	// service directory, a submodule under the umbrella project): the
	// card, plus the offer to register a service directory.
	SituationInsideProject
)

// String is the situation as the card names it.
func (s Situation) String() string {
	switch s {
	case SituationEmptyDir:
		return "empty directory"
	case SituationGitBelowRoot:
		return "git repository, below its root, no project"
	case SituationBareDir:
		return "directory without a project"
	case SituationProjectRoot:
		return "project root"
	case SituationInsideProject:
		return "inside a project"
	}
	return "unknown"
}

// Terminal is what decides between the wizard and the help text.
type Terminal struct {
	// StdinTTY and StdoutTTY report whether the two streams are terminals.
	StdinTTY, StdoutTTY bool
	// CI is whether the CI environment variable is set (any value).
	CI bool
	// Yes is the --yes flag.
	Yes bool
}

// Interactive reports whether the wizard may run: both streams are
// terminals, CI is unset and --yes was not given.
func (t Terminal) Interactive() bool {
	return t.StdinTTY && t.StdoutTTY && !t.CI && !t.Yes
}

// DetectTerminal reads the two streams and the CI variable.
func DetectTerminal(stdin, stdout *os.File, yes bool) Terminal {
	_, ci := os.LookupEnv("CI")
	return Terminal{
		StdinTTY:  stdin != nil && term.IsTerminal(int(stdin.Fd())),
		StdoutTTY: stdout != nil && term.IsTerminal(int(stdout.Fd())),
		CI:        ci,
		Yes:       yes,
	}
}

// Git is what git says about the working directory.
type Git struct {
	// Available is whether git ran at all.
	Available bool
	// Root is the repository root ("" = not a repository), in the path
	// form of State.Cwd (git resolves symlinks; the card prints one form).
	Root string
	// AtRoot is whether cwd is the repository root.
	AtRoot bool
	// Branch is the current branch ("" = detached HEAD).
	Branch string
	// Uncommitted counts the paths `git status --porcelain` lists.
	Uncommitted int
	// Submodule is whether Root carries a `.git` FILE (a submodule
	// checkout, or a worktree) rather than a directory.
	Submodule bool
}

// Dirty is whether the working tree has uncommitted changes.
func (g Git) Dirty() bool { return g.Uncommitted > 0 }

// Domain is one directory under clusters/ that carries a spec.
type Domain struct {
	Name string
	// Kind is the driver of cluster.lok8s.yaml (lowercase), "deploy" for
	// a deploy-only domain, "?" when unreadable.
	Kind string
}

// Project is what exists in the project the user stands in.
type Project struct {
	// Root is the project root (the marker walk from cwd).
	Root string
	// AtRoot is whether cwd is Root.
	AtRoot bool
	// Name is metadata.name of the project file ("" when the marker is
	// clusters/ alone).
	Name string
	// ProjectFile is whether Root/lok8s.yaml is a `kind: Project` file.
	ProjectFile bool
	// Clusters is whether Root/clusters exists.
	Clusters bool
	// Domains lists the domains under clusters/ that carry a spec.
	Domains []Domain
	// Active is clusters/.active ("" when unset or invalid).
	Active string
	// EnvFile is the environment file present: "mise" (mise.toml),
	// "direnv" (.envrc), "" (none). With both present, mise.
	EnvFile string
	// BYAML is whether .bin/b.yaml exists.
	BYAML bool
	// Tools counts the pinned tools: b itself plus every `binaries:`
	// entry of .bin/b.yaml. ToolsMissing lists the ones that do not
	// resolve under the project (by name); nil = all present.
	Tools        int
	ToolsMissing []string
	// BashTree is whether Root/.lok8s/lo exists (an ejected or vendored
	// bash tree).
	BashTree bool
	// Implementation is spec.implementation.default ("go" or "bash";
	// "go" when the block is absent), ImplementationErr the loader's
	// error text when the block is invalid.
	Implementation    string
	ImplementationErr string
	// Services is whether Root/services.yaml exists; Tests whether
	// Root/tests is a directory.
	Services, Tests bool
}

// State is what Detect found.
type State struct {
	// Cwd is the working directory, absolute.
	Cwd string
	// Empty is whether Cwd has no entries besides `.git`.
	Empty bool
	// Entries counts the entries of Cwd besides `.git`.
	Entries int
	// ServiceDir is whether Cwd holds a kind-less lok8s.yaml (a service
	// directory).
	ServiceDir bool
	// Git is the git state; Project the project state (nil = no project
	// above Cwd).
	Git     Git
	Project *Project
	// Terminal is set by the caller (DetectTerminal); Detect leaves it
	// zero.
	Terminal Terminal
}

// Situation classifies the state.
func (s State) Situation() Situation {
	switch {
	case s.Project != nil && s.Project.AtRoot:
		return SituationProjectRoot
	case s.Project != nil:
		return SituationInsideProject
	case s.Empty:
		return SituationEmptyDir
	case s.Git.Root != "" && !s.Git.AtRoot:
		return SituationGitBelowRoot
	}
	return SituationBareDir
}

// ServiceName is the service name a service directory implies (its base
// name), "" outside one.
func (s State) ServiceName() string {
	if !s.ServiceDir {
		return ""
	}
	return filepath.Base(s.Cwd)
}

// Detect reads the room from cwd. r runs git (nil = no git: the state
// reports it unavailable).
func Detect(ctx context.Context, cwd string, r execx.Runner) (State, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return State{}, err
	}
	s := State{Cwd: abs}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return State{}, err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		s.Entries++
	}
	s.Empty = s.Entries == 0
	s.ServiceDir = isServiceFile(filepath.Join(abs, config.ProjectFile))
	s.Git = detectGit(ctx, abs, r)
	if root := config.FindProjectRoot(abs); config.IsProjectRoot(root) {
		s.Project = detectProject(root, abs)
	}
	return s, nil
}

// detectGit runs the three git reads through r: the root, the branch
// (symbolic-ref, so an unborn branch still has a name and a detached
// HEAD reads as none) and the porcelain status.
func detectGit(ctx context.Context, cwd string, r execx.Runner) Git {
	g := Git{}
	if r == nil {
		return g
	}
	out, err := execx.Output(ctx, r, execx.Cmd{Name: "git", Args: []string{"rev-parse", "--show-toplevel"}, Dir: cwd})
	if err != nil {
		if errors.Is(err, execx.ErrNotFound) {
			return g
		}
		// git ran and said no (rc 128 outside a repository).
		g.Available = true
		return g
	}
	g.Available = true
	root := strings.TrimRight(string(out), "\n")
	if root == "" {
		return g
	}
	g.Root = cwdForm(cwd, root)
	g.AtRoot = samePath(root, cwd)
	if info, err := os.Lstat(filepath.Join(root, ".git")); err == nil && !info.IsDir() {
		g.Submodule = true
	}
	if out, err := execx.Output(ctx, r, execx.Cmd{Name: "git", Args: []string{"symbolic-ref", "--short", "-q", "HEAD"}, Dir: cwd}); err == nil {
		g.Branch = strings.TrimSpace(string(out))
	}
	if out, err := execx.Output(ctx, r, execx.Cmd{Name: "git", Args: []string{"status", "--porcelain"}, Dir: cwd}); err == nil {
		g.Uncommitted = len(strings.Split(strings.TrimRight(string(out), "\n"), "\n"))
		if strings.TrimSpace(string(out)) == "" {
			g.Uncommitted = 0
		}
	}
	return g
}

// cwdForm rewrites root (a path git printed, symlinks resolved) into the
// form of cwd (the path the user typed): the same relative walk from
// the resolved cwd, re-rooted on the typed one. root unchanged when the
// two forms cannot be related.
func cwdForm(cwd, root string) string {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil || resolved == cwd {
		return root
	}
	rel, err := filepath.Rel(resolved, root)
	if err != nil {
		return root
	}
	typed := filepath.Join(cwd, rel)
	// A symlink below the root breaks the walk: keep git's form then.
	if back, err := filepath.EvalSymlinks(typed); err != nil || back != root {
		return root
	}
	return typed
}

// samePath compares two paths with symlinks resolved where possible.
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return ra == rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// detectProject reads what exists under root.
func detectProject(root, cwd string) *Project {
	p := &Project{Root: root, AtRoot: samePath(root, cwd)}
	p.ProjectFile, p.Name = projectFileName(filepath.Join(root, config.ProjectFile))
	clusters := filepath.Join(root, "clusters")
	p.Clusters = fsutil.DirExists(clusters)
	p.Domains = listDomains(clusters)
	if raw, err := os.ReadFile(filepath.Join(clusters, ".active")); err == nil {
		active := strings.TrimRight(string(raw), "\n")
		if domain.NameRe.MatchString(active) {
			p.Active = active
		}
	}
	switch {
	case fsutil.FileExists(filepath.Join(root, "mise.toml")):
		p.EnvFile = "mise"
	case fsutil.FileExists(filepath.Join(root, ".envrc")):
		p.EnvFile = "direnv"
	}
	byaml := filepath.Join(root, ".bin", "b.yaml")
	p.BYAML = fsutil.FileExists(byaml)
	p.Tools, p.ToolsMissing = pinnedTools(byaml)
	p.BashTree = fsutil.FileExists(filepath.Join(root, ".lok8s", "lo"))
	impl, err := config.LoadImplementation(root)
	p.Implementation = impl.Default
	if err != nil {
		p.ImplementationErr = err.Error()
	}
	p.Services = fsutil.FileExists(filepath.Join(root, "services.yaml"))
	p.Tests = fsutil.DirExists(filepath.Join(root, "tests"))
	return p
}

// projectFileName reads a `kind: Project` file's metadata.name.
func projectFileName(path string) (bool, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if yaml.Unmarshal(raw, &doc) != nil || doc.Kind != config.ProjectKind {
		return false, ""
	}
	return true, doc.Metadata.Name
}

// isServiceFile reports whether path is a lok8s.yaml without a kind: the
// bare per-service object `lo init service` writes (`kind: Service` files
// count too; a `kind: Project` file does not).
func isServiceFile(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		Kind string `yaml:"kind"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return false
	}
	return doc.Kind != config.ProjectKind
}

// listDomains lists the domains under clusters/ that carry a spec, in
// byte order.
func listDomains(clusters string) []Domain {
	entries, err := os.ReadDir(clusters)
	if err != nil {
		return nil
	}
	var out []Domain
	for _, e := range entries {
		if !e.IsDir() || !domain.NameRe.MatchString(e.Name()) {
			continue
		}
		kind, err := domain.Driver(clusters, e.Name())
		if err != nil {
			if errors.Is(err, domain.ErrNoDriver) && !fsutil.FileExists(filepath.Join(clusters, e.Name(), "cluster.lok8s.yaml")) {
				continue
			}
			kind = "?"
		}
		out = append(out, Domain{Name: e.Name(), Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pinnedTools counts the toolchain byaml pins and lists the entries that
// do not resolve: b itself (<bin>/b), then every `binaries:` entry at the
// path b installs it to — `file:` relative to the b.yaml directory, else
// <bin>/<alias or the last segment of the key>. A filesystem check only
// (no probe): `lo toolchain doctor` verifies versions. Without a b.yaml
// nothing is pinned: 0, nil.
func pinnedTools(byaml string) (int, []string) {
	raw, err := os.ReadFile(byaml)
	if err != nil {
		return 0, nil
	}
	var doc struct {
		Binaries map[string]*struct {
			Alias string `yaml:"alias"`
			File  string `yaml:"file"`
		} `yaml:"binaries"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return 0, nil
	}
	bin := filepath.Dir(byaml)
	type pin struct{ name, path string }
	pins := []pin{{"b", filepath.Join(bin, "b")}}
	keys := make([]string, 0, len(doc.Binaries))
	for k := range doc.Binaries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := doc.Binaries[k]
		name := path.Base(k)
		if e != nil && e.Alias != "" {
			name = e.Alias
		}
		file := filepath.Join(bin, name)
		if e != nil && e.File != "" {
			file = filepath.Join(bin, filepath.FromSlash(e.File))
		}
		pins = append(pins, pin{name, file})
	}
	var missing []string
	for _, t := range pins {
		if !fsutil.IsExecutable(t.path) {
			missing = append(missing, t.name)
		}
	}
	return len(pins), missing
}
