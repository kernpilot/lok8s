package addons

// execplugins.go — the dry-run exec-plugin allowlist (review round 2, item
// C). The kustomize `bash:` secrets generator already refuses to run under
// --dry-run on its own (kustomize/plugins/secret/generator/bash.go); this
// is the broader gate the round-2 review asked for: under --dry-run,
// nothing but lok8s' OWN two built-in plugin kinds may execute at all —
// the secrets generator and khelm — unless --allow-generators is passed.
// A third-party (or simply unexpected) exec plugin referenced from an
// addon's kustomization tree is refused BEFORE kustomize ever runs it,
// rather than relying on it happening to fail for some other reason.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// PluginRef is one generator/transformer/validator file a kustomization
// references, identified by its own apiVersion+kind (a kustomize "plugin
// config" is just a Kubernetes-shaped CR naming the plugin that reads it).
type PluginRef struct {
	Path       string
	APIVersion string
	Kind       string
}

// builtinPlugins is the only apiVersion+kind pairs a dry run runs without
// --allow-generators: the secrets.lok8s.dev generator (kustomize/plugins/
// secret) and khelm's ChartRenderer (internal/render/khelm.go). Anything
// else found in a generators:/transformers:/validators: list is refused.
var builtinPlugins = map[string]bool{
	"secrets.lok8s.dev/v1/Secret":                  true,
	"khelm.mgoltzsche.github.com/v2/ChartRenderer": true,
}

// kustomizationKind is kustomize's own apiVersion+kind — a file carrying it
// is a (sub-)kustomization to recurse into, never a plugin to dispatch.
const kustomizationKind = "kustomize.config.k8s.io/v1beta1/Kustomization"

// ScanExecPlugins walks dir's kustomization tree — dir's own
// kustomization.yaml, then every LOCAL resources:/bases:/components: entry
// that is itself a kustomization (recursively, cycle-safe) — and returns
// every generators:/transformers:/validators: file whose apiVersion+kind is
// not in builtinPlugins. A file that fails to parse is reported as a
// plugin too (fail closed: an unreadable "plugin config" might still be
// one, so don't wave it through).
func ScanExecPlugins(dir string) ([]PluginRef, error) {
	visited := map[string]bool{}
	var refs []PluginRef
	if err := scanKustomization(dir, visited, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func scanKustomization(dir string, visited map[string]bool, refs *[]PluginRef) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if visited[abs] {
		return nil
	}
	visited[abs] = true

	kFile := findKustomizationFile(dir)
	if kFile == "" {
		return nil
	}
	raw, err := os.ReadFile(kFile)
	if err != nil {
		return fmt.Errorf("exec-plugin scan: %s: %w", kFile, err)
	}
	var doc struct {
		Resources    []string `yaml:"resources"`
		Bases        []string `yaml:"bases"`
		Components   []string `yaml:"components"`
		Generators   []string `yaml:"generators"`
		Transformers []string `yaml:"transformers"`
		Validators   []string `yaml:"validators"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("exec-plugin scan: %s: %w", kFile, err)
	}

	for _, rel := range doc.Generators {
		if err := classifyPluginFile(dir, rel, refs); err != nil {
			return err
		}
	}
	for _, rel := range doc.Transformers {
		if err := classifyPluginFile(dir, rel, refs); err != nil {
			return err
		}
	}
	for _, rel := range doc.Validators {
		if err := classifyPluginFile(dir, rel, refs); err != nil {
			return err
		}
	}

	// resources:/bases:/components: entries are either a plain manifest
	// (not a plugin reference — skip) or another kustomization (recurse).
	// A remote/absolute reference outside dir is left alone: it is not
	// part of the project tree this scan owns, and the render's own
	// restrictions (LoadRestrictionsRootOnly) already bound what it can
	// read from there.
	for _, rel := range append(append(doc.Resources, doc.Bases...), doc.Components...) {
		if filepath.IsAbs(rel) || strings.Contains(rel, "://") {
			continue
		}
		next := filepath.Join(dir, rel)
		if fsutil.DirExists(next) {
			if err := scanKustomization(next, visited, refs); err != nil {
				return err
			}
			continue
		}
		if fsutil.FileExists(next) {
			kind, err := readAPIVersionKind(next)
			if err == nil && kind == kustomizationKind {
				if err := scanKustomization(filepath.Dir(next), visited, refs); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func findKustomizationFile(dir string) string {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		p := filepath.Join(dir, name)
		if fsutil.FileExists(p) {
			return p
		}
	}
	return ""
}

// classifyPluginFile resolves rel (relative to dir, a generators:/
// transformers:/validators: entry) and appends it to refs when its
// apiVersion+kind is not one of builtinPlugins.
func classifyPluginFile(dir, rel string, refs *[]PluginRef) error {
	if filepath.IsAbs(rel) || strings.Contains(rel, "://") {
		// Outside the project tree this scan owns — leave it to the
		// render's own LoadRestrictions, same as the resources:/bases:
		// case above.
		return nil
	}
	path := filepath.Join(dir, rel)
	key, err := readAPIVersionKind(path)
	if err != nil {
		return fmt.Errorf("exec-plugin scan: %s: %w", path, err)
	}
	if !builtinPlugins[key] {
		// key is "apiVersion/Kind"; apiVersion itself may contain a slash
		// (group/version), so split on the LAST one for Kind.
		av, k := key, ""
		if i := strings.LastIndex(key, "/"); i >= 0 {
			av, k = key[:i], key[i+1:]
		}
		*refs = append(*refs, PluginRef{Path: path, APIVersion: av, Kind: k})
	}
	return nil
}

// readAPIVersionKind reads path's apiVersion+kind as "apiVersion/kind".
func readAPIVersionKind(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if doc.APIVersion == "" || doc.Kind == "" {
		return "", fmt.Errorf("no apiVersion/kind")
	}
	return doc.APIVersion + "/" + doc.Kind, nil
}
