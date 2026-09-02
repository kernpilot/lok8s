package scaffold

// Port of tests/unit/init_test.bats. The bash suite pinned three guarantees:
// the emitted lok8s.yaml passes the per-service validator, services.yaml
// gains a services.<name>.path entry (template when absent, merge when not),
// and the Tiltfile is the canonical 2-line loader (never clobbering a
// hand-rolled docker_build). Plus idempotency, the name guard, and the
// Playwright scaffold's no-clobber / force semantics.

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"api", "my-svc.2"} {
		if err := ValidateName(ok, io.Discard); err != nil {
			t.Errorf("%q: unexpected reject", ok)
		}
	}
	for _, bad := range []string{"../evil", "foo/bar", "Foo", ""} {
		var stderr bytes.Buffer
		if err := ValidateName(bad, &stderr); err == nil {
			t.Errorf("%q: expected reject", bad)
		} else if !strings.Contains(stderr.String(), "invalid service name") {
			t.Errorf("%q: message: %s", bad, stderr.String())
		}
	}
}

// The scaffolded lok8s.yaml must satisfy _validate_service: only the build
// key active (comments are not parsed), build a mapping with string
// context/dockerfile.
func TestScaffoldLokYAMLSatisfiesValidator(t *testing.T) {
	dir := t.TempDir()
	if err := ScaffoldLokYAML(dir+"/foo", "foo", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "foo", "lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 1 {
		t.Fatalf("top-level keys = %v, want only build", doc)
	}
	build, ok := doc["build"].(map[string]any)
	if !ok {
		t.Fatalf("build is %T, want mapping", doc["build"])
	}
	if _, ok := build["context"].(string); !ok {
		t.Errorf("build.context = %v, want string", build["context"])
	}
	if _, ok := build["dockerfile"].(string); !ok {
		t.Errorf("build.dockerfile = %v, want string", build["dockerfile"])
	}
	// The template mentions the service name in the commented knobs.
	if !strings.Contains(string(raw), "# workloads: [foo]") {
		t.Errorf("name not substituted:\n%s", raw)
	}
}

