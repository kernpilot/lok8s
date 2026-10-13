package render

// chartgate.go — the ChartRenderer gate of `lo bootstrap --dry-run` (both
// builds; the bash twin is the wrapper script addons::dry_run_boundary
// writes). khelm builds the dependencies of a LOCAL chart into the chart
// directory (Chart.lock, charts/, tmpcharts/) whenever they are not vendored
// or one of them is a file:// chart. A dry run must not write there, so it
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

	"gopkg.in/yaml.v3"
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
// the kustomization root) and declares a file:// dependency, or a
// dependency that charts/ does not hold. Chart.yaml and requirements.yaml
// (apiVersion v1) dependencies both count. chart is the local chart path
// ("" for a remote chart).
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

type chartDeps struct {
	Dependencies []struct {
		Name       string `yaml:"name"`
		Repository string `yaml:"repository"`
	} `yaml:"dependencies"`
}

// chartBuildsDeps applies khelm's rule to one chart directory: a file://
// dependency is always built (so there is no walk into it, and no nesting
// limit); another dependency is built when charts/ lacks it.
func chartBuildsDeps(dir string) bool {
	var deps chartDeps
	for _, f := range []string{"Chart.yaml", "requirements.yaml"} {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		var d chartDeps
		if yaml.Unmarshal(raw, &d) == nil {
			deps.Dependencies = append(deps.Dependencies, d.Dependencies...)
		}
	}
	for _, dep := range deps.Dependencies {
		if strings.HasPrefix(dep.Repository, "file://") {
			return true
		}
		if !chartVendored(filepath.Join(dir, "charts"), dep.Name) {
			return true
		}
	}
	return false
}

// chartVendored reports a dependency charts/ holds: a directory named after
// it, or an archive <name>-<version>.tgz.
func chartVendored(chartsDir, name string) bool {
	if name == "" {
		return false
	}
	if info, err := os.Stat(filepath.Join(chartsDir, name)); err == nil && info.IsDir() {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(chartsDir, name+"-*.tgz"))
	return len(matches) > 0
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
