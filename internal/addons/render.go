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

// protectedEnvNames are the safety variables no entry's `env:` may set
// (review round 2, item B) — the bootstrap engine's own PATH_SECRETS/HOME/
// XDG_*/CAROOT/HELM_*/KUBECONFIG/LOK8S_BOOTSTRAP_* sandbox, never an
// addon's. renderAddon forces every one of these back to the engine's own
// current value AFTER merging the entry's env (below), and
// ProtectedEnvOverride lets a caller detect the attempt and report it
// explicitly instead of letting it silently do nothing.
var protectedEnvNames = []string{
	"LOK8S_BOOTSTRAP_DRYRUN", "LOK8S_BOOTSTRAP_ALLOW_GENERATORS",
	"PATH_SECRETS",
	"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME",
	"CAROOT", "HELM_CACHE_HOME", "HELM_CONFIG_HOME", "HELM_DATA_HOME",
	"KUBECONFIG",
}

// ProtectedEnv is protectedEnvNames, exported for the bootstrap engine's
// sandbox setup (internal/bootstrap/isolation.go) and for either side's
// tests — one list, so the sandbox and the override guard can never drift
// apart on what counts as protected.
func ProtectedEnv() []string { return append([]string{}, protectedEnvNames...) }

// ProtectedEnvOverride reports the first protected variable name env
// attempts to set, or "" when none does. Called before a render, by both
// applyOne and dryRunOne, so an entry that tries to override one of these
// fails loudly, naming the variable, rather than having its attempt
// silently neutralized by the render.go merge order below.
func ProtectedEnvOverride(env map[string]string) string {
	for _, k := range protectedEnvNames {
		if _, ok := env[k]; ok {
			return k
		}
	}
	return ""
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
	return renderAddon(ctx, paths, runner, stderr, addonDir, kind, providerName, inlineValues, env, false)
}

// RenderIsolated is Render for a read-only caller (the bootstrap engine's
// --dry-run): a chart.yaml addon that needs no value-stack copy normally
// renders IN PLACE against the real addon directory (see the comment below)
// — fine for a real apply, but khelm's chart-dependency resolution can still
// write Chart.lock/charts there, which a dry run must never do. Force the
// temp-dir copy for EVERY chart.yaml addon, stacked or not, so nothing lands
// outside --render-dir.
//
// Known, accepted limitation: a chart that pins its OWN chart.yaml
// valueFiles to a path OUTSIDE its directory (a cross-tree ref, e.g.
// monitoring/gatus's `../../../../.lok8s/addons/<x>/values.yaml`) breaks
// once copied — the relative path no longer resolves from the temp dir. The
// render then fails and the entry reports `error`, exactly like any other
// render failure; a real apply (or `lo build` for that target) still works.
// Trading that for "a dry run against production never writes into the
// project" is the right side of the line.
// allowGenerators controls the exec-plugin allowlist (review round 2, item
// C): when false (the default), a generators:/transformers:/validators:
// reference anywhere in addonDir's kustomization tree whose apiVersion+kind
// is not lok8s' own built-in set (the secrets generator, khelm's
// ChartRenderer) is refused BEFORE kustomize ever runs it. The `bash:`
// secrets sub-generator keeps its OWN separate refusal inside the secrets
// plugin itself (it is a builtin, so this scan waves it through, but the
// LOK8S_BOOTSTRAP_ALLOW_GENERATORS env var — set by the caller alongside
// this flag — still gates whether it actually executes).
func RenderIsolated(ctx context.Context, paths *config.Paths, runner execx.Runner, stderr io.Writer, addonDir, kind, providerName, inlineValues string, env map[string]string, allowGenerators bool) (string, error) {
	if !allowGenerators {
		refs, err := ScanExecPlugins(addonDir)
		if err != nil {
			return "", fmt.Errorf("addons render: exec-plugin scan for %s: %w", addonDir, err)
		}
		if len(refs) > 0 {
			return "", fmt.Errorf("exec plugin %s/%s not run in dry-run (pass --allow-generators to run it): %s", refs[0].APIVersion, refs[0].Kind, refs[0].Path)
		}
	}
	return renderAddon(ctx, paths, runner, stderr, addonDir, kind, providerName, inlineValues, env, true)
}