func TestScaffoldLokYAMLNoClobberWithoutForce(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/foo", 0o755)
	os.WriteFile(dir+"/foo/lok8s.yaml", []byte("build: { context: ., dockerfile: Keep }\n"), 0o644)
	var stderr bytes.Buffer
	if err := ScaffoldLokYAML(dir+"/foo", "foo", false, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "not overwriting") {
		t.Errorf("stderr: %s", stderr.String())
	}
	raw, _ := os.ReadFile(dir + "/foo/lok8s.yaml")
	if !strings.Contains(string(raw), "Keep") {
		t.Errorf("file clobbered:\n%s", raw)
	}

	if err := ScaffoldLokYAML(dir+"/foo", "foo", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(dir + "/foo/lok8s.yaml")
	if !strings.Contains(string(raw), "dockerfile: Dockerfile") {
		t.Errorf("force did not overwrite:\n%s", raw)
	}
}

// yqMergedTemplate is what the bash `yq -i` (mikefarah yq v4.53) emits for the
// fresh services.yaml template plus `services.foo.path = ./foo`. Captured
// from the bash implementation; the Go round-trip must match it byte for
// byte (hack/parity-leaves.sh re-checks against the live yq).
const yqMergedTemplate = `# lok8s service definitions — committed.
# Personal overrides go in services.local.yaml (gitignored, loaded via
# LOK8S_SERVICE_CONFIG=local). See docs/guide/services.md for the full schema.

registry:
  endpoint: "${DOCKER_REGISTRY}" # CI registry endpoint (env-substituted)
  branch: "${DOCKER_PROJECT}" # PR/branch slug — what the CI publishes under
  tag: "${DOCKER_TAG}" # commit SHA or build tag
  prefix: lok8s.local # canonical local image prefix
defaults:
  build: true # default for services.<name>.build (true = build locally + Tilt live-update)
  dockerfile: service
services:
  foo:
    path: ./foo
`

func TestMergeServicesCreatesTemplateAndMatchesYq(t *testing.T) {
	dir := t.TempDir()
	s := dir + "/services.yaml"
	if err := MergeServices(s, "foo", "./foo", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s)
	if string(raw) != yqMergedTemplate {
		t.Errorf("merged services.yaml differs from the yq output:\n--- got\n%s\n--- want\n%s", raw, yqMergedTemplate)
	}
	var doc struct {
		Registry struct {
			Prefix string `yaml:"prefix"`
		} `yaml:"registry"`
		Defaults struct {
			Build      bool   `yaml:"build"`
			Dockerfile string `yaml:"dockerfile"`
		} `yaml:"defaults"`
		Services map[string]struct {
			Path string `yaml:"path"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Registry.Prefix != "lok8s.local" || doc.Defaults.Dockerfile != "service" || !doc.Defaults.Build {
		t.Errorf("template shape: %+v", doc)
	}
	if doc.Services["foo"].Path != "./foo" {
		t.Errorf("services.foo.path = %q", doc.Services["foo"].Path)
	}

	// Additive merge preserves siblings (and the bash yq output order).
	if err := MergeServices(s, "bar", "./services/bar", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(s)
	want := yqMergedTemplate + "  bar:\n    path: ./services/bar\n"
	if string(raw) != want {
		t.Errorf("second merge:\n--- got\n%s\n--- want\n%s", raw, want)
	}
}

func TestMergeServicesRewritesExistingEntry(t *testing.T) {
	dir := t.TempDir()
	s := dir + "/services.yaml"
	os.WriteFile(s, []byte("services:\n  foo:\n    path: ./old\n    build: false\n"), 0o644)
	if err := MergeServices(s, "foo", "./new", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s)
	if want := "services:\n  foo:\n    path: ./new\n    build: false\n"; string(raw) != want {
		t.Errorf("got:\n%s\nwant:\n%s", raw, want)
	}
}

func TestEnsureTiltfile(t *testing.T) {
	dir := t.TempDir()
	tf := dir + "/Tiltfile"

	// Absent → canonical form.
	var out bytes.Buffer
	if err := EnsureTiltfile(tf, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(tf)
	if string(raw) != canonicalTiltfile {
		t.Errorf("wrote:\n%s", raw)
	}
	if !strings.Contains(out.String(), "Wrote canonical Tiltfile") {
		t.Errorf("stdout: %s", out.String())
	}

	// Already canonical (+ a note) → untouched.
	os.WriteFile(tf, []byte("load('./.lok8s/tilt/Tiltfile', 'lok8s')\nlok8s()\n# my note\n"), 0o644)
	if err := EnsureTiltfile(tf, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(tf)
	if !strings.Contains(string(raw), "# my note") {
		t.Error("canonical Tiltfile was rewritten")
	}

	// Hand-rolled docker_build → NOT clobbered, loud warning.
	os.WriteFile(tf, []byte("docker_build('img', '.')\nk8s_yaml('d.yaml')\n"), 0o644)
	var stderr bytes.Buffer
	if err := EnsureTiltfile(tf, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "NOT overwriting") {
		t.Errorf("stderr: %s", stderr.String())
	}
	raw, _ = os.ReadFile(tf)
	if !strings.Contains(string(raw), "docker_build('img', '.')") {
		t.Error("docker_build Tiltfile clobbered")
	}

	// Unknown custom form → warning, untouched.
	os.WriteFile(tf, []byte("print('hi')\n"), 0o644)
	stderr.Reset()
	if err := EnsureTiltfile(tf, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "does not load the lok8s extension") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestServiceEndToEnd(t *testing.T) {
	base := t.TempDir()
	var out bytes.Buffer
	if err := Service(base, "foo", base+"/foo", false, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base + "/foo/lok8s.yaml"); err != nil {
		t.Error("lok8s.yaml missing")
	}
	raw, _ := os.ReadFile(base + "/services.yaml")
	if !strings.Contains(string(raw), "    path: "+base+"/foo\n") {
		t.Errorf("services.yaml:\n%s", raw)
	}
	tf, _ := os.ReadFile(base + "/Tiltfile")
	if string(tf) != canonicalTiltfile {
		t.Errorf("Tiltfile:\n%s", tf)
	}
	if !strings.Contains(out.String(), "Done. Next: add a Dockerfile in "+base+"/foo,") {
		t.Errorf("stdout: %s", out.String())
	}
}

func TestServiceDefaultsPathAndIdempotency(t *testing.T) {
	base := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if err := Service(base, "foo", "", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(base + "/services.yaml")
	if !strings.Contains(string(raw), "    path: ./foo\n") {
		t.Errorf("default path not ./<name>:\n%s", raw)
	}
	if _, err := os.Stat(base + "/foo/lok8s.yaml"); err != nil {
		t.Error("lok8s.yaml missing at ./foo")
	}

	// Re-run without force keeps a user edit; with force replaces it.
	f, _ := os.OpenFile(base+"/foo/lok8s.yaml", os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("# user-edit-marker\n")
	f.Close()
	if err := Service(base, "foo", "", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(base + "/foo/lok8s.yaml")
	if !strings.Contains(string(raw), "# user-edit-marker") {
		t.Error("re-run without force clobbered the lok8s.yaml")
	}
	if err := Service(base, "foo", "", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(base + "/foo/lok8s.yaml")
	if strings.Contains(string(raw), "# user-edit-marker") {
		t.Error("force did not overwrite the lok8s.yaml")
	}
}

func TestServiceGuards(t *testing.T) {
	base := t.TempDir()
	var stderr bytes.Buffer
	if err := Service(base, "../evil", "", false, io.Discard, &stderr); err == nil {
		t.Error("traversal name accepted")
	}
	if !strings.Contains(stderr.String(), "invalid service name") {
		t.Errorf("stderr: %s", stderr.String())
	}
	if _, err := os.Stat(base + "/services.yaml"); err == nil {
		t.Error("services.yaml written for a rejected name")
	}
	stderr.Reset()
	if err := Service(base, "", "", false, io.Discard, &stderr); err == nil {
		t.Error("empty name accepted")
	}
	if !strings.Contains(stderr.String(), "required") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

// ── lo init test (Playwright scaffold) ─────────────────────────────────

func TestScaffoldTestsCopiesTheGenericSuite(t *testing.T) {
	dest := t.TempDir() + "/tests"
	if err := ScaffoldTests(TestTemplate(), dest, false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"playwright.config.ts", "utils/config.ts", "utils/mailpit.ts", "utils/resolver.ts",
		"utils/tls.ts", "fixtures/test.ts", "pages/BasePage.ts", "config/dev.ts",
		"setup/auth.setup.ts", "package.json", ".gitignore",
	} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("%s not scaffolded", f)
		}
	}
	// Generic: no kubehz hostnames/specifics; the neutral LOK8S_TEST_ prefix.
	filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		if bytes.Contains(raw, []byte("kubehz")) || bytes.Contains(raw, []byte("KUBEHZ_")) {
			t.Errorf("%s carries kubehz specifics", path)
		}
		return nil
	})
	cfg, _ := os.ReadFile(filepath.Join(dest, "utils", "config.ts"))
	if !bytes.Contains(cfg, []byte("LOK8S_TEST_DOMAIN")) {
		t.Error("utils/config.ts lacks LOK8S_TEST_DOMAIN")
	}
}

func TestScaffoldTestsNoClobberAndForce(t *testing.T) {
	dest := t.TempDir() + "/tests"
	os.MkdirAll(dest, 0o755)
	os.WriteFile(dest+"/MINE.txt", []byte("keep me\n"), 0o644)

	var stderr bytes.Buffer
	if err := ScaffoldTests(TestTemplate(), dest, false, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "not overwriting") {
		t.Errorf("stderr: %s", stderr.String())
	}
	if _, err := os.Stat(dest + "/playwright.config.ts"); err == nil {
		t.Error("non-empty dir was written into without force")
	}

	if err := ScaffoldTests(TestTemplate(), dest, true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest + "/playwright.config.ts"); err != nil {
		t.Error("force did not write the template")
	}
	if _, err := os.Stat(dest + "/MINE.txt"); err != nil {
		t.Error("force wiped a local addition (copy, not wipe)")
	}

	// A file at the destination is an error, not a clobber.
	file := t.TempDir() + "/notadir"
	os.WriteFile(file, []byte("x"), 0o644)
	stderr.Reset()
	if err := ScaffoldTests(TestTemplate(), file, false, io.Discard, &stderr); err == nil {
		t.Error("file destination accepted")
	}
	if !strings.Contains(stderr.String(), "not a directory") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestTestsDefaultsDestination(t *testing.T) {
	base := t.TempDir()
	var out bytes.Buffer
	if err := Tests(TestTemplate(), base, "", false, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base + "/tests/playwright.config.ts"); err != nil {
		t.Error("default destination is not <base>/tests")
	}
	if !strings.Contains(out.String(), "  cd "+base+"/tests && npm install") {
		t.Errorf("stdout: %s", out.String())
	}
}

// The embedded template is a vendored copy of .lok8s/libs/init.d/test — the
// tree the bash implementation copies at runtime. Both must stay identical:
// same file set, same bytes.
func TestEmbeddedTemplateMatchesFrameworkTree(t *testing.T) {
	fw, err := filepath.Abs(filepath.Join("..", "..", ".lok8s", "libs", "init.d", "test"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(fw); err != nil || !info.IsDir() {
		t.Skip("framework template tree not available")
	}
	embedded, err := TemplateFiles(TestTemplate())
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) < 10 {
		t.Fatalf("embedded template suspiciously small: %v", embedded)
	}
	var onDisk []string
	filepath.Walk(fw, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(fw, path)
		onDisk = append(onDisk, filepath.ToSlash(rel))
		return nil
	})
	if strings.Join(embedded, "\n") != strings.Join(sorted(onDisk), "\n") {
		t.Fatalf("file set drift\nembedded: %v\non disk:  %v", embedded, onDisk)
	}
	for _, f := range embedded {
		want, _ := os.ReadFile(filepath.Join(fw, filepath.FromSlash(f)))
		got, _ := fs.ReadFile(TestTemplate(), f)
		if !bytes.Equal(want, got) {
			t.Errorf("%s: embedded bytes differ from .lok8s/libs/init.d/test", f)
		}
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
