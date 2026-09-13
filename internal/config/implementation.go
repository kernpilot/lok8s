package config

// implementation.go — spec.implementation of the project file lok8s.yaml:
// which implementation runs a command, Go (the default) or the bash tree
// in the project. The file is the ONLY switch. No environment variable
// selects code: a variable is inherited by every child, survives across
// directories and can be set by anything that ran before lo. The file is
// committed, reviewed and scoped to one project.
//
//	kind: Project
//	spec:
//	  implementation:
//	    default: go            # go | bash; go when absent
//	    bash:
//	      commands: [registry] # top-level command names routed to bash
//	      tree: .lok8s         # project-relative; the executable is <tree>/lo
//
// This package validates the shape of the block: the default value and
// the tree path (relative, inside the project, no symlink escape). The
// command names and the presence of the tree are checked by the cli,
// which knows the command tree and decides when a tree is required.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectFile is the project-root file that carries spec.implementation
// (and marks the project root; see isProjectRoot).
const ProjectFile = "lok8s.yaml"

// The two implementations spec.implementation.default can name.
const (
	ImplGo   = "go"
	ImplBash = "bash"
)

// DefaultBashTree is spec.implementation.bash.tree when absent.
const DefaultBashTree = ".lok8s"

// Implementation is the parsed spec.implementation block with its
// defaults applied. The zero value (Default go, no commands, tree .lok8s
// after Load) is what a project without the block gets.
type Implementation struct {
	// Default is go or bash: the implementation of every command not
	// listed in Commands.
	Default string
	// Commands lists the top-level command names routed to bash, in file
	// order, unvalidated (the cli validates them against its tree).
	Commands []string
	// Tree is the bash tree, project-relative and cleaned (default
	// .lok8s). TreeDir is the same as an absolute path.
	Tree string
	// TreeDir is <base>/<Tree>.
	TreeDir string
	// Warnings lists the keys the loader did not recognise (a typo such
	// as `defaults:` or `spec.implementations:` would otherwise select
	// Go silently); `lo lint` prints them.
	Warnings []string
}

// Routes reports whether the block routes anything to bash: the whole
// tree (Default bash) or at least one command.
func (i Implementation) Routes() bool {
	return i.Default == ImplBash || len(i.Commands) > 0
}

// ImplementationError is a validation error of the spec.implementation
// block: one sentence in Simplified Technical English, printed verbatim
// after `lo: ` (every command) or `[error] ` (lo lint).
type ImplementationError string

func (e ImplementationError) Error() string { return string(e) }

// implErrorf builds an ImplementationError.
func implErrorf(format string, a ...any) error {
	return ImplementationError(ProjectFile + ": " + fmt.Sprintf(format, a...))
}

// implementationFile is the subset of lok8s.yaml the loader reads.
type implementationFile struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Implementation struct {
			Default string `yaml:"default"`
			Bash    struct {
				Commands []string `yaml:"commands"`
				Tree     string   `yaml:"tree"`
			} `yaml:"bash"`
		} `yaml:"implementation"`
	} `yaml:"spec"`
}

// LoadImplementation reads spec.implementation from <base>/lok8s.yaml. A
// missing file, or a lok8s.yaml that is not a `kind: Project` document (a
// service file), yields the defaults. A malformed file, an unknown
// default, or a tree that is absolute or leaves the project is an error;
// a tree that resolves through a symlink to a directory outside the
// project is an error only when the block routes something (a project
// that runs Go may link its .lok8s anywhere). The messages name the file
// and the field the way `lo lint` reports them.
func LoadImplementation(base string) (Implementation, error) {
	impl := Implementation{Default: ImplGo, Tree: DefaultBashTree}
	raw, err := os.ReadFile(filepath.Join(base, ProjectFile))
	switch {
	case os.IsNotExist(err):
		impl.TreeDir = filepath.Join(base, impl.Tree)
		return impl, nil
	case err != nil:
		return impl, fmt.Errorf("%s: %w", ProjectFile, err)
	}
	var doc implementationFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return impl, fmt.Errorf("%s: %w", ProjectFile, err)
	}
	if doc.Kind != ProjectKind {
		impl.TreeDir = filepath.Join(base, impl.Tree)
		return impl, nil
	}
	block := doc.Spec.Implementation
	impl.Warnings = unknownImplementationKeys(raw)

	switch block.Default {
	case "", ImplGo:
		impl.Default = ImplGo
	case ImplBash:
		impl.Default = ImplBash
	default:
		return impl, implErrorf("spec.implementation.default %q is not %q or %q.", block.Default, ImplGo, ImplBash)
	}
	impl.Commands = block.Bash.Commands

	if block.Bash.Tree != "" {
		impl.Tree = block.Bash.Tree
	}
	tree, err := implementationTree(base, impl.Tree, impl.Routes())
	if err != nil {
		return impl, err
	}
	impl.Tree, impl.TreeDir = tree, filepath.Join(base, tree)
	return impl, nil
}

