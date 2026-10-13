package addons

// dryrun.go — the render boundary of `lo bootstrap --dry-run` (bash:
// addons::dry_run_boundary and the dry-run branch of addons::render).
//
// A dry run renders IN PLACE, exactly as a real run does: the same project,
// the same HOME, credentials and CA, so sops, exec auth and kubectl behave
// as in a real run. Two things are different:
//
//   - lo's own state writes are suppressed at their source (the secrets
//     plugin keeps new values in memory, the cert generator never writes
//     CAROOT, the Lo driver never writes .registries.json, assets are
//     peeked, never ejected) — LOK8S_BOOTSTRAP_DRYRUN=1.
//   - executable addon code is refused where it would execute: kustomize
//     runs with a plugin home that holds only lok8s' two built-in plugins
//     (so any other legacy exec or Go plugin does not resolve), without
//     --enable-exec (so a KRM exec function never runs), and with a PATH
//     whose `docker` and `git` refuse (so a KRM container function and a
//     remote git base fail). The `bash:` secrets generator refuses on its
//     own. --allow-generators lifts all of this: the addon code then runs
//     as trusted, with the side effects of a real run.
//
// The dry run does not confine addon code that runs anyway (khelm's chart
// downloads, a remote file URL, the code --allow-generators runs).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

const (
	// DryRunEnv is the dry-run sentinel every lo state writer checks.
	DryRunEnv = "LOK8S_BOOTSTRAP_DRYRUN"
	// AllowGeneratorsEnv lets the `bash:` secrets generator run in a dry run.
	AllowGeneratorsEnv = "LOK8S_BOOTSTRAP_ALLOW_GENERATORS"
	// DryRunReportEnv names the file the secrets plugin reports generated
	// material to (kustomize/pkg/cache.DryRunReportEnv).
	DryRunReportEnv = "LOK8S_BOOTSTRAP_DRYRUN_REPORT"
	// KustomizePluginHomeEnv is kustomize's plugin-home variable.
	KustomizePluginHomeEnv = "KUSTOMIZE_PLUGIN_HOME"
)

// Refusal stubs: the `docker` and `git` a dry run puts first on PATH. The
// texts are part of the contract (bash writes the same scripts).
const (
	dockerRefusal = "lok8s dry-run: a KRM container function is not run in a dry run (pass --allow-generators to run it)"
	gitRefusal    = "lok8s dry-run: a remote base is not fetched in a dry run (pass --allow-generators to fetch it)"
)

// DryRunBoundary is the per-run state of a dry run: a 0700 temp directory
// holding the restricted plugin home, the refusing stubs, the report file
// and every per-chart copy. Close removes it.
type DryRunBoundary struct {
	Dir        string
	PluginHome string
	StubBin    string
	Report     string
}

// NewDryRunBoundary builds the boundary of one dry run. The restricted
// plugin home links the project's own Secret and ChartRenderer plugins
// (config.KustomizePluginHome): a plugin the project does not have is
// missing there too, exactly as in a real run. In the exec pipeline the
// Secret plugin must support the dry run (`Secret --capabilities` lists
// "dry-run"); an older one would write the secrets store, so it is refused.
func NewDryRunBoundary(ctx context.Context, paths *config.Paths, runner execx.Runner) (*DryRunBoundary, error) {
	dir, err := os.MkdirTemp("", "lok8s-dryrun-")
	if err != nil {
		return nil, fmt.Errorf("dry-run boundary: %w", err)
	}
	b := &DryRunBoundary{
		Dir:        dir,
		PluginHome: filepath.Join(dir, "plugins"),
		StubBin:    filepath.Join(dir, "bin"),
		Report:     filepath.Join(dir, "report"),
	}
	if err := b.build(ctx, paths, runner); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *DryRunBoundary) build(ctx context.Context, paths *config.Paths, runner execx.Runner) error {
	home := config.KustomizePluginHome(paths)
	for _, rel := range []string{toolchain.SecretPluginRel, toolchain.ChartRendererPluginRel} {
		src := filepath.Join(home, filepath.FromSlash(rel))
		if !fsutil.FileExists(src) {
			continue
		}
		dst := filepath.Join(b.PluginHome, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("dry-run boundary: %w", err)
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("dry-run boundary: %w", err)
		}
	}
	if err := os.MkdirAll(b.PluginHome, 0o700); err != nil {
		return fmt.Errorf("dry-run boundary: %w", err)
	}
	if err := os.MkdirAll(b.StubBin, 0o700); err != nil {
		return fmt.Errorf("dry-run boundary: %w", err)
	}
	for name, msg := range map[string]string{"docker": dockerRefusal, "git": gitRefusal} {
		script := "#!/bin/sh\necho '" + msg + "' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(b.StubBin, name), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable refusal stub
			return fmt.Errorf("dry-run boundary: %w", err)
		}
	}
	if err := os.WriteFile(b.Report, nil, 0o600); err != nil {
		return fmt.Errorf("dry-run boundary: %w", err)
	}
	return b.checkSecretPlugin(ctx, paths, runner, home)
}

