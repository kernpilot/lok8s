package render

// chartgate.go — the ChartRenderer gate of `lo bootstrap --dry-run` (both
// builds; the bash twin is the wrapper script addons::dry_run_boundary
// writes). khelm builds the dependencies of a LOCAL chart into the chart
// directory (Chart.lock, charts/, tmpcharts/) whenever Helm's
// action.CheckDependencies fails on the chart as Helm's loader loads it, or
// one of them is a file:// chart. The gate decides with Helm's own loader
// (.helmignore honoured, dependencies matched by their metadata name) and
// the status rule of `helm dependency list`, which the bash twin runs. A dry
// run must not write there, so it refuses that render instead: the entry is
// reported `inconclusive (chart dependencies not vendored)`. The gate runs where khelm
// runs, for every ChartRenderer config of the render (any file name, any
// nested base, an absolute chart path): in-process (lo-full) before khelm,
// and in the exec pipeline as the dry run's ChartRenderer, which execs the
// project's real ChartRenderer (DryRunChartRendererEnv) when the chart may
// render.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// DryRunChartRendererEnv names the project's real ChartRenderer binary the
// dry run's gate execs after the check (the exec pipeline only).
const DryRunChartRendererEnv = "LOK8S_BOOTSTRAP_CHARTRENDERER"

// chartGateApplies reports a dry run that has not lifted the gate.
func chartGateApplies() bool {
	return os.Getenv("LOK8S_BOOTSTRAP_DRYRUN") == "1" && os.Getenv("LOK8S_BOOTSTRAP_ALLOW_GENERATORS") != "1"
}

// vendorAdvice ends the refusal of a chart whose dependencies helm does not
// report as vendored (the bash twin prints the same text).
const vendorAdvice = "khelm may build the dependencies into the chart directory. Vendor them into charts/, or pass --allow-generators."

// ChartRefusal reads a ChartRenderer config (khelm's generator config) and
// returns why the dry run refuses to render it ("" when it may render): the
// chart is local (no repository, the path exists, relative to baseDir, the
// kustomization root) and chartRefusal refuses it. chartPath is the local
// chart path as the config names it ("" for a remote chart).
func ChartRefusal(config []byte, baseDir string) (chartPath, refusal string, err error) {
	var cfg struct {
		Repository string `yaml:"repository"`
		Chart      string `yaml:"chart"`
	}
	if err := yaml.Unmarshal(config, &cfg); err != nil {
		return "", "", fmt.Errorf("chart config: %w", err)
	}
	if cfg.Repository != "" || cfg.Chart == "" {
		return "", "", nil
	}
	path := cfg.Chart
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", "", nil // a repo shorthand or an OCI reference: khelm pulls into its cache
	}
	return cfg.Chart, chartRefusal(path), nil
}