func renderAddon(ctx context.Context, paths *config.Paths, runner execx.Runner, stderr io.Writer, addonDir, kind, providerName, inlineValues string, env map[string]string, forceCopy bool) (string, error) {
	buildDir := addonDir
	if fsutil.FileExists(filepath.Join(addonDir, "chart.yaml")) {
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
		// Copy to a temp build dir when there's overlay stacking to do, OR
		// (RenderIsolated only) unconditionally, to keep khelm's dependency
		// resolution off the real tree. A chart that pins its OWN chart.yaml
		// valueFiles — e.g. a target referencing a cross-tree base
		// `../../../../.lok8s/addons/<x>/values.yaml` (monitoring, gatus) —
		// must render IN PLACE on a real apply: a copy breaks those relative
		// paths. Stacking addons carry no cross-tree refs.
		if inlineValues != "" || len(valueFiles) > 0 || forceCopy {
			tmp, err := os.MkdirTemp("", "lok8s-addon-")
			if err != nil {
				ui.ErrorTo(stderr, "addons::render: failed to create temp dir for %s", addonDir)
				return "", fmt.Errorf("addons render: temp dir: %w", err)
			}
			buildDir = tmp
			defer func() { _ = os.RemoveAll(tmp) }()
			// Dotfiles too (bash: `cp -r dir/.`) — a glob drops them and a
			// chart referencing a dotfile would fail to render.
			if err := copyTree(addonDir, tmp); err != nil {
				ui.ErrorTo(stderr, "addons::render: failed to copy %s to temp dir", addonDir)
				return "", fmt.Errorf("addons render: copy: %w", err)
			}
			// The merge/chart.yaml-rewrite below is for ACTUAL stacking
			// only — a forceCopy-only copy (RenderIsolated, nothing to
			// stack) renders from chart.yaml's own valueFiles, unchanged,
			// now resolved from the copy (see the cross-tree-ref caveat on
			// RenderIsolated's doc comment).
			if inlineValues != "" || len(valueFiles) > 0 {
				if inlineValues != "" {
					inline := filepath.Join(tmp, "values.inline.yaml")
					if err := os.WriteFile(inline, []byte(inlineValues+"\n"), 0o644); err != nil {
						ui.ErrorTo(stderr, "addons::render: failed to merge values for %s", addonDir)
						return "", fmt.Errorf("addons render: write inline values: %w", err)
					}
					valueFiles = append(valueFiles, inline)
				}
				merged, err := MergeValueFiles(valueFiles...)
				if err != nil {
					ui.ErrorTo(stderr, "addons::render: failed to merge values for %s", addonDir)
					return "", fmt.Errorf("addons render: merge values: %w", err)
				}
				if err := os.WriteFile(filepath.Join(tmp, "values.merged.yaml"), merged, 0o644); err != nil {
					ui.ErrorTo(stderr, "addons::render: failed to merge values for %s", addonDir)
					return "", fmt.Errorf("addons render: write merged values: %w", err)
				}
				if err := setChartValueFiles(filepath.Join(tmp, "chart.yaml")); err != nil {
					ui.ErrorTo(stderr, "addons::render: failed to set valueFiles in chart.yaml for %s", addonDir)
					return "", fmt.Errorf("addons render: chart.yaml valueFiles: %w", err)
				}
			}
		}
	}

	// KHELM_TRUST_ANY_REPO: chart repos are declared in our own
	// version-controlled chart.yaml (the trust boundary is code review), so
	// trust them — no manual `helm repo add`, versions stay pinned.
	// --enable-exec: secrets.lok8s.dev is an exec plugin (an addon may carry
	// a Secret.*.yaml generator next to its chart).
	cmdEnv := []string{"KHELM_TRUST_ANY_REPO=true"}
	envNames := make([]string, 0, len(env))
	for k := range env {
		envNames = append(envNames, k)
	}
	sort.Strings(envNames)
	for _, k := range envNames {
		cmdEnv = append(cmdEnv, k+"="+env[k])
	}
	// The engine's own safety variables are forced back AFTER the entry's
	// env is merged (review round 2, item B): a kustomize child's
	// environment is the whole sandbox's trust boundary (KUBECONFIG, HOME,
	// the secrets store, …), and os/exec keeps only the LAST value for a
	// duplicate key — so an entry's `env:` naming one of these must never
	// be the one that survives. ProtectedEnvOverride, called by the
	// bootstrap engine before this is ever reached, is what turns an
	// attempt into a hard, named error instead of a silent no-op; this is
	// the backstop that holds even if a future caller skips that check.
	for _, k := range ProtectedEnv() {
		if v, ok := os.LookupEnv(k); ok {
			cmdEnv = append(cmdEnv, k+"="+v)
		}
	}
	// The render goes through internal/render: in-process by default, the
	// pinned kustomize binary via the runner seam under LO_RENDER=exec
	// (which is also how the hermetic tests stub it).
	out, err := render.Build(ctx, buildDir, render.Options{
		Paths:      paths,
		Runner:     runner,
		EnableExec: true,
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
