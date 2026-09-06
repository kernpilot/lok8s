// Package scaffold is the Go port of .lok8s/libs/init — `lo init service`
// and `lo init test`: scaffold lok8s project/service config from a correct
// template.
//
// The point of these commands is that nobody hand-writes service config from
// imagination: the emitted lok8s.yaml is shaped to pass the per-service
// validator in .lok8s/tilt/Tiltfile (build: block required; build is a
// mapping; dockerfile/context are strings) and the services.yaml entry is
// shaped to pass _validate_services_yaml. Everything optional is emitted
// commented-out so the minimal artifact is also a correct one.
//
// The Playwright suite template ships INSIDE the binary (go:embed of
// templates/test — a byte-identical copy of .lok8s/libs/init.d/test, gated
// by TestEmbeddedTemplateMatchesFrameworkTree), so `lo init test` works
// without a synced .lok8s tree. Every emitted byte matches the bash
// implementation.
package scaffold

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrHandled marks an error whose message was already printed in the bash
// format ([error] … on stderr).
var ErrHandled = ui.ErrHandled // one sentinel for every package; see internal/ui

// templates is the vendored test scaffold tree. `all:` keeps the dotfiles
// (.gitignore) the bash `cp -R src/.` copied too.
//
//go:embed all:templates/test
var templates embed.FS

// TestTemplate returns the embedded Playwright suite template rooted at its
// top (the equivalent of ${PATH_LOK8S}/libs/init.d/test).
func TestTemplate() fs.FS {
	sub, err := fs.Sub(templates, "templates/test")
	if err != nil {
		panic("scaffold: embedded template tree missing: " + err.Error())
	}
	return sub
}

// serviceNameRe is the character allowlist / path-traversal guard (bash:
// init::_validate_name). Mirrors use::_set_active: a name becomes a
// directory and a services.yaml key, so reject anything that could escape
// the project or break the YAML key.
var serviceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidateName is init::_validate_name.
func ValidateName(name string, stderr io.Writer) error {
	if !serviceNameRe.MatchString(name) {
		ui.Errorf(stderr, "invalid service name: '%s' (must match ^[a-z0-9][a-z0-9._-]*$)", name)
		return ErrHandled
	}
	return nil
}