// chartRefusal returns why the dry run refuses the local chart in dir ("" when
// it may render), decided on the chart as Helm's loader loads it (the one
// khelm loads: .helmignore honoured, a subchart named by its metadata). It
// refuses what khelm builds dependencies for, and also what `helm dependency
// list` does not call ok or unpacked, because the bash twin decides with
// that command (khelm renders some of those charts without a build, but the
// twin cannot tell them apart). Each cause has its own message and next
// action; the vendor advice is only for a dependency that is not vendored:
//   - the chart does not load (khelm fails too; the dry run cannot say),
//   - a dependency is a file:// chart (khelm always builds those, so there
//     is no walk into one and no nesting limit),
//   - dependencyStatus, the status column of `helm dependency list`, is
//     not ok or unpacked for a dependency (missing, misnamed, wrong
//     version, invalid version, too many matches, corrupt), or
//   - no loaded subchart has the name of a dependency (action.
//     CheckDependencies fails, and khelm builds; `helm lint` says "chart
//     directory is missing these dependencies").
//
// The order is the bash twin's: the table rows in dependency order, then
// the lint check. chartgate_test.go pins this against action.
// CheckDependencies and action.Dependency.List on real chart fixtures, and
// the bats twin pins the bash gate against the real helm v3 on the same
// fixtures (testdata/chartgate-fixtures.txt).
func chartRefusal(dir string) string {
	ch, err := loader.Load(dir)
	if err != nil {
		return fmt.Sprintf("helm cannot load the chart: %v. Fix the chart so that helm can load it.", err)
	}
	if ch.Metadata == nil {
		return "helm cannot load the chart: no Chart.yaml. Fix the chart so that helm can load it."
	}
	for _, dep := range ch.Metadata.Dependencies {
		if strings.HasPrefix(dep.Repository, "file://") {
			return fmt.Sprintf("dependency %s is a file:// chart. khelm builds it into the chart directory on each render. Pass --allow-generators to render it.", dep.Name)
		}
	}
	for _, dep := range ch.Metadata.Dependencies {
		if s := dependencyStatus(dir, dep, ch); s != "ok" && s != "unpacked" {
			return fmt.Sprintf("dependency %s: %s (helm dependency list). %s", dep.Name, s, vendorAdvice)
		}
	}
	var missing []string
	for _, dep := range ch.Metadata.Dependencies {
		if loadedSubchart(ch, dep.Name) == nil {
			missing = append(missing, dep.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("dependency %s: not in the chart that helm loads (helm lint). %s", strings.Join(missing, ","), vendorAdvice)
	}
	return ""
}

// loadedSubchart is the last loaded subchart of parent named name, or nil
// (helm's dependencyStatus takes the last one too).
func loadedSubchart(parent *chart.Chart, name string) *chart.Chart {
	var found *chart.Chart
	for _, sub := range parent.Dependencies() {
		if sub.Name() == name {
			found = sub
		}
	}
	return found
}

// dependencyStatus is the STATUS column of `helm dependency list` for one
// dependency: Helm v3's action.Dependency.dependencyStatus (helm.sh/helm/v3
// v3.21.2, pkg/action/dependency.go), which is unexported, step by step.
// An archive charts/<name>-*.tgz decides first (with more than one, only
// the names whose suffix is a strict semver count); otherwise the loaded
// subchart does.
func dependencyStatus(chartPath string, dep *chart.Dependency, parent *chart.Chart) string {
	archives, err := filepath.Glob(filepath.Join(chartPath, "charts", dep.Name+"-*.tgz"))
	switch {
	case err != nil:
		return "bad pattern"
	case len(archives) > 1:
		var found []string
		for _, arc := range archives {
			maybeVersion := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(arc), ".tgz"), dep.Name+"-")
			if _, err := semver.StrictNewVersion(maybeVersion); err == nil {
				found = append(found, arc)
			}
		}
		if len(found) > 1 {
			return "too many matches"
		}
		if len(found) == 1 {
			if r := archiveStatus(found[0], dep); r != "" {
				return r
			}
		}
	case len(archives) == 1:
		if r := archiveStatus(archives[0], dep); r != "" {
			return r
		}
	}
	sub := loadedSubchart(parent, dep.Name)
	if sub == nil {
		return "missing"
	}
	return versionStatus(dep.Version, sub.Metadata.Version, "unpacked")
}

// archiveStatus is Helm's statArchiveForStatus: the status of one chart
// archive for dep, or "" when the archive is not there.
func archiveStatus(archive string, dep *chart.Dependency) string {
	if _, err := os.Stat(archive); err != nil {
		return ""
	}
	c, err := loader.Load(archive)
	if err != nil {
		return "corrupt"
	}
	if c.Name() != dep.Name {
		return "misnamed"
	}
	return versionStatus(dep.Version, c.Metadata.Version, "ok")
}

// versionStatus is the version check of `helm dependency list`: ok when the
// subchart's version equals the dependency's version or is in its
// constraint.
func versionStatus(constraint, version, ok string) string {
	if version == constraint {
		return ok
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "invalid version"
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return "invalid version"
	}
	if !c.Check(v) {
		return "wrong version"
	}
	return ok
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
	// Helm's loader logs to the standard logger (a symlinked subchart, a v2
	// chart with requirements.yaml). The check is silent, as in the bash
	// twin, which keeps helm's stderr; khelm logs the same lines when the
	// chart renders. The gate runs in its own plugin process.
	prev := log.Writer()
	log.SetOutput(io.Discard)
	chartPath, refusal, err := ChartRefusal(config, wd)
	log.SetOutput(prev)
	if err != nil || refusal == "" {
		return false, err
	}
	fmt.Fprintf(stderr, "lok8s dry-run: chart %s: %s\n", chartPath, refusal)
	if report := os.Getenv("LOK8S_BOOTSTRAP_DRYRUN_REPORT"); report != "" {
		f, ferr := os.OpenFile(report, os.O_WRONLY|os.O_APPEND, 0)
		if ferr != nil {
			return true, ferr
		}
		_, werr := fmt.Fprintf(f, "chart-deps %s\n", chartPath)
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
