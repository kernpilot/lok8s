package scaffold

// project.go — `lo init project`: the smallest project the eject model
// needs. No .lok8s/ tree is written; `lo` ejects the framework assets a
// cluster references on first use (internal/assets). Go-only — the frozen
// implementation cannot run without a synced .lok8s tree, so it has no
// twin.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// projectFile is the project-root lok8s.yaml (also the project marker
// config.ResolvePaths recognizes).
func projectFile(name string) string {
	return "# lok8s project — the marker `lo` resolves the project root from.\n" +
		"# Clusters live under clusters/<domain>/ (lo use <domain>, lo up); the\n" +
		"# service catalog is services.yaml (lo init service <name>). Framework\n" +
		"# assets a cluster references are ejected into .lok8s/ on first use\n" +
		"# (lo assets list|diff|update); commit them.\n" +
		"apiVersion: lok8s.dev/v1\n" +
		"kind: Project\n" +
		"metadata:\n" +
		"  name: " + name + "\n"
}

// bYAML is the minimal b-managed toolchain: the lo binary plus the
// third-party tools every `lo up` execs. See https://github.com/fentas/b.
func bYAML(name string) string {
	return "# " + name + " — managed by b (github.com/fentas/b). Run `b install`.\n" +
		"# The lo binary plus the third-party tools it execs for a local\n" +
		"# cluster; drop what your drivers do not need, add what they do\n" +
		"# (kubeone, clusterctl, hcloud, sops, …).\n" +
		"binaries:\n" +
		"  github.com/kernpilot/lok8s:\n" +
		"    alias: lo\n" +
		"    asset: lo-*.tar.gz\n" +
		"  kubectl: {}\n" +
		"  kustomize: {}\n" +
		"  yq: {}\n" +
		"  kind: {}\n" +
		"  tilt: {}\n"
}

// gitignoreEntries are appended to .gitignore when absent (each with the
// reason it is there).
var gitignoreEntries = []string{
	"# lok8s — toolchain binaries (b installs them; b.yaml/b.lock are committed)",
	".bin/*",
	"!.bin/b.yaml",
	"!.bin/b.lock",
	"# lok8s — kubeconfigs, built kustomize plugins, the deprecated flat secrets store",
	".kubeconfig/",
	".kustomize/",
	".secrets/",
	".lok8s/**/secret.yaml",
}

// Project scaffolds a project into dir (default: base): clusters/,
// lok8s.yaml, .gitignore entries, .bin/b.yaml. Existing files are kept
// unless force; .gitignore is only ever appended to.
func Project(base, name, dir string, force bool, out, stderr io.Writer) error {
	if dir == "" {
		dir = base
	}
	if name == "" {
		name = filepath.Base(dir)
	}
	if err := ValidateName(name, stderr); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	clusters := filepath.Join(dir, "clusters")
	if err := os.MkdirAll(clusters, 0o755); err != nil {
		return err
	}
	if err := writeUnlessPresent(filepath.Join(clusters, ".gitkeep"), "", force, out); err != nil {
		return err
	}
	if err := writeUnlessPresent(filepath.Join(dir, "lok8s.yaml"), projectFile(name), force, out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".bin"), 0o755); err != nil {
		return err
	}
	if err := writeUnlessPresent(filepath.Join(dir, ".bin", "b.yaml"), bYAML(name), force, out); err != nil {
		return err
	}
	if err := appendGitignore(filepath.Join(dir, ".gitignore"), out); err != nil {
		return err
	}
	fmt.Fprintln(out, "Done. Next:")
	fmt.Fprintf(out, "  cd %s && b install\n", dir)
	fmt.Fprintln(out, "  lo use <domain>         # after adding clusters/<domain>/cluster.lok8s.yaml")
	fmt.Fprintln(out, "  lo assets eject         # optional: pin the referenced framework assets now")
	return nil
}

// writeUnlessPresent writes content to path unless it exists (force
// overwrites), reporting either way.
func writeUnlessPresent(path, content string, force bool, out io.Writer) error {
	if fileExists(path) && !force {
		fmt.Fprintf(out, "Kept %s (exists; --force overwrites)\n", path)
		return nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Scaffolded %s\n", path)
	return nil
}

// appendGitignore adds the missing entries (never removes or reorders
// existing lines).
func appendGitignore(path string, out io.Writer) error {
	existing := map[string]bool{}
	raw, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			existing[strings.TrimSpace(line)] = true
		}
	}
	var add []string
	for _, e := range gitignoreEntries {
		if strings.HasPrefix(e, "#") || !existing[e] {
			add = append(add, e)
		}
	}
	// Only comments left → nothing real to add.
	real := 0
	for _, e := range add {
		if !strings.HasPrefix(e, "#") {
			real++
		}
	}
	if real == 0 {
		fmt.Fprintf(out, "Kept %s (entries present)\n", path)
		return nil
	}
	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" {
		body += "\n"
	}
	body += strings.Join(add, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	if raw == nil {
		fmt.Fprintf(out, "Scaffolded %s\n", path)
	} else {
		fmt.Fprintf(out, "Appended %d entries to %s\n", real, path)
	}
	return nil
}
