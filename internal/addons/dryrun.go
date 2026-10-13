package addons

// dryrun.go — the render boundary of `lo bootstrap --dry-run` (bash:
// addons::dry_run_boundary and the dry-run branch of addons::render).
//
// Scope: the dry run prevents ACCIDENTAL side effects of legitimate addon
// code and configuration. It is NOT a security boundary against an addon
// crafted to do harm: the real bootstrap runs that same addon with full
// credentials, and a malicious addon can still execute code during a dry
// run (docs/guide/deployment.md, "Trust model").
//
// A dry run renders IN PLACE, exactly as a real run does: the same project,
// the same HOME, credentials and CA, so sops, exec auth and kubectl behave
// as in a real run, and the helm caches may be populated. What differs:
//
//   - lo's own state writes are suppressed at their source
//     (LOK8S_BOOTSTRAP_DRYRUN=1): the secrets plugin reads the store and
//     keeps new or updated values in the RUN store (a 0700 directory in the
//     boundary, removed with it), the cert generator never writes CAROOT, the
//     Lo driver never writes .registries.json, assets are peeked, never
//     ejected.
//   - the code an addon runs is refused where it would execute: kustomize
//     runs with a plugin home that holds only lok8s' Secret plugin and the
//     ChartRenderer gate (any other legacy exec or Go plugin does not
//     resolve), without --enable-exec (a KRM exec function never runs), with
//     `docker`, `git` and `helm pull` refused first on PATH (a KRM container
//     function, a remote base, kustomize's helmCharts pull), and with an
//     empty HELM_PLUGINS (no helm downloader plugin). The ChartRenderer gate
//     (internal/render/chartgate.go) refuses a local chart whose
//     dependencies khelm would build into the chart directory. The `bash:`
//     secrets generator refuses on its own.
//   - every temp file of the render (kustomize's plugin configs, helm's
//     values) goes to the boundary: TMPDIR.
//
// --allow-generators lifts the code refusals: the addon code then runs as
// trusted, with the side effects of a real run. lo's own writes stay
// suppressed.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	// DryRunReportEnv names the file the secrets plugin and the chart gate
	// report to (kustomize/pkg/cache.DryRunReportEnv).
	DryRunReportEnv = "LOK8S_BOOTSTRAP_DRYRUN_REPORT"
	// DryRunStoreEnv names the run store (kustomize/pkg/cache.DryRunStoreEnv).
	DryRunStoreEnv = "LOK8S_BOOTSTRAP_DRYRUN_STORE"
	// KustomizePluginHomeEnv is kustomize's plugin-home variable.
	KustomizePluginHomeEnv = "KUSTOMIZE_PLUGIN_HOME"
	// HelmPluginsEnv is helm's plugin directory (downloader/getter plugins).
	HelmPluginsEnv = "HELM_PLUGINS"
)

// Refusal stubs: the `docker`, `git` and `helm` a dry run puts first on
// PATH. The texts are part of the contract (bash writes the same scripts).
const (
	dockerRefusal = "lok8s dry-run: a KRM container function is not run in a dry run (pass --allow-generators to run it)"
	gitRefusal    = "lok8s dry-run: a remote base is not fetched in a dry run (pass --allow-generators to fetch it)"
	helmRefusal   = "lok8s dry-run: helmCharts needs --allow-generators in a dry run (helm pull writes the chart into the project)"
)

// DryRunBoundary is the per-run state of a dry run: a 0700 temp directory
// holding the restricted plugin home, the refusing stubs, an empty helm
// plugin directory, the run store, the render's TMPDIR and the report file.
// Close removes it.
type DryRunBoundary struct {
	Dir         string
	PluginHome  string
	StubBin     string
	HelmPlugins string
	Store       string
	Tmp         string
	Report      string
	// ChartRenderer is the project's real ChartRenderer the gate execs in
	// the exec pipeline ("" when the project has none).
	ChartRenderer string
}

// NewDryRunBoundary builds the boundary of one dry run. The restricted
// plugin home links the project's Secret plugin and, in place of the
// project's ChartRenderer, this binary as the ChartRenderer gate (a plugin
// the project does not have is missing there too, exactly as in a real
// run). In the exec pipeline the Secret plugin must support the dry run
// (`Secret --capabilities` lists "dry-run"); an older one would write the
// secrets store, so it is refused.
func NewDryRunBoundary(ctx context.Context, paths *config.Paths, runner execx.Runner) (*DryRunBoundary, error) {
	dir, err := os.MkdirTemp("", "lok8s-dryrun-")
	if err != nil {
		return nil, fmt.Errorf("dry-run boundary: %w", err)
	}
	b := &DryRunBoundary{
		Dir:         dir,
		PluginHome:  filepath.Join(dir, "plugins"),
		StubBin:     filepath.Join(dir, "bin"),
		HelmPlugins: filepath.Join(dir, "helm-plugins"),
		Store:       filepath.Join(dir, "store"),
		Tmp:         filepath.Join(dir, "tmp"),
		Report:      filepath.Join(dir, "report"),
	}
	if err := b.build(ctx, paths, runner); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *DryRunBoundary) build(ctx context.Context, paths *config.Paths, runner execx.Runner) error {
	for _, d := range []string{b.PluginHome, b.StubBin, b.HelmPlugins, b.Store, b.Tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("dry-run boundary: %w", err)
		}
	}
	home := config.KustomizePluginHome(paths)
	if src := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel)); fsutil.FileExists(src) {
		if err := b.link(src, toolchain.SecretPluginRel); err != nil {
			return err
		}
	}
	if src := filepath.Join(home, filepath.FromSlash(toolchain.ChartRendererPluginRel)); fsutil.FileExists(src) {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("dry-run boundary: %w", err)
		}
		if err := b.link(self, toolchain.ChartRendererPluginRel); err != nil {
			return err
		}
		b.ChartRenderer = src
	}
	stubs := map[string]string{
		"docker": "#!/bin/sh\necho '" + dockerRefusal + "' >&2\nexit 1\n",
		"git":    "#!/bin/sh\necho '" + gitRefusal + "' >&2\nexit 1\n",
		"helm":   helmStub(b.StubBin),
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(b.StubBin, name), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable refusal stub
			return fmt.Errorf("dry-run boundary: %w", err)
		}
	}
	if err := os.WriteFile(b.Report, nil, 0o600); err != nil {
		return fmt.Errorf("dry-run boundary: %w", err)
	}
	return b.checkSecretPlugin(ctx, paths, runner, home)
}