// ScaffoldLokYAML writes a bare service lok8s.yaml into path (bash:
// init::_scaffold_lokyaml).
//
// The active block is the smallest config the validator accepts: a `build:`
// mapping with string context/dockerfile. Every other knob (live_update,
// ports, workloads, tilt, the multi-image `components:` form) is emitted
// commented so the file documents the schema without tripping
// `_validate_service` (which rejects `components` as an unknown top-level
// key — multi-image repos swap `build:` for `components:`, they do not add
// it alongside).
//
// Idempotent: refuses to clobber an existing lok8s.yaml unless force.
func ScaffoldLokYAML(path, name string, force bool, out, stderr io.Writer) error {
	file := path + "/lok8s.yaml"
	if _, err := os.Stat(file); err == nil && !force {
		ui.Warnf(stderr, "exists, not overwriting: %s (re-run with --force to replace)", file)
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(lokYAML(name)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Scaffolded %s\n", file)
	return nil
}

// lokYAML is the per-service template, verbatim from the bash heredoc.
func lokYAML(name string) string {
	return "# Per-service lok8s.yaml for \"" + name + "\" — a bare object (no apiVersion/kind/\n" +
		"# metadata/spec). The build: block is handed verbatim to Tilt's docker_build().\n" +
		"# See docs/guide/services.md for the full schema.\n" +
		"build:\n" +
		"  context: .\n" +
		"  dockerfile: Dockerfile          # or lok8s.Dockerfile\n" +
		"  # live_update:                   # hot-reload without a full image rebuild\n" +
		"  #   sync: [{ local_path: ./src, remote_path: /app/src }]\n" +
		"  #   fall_back_on: { files: [package.json] }\n" +
		"# ports:                           # host:container forwards for local dev\n" +
		"#   - { from: 3000, to: 3000 }\n" +
		"# workloads: [" + name + "]             # optional; default [" + name + "]\n" +
		"# tilt: { labels: [" + name + "] }\n" +
		"# ---- multi-image repos: use `components:` instead of `build:` ----\n" +
		"# components:\n" +
		"#   - { name: api, build: { context: ., dockerfile: Dockerfile.api } }\n" +
		"#   - { name: worker, build: { context: ., dockerfile: Dockerfile.worker } }\n"
}

// servicesTemplate is the project-root services.yaml written when absent,
// verbatim from the bash heredoc.
const servicesTemplate = `# lok8s service definitions — committed.
# Personal overrides go in services.local.yaml (gitignored, loaded via
# LOK8S_SERVICE_CONFIG=local). See docs/guide/services.md for the full schema.

registry:
  endpoint: "${DOCKER_REGISTRY}"   # CI registry endpoint (env-substituted)
  branch: "${DOCKER_PROJECT}"      # PR/branch slug — what the CI publishes under
  tag: "${DOCKER_TAG}"             # commit SHA or build tag
  prefix: lok8s.local              # canonical local image prefix

defaults:
  build: true        # default for services.<name>.build (true = build locally + Tilt live-update)
  dockerfile: service

services: {}
`

// MergeServices adds services.<name>.path to servicesFile (bash:
// init::_merge_services).
//
// Creates services.yaml from a correct template (registry / defaults /
// services) when absent. When present, merges so existing entries are
// preserved — `lo init service` is additive and idempotent. The merged
// shape stays within the keys _validate_services_yaml allows
// (services.<name>.path). The rewrite is a yaml.Node round-trip, matching
// the bash `yq -i` byte for byte (comments kept, blank lines between
// top-level keys dropped, line comments re-spaced).
func MergeServices(servicesFile, name, path string, out, stderr io.Writer) error {
	if !fileExists(servicesFile) {
		if err := os.WriteFile(servicesFile, []byte(servicesTemplate), 0o644); err != nil {
			return err
		}
		ui.Debugf(stderr, "created services.yaml from template: %s", servicesFile)
	}
	if err := setServicePath(servicesFile, name, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Registered services.%s.path = %s in %s\n", name, path, servicesFile)
	return nil
}

// setServicePath is `yq -i '.services[NAME].path = PATH'`: find-or-create
// the services map, the <name> map, and set path as a string scalar.
func setServicePath(file, name, path string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if root.Kind == 0 {
		// Empty document: yq materializes a fresh mapping.
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", file)
	}
	services := ensureMap(doc, "services")
	entry := ensureMap(services, name)
	setScalar(entry, "path", path)

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

// ensureMap returns the mapping under key, creating it (or converting a
// null/flow-empty value) as a block mapping.
func ensureMap(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind != yaml.MappingNode {
				v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				m.Content[i+1] = v
			}
			// A `{}` value carries flow style; adding entries must not emit
			// `{foo: {path: x}}` — yq re-emits it as a block mapping.
			v.Style = 0
			return v
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func setScalar(m *yaml.Node, key, value string) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = node
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
}

// canonicalTiltfile is the thin loader the project-root Tiltfile must be.
const canonicalTiltfile = "load('./.lok8s/tilt/Tiltfile', 'lok8s')\nlok8s()\n"

var dockerBuildRe = regexp.MustCompile(`\bdocker_build\b`)

// EnsureTiltfile guarantees the canonical 2-line Tiltfile (bash:
// init::_ensure_tiltfile).
//
// The project-root Tiltfile must be a thin loader so the lok8s extension
// owns all the build/live-update logic. If it does not exist we write it. If
// it exists and already loads the extension we leave it alone. If it exists
// but hardcodes docker_build (the legacy hand-rolled form), we DO NOT
// clobber it — replacing it could drop bespoke logic — and instead warn
// loudly.
func EnsureTiltfile(tiltfile string, out, stderr io.Writer) error {
	raw, err := os.ReadFile(tiltfile)
	if err != nil {
		if err := os.WriteFile(tiltfile, []byte(canonicalTiltfile), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote canonical Tiltfile: %s\n", tiltfile)
		return nil
	}
	body := string(raw)
	if strings.Contains(body, "load('./.lok8s/tilt/Tiltfile', 'lok8s')") {
		ui.Debugf(stderr, "Tiltfile already loads the lok8s extension: %s", tiltfile)
		return nil
	}
	if dockerBuildRe.MatchString(body) {
		ui.Warnf(stderr, "Tiltfile %s hardcodes docker_build — NOT overwriting.", tiltfile)
		ui.Warnf(stderr, "Replace its body with the canonical 2-line form so the lok8s extension owns builds:")
		ui.Warnf(stderr, "    load('./.lok8s/tilt/Tiltfile', 'lok8s')")
		ui.Warnf(stderr, "    lok8s()")
		return nil
	}
	// Exists, no docker_build, but also no extension load — unknown custom
	// form.
	ui.Warnf(stderr, "Tiltfile %s exists but does not load the lok8s extension.", tiltfile)
	ui.Warnf(stderr, "Add the canonical 2-line form so services are picked up:")
	ui.Warnf(stderr, "    load('./.lok8s/tilt/Tiltfile', 'lok8s')")
	ui.Warnf(stderr, "    lok8s()")
	return nil
}

// Service orchestrates the service scaffold (bash: init::_service). base is
// the project root (PATH_BASE); path defaults to ./<name>.
func Service(base, name, path string, force bool, out, stderr io.Writer) error {
	if name == "" {
		ui.Errorf(stderr, "service name is required: lo init service <name>")
		return ErrHandled
	}
	if err := ValidateName(name, stderr); err != nil {
		return err
	}
	if path == "" {
		path = "./" + name
	}
	if err := ScaffoldLokYAML(path, name, force, out, stderr); err != nil {
		return err
	}
	if err := MergeServices(base+"/services.yaml", name, path, out, stderr); err != nil {
		return err
	}
	if err := EnsureTiltfile(base+"/Tiltfile", out, stderr); err != nil {
		return err
	}
	fmt.Fprintf(out, "Done. Next: add a Dockerfile in %s, then run 'lo up' (or 'tilt up').\n", path)
	return nil
}

// ScaffoldTests copies the test scaffold into dest (bash:
// init::_scaffold_tests).
//
// Copies the GENERIC, project- and domain-agnostic Playwright suite template
// into the project's tests/ dir. The template carries NO project-specific
// hostnames or specs — it is parameterized entirely by LOK8S_TEST_DOMAIN /
// config.urls and the SERVICES map the user edits.
//
// Idempotent + safe: refuses to write into a NON-EMPTY destination unless
// force, so an existing suite is never clobbered. With force it overwrites
// file-by-file (it does NOT delete files the template doesn't ship, so local
// additions survive).
func ScaffoldTests(src fs.FS, dest string, force bool, out, stderr io.Writer) error {
	if info, err := os.Stat(dest); err == nil {
		if !info.IsDir() {
			ui.Errorf(stderr, "destination exists and is not a directory: %s", dest)
			return ErrHandled
		}
		// Non-empty (any content counts, dotfiles included) and not forced
		// -> refuse, to protect an existing suite.
		entries, _ := os.ReadDir(dest)
		if len(entries) > 0 && !force {
			ui.Warnf(stderr, "exists and is not empty, not overwriting: %s (re-run with --force)", dest)
			return nil
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := copyFS(src, dest); err != nil {
		return err
	}
	fmt.Fprintf(out, "Scaffolded Playwright test suite into %s\n", dest)
	return nil
}

// copyFS writes every file of src under dest (bash: cp -R src/. dest/ —
// dotfiles included, existing files overwritten, extra files kept).
func copyFS(src fs.FS, dest string) error {
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		target := filepath.Join(dest, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// Tests orchestrates the test scaffold (bash: init::_test). base is the
// project root; path defaults to <base>/tests.
func Tests(src fs.FS, base, path string, force bool, out, stderr io.Writer) error {
	if path == "" {
		path = base + "/tests"
	}
	if err := ScaffoldTests(src, path, force, out, stderr); err != nil {
		return err
	}
	fmt.Fprintln(out, "Done. Next:")
	fmt.Fprintf(out, "  cd %s && npm install && npx playwright install chromium\n", path)
	fmt.Fprintln(out, "  edit utils/config.ts (the SERVICES map) for your stack, then:")
	fmt.Fprintln(out, "  LOK8S_TEST_DOMAIN=<your-domain> npm test")
	return nil
}

// TemplateFiles lists the template's file paths (slash-separated, sorted) —
// the drift test's enumeration.
func TemplateFiles(src fs.FS) ([]string, error) {
	var files []string
	err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
