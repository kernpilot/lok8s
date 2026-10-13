// Package addons is the Go port of the addon render pipeline
// (.lok8s/libs/addons: addons::render) — the SINGLE canonical khelm render
// shared by the bootstrap engine and the KubeOne driver's pre-apply addon
// staging. The list/show/detail CLI half of libs/addons stays in bash; this
// package holds exactly what the bootstrap engine consumes.
//
// Boundary note: the render lives HERE (not inside internal/bootstrap)
// because the bash addons::render has a second consumer
// (kubeone::render_addons); the bootstrap engine imports this package the
// same way libs/bootstrap imports libs/addons.
package addons

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/internal/yqsem"
	"gopkg.in/yaml.v3"
)

// ProtectedEnvOverride reports the first variable (in byte order) an
// entry's `env:` may not set, or "" when there is none. These are lo's
// orchestration and safety variables: LOK8S_BOOTSTRAP_*, BOOTSTRAP_*,
// bootstrap_* (the bash engine's own names), PATH_SECRETS, KUBECONFIG and
// KUSTOMIZE_PLUGIN_HOME. An entry that names one is an entry error on a
// real run and on a dry run alike (bash: bootstrap::_protected_env_override).
func ProtectedEnvOverride(env map[string]string) string {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if IsProtectedEnv(k) {
			return k
		}
	}
	return ""
}

// IsProtectedEnv reports whether an entry's `env:` may not set name.
func IsProtectedEnv(name string) bool {
	for _, p := range []string{"LOK8S_BOOTSTRAP_", "BOOTSTRAP_", "bootstrap_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	switch name {
	case "PATH_SECRETS", "KUBECONFIG", KustomizePluginHomeEnv:
		return true
	}
	return false
}

// Render renders a framework addon via the canonical khelm path (bash:
// addons::render). Stacks values base < driver(kind) < provider < inline
// (later wins), rewrites the chart's valueFiles to the merged set, then
// `kustomize build --enable-alpha-plugins --enable-exec` (internal/render:
// in-process, or the pinned binary under LO_RENDER=exec) + envsubst
// (LOK8S_USER_*/LOK8S_SPEC_* placeholders) + the container-env tostring
// coercion.
//
// paths locates the project: in exec mode the kustomize child gets
// KUSTOMIZE_PLUGIN_HOME from config.KustomizePluginHome (the variable when
// set, else <Base>/.kustomize), the same value `lo doctor` prints. Without
// it a shell that exports only PATH left the child with no plugin home and
// the bootstrap render of every addon failed on `unable to find plugin
// root`.
//
// env carries the per-entry `env:` overrides (bash exports them in the
// entry's subshell before render): they join the kustomize process env AND
// the envsubst whitelist/lookup. In exec mode they ride the child's
// environment; in-process, render.Build installs them for the duration of
// the render under its mutex and restores them (concurrent DAG entries
// serialize on the render, never on each other's values).
func Render(ctx context.Context, paths *config.Paths, runner execx.Runner, stderr io.Writer, addonDir, kind, providerName, inlineValues string, env map[string]string) (string, error) {
	return renderAddon(ctx, paths, runner, stderr, addonDir, kind, providerName, inlineValues, env, nil, false)
}

// RenderDryRun is Render for `lo bootstrap --dry-run` (see dryrun.go). It
// renders in place like Render, with three differences:
//
//   - kustomize runs with the boundary's plugin home (lok8s' two built-in
//     plugins only), without --enable-exec, and with the refusing docker
//     and git first on PATH. allowGenerators keeps the real run's plugin
//     home, flags and PATH: the addon code then runs as trusted.
//   - a local chart, or a file:// dependency of it, that resolves outside
//     the project is an error.
//   - a local chart that khelm builds dependencies for (it writes
//     Chart.lock and charts/) renders from a copy in the boundary when the
//     real run would render it in place.
func RenderDryRun(ctx context.Context, paths *config.Paths, runner execx.Runner, stderr io.Writer, addonDir, kind, providerName, inlineValues string, env map[string]string, b *DryRunBoundary, allowGenerators bool) (string, error) {
	if b == nil {
		return "", errors.New("addons render: a dry run needs its boundary")
	}
	return renderAddon(ctx, paths, runner, stderr, addonDir, kind, providerName, inlineValues, env, b, allowGenerators)
}