// checkSecretPlugin refuses a Secret plugin that predates the dry run. The
// in-process render serves the plugin from this binary, so only the exec
// pipeline needs the check.
func (b *DryRunBoundary) checkSecretPlugin(ctx context.Context, paths *config.Paths, runner execx.Runner, home string) error {
	if mode, err := render.CurrentMode(); err != nil || mode != render.ModeExec {
		return nil
	}
	plugin := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel))
	if !fsutil.FileExists(plugin) {
		return nil
	}
	if runner == nil {
		runner = execx.NewRunner(paths)
	}
	var out strings.Builder
	err := runner.Run(ctx, execx.Cmd{Name: plugin, Args: []string{"--capabilities"}, Stdout: &out, Stderr: io.Discard})
	if err == nil {
		for line := range strings.SplitSeq(out.String(), "\n") {
			if strings.TrimSpace(line) == "dry-run" {
				return nil
			}
		}
	}
	return fmt.Errorf("the Secret plugin %s does not support a dry run (it would write the secrets store) — run 'lo toolchain install' to install the plugin of this lo", config.RelTo(paths.Base, plugin))
}

// Close removes the boundary directory and everything in it.
func (b *DryRunBoundary) Close() {
	if b != nil && b.Dir != "" {
		_ = os.RemoveAll(b.Dir)
	}
}

// ResetReport empties the report file before an entry renders.
func (b *DryRunBoundary) ResetReport() error {
	return os.WriteFile(b.Report, nil, 0o600)
}

