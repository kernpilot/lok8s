package config

// implementation_set_test.go — SetImplementationDefault: the yaml.Node
// edit keeps the rest of the file, creates a project file when none
// exists, and refuses a service file or an unknown value.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetImplementationDefault(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, ProjectFile)

	// No file: a project file named name is created.
	if err := SetImplementationDefault(base, "acme", ImplBash); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	want := "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: acme\nspec:\n  implementation:\n    default: bash\n"
	if string(raw) != want {
		t.Errorf("created:\n%s\nwant:\n%s", raw, want)
	}
	impl, err := LoadImplementation(base)
	if err != nil || impl.Default != ImplBash {
		t.Errorf("load: %+v %v", impl, err)
	}

	// An existing file: the other keys and the comments survive, the
	// value flips.
	os.WriteFile(file, []byte("# the marker\napiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: keep # me\nspec:\n  implementation:\n    default: bash\n    bash:\n      commands: [registry]\n"), 0o600)
	if err := SetImplementationDefault(base, "ignored", ImplGo); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(file)
	for _, s := range []string{"# the marker\n", "name: keep # me\n", "    default: go\n", "commands: [registry]\n"} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("missing %q in:\n%s", s, raw)
		}
	}
	if strings.Contains(string(raw), "ignored") {
		t.Error("the name overwrote an existing file's")
	}

	// A file without spec: the block is added.
	os.WriteFile(file, []byte("kind: Project\nmetadata:\n  name: x\n"), 0o600)
	if err := SetImplementationDefault(base, "x", ImplBash); err != nil {
		t.Fatal(err)
	}
	if impl, _ := LoadImplementation(base); impl.Default != ImplBash {
		t.Error("block not added")
	}

	// Refusals: an unknown value, a service file, a broken file.
	if err := SetImplementationDefault(base, "x", "python"); err == nil {
		t.Error("python accepted")
	}
	os.WriteFile(file, []byte("build:\n  context: .\n"), 0o600)
	if err := SetImplementationDefault(base, "x", ImplGo); err == nil || !strings.Contains(err.Error(), `kind "" is not "Project"`) {
		t.Errorf("service file: %v", err)
	}
	os.WriteFile(file, []byte("- a list\n"), 0o600)
	if err := SetImplementationDefault(base, "x", ImplGo); err == nil {
		t.Error("a list accepted")
	}
	os.WriteFile(file, []byte("kind: Project\nspec: [\n"), 0o600)
	if err := SetImplementationDefault(base, "x", ImplGo); err == nil {
		t.Error("broken yaml accepted")
	}
}