// unknownImplementationKeys lists the keys under spec.implementation (and
// spec.implementation.bash) the loader does not read, plus a
// `spec.implementations` typo, as lint warnings. Unknown keys elsewhere
// in the file are not this loader's business.
func unknownImplementationKeys(raw []byte) []string {
	var generic struct {
		Spec map[string]any `yaml:"spec"`
	}
	if yaml.Unmarshal(raw, &generic) != nil {
		return nil
	}
	var warnings []string
	if _, ok := generic.Spec["implementations"]; ok {
		warnings = append(warnings, ProjectFile+": spec.implementations: unknown key. Use spec.implementation.")
	}
	block, _ := generic.Spec["implementation"].(map[string]any)
	for _, key := range sortedKeys(block) {
		if key != "default" && key != "bash" {
			warnings = append(warnings, fmt.Sprintf("%s: spec.implementation: unknown key %q.", ProjectFile, key))
		}
	}
	bash, _ := block["bash"].(map[string]any)
	for _, key := range sortedKeys(bash) {
		if key != "commands" && key != "tree" {
			warnings = append(warnings, fmt.Sprintf("%s: spec.implementation.bash: unknown key %q.", ProjectFile, key))
		}
	}
	return warnings
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// implementationTree validates spec.implementation.bash.tree and returns
// it cleaned. The tree must be relative and stay inside the project: no
// absolute path, no `..` after cleaning. With routes set (the block
// routes something), the directory's symlink-resolved location must
// also lie under the resolved project root; without a routing the tree
// is never exec'd, so a linked .lok8s (a checkout beside the project)
// stays legal. A tree that does not exist yet passes here; the cli
// reports it as missing when it needs it.
func implementationTree(base, tree string, routes bool) (string, error) {
	const field = "spec.implementation.bash.tree"
	if filepath.IsAbs(tree) {
		return "", implErrorf("%s %q is absolute. Use a path relative to the project.", field, tree)
	}
	clean := filepath.Clean(tree)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", implErrorf("%s %q leaves the project.", field, tree)
	}
	if !routes {
		return clean, nil
	}
	dir := filepath.Join(base, clean)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return clean, nil
		}
		return "", fmt.Errorf("%s: %s %q: %w", ProjectFile, field, tree, err)
	}
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("%s: %s %q: %w", ProjectFile, field, tree, err)
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", implErrorf("%s %q resolves to %s, outside the project.", field, tree, resolved)
	}
	return clean, nil
}

// SetImplementationDefault writes spec.implementation.default = impl into
// <base>/lok8s.yaml (a yaml.Node edit: comments and the other keys stay).
// A missing file is created as the `kind: Project` file `lo init project`
// writes, named name. The init wizard's implementation switch; the flag
// twin is the yq edit it prints.
func SetImplementationDefault(base, name, impl string) error {
	if impl != ImplGo && impl != ImplBash {
		return implErrorf("spec.implementation.default %q is not %q or %q.", impl, ImplGo, ImplBash)
	}
	file := filepath.Join(base, ProjectFile)
	raw, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%s: %w", ProjectFile, err)
	}
	var root yaml.Node
	if err == nil {
		if err := yaml.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("%s: %w", ProjectFile, err)
		}
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return implErrorf("top level is not a mapping.")
	}
	kind := mappingValue(doc, "kind")
	switch {
	case kind == nil && raw != nil:
		// A lok8s.yaml without a kind is a service file, never a project.
		return implErrorf("kind %q is not %q.", "", ProjectKind)
	case kind == nil:
		setMappingScalar(doc, "apiVersion", "lok8s.dev/v1")
		setMappingScalar(doc, "kind", ProjectKind)
		metadata := ensureMapping(doc, "metadata")
		setMappingScalar(metadata, "name", name)
	case kind.Value != ProjectKind:
		return implErrorf("kind %q is not %q.", kind.Value, ProjectKind)
	}
	spec := ensureMapping(doc, "spec")
	block := ensureMapping(spec, "implementation")
	setMappingScalar(block, "default", impl)

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(buf.String()), 0o644)
}

// mappingValue returns the value node of key in mapping m, nil when absent.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// ensureMapping returns the block mapping under key, creating it or
// replacing a non-mapping value.
func ensureMapping(m *yaml.Node, key string) *yaml.Node {
	if v := mappingValue(m, key); v != nil {
		if v.Kind != yaml.MappingNode {
			*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		v.Style = 0
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// setMappingScalar sets key to the string value in mapping m.
func setMappingScalar(m *yaml.Node, key, value string) {
	if v := mappingValue(m, key); v != nil {
		*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}