// Minted returns the report lines of the last render: one per value the
// secrets plugin generated in memory (a store entry, a CA). Empty means the
// render is repeatable.
func (b *DryRunBoundary) Minted() []string {
	raw, err := os.ReadFile(b.Report)
	if err != nil {
		return []string{"report unreadable: " + err.Error()}
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// StubPath returns path with the refusing stubs first.
func (b *DryRunBoundary) StubPath(path string) string {
	if path == "" {
		return b.StubBin
	}
	return b.StubBin + string(os.PathListSeparator) + path
}

// chartRef is the part of a khelm ChartRenderer config the dry run reads.
type chartRef struct {
	Repository string   `yaml:"repository"`
	Chart      string   `yaml:"chart"`
	ValueFiles []string `yaml:"valueFiles"`
}

// chartDeps is the dependency list of a Chart.yaml.
type chartDeps struct {
	Dependencies []struct {
		Name       string `yaml:"name"`
		Repository string `yaml:"repository"`
	} `yaml:"dependencies"`
}

// localChart is a chart.yaml addon's local chart, resolved for a dry run.
type localChart struct {
	root       string   // the containment root (resolved)
	addon      string   // the addon directory (resolved)
	dirs       []string // the chart and its file:// dependencies (resolved)
	buildsDeps bool     // khelm builds dependencies (writes Chart.lock, charts/)
	valueFiles []string // chart.yaml's local valueFiles (resolved)
}

// resolveLocalChart reads addonDir/chart.yaml. A remote chart returns nil.
// A local chart and every file:// dependency must resolve inside the
// project (or, for an addon served outside the project, inside the addon
// directory): a dry run refuses anything else as an entry error.
func resolveLocalChart(addonDir, projectBase string) (*localChart, error) {
	raw, err := os.ReadFile(filepath.Join(addonDir, "chart.yaml"))
	if err != nil {
		return nil, err
	}
	var ref chartRef
	if err := yaml.Unmarshal(raw, &ref); err != nil {
		return nil, fmt.Errorf("chart.yaml: %w", err)
	}
	if ref.Repository != "" || ref.Chart == "" {
		return nil, nil
	}
	chartPath := ref.Chart
	if !filepath.IsAbs(chartPath) {
		chartPath = filepath.Join(addonDir, chartPath)
	}
	if !fsutil.DirExists(chartPath) {
		return nil, nil // a repo shorthand (@repo/chart) or an OCI reference
	}
	addon, err := filepath.EvalSymlinks(addonDir)
	if err != nil {
		return nil, err
	}
	root := addon
	if base, err := filepath.EvalSymlinks(projectBase); err == nil && within(base, addon) {
		root = base
	}
	lc := &localChart{root: root, addon: addon}
	if err := lc.walk(chartPath, "chart "+ref.Chart, map[string]bool{}, 0); err != nil {
		return nil, err
	}
	for _, vf := range ref.ValueFiles {
		if strings.Contains(vf, "://") {
			continue
		}
		p := vf
		if !filepath.IsAbs(p) {
			p = filepath.Join(addonDir, p)
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			lc.valueFiles = append(lc.valueFiles, resolved)
		}
	}
	return lc, nil
}

func (lc *localChart) walk(chartPath, what string, seen map[string]bool, depth int) error {
	if depth > 20 {
		return errors.New("local chart dependencies nest deeper than 20 levels")
	}
	resolved, err := filepath.EvalSymlinks(chartPath)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !within(lc.root, resolved) {
		return fmt.Errorf("%s resolves outside the project (%s) — a dry run renders only charts inside the project", what, resolved)
	}
	if seen[resolved] {
		return nil
	}
	seen[resolved] = true
	lc.dirs = append(lc.dirs, resolved)
	raw, err := os.ReadFile(filepath.Join(resolved, "Chart.yaml"))
	if err != nil {
		return nil // not a chart directory: khelm reports it
	}
	var meta chartDeps
	if yaml.Unmarshal(raw, &meta) != nil {
		return nil
	}
	if len(meta.Dependencies) > 0 {
		lc.buildsDeps = true
	}
	for _, dep := range meta.Dependencies {
		rel, ok := strings.CutPrefix(dep.Repository, "file://")
		if !ok {
			continue
		}
		next := rel
		if !filepath.IsAbs(next) {
			next = filepath.Join(resolved, next)
		}
		if err := lc.walk(next, "file:// dependency "+dep.Name, seen, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// mirror copies the addon directory, the chart closure and the local
// valueFiles under tmp, at their paths relative to the containment root,
// so every relative reference among them resolves as in the project. khelm
// then writes Chart.lock and charts/ into the copy. Returns the copy of the
// addon directory, the directory to render.
func (lc *localChart) mirror(tmp string) (string, error) {
	copied := []string{}
	place := func(src string) string { return filepath.Join(tmp, mustRel(lc.root, src)) }
	for _, d := range append([]string{lc.addon}, lc.dirs...) {
		if coveredBy(copied, d) {
			continue
		}
		if err := copyTreeDeref(d, place(d), 0); err != nil {
			return "", err
		}
		copied = append(copied, d)
	}
	for _, vf := range lc.valueFiles {
		if coveredBy(copied, vf) {
			continue
		}
		if !within(lc.root, vf) {
			return "", fmt.Errorf("valueFile %s resolves outside the project — a dry run cannot isolate this chart", vf)
		}
		if err := copyFileDeref(vf, place(vf)); err != nil {
			return "", err
		}
	}
	return place(lc.addon), nil
}

// within reports whether p is root or below it (both resolved, absolute).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func coveredBy(dirs []string, p string) bool {
	for _, d := range dirs {
		if within(d, p) {
			return true
		}
	}
	return false
}

func mustRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.Base(p)
	}
	return rel
}

// copyTreeDeref copies src's contents into dst and copies a symlink as its
// target's content, so a write into the copy can never follow a link back
// into the project. A dangling symlink is skipped.
func copyTreeDeref(src, dst string, depth int) error {
	if depth > 40 {
		return fmt.Errorf("copy %s: symlinks nest too deep", src)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, en := range entries {
		s, d := filepath.Join(src, en.Name()), filepath.Join(dst, en.Name())
		info, err := os.Stat(s) // follows a symlink
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if info.IsDir() {
			if err := copyTreeDeref(s, d, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := copyFileDeref(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFileDeref(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}