func renderAddon(ctx context.Context, paths *config.Paths, runner execx.Runner, stderr io.Writer, addonDir, kind, providerName, inlineValues string, env map[string]string, dry *DryRunBoundary, allowGenerators bool) (string, error) {
	// The engine refuses a protected `env:` name before it renders (an
	// entry error, named); this is the backstop for any other caller.
	if v := ProtectedEnvOverride(env); v != "" {
		ui.ErrorTo(stderr, "addons::render: env: may not set %s", v)
		return "", fmt.Errorf("addons render: env: may not set %s", v)
	}
	buildDir, cleanup, err := stageBuildDir(paths, stderr, addonDir, kind, providerName, inlineValues, dry)
	defer cleanup()
	if err != nil {
		return "", err
	}

	// KHELM_TRUST_ANY_REPO: chart repos are declared in our own
	// version-controlled chart.yaml (the trust boundary is code review), so
	// trust them — no manual `helm repo add`, versions stay pinned.
	cmdEnv := []string{"KHELM_TRUST_ANY_REPO=true"}
	envNames := make([]string, 0, len(env))
	for k := range env {
		envNames = append(envNames, k)
	}
	sort.Strings(envNames)
	for _, k := range envNames {
		cmdEnv = append(cmdEnv, k+"="+env[k])
	}
	// A real run passes --enable-exec (KRM exec functions). A dry run does
	// not: the built-in plugins are legacy exec plugins, which
	// --enable-alpha-plugins enables alone, so a KRM exec function never
	// runs there. The dry run's own values come last, so they win over the
	// entry's env (os/exec keeps the last value of a key).
	enableExec := true
	if dry != nil {
		cmdEnv = append(cmdEnv, DryRunEnv+"=1", DryRunReportEnv+"="+dry.Report)
		if allowGenerators {
			cmdEnv = append(cmdEnv, AllowGeneratorsEnv+"=1")
		} else {
			enableExec = false
			path, ok := env["PATH"]
			if !ok {
				path = os.Getenv("PATH")
			}
			cmdEnv = append(cmdEnv, KustomizePluginHomeEnv+"="+dry.PluginHome, "PATH="+dry.StubPath(path))
		}
	}
	// In-process, kustomize resolves docker and git through THIS process's
	// PATH, so a dry run puts the refusing stubs first for the length of
	// the render (a dry run renders one entry at a time). The exec pipeline
	// gets the same PATH through the overlay above.
	if dry != nil && !allowGenerators {
		defer scopePath(dry.StubPath(os.Getenv("PATH")))()
	}
	// The render goes through internal/render: in-process by default, the
	// pinned kustomize binary via the runner seam under LO_RENDER=exec
	// (which is also how the hermetic tests stub it).
	out, err := render.Build(ctx, buildDir, render.Options{
		Paths:      paths,
		Runner:     runner,
		EnableExec: enableExec,
		Env:        cmdEnv,
		Stderr:     stderr,
	})
	if err != nil {
		return "", fmt.Errorf("addons render: kustomize build %s: %w", addonDir, err)
	}

	// Envsubst restricted to the whitelist (LOK8S_SPEC_*/LOK8S_USER_* from
	// the process env, plus the per-entry overrides).
	names := build.EnvsubstWhitelist()
	for _, k := range envNames {
		if strings.HasPrefix(k, "LOK8S_SPEC_") || strings.HasPrefix(k, "LOK8S_USER_") {
			names = append(names, k)
		}
	}
	substituted := build.EnvsubstWith(out, names, func(name string) string {
		if v, ok := env[name]; ok {
			return v
		}
		return os.Getenv(name)
	})

	// Container env `value`s MUST be strings (k8s schema). Helm coerces
	// numeric values (e.g. cilium's KUBERNETES_SERVICE_PORT from
	// k8sServicePort) back to ints and charts emit them unquoted → `kubectl
	// apply` rejects them. Coerce every container env value to a string;
	// `valueFrom` entries (no `value`) are left untouched.
	coerced, err := coerceEnvValues(substituted)
	if err != nil {
		return "", fmt.Errorf("addons render: env coercion for %s: %w", addonDir, err)
	}

	// A successful build that yields nothing means the addon rendered no
	// resources — almost always a misconfig. Fail loud rather than report
	// success for an empty apply.
	if strings.TrimSpace(string(coerced)) == "" {
		ui.ErrorTo(stderr, "addons::render: empty output for %s (no resources rendered)", addonDir)
		return "", fmt.Errorf("addons render: empty output for %s", addonDir)
	}
	return string(coerced), nil
}

