package render

// chartgate.go — the ChartRenderer gate of `lo bootstrap --dry-run` (both
// builds; the bash twin is the wrapper script addons::dry_run_boundary
// writes). khelm builds the dependencies of a LOCAL chart into the chart
// directory (Chart.lock, charts/, tmpcharts/) whenever Helm's
// action.CheckDependencies fails on the chart as Helm's loader loads it, or
// one of them is a file:// chart. The gate decides with Helm's own loader
// (.helmignore honoured, dependencies matched by their metadata name). A dry run must not write there, so it
// refuses that render instead: the entry is reported
// `inconclusive (chart dependencies not vendored)`. The gate runs where khelm
// runs, for every ChartRenderer config of the render (any file name, any
// nested base, an absolute chart path): in-process (lo-full) before khelm,
// and in the exec pipeline as the dry run's ChartRenderer, which execs the
// project's real ChartRenderer (DryRunChartRendererEnv) when the chart may
// render.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// DryRunChartRendererEnv names the project's real ChartRenderer binary the
// dry run's gate execs after the check (the exec pipeline only).
const DryRunChartRendererEnv = "LOK8S_BOOTSTRAP_CHARTRENDERER"

// chartGateApplies reports a dry run that has not lifted the gate.
func chartGateApplies() bool {
	return os.Getenv("LOK8S_BOOTSTRAP_DRYRUN") == "1" && os.Getenv("LOK8S_BOOTSTRAP_ALLOW_GENERATORS") != "1"
}

// ChartDepsNotVendored reads a ChartRenderer config (khelm's generator
// config) and reports whether khelm would build chart dependencies for it:
// the chart is local (no repository, the path exists, relative to baseDir,
// the kustomization root) and chartBuildsDeps says so. chart is the local
// chart path ("" for a remote chart).
func ChartDepsNotVendored(config []byte, baseDir string) (chart string, unvendored bool, err error) {
	var cfg struct {
		Repository string `yaml:"repository"`
		Chart      string `yaml:"chart"`
	}
	if err := yaml.Unmarshal(config, &cfg); err != nil {
		return "", false, fmt.Errorf("chart config: %w", err)
	}
	if cfg.Repository != "" || cfg.Chart == "" {
		return "", false, nil
	}
	path := cfg.Chart
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", false, nil // a repo shorthand or an OCI reference: khelm pulls into its cache
	}
	return cfg.Chart, chartBuildsDeps(path), nil
}

// chartBuildsDeps reports whether khelm would build the dependencies of the
// local chart in dir, decided on the chart as Helm's loader loads it (the
// one khelm loads: .helmignore honoured, a subchart named by its metadata).
// It is conservative: true when
//   - the chart does not load (khelm fails too; the dry run cannot say),
//   - a dependency is a file:// chart (khelm always builds those, so there
//     is no walk into one and no nesting limit),
//   - action.CheckDependencies would fail (a dependency that no loaded
//     subchart has the name of: khelm builds then), or
//   - `helm dependency list` would not say ok: more than one subchart has
//     the name, or the subchart's version is not in the dependency's
//     version constraint (khelm itself renders that one; the bash twin
//     reads `helm dependency list`, so both refuse it).
//
// chartgate_test.go pins this against action.CheckDependencies and
// action.Dependency.List on real chart fixtures.
func chartBuildsDeps(dir string) bool {
	ch, err := loader.Load(dir)
	if err != nil || ch.Metadata == nil {
		return true
	}
	for _, dep := range ch.Metadata.Dependencies {
		if strings.HasPrefix(dep.Repository, "file://") {
			return true
		}
	}
	for _, dep := range ch.Metadata.Dependencies {
		var version string
		n := 0
		for _, sub := range ch.Dependencies() {
			if sub.Name() == dep.Name {
				n++
				version = sub.Metadata.Version
			}
		}
		if n != 1 || !versionAccepted(dep.Version, version) {
			return true
		}
	}
	return false
}

// versionAccepted is the version check of `helm dependency list`: the
// subchart's version equals the dependency's version, or is in its
// constraint.
func versionAccepted(constraint, version string) bool {
	if version == constraint {
		return true
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	return c.Check(v)
}

// chartGate runs the check for a ChartRenderer invocation (argv as kustomize
// starts the plugin, the cwd the kustomization root). refused: the chart
// must not render in this dry run; the refusal is printed and reported.
func chartGate(args []string, stderr io.Writer) (refused bool, err error) {
	config := []byte(os.Getenv("KUSTOMIZE_PLUGIN_CONFIG_STRING"))
	if len(config) == 0 {
		if len(args) < 2 || args[1] == "" {
			return false, errors.New("no generator config")
		}
		if config, err = os.ReadFile(args[1]); err != nil {
			return false, err
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	chart, unvendored, err := ChartDepsNotVendored(config, wd)
	if err != nil || !unvendored {
		return false, err
	}
	fmt.Fprintf(stderr, "lok8s dry-run: chart %s: its dependencies are not vendored (khelm would build them into the chart directory) — vendor them into charts/ or pass --allow-generators\n", chart)
	if report := os.Getenv("LOK8S_BOOTSTRAP_DRYRUN_REPORT"); report != "" {
		f, ferr := os.OpenFile(report, os.O_WRONLY|os.O_APPEND, 0)
		if ferr != nil {
			return true, ferr
		}
		_, werr := fmt.Fprintf(f, "chart-deps %s\n", chart)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return true, werr
	}
	return true, nil
}

// dispatchChartGate serves the dry run's ChartRenderer in the exec pipeline
// (both builds): kustomize starts this binary under the plugin's name from
// the dry run's plugin home, with DryRunChartRendererEnv naming the
// project's real ChartRenderer. A chart that may render execs that binary
// with the same argv and environment; a refused one exits 1.
func dispatchChartGate(args []string, stderr io.Writer) (handled bool, rc int) {
	real := os.Getenv(DryRunChartRendererEnv)
	if real == "" || len(args) == 0 || !strings.HasSuffix(filepath.ToSlash(args[0]), "/chartrenderer/ChartRenderer") {
		return false, 0
	}
	if chartGateApplies() {
		refused, err := chartGate(args, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "lok8s dry-run: chart gate: %v\n", err)
			return true, 1
		}
		if refused {
			return true, 1
		}
	}
	argv := append([]string{real}, args[1:]...)
	// #nosec G204 G702 -- the project's own ChartRenderer with kustomize's
	// argv, as a real run starts it; no shell parses it.
	err := syscall.Exec(real, argv, os.Environ())
	fmt.Fprintf(stderr, "lok8s dry-run: exec %s: %v\n", real, err)
	return true, 1
}
