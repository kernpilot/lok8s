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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
	"gopkg.in/yaml.v3"
)

// Render renders a framework addon via the canonical khelm path (bash:
// addons::render). Stacks values base < driver(kind) < provider < inline
// (later wins), rewrites the chart's valueFiles to the merged set, then
// `kustomize build --enable-alpha-plugins --enable-exec` + envsubst
// (LOK8S_USER_*/LOK8S_SPEC_* placeholders) + the container-env tostring
// coercion.
//
// env carries the per-entry `env:` overrides (bash exports them in the
// entry's subshell before render): they join the kustomize process env AND
// the envsubst whitelist/lookup, without ever touching the shared process
// environment (concurrent DAG entries would race on it).
func Render(ctx context.Context, runner execx.Runner, stderr io.Writer, addonDir, kind, providerName, inlineValues string, env map[string]string) (string, error) {
	buildDir := addonDir
	if fileExists(filepath.Join(addonDir, "chart.yaml")) {
		// Stack values: base < driver < provider < inline (later wins).
		// Facts (provider env) beat preferences (driver flavor) beat chart
		// defaults (base); explicit caller intent (inline) beats all.
		var valueFiles []string
		if f := filepath.Join(addonDir, "values.yaml"); fileExists(f) {
			valueFiles = append(valueFiles, f)
		}
		if f := filepath.Join(addonDir, "values."+kind+".yaml"); kind != "" && fileExists(f) {
			valueFiles = append(valueFiles, f)
		}
		if f := filepath.Join(addonDir, "values."+providerName+".yaml"); providerName != "" && fileExists(f) {
			valueFiles = append(valueFiles, f)
		}
		// Copy to a temp build dir ONLY when there's overlay stacking to do.
		// A chart that pins its OWN chart.yaml valueFiles — e.g. a target
		// referencing a cross-tree base
		// `../../../../.lok8s/addons/<x>/values.yaml` (monitoring, gatus) —
		// must render IN PLACE: a copy breaks those relative paths.
		// Stacking addons carry no cross-tree refs.
		if inlineValues != "" || len(valueFiles) > 0 {
			tmp, err := os.MkdirTemp("", "lok8s-addon-")
			if err != nil {
				ui.Errorf(stderr, "addons::render: failed to create temp dir for %s", addonDir)
				return "", fmt.Errorf("addons render: temp dir: %w", err)
			}
			buildDir = tmp
			defer func() { _ = os.RemoveAll(tmp) }()
			// Dotfiles too (bash: `cp -r dir/.`) — a glob drops them and a
			// chart referencing a dotfile would fail to render.
			if err := copyTree(addonDir, tmp); err != nil {
				ui.Errorf(stderr, "addons::render: failed to copy %s to temp dir", addonDir)
				return "", fmt.Errorf("addons render: copy: %w", err)
			}
			if inlineValues != "" {
				inline := filepath.Join(tmp, "values.inline.yaml")
				if err := os.WriteFile(inline, []byte(inlineValues+"\n"), 0o644); err != nil {
					ui.Errorf(stderr, "addons::render: failed to merge values for %s", addonDir)
					return "", fmt.Errorf("addons render: write inline values: %w", err)
				}
				valueFiles = append(valueFiles, inline)
			}
			merged, err := MergeValueFiles(valueFiles...)
			if err != nil {
				ui.Errorf(stderr, "addons::render: failed to merge values for %s", addonDir)
				return "", fmt.Errorf("addons render: merge values: %w", err)
			}
			if err := os.WriteFile(filepath.Join(tmp, "values.merged.yaml"), merged, 0o644); err != nil {
				ui.Errorf(stderr, "addons::render: failed to merge values for %s", addonDir)
				return "", fmt.Errorf("addons render: write merged values: %w", err)
			}
			if err := setChartValueFiles(filepath.Join(tmp, "chart.yaml")); err != nil {
				ui.Errorf(stderr, "addons::render: failed to set valueFiles in chart.yaml for %s", addonDir)
				return "", fmt.Errorf("addons render: chart.yaml valueFiles: %w", err)
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
	var out strings.Builder
	err := runner.Run(ctx, execx.Cmd{
		Name:   "kustomize",
		Args:   []string{"build", "--enable-alpha-plugins", "--enable-exec", buildDir},
		Env:    cmdEnv,
		Stdout: &out,
		Stderr: stderr,
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
	substituted := build.EnvsubstWith([]byte(out.String()), names, func(name string) string {
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
		ui.Errorf(stderr, "addons::render: empty output for %s (no resources rendered)", addonDir)
		return "", fmt.Errorf("addons render: empty output for %s", addonDir)
	}
	return string(coerced), nil
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
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
	doc := derefNode(&root)
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
			if err == io.EOF {
				break
			}
			return nil, err
		}
		walkCoerce(derefNode(&doc))
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
			key, val := n.Content[i], derefNode(n.Content[i+1])
			if key.Value == "env" && val != nil && val.Kind == yaml.SequenceNode {
				for _, el := range val.Content {
					el = derefNode(el)
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
			walkCoerce(derefNode(c))
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

func derefNode(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.DocumentNode {
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
			continue
		}
		n = n.Alias
	}
	return n
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