// stageBuildDir returns the directory kustomize builds for one addon and
// the cleanup of anything it staged (bash: the build_dir half of
// addons::render): the addon directory itself, or a temp copy when values
// stack (inline values, values[.<kind>|.<provider>].yaml) and, in a dry
// run, when khelm would build a local chart's dependencies.
func stageBuildDir(paths *config.Paths, stderr io.Writer, addonDir, kind, providerName, inlineValues string, dry *DryRunBoundary) (buildDir string, cleanup func(), err error) {
	buildDir, cleanup = addonDir, func() {}
	if !fsutil.FileExists(filepath.Join(addonDir, "chart.yaml")) {
		return buildDir, cleanup, nil
	}
	// Stack values: base < driver < provider < inline (later wins).
	// Facts (provider env) beat preferences (driver flavor) beat chart
	// defaults (base); explicit caller intent (inline) beats all.
	var valueFiles []string
	if f := filepath.Join(addonDir, "values.yaml"); fsutil.FileExists(f) {
		valueFiles = append(valueFiles, f)
	}
	if f := filepath.Join(addonDir, "values."+kind+".yaml"); kind != "" && fsutil.FileExists(f) {
		valueFiles = append(valueFiles, f)
	}
	if f := filepath.Join(addonDir, "values."+providerName+".yaml"); providerName != "" && fsutil.FileExists(f) {
		valueFiles = append(valueFiles, f)
	}
	stack := inlineValues != "" || len(valueFiles) > 0
	// A dry run checks the local chart first: a chart or a file://
	// dependency outside the project is an entry error, and a chart
	// khelm builds dependencies for must not write into the project.
	var local *localChart
	if dry != nil {
		lc, err := resolveLocalChart(addonDir, paths.Base)
		if err != nil {
			ui.ErrorTo(stderr, "addons::render: %s: %v", addonDir, err)
			return "", cleanup, fmt.Errorf("addons render: %w", err)
		}
		local = lc
	}
	switch {
	case stack:
		// Copy to a temp build dir when there's overlay stacking to do.
		// A chart that pins its OWN chart.yaml valueFiles — e.g. a
		// target referencing a cross-tree base
		// `../../../../.lok8s/addons/<x>/values.yaml` (monitoring,
		// gatus) — must render IN PLACE: a copy breaks those relative
		// paths. Stacking addons carry no cross-tree refs.
		tmp, err := os.MkdirTemp(dryTempRoot(dry), "lok8s-addon-")
		if err != nil {
			ui.ErrorTo(stderr, "addons::render: failed to create temp dir for %s", addonDir)
			return "", cleanup, fmt.Errorf("addons render: temp dir: %w", err)
		}
		buildDir = tmp
		cleanup = func() { _ = os.RemoveAll(tmp) }
		// Dotfiles too (bash: `cp -r dir/.`) — a glob drops them and a
		// chart referencing a dotfile would fail to render. A dry run
		// copies a symlink as its target, so khelm can never write
		// through one into the project.
		copyFn := copyTree
		if dry != nil {
			copyFn = func(src, dst string) error { return copyTreeDeref(src, dst, 0) }
		}
		if err := copyFn(addonDir, tmp); err != nil {
			ui.ErrorTo(stderr, "addons::render: failed to copy %s to temp dir", addonDir)
			return "", cleanup, fmt.Errorf("addons render: copy: %w", err)
		}
		if inlineValues != "" {
			inline := filepath.Join(tmp, "values.inline.yaml")
			if err := os.WriteFile(inline, []byte(inlineValues+"\n"), 0o644); err != nil {
				ui.ErrorTo(stderr, "addons::render: failed to merge values for %s", addonDir)
				return "", cleanup, fmt.Errorf("addons render: write inline values: %w", err)
			}
			valueFiles = append(valueFiles, inline)
		}
		merged, err := MergeValueFiles(valueFiles...)
		if err != nil {
			ui.ErrorTo(stderr, "addons::render: failed to merge values for %s", addonDir)
			return "", cleanup, fmt.Errorf("addons render: merge values: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "values.merged.yaml"), merged, 0o644); err != nil {
			ui.ErrorTo(stderr, "addons::render: failed to merge values for %s", addonDir)
			return "", cleanup, fmt.Errorf("addons render: write merged values: %w", err)
		}
		if err := setChartValueFiles(filepath.Join(tmp, "chart.yaml")); err != nil {
			ui.ErrorTo(stderr, "addons::render: failed to set valueFiles in chart.yaml for %s", addonDir)
			return "", cleanup, fmt.Errorf("addons render: chart.yaml valueFiles: %w", err)
		}
	case local != nil && local.buildsDeps:
		// Dry run only: the real run renders this chart in place and
		// khelm writes Chart.lock and charts/ next to it. Render from a
		// copy that keeps the relative layout instead.
		tmp, err := os.MkdirTemp(dryTempRoot(dry), "lok8s-chart-")
		if err != nil {
			ui.ErrorTo(stderr, "addons::render: failed to create temp dir for %s", addonDir)
			return "", cleanup, fmt.Errorf("addons render: temp dir: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(tmp) }
		dir, err := local.mirror(tmp)
		if err != nil {
			ui.ErrorTo(stderr, "addons::render: %s: %v", addonDir, err)
			return "", cleanup, fmt.Errorf("addons render: copy: %w", err)
		}
		buildDir = dir
	}
	return buildDir, cleanup, nil
}

// scopePath sets PATH and returns the restore.
func scopePath(path string) (restore func()) {
	prev, had := os.LookupEnv("PATH")
	os.Setenv("PATH", path)
	return func() {
		if had {
			os.Setenv("PATH", prev)
		} else {
			os.Unsetenv("PATH")
		}
	}
}

// dryTempRoot is where a dry run keeps its copies: inside the boundary, so
// an interrupted run leaves nothing behind once the boundary is removed.
func dryTempRoot(dry *DryRunBoundary) string {
	if dry == nil {
		return ""
	}
	return dry.Dir
}

// copyTree copies src's CONTENTS into dst (bash: cp -r src/. dst/ —
// dotfiles included), preserving symlinks.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// setChartValueFiles rewrites chart.yaml's valueFiles to the merged set
// (bash: yq -i '.valueFiles = ["values.merged.yaml"]').
func setChartValueFiles(chartPath string) error {
	raw, err := os.ReadFile(chartPath)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return err
	}
	doc := yqsem.Deref(&root)
	if doc == nil || doc.Kind != yaml.MappingNode {
		return fmt.Errorf("chart.yaml is not a mapping")
	}
	entry := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "values.merged.yaml"},
	}}
	replaced := false
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "valueFiles" {
			doc.Content[i+1] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "valueFiles"}, entry)
	}
	out, err := marshalNode(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(chartPath, out, 0o644)
}

