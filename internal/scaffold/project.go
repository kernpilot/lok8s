package scaffold

// project.go — `lo init project`: the smallest project the eject model
// needs, files only. No .lok8s/ tree is written (`lo` ejects the framework
// assets a cluster references on first use, internal/assets), no network
// is used (the toolchain is `lo toolchain install`). Go-only — the frozen
// implementation cannot run without a synced .lok8s tree, so it has no
// twin. Also here: WriteBYAML and EnsureGitignore, the two writers `lo
// toolchain install` shares with the scaffold.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/fsutil"
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

// EnvFiles are the values of `lo init project --env`: "mise" (mise.toml,
// the default), "direnv" (.envrc), or "none" (CI). One file is written,
// never two: the two writers render the same EnvSpec, so the files say
// the same thing, and a project activates one tool. The binary needs
// nothing exported — it resolves the project from lok8s.yaml — so the
// file only puts the b-managed toolchain on PATH and deliberately pins
// no PATH_* variable: an ambient PATH_BASE once redirected a run (and a
// test harness) into the wrong project.
var EnvFiles = []string{"mise", "direnv", "none"}

// EnvSpec is what an environment file states: the one data struct the
// two writers (miseTOML, envrc) render.
type EnvSpec struct {
	// Project is the project name (the file header).
	Project string
	// BinRel is the toolchain directory, relative to the project root,
	// that goes on PATH.
	BinRel string
	// BVersion is the b release `lo toolchain install` pins; the mise
	// writer shows how to let mise bootstrap b itself.
	BVersion string
}

// miseTOML renders the project mise.toml: PATH via mise's env activation,
// no tool pins (those live in .bin/b.yaml).
func miseTOML(e EnvSpec) string {
	return "# mise.toml — " + e.Project + ": the lok8s project environment (https://mise.jdx.dev).\n" +
		"#   mise trust    # once per checkout; `mise activate` in your shell puts " + e.BinRel + " on PATH\n" +
		"# `lo` needs nothing exported — it resolves the project from lok8s.yaml — so this\n" +
		"# only puts the b-managed toolchain on PATH. No PATH_* pins on purpose.\n" +
		"[env]\n" +
		"_.path = [\"{{config_root}}/" + e.BinRel + "\"]\n" +
		"# KUBECONFIG = \"{{config_root}}/.kubeconfig/<cluster>.yaml\"   # lo sets it per domain; `lo kubeconfig` prints it\n" +
		"\n" +
		"[tools]\n" +
		"# Tools are pinned in .bin/b.yaml (`lo toolchain install`, `b install`). To let mise bootstrap b itself:\n" +
		"# \"github:fentas/b\" = \"" + e.BVersion + "\"\n"
}

// envrc renders the project .envrc for direnv: PATH only.
func envrc(e EnvSpec) string {
	return "# .envrc — " + e.Project + ": direnv puts the b-managed toolchain on PATH.\n" +
		"# `lo` needs nothing exported (it resolves the project from lok8s.yaml);\n" +
		"# no PATH_* pins on purpose — an ambient PATH_BASE redirects runs elsewhere.\n" +
		"PATH_add " + e.BinRel + "\n" +
		"# export KUBECONFIG=\"${PWD}/.kubeconfig/<cluster>.yaml\"   # lo sets it per domain; `lo kubeconfig` prints it\n"
}

// envWriters maps an --env value to the file it writes and its renderer.
var envWriters = map[string]struct {
	file   string
	render func(EnvSpec) string
}{
	"mise":   {"mise.toml", miseTOML},
	"direnv": {".envrc", envrc},
}

// EnvFileError is the --env validation: the removed "both" names the two
// files it used to write; any other unknown value lists the valid ones.
func EnvFileError(env string) error {
	if env == "both" {
		return fmt.Errorf("--env both was removed: one file per project. Use --env mise (the default) or --env direnv")
	}
	return fmt.Errorf("--env must be one of %s, got %q", strings.Join(EnvFiles, "|"), env)
}

// writeEnvFile writes the selected environment file (kept unless force).
// One file per project: when the OTHER writer's file already exists
// (`--env direnv` on a project with a mise.toml, or the reverse), the
// selected file is not written and a `Kept` line names the existing one;
// --force writes it and says so.
func writeEnvFile(dir, env string, spec EnvSpec, force bool, out io.Writer) error {
	if env == "none" {
		return nil
	}
	w, ok := envWriters[env]
	if !ok {
		return EnvFileError(env)
	}
	for other, ow := range envWriters {
		if other == env || !fsutil.FileExists(filepath.Join(dir, ow.file)) {
			continue
		}
		if !force {
			fmt.Fprintf(out, "Kept %s (exists; one environment file per project, so %s is not written; --force writes it beside)\n", filepath.Join(dir, ow.file), w.file)
			return nil
		}
		fmt.Fprintf(out, "Writing %s beside %s (--force; one environment file per project is the rule, two are yours to keep in sync)\n", w.file, ow.file)
	}
	return writeUnlessPresent(filepath.Join(dir, w.file), w.render(spec), force, out)
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

// ProjectOptions shapes one `lo init project` run.
type ProjectOptions struct {
	// Dir is the project directory ("" = Base).
	Dir string
	// Name is metadata.name ("" = the directory name).
	Name string
	// Env selects the environment file (EnvFiles; "" = mise).
	Env string
	// BVersion is the b release the mise file shows (EnvSpec.BVersion).
	BVersion string
	// Force overwrites existing files (.gitignore is still only appended to).
	Force bool
}

// Project scaffolds a project into o.Dir (default: base): clusters/,
// lok8s.yaml, the .gitignore entries and one environment file. Files
// only, no network, idempotent: an existing file is kept unless o.Force;
// .gitignore is only ever appended to. It ends with the next steps, the
// first of which is `lo toolchain install`.
func Project(base string, o ProjectOptions, out, stderr io.Writer) error {
	dir := o.Dir
	if dir == "" {
		dir = base
	}
	name := o.Name
	if name == "" {
		name = filepath.Base(dir)
	}
	env := o.Env
	if env == "" {
		env = "mise"
	}
	if env != "none" {
		if _, ok := envWriters[env]; !ok {
			return EnvFileError(env)
		}
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
	if err := writeUnlessPresent(filepath.Join(clusters, ".gitkeep"), "", o.Force, out); err != nil {
		return err
	}
	if err := writeUnlessPresent(filepath.Join(dir, "lok8s.yaml"), projectFile(name), o.Force, out); err != nil {
		return err
	}
	if err := appendGitignore(filepath.Join(dir, ".gitignore"), out); err != nil {
		return err
	}
	if err := writeEnvFile(dir, env, EnvSpec{Project: name, BinRel: ".bin", BVersion: o.BVersion}, o.Force, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "Done. Next:")
	fmt.Fprintf(out, "  cd %s && lo toolchain install   # .bin/b.yaml, b and the pinned toolchain into .bin/ (network)\n", dir)
	switch env {
	case "mise":
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
		for line := range strings.SplitSeq(strings.TrimRight(res.Diff, "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
		fmt.Fprintln(out, "  To adopt the template: move the file aside and re-run `lo toolchain install`.")
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
	if fsutil.FileExists(path) && !force {
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
		for line := range strings.SplitSeq(string(raw), "\n") {
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
