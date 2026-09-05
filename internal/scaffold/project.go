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

	"github.com/kernpilot/lok8s/internal/toolchain"
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

// ProjectToolchain is how `lo init project` provisions the toolchain:
// the .bin/b.yaml template (internal/toolchain.Template with the pins of
// the running lo; the cli supplies it) and the bootstrap that installs b
// and runs `b install` (nil = --no-toolchain: the file is written, the
// network step is skipped and the hint printed instead).
type ProjectToolchain struct {
	// Template renders the b.yaml for the project name.
	Template func(name string) string
	// Bootstrap installs b into <dir>/.bin and runs `b install` there.
	Bootstrap func(dir string) error
	// Env selects the shell-environment files (see EnvFiles); "" = mise.
	Env string
	// BVersion is the b release the bootstrap pins (shown in mise.toml).
	BVersion string
}

// bYAML is the fallback b.yaml when no ProjectToolchain.Template is
// supplied (library callers): the lo binary plus the third-party tools
// every `lo up` execs. The cli always supplies the pinned template.
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

// EnvFiles selects the shell-environment files `lo init project` writes:
// "mise" (mise.toml, the default), "direnv" (.envrc), "both", or "none".
// The binary needs nothing exported — it resolves the project from
// lok8s.yaml — so both files only put the b-managed toolchain on PATH
// and deliberately pin no PATH_* variable: an ambient PATH_BASE once
// redirected a run (and a test harness) into the wrong project.
var EnvFiles = []string{"mise", "direnv", "both", "none"}

// miseTOML is the project mise.toml: PATH via mise's env activation, no
// tool pins (those live in .bin/b.yaml; b can be bootstrapped by mise).
func miseTOML(name, bVersion string) string {
	return "# mise.toml — " + name + ": the lok8s project environment (https://mise.jdx.dev).\n" +
		"#   mise trust    # once per checkout; `mise activate` in your shell puts .bin on PATH\n" +
		"# `lo` needs nothing exported — it resolves the project from lok8s.yaml — so this\n" +
		"# only puts the b-managed toolchain on PATH. No PATH_* pins on purpose.\n" +
		"[env]\n" +
		"_.path = [\"{{config_root}}/.bin\"]\n" +
		"# KUBECONFIG = \"{{config_root}}/.kubeconfig/<cluster>.yaml\"   # lo sets it per domain; `lo kubeconfig` prints it\n" +
		"\n" +
		"[tools]\n" +
		"# Tools are pinned in .bin/b.yaml (`b install`). To let mise bootstrap b itself:\n" +
		"# \"github:fentas/b\" = \"" + bVersion + "\"\n"
}

// envrc is the project .envrc for direnv: PATH only (see EnvFiles).
func envrc(name string) string {
	return "# .envrc — " + name + ": direnv puts the b-managed toolchain on PATH.\n" +
		"# `lo` needs nothing exported (it resolves the project from lok8s.yaml);\n" +
		"# no PATH_* pins on purpose — an ambient PATH_BASE redirects runs elsewhere.\n" +
		"PATH_add .bin\n" +
		"# export KUBECONFIG=\"${PWD}/.kubeconfig/<cluster>.yaml\"   # lo sets it per domain; `lo kubeconfig` prints it\n"
}

