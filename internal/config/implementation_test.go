package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProjectFile(t *testing.T, base, spec string) {
	t.Helper()
	content := "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: t\n" + spec
	if err := os.WriteFile(filepath.Join(base, ProjectFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadImplementationDefaults(t *testing.T) {
	base := t.TempDir()
	want := Implementation{Default: ImplGo, Tree: ".lok8s", TreeDir: filepath.Join(base, ".lok8s")}

	// No project file at all.
	got, err := LoadImplementation(base)
	if err != nil || got.Default != want.Default || got.Tree != want.Tree || got.TreeDir != want.TreeDir || len(got.Commands) != 0 {
		t.Errorf("no file: %+v %v", got, err)
	}
	if got.Routes() {
		t.Error("no file routes")
	}

	// A service file at the base is not a project file.
	os.WriteFile(filepath.Join(base, ProjectFile), []byte("kind: Service\nspec:\n  implementation:\n    default: bash\n"), 0o600)
	got, err = LoadImplementation(base)
	if err != nil || got.Default != ImplGo || got.Routes() {
		t.Errorf("service file: %+v %v", got, err)
	}

	// A project file without the block.
	writeProjectFile(t, base, "")
	got, err = LoadImplementation(base)
	if err != nil || got.Default != ImplGo || got.Tree != ".lok8s" || got.Routes() {
		t.Errorf("no block: %+v %v", got, err)
	}

	// default: go, explicit.
	writeProjectFile(t, base, "spec:\n  implementation:\n    default: go\n")
	got, err = LoadImplementation(base)
	if err != nil || got.Default != ImplGo || got.Routes() {
		t.Errorf("default go: %+v %v", got, err)
	}
}

func TestLoadImplementationReadsTheBlock(t *testing.T) {
	base := t.TempDir()
	writeProjectFile(t, base, "spec:\n  implementation:\n    default: bash\n    bash:\n      commands: [registry, image]\n      tree: vendor/lok8s/\n")
	got, err := LoadImplementation(base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Default != ImplBash || !got.Routes() {
		t.Errorf("default = %q", got.Default)
	}
	if strings.Join(got.Commands, ",") != "registry,image" {
		t.Errorf("commands = %v", got.Commands)
	}
	if got.Tree != filepath.Join("vendor", "lok8s") || got.TreeDir != filepath.Join(base, "vendor", "lok8s") {
		t.Errorf("tree = %q dir = %q", got.Tree, got.TreeDir)
	}

	// commands alone route too.
	writeProjectFile(t, base, "spec:\n  implementation:\n    bash:\n      commands: [registry]\n")
	got, err = LoadImplementation(base)
	if err != nil || got.Default != ImplGo || !got.Routes() {
		t.Errorf("commands only: %+v %v", got, err)
	}
}

func TestLoadImplementationRejectsBadValues(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(base, "escape"))
	os.MkdirAll(filepath.Join(base, "real", "tree"), 0o755)
	os.Symlink(filepath.Join(base, "real", "tree"), filepath.Join(base, "inside"))

	cases := []struct{ name, spec, want string }{
		{"default", "spec:\n  implementation:\n    default: python\n", `lok8s.yaml: spec.implementation.default "python" is not "go" or "bash".`},
		{"dotdot", "spec:\n  implementation:\n    bash:\n      tree: ..\n", `lok8s.yaml: spec.implementation.bash.tree ".." leaves the project.`},
		{"dotdot-nested", "spec:\n  implementation:\n    bash:\n      tree: ../sibling/.lok8s\n", `lok8s.yaml: spec.implementation.bash.tree "../sibling/.lok8s" leaves the project.`},
		{"dotdot-cleaned", "spec:\n  implementation:\n    bash:\n      tree: a/../../b\n", `lok8s.yaml: spec.implementation.bash.tree "a/../../b" leaves the project.`},
		{"absolute", "spec:\n  implementation:\n    bash:\n      tree: /opt/lok8s\n", `lok8s.yaml: spec.implementation.bash.tree "/opt/lok8s" is absolute. Use a path relative to the project.`},
		{"symlink-out", "spec:\n  implementation:\n    default: bash\n    bash:\n      tree: escape\n", `lok8s.yaml: spec.implementation.bash.tree "escape" resolves to ` + mustEval(outside) + `, outside the project.`},
	}
	for _, c := range cases {
		writeProjectFile(t, base, c.spec)
		_, err := LoadImplementation(base)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: err = %v\nwant %s", c.name, err, c.want)
		}
		if _, ok := errors.AsType[ImplementationError](err); !ok {
			t.Errorf("%s: not an ImplementationError: %T", c.name, err)
		}
	}

	// A symlink that stays inside the project is fine; the tree is the
	// path as written (cleaned), not the resolved one.
	writeProjectFile(t, base, "spec:\n  implementation:\n    default: bash\n    bash:\n      tree: inside\n")
	got, err := LoadImplementation(base)
	if err != nil || got.Tree != "inside" || got.TreeDir != filepath.Join(base, "inside") {
		t.Errorf("symlink inside: %+v %v", got, err)
	}

	// Malformed YAML names the file.
	os.WriteFile(filepath.Join(base, ProjectFile), []byte("kind: Project\nspec: [\n"), 0o600)
	if _, err := LoadImplementation(base); err == nil || !strings.HasPrefix(err.Error(), "lok8s.yaml: ") {
		t.Errorf("malformed: %v", err)
	}
}

// The symlink escape rule applies only when the block routes something.
func TestLoadImplementationSymlinkRuleOnlyWhenRouting(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(base, ".lok8s"))
	for _, spec := range []string{"", "spec:\n  implementation:\n    default: go\n"} {
		writeProjectFile(t, base, spec)
		if got, err := LoadImplementation(base); err != nil || got.Routes() {
			t.Errorf("spec %q: %+v %v", spec, got, err)
		}
	}
	writeProjectFile(t, base, "spec:\n  implementation:\n    bash:\n      commands: [registry]\n")
	want := `lok8s.yaml: spec.implementation.bash.tree ".lok8s" resolves to ` + mustEval(outside) + `, outside the project.`
	if _, err := LoadImplementation(base); err == nil || err.Error() != want {
		t.Errorf("routing: %v\nwant %s", err, want)
	}
	// The shape rules stay unconditional.
	writeProjectFile(t, base, "spec:\n  implementation:\n    default: go\n    bash:\n      tree: ../x\n")
	if _, err := LoadImplementation(base); err == nil || !strings.HasSuffix(err.Error(), "leaves the project.") {
		t.Errorf("shape rule without routing: %v", err)
	}
}

func TestLoadImplementationWarnsOnUnknownKeys(t *testing.T) {
	base := t.TempDir()
	writeProjectFile(t, base, "spec:\n  implementations: {default: bash}\n  implementation:\n    zeta: 1\n    defaults: bash\n    bash:\n      commands: []\n      trees: x\n")
	got, err := LoadImplementation(base)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"lok8s.yaml: spec.implementations: unknown key. Use spec.implementation.",
		`lok8s.yaml: spec.implementation: unknown key "defaults".`,
		`lok8s.yaml: spec.implementation: unknown key "zeta".`,
		`lok8s.yaml: spec.implementation.bash: unknown key "trees".`,
	}, "\n")
	if strings.Join(got.Warnings, "\n") != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(got.Warnings, "\n"), want)
	}
	writeProjectFile(t, base, "spec:\n  implementation:\n    default: go\n")
	if got, _ := LoadImplementation(base); len(got.Warnings) != 0 {
		t.Errorf("clean block warned: %v", got.Warnings)
	}
}