// coerceEnvValues applies the yq transform
//
//	(.. | select(tag == "!!map" and has("env") and (.env | tag == "!!seq"))
//	   | .env[] | select(has("value")) | .value) |= tostring
//
// over the whole (multi-doc) manifest stream: every mapping with an `env:`
// sequence gets each element's `value:` scalar coerced to a string.
func coerceEnvValues(data []byte) ([]byte, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var outDocs []string
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		walkCoerce(yqsem.Deref(&doc))
		out, err := marshalNode(&doc)
		if err != nil {
			return nil, err
		}
		outDocs = append(outDocs, string(out))
	}
	return []byte(strings.Join(outDocs, "---\n")), nil
}

func walkCoerce(n *yaml.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], yqsem.Deref(n.Content[i+1])
			if key.Value == "env" && val != nil && val.Kind == yaml.SequenceNode {
				for _, el := range val.Content {
					el = yqsem.Deref(el)
					if el == nil || el.Kind != yaml.MappingNode {
						continue
					}
					for j := 0; j+1 < len(el.Content); j += 2 {
						if el.Content[j].Value == "value" {
							coerceToString(el.Content[j+1])
						}
					}
				}
			}
			walkCoerce(val)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			walkCoerce(yqsem.Deref(c))
		}
	}
}

// coerceToString is yq's `tostring` on a scalar: non-string scalars keep
// their literal text but become quoted strings.
func coerceToString(n *yaml.Node) {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!str" {
		return
	}
	n.Tag = "!!str"
	n.Style = yaml.DoubleQuotedStyle
}

func marshalNode(n *yaml.Node) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