// writeEnvFiles writes the selected environment files (kept unless force).
func writeEnvFiles(dir, name, env, bVersion string, force bool, out io.Writer) error {
	switch env {
	case "none":
		return nil
	case "mise", "direnv", "both":
	default:
		return fmt.Errorf("--env must be one of %s, got %q", strings.Join(EnvFiles, "|"), env)
	}
	if env == "mise" || env == "both" {
		if err := writeUnlessPresent(filepath.Join(dir, "mise.toml"), miseTOML(name, bVersion), force, out); err != nil {
			return err
		}
	}
	if env == "direnv" || env == "both" {
		if err := writeUnlessPresent(filepath.Join(dir, ".envrc"), envrc(name), force, out); err != nil {
			return err
		}
	}
	return nil
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
// lok8s.yaml, .gitignore entries, .bin/b.yaml, then the toolchain
// (tc.Bootstrap: b into .bin/ + `b install`) unless tc.Bootstrap is nil.
// Existing files are kept unless force — except .bin/b.yaml, which is
// NEVER overwritten (a differing one is diffed against the template);
// .gitignore is only ever appended to.
func Project(base, name, dir string, force bool, out, stderr io.Writer, tc ProjectToolchain) error {
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
	content := bYAML(name)
	if tc.Template != nil {
		content = tc.Template(name)
	}
	if err := WriteBYAML(filepath.Join(dir, ".bin"), content, false, out); err != nil {
		return err
	}
	if err := appendGitignore(filepath.Join(dir, ".gitignore"), out); err != nil {
		return err
	}
	env := tc.Env
	if env == "" {
		env = "mise"
	}
	if err := writeEnvFiles(dir, name, env, tc.BVersion, force, out); err != nil {
		return err
	}
	if tc.Bootstrap != nil {
		fmt.Fprintln(out, "Toolchain (b → .bin/):")
		if err := tc.Bootstrap(dir); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "Done. Next:")
	if tc.Bootstrap == nil {
		fmt.Fprintf(out, "  cd %s && lo init toolchain   # b + the pinned toolchain into .bin/ (skipped: --no-toolchain)\n", dir)
	} else {
		fmt.Fprintln(out, "  lo doctor               # verify the toolchain landed")
	}
	switch env {
	case "mise", "both":
		fmt.Fprintln(out, "  mise trust              # then `mise activate` in your shell — .bin lands on PATH")
	case "direnv":
		fmt.Fprintln(out, "  direnv allow            # .bin lands on PATH")
	}
	fmt.Fprintln(out, "  lo use <domain>         # after adding clusters/<domain>/cluster.lok8s.yaml")
	fmt.Fprintln(out, "  lo assets eject         # optional: pin the referenced framework assets now")
	return nil
}

// WriteBYAML places content at <bin>/b.yaml unless it exists — never
// overwriting (the rule ejected assets follow): an existing, differing
// file is reported with a unified diff against the template and the
// instructions; an identical one as kept. dryRun reports without writing.
func WriteBYAML(bin, content string, dryRun bool, out io.Writer) error {
	res, err := toolchain.Write(bin, content, dryRun)
	if err != nil {
		return err
	}
	switch {
	case res.Written:
		fmt.Fprintf(out, "Scaffolded %s\n", res.Path)
	case res.Same:
		fmt.Fprintf(out, "Kept %s (exists; matches the template)\n", res.Path)
	case res.Diff != "":
		fmt.Fprintf(out, "Kept %s (exists; never overwritten). It differs from the template this lo would write:\n", res.Path)
		for _, line := range strings.Split(strings.TrimRight(res.Diff, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		fmt.Fprintln(out, "  To adopt the template: move the file aside and re-run `lo init toolchain`.")
		fmt.Fprintln(out, "  To keep yours: merge the pins by hand — `lo doctor` reports what differs from the pins.")
	case dryRun:
		fmt.Fprintf(out, "would write %s\n", res.Path)
	}
	return nil
}

// EnsureGitignore appends the lok8s .gitignore entries (idempotent).
func EnsureGitignore(dir string, out io.Writer) error {
	return appendGitignore(filepath.Join(dir, ".gitignore"), out)
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
	// A comment header is emitted only when a missing entry follows it —
	// otherwise a re-run keeps appending headers with nothing under them.
	var add []string
	var pending string
	real := 0
	for _, e := range gitignoreEntries {
		if strings.HasPrefix(e, "#") {
			pending = e
			continue
		}
		if existing[e] {
			continue
		}
		if pending != "" {
			add = append(add, pending)
			pending = ""
		}
		add = append(add, e)
		real++
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