// helmStub refuses `helm pull` (kustomize's helmCharts pulls a chart that
// chartHome lacks into the project) and runs the real helm, the next one on
// PATH, for everything else (version, template of a vendored chart).
func helmStub(stubDir string) string {
	return "#!/bin/sh\n" +
		"if [ \"${1:-}\" = pull ]; then\n" +
		"  echo '" + helmRefusal + "' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"IFS=:\n" +
		"for d in $PATH; do\n" +
		"  [ \"$d\" = '" + stubDir + "' ] && continue\n" +
		"  if [ -x \"$d/helm\" ] && [ ! -d \"$d/helm\" ]; then exec \"$d/helm\" \"$@\"; fi\n" +
		"done\n" +
		"echo 'lok8s dry-run: helm: not found on PATH' >&2\n" +
		"exit 127\n"
}

func (b *DryRunBoundary) link(src, rel string) error {
	dst := filepath.Join(b.PluginHome, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("dry-run boundary: %w", err)
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("dry-run boundary: %w", err)
	}
	return nil
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

// Report is what the secrets plugin and the chart gate reported for the
// last entry.
type Report struct {
	// Generated lists the material the dry run generated or updated in its
	// run store (`secret`, `update`, `secretRef`, `caroot` lines): the
	// entry is non-repeatable.
	Generated []string
	// UnvendoredCharts lists the local charts the gate refused (`chart-deps`
	// lines): the entry is inconclusive.
	UnvendoredCharts []string
}

// ReadReport parses the report file. A report that cannot be read counts as
// generated material (the entry is then non-repeatable, never silently
// repeatable).
func (b *DryRunBoundary) ReadReport() Report {
	raw, err := os.ReadFile(b.Report)
	if err != nil {
		return Report{Generated: []string{"report unreadable: " + err.Error()}}
	}
	var r Report
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		switch kind, rest, _ := strings.Cut(line, " "); kind {
		case "":
		case "chart-deps":
			r.UnvendoredCharts = append(r.UnvendoredCharts, rest)
		default:
			r.Generated = append(r.Generated, line)
		}
	}
	return r
}

// StubPath returns path with the refusing stubs first.
func (b *DryRunBoundary) StubPath(path string) string {
	if path == "" {
		return b.StubBin
	}
	return b.StubBin + string(os.PathListSeparator) + path
}

// renderEnv is the overlay a dry run's render gets AFTER the entry's env
// (os/exec keeps the last value of a key). entryPath is the entry's PATH
// ("" = the process PATH).
func (b *DryRunBoundary) renderEnv(allowGenerators bool, entryPath string) []string {
	env := []string{
		DryRunEnv + "=1",
		DryRunReportEnv + "=" + b.Report,
		DryRunStoreEnv + "=" + b.Store,
		"TMPDIR=" + b.Tmp,
	}
	if allowGenerators {
		return append(env, AllowGeneratorsEnv+"=1")
	}
	if entryPath == "" {
		entryPath = os.Getenv("PATH")
	}
	env = append(env,
		KustomizePluginHomeEnv+"="+b.PluginHome,
		"PATH="+b.StubPath(entryPath),
		HelmPluginsEnv+"="+b.HelmPlugins,
	)
	if b.ChartRenderer != "" {
		env = append(env, render.DryRunChartRendererEnv+"="+b.ChartRenderer)
	}
	return env
}

// KRMFunctionDeclared reports an addon directory whose YAML declares a KRM
// function (`config.kubernetes.io/function` or `config.k8s.io/function`):
// without --enable-exec kustomize skips an exec function silently, so the
// dry run marks that entry `partial`. A hint, not a boundary: only the
// addon's own directory is read.
func KRMFunctionDeclared(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found || d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err == nil && (strings.Contains(string(raw), "config.kubernetes.io/function") || strings.Contains(string(raw), "config.k8s.io/function")) {
			found = true
		}
		return nil
	})
	return found
}
