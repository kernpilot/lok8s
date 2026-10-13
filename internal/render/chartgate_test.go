package render

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func writeGateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeChart writes a minimal real chart (Chart.yaml, one template) into dir.
func writeChart(t *testing.T, dir, name, version string) {
	t.Helper()
	writeGateFile(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v2\nname: "+name+"\nversion: "+version+"\n")
	writeGateFile(t, filepath.Join(dir, "templates", "cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: "+name+"\n")
}

// packChart writes the chart in src as a chart archive (a gzipped tar with
// the chart under one top-level directory, as `helm package` writes it).
func packChart(t *testing.T, src, dest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	top := filepath.Base(src)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(raw))}); err != nil {
			return err
		}
		_, err = tw.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// helmSays is Helm's own verdict on a local chart: khelm builds the
// dependencies when action.CheckDependencies fails on the loaded chart or a
// dependency is a file:// chart; the bash twin also refuses what `helm
// dependency list` does not call ok or unpacked. The table is read the way
// the bash twin reads it: the rows after the header, four tab-separated
// columns, the status last; other lines (the warnings about extra charts)
// are not rows.
func helmSays(t *testing.T, dir string) bool {
	t.Helper()
	ch, err := loader.Load(dir)
	if err != nil {
		return true
	}
	for _, dep := range ch.Metadata.Dependencies {
		if strings.HasPrefix(dep.Repository, "file://") {
			return true
		}
	}
	if action.CheckDependencies(ch, ch.Metadata.Dependencies) != nil {
		return true
	}
	var out strings.Builder
	if err := action.NewDependency().List(dir, &out); err != nil {
		return true
	}
	header := false
	for line := range strings.SplitSeq(out.String(), "\n") {
		cols := strings.Split(line, "\t")
		if len(cols) != 4 {
			continue
		}
		status := strings.TrimSpace(cols[3])
		if !header {
			header = strings.TrimSpace(cols[0]) == "NAME" && status == "STATUS"
			continue
		}
		if status != "ok" && status != "unpacked" {
			return true
		}
	}
	return !header && !strings.HasPrefix(out.String(), "WARNING: no dependencies at ")
}

// khelm's rule on real chart fixtures, decided as Helm's loader sees the
// chart: a vendored archive or directory renders; an archive that
// .helmignore hides, a misnamed one, a version outside the constraint, a
// missing dependency, a corrupt archive, two archives of one dependency and
// a file:// dependency (vendored or not) are refused. Each local fixture is
// also checked against Helm's own functions (action.CheckDependencies and
// action.Dependency.List).
//
// Review round 6, item 6: the cases where the two implementations used to
// disagree, now decided by the status rule of `helm dependency list` on
// both sides (the bats twin runs the real helm v3 on the same fixtures): a
// symlinked subchart and a v2 chart with requirements.yaml render (helm
// logs a line about each on stderr, which is not part of the table); a
// directory next to a <name>-<other>-<version>.tgz is misnamed; an archive
// next to a directory of the same chart is ok; two directories of one
// dependency are unpacked; a chart or a subchart without a version does not
// load, and a dependency without a version is an invalid version.
func TestChartDepsNotVendored(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard) // Helm's loader logs about the symlink and the v2 requirements.yaml
	t.Cleanup(func() { log.SetOutput(prev) })
	base := t.TempDir()
	src := t.TempDir()
	writeChart(t, filepath.Join(src, "redis"), "redis", "1.0.0")
	writeChart(t, filepath.Join(src, "other", "redis"), "other", "1.0.0") // an archive named redis-*.tgz that holds chart "other"
	redis11 := filepath.Join(src, "v11", "redis")
	writeChart(t, redis11, "redis", "1.1.0")

	parent := func(name, deps string) string {
		dir := filepath.Join(base, name)
		writeChart(t, dir, name, "0.1.0")
		if deps != "" {
			writeGateFile(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v2\nname: "+name+"\nversion: 0.1.0\n"+deps)
		}
		return dir
	}
	redisDep := func(version string) string {
		return "dependencies:\n  - name: redis\n    version: \"" + version + "\"\n    repository: https://charts.example.test\n"
	}
	parent("plain", "")
	parent("missing", redisDep("1.0.0"))
	parent("missing-no-version", "dependencies:\n  - name: redis\n    repository: https://charts.example.test\n")
	packChart(t, filepath.Join(src, "redis"), filepath.Join(parent("vendored-tgz", redisDep("1.0.0")), "charts", "redis-1.0.0.tgz"))
	packChart(t, filepath.Join(src, "redis"), filepath.Join(parent("range", redisDep("~1.0.0")), "charts", "redis-1.0.0.tgz"))
	writeChart(t, filepath.Join(parent("vendored-dir", redisDep("1.0.0")), "charts", "redis"), "redis", "1.0.0")
	ignored := parent("helmignored", redisDep("1.0.0"))
	packChart(t, filepath.Join(src, "redis"), filepath.Join(ignored, "charts", "redis-1.0.0.tgz"))
	writeGateFile(t, filepath.Join(ignored, ".helmignore"), "*.tgz\n")
	packChart(t, filepath.Join(src, "other", "redis"), filepath.Join(parent("misnamed", redisDep("1.0.0")), "charts", "redis-1.0.0.tgz"))
	packChart(t, filepath.Join(src, "redis"), filepath.Join(parent("wrong-version", redisDep("2.0.0")), "charts", "redis-1.0.0.tgz"))
	two := parent("two-archives", redisDep(">=1.0.0"))
	packChart(t, filepath.Join(src, "redis"), filepath.Join(two, "charts", "redis-1.0.0.tgz"))
	packChart(t, redis11, filepath.Join(two, "charts", "redis-1.1.0.tgz"))
	writeGateFile(t, filepath.Join(parent("corrupt", redisDep("1.0.0")), "charts", "redis-1.0.0.tgz"), "x")
	parent("common", "")
	fileDep := "dependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../common\n"
	parent("filedep", fileDep)
	writeChart(t, filepath.Join(parent("filedep-vendored", fileDep), "charts", "common"), "common", "0.1.0")
	v1 := filepath.Join(base, "v1")
	writeGateFile(t, filepath.Join(v1, "Chart.yaml"), "apiVersion: v1\nname: v1\nversion: 0.1.0\n")
	writeGateFile(t, filepath.Join(v1, "requirements.yaml"), redisDep("1.0.0"))
	v1v := filepath.Join(base, "v1-vendored")
	writeGateFile(t, filepath.Join(v1v, "Chart.yaml"), "apiVersion: v1\nname: v1-vendored\nversion: 0.1.0\n")
	writeGateFile(t, filepath.Join(v1v, "requirements.yaml"), redisDep("1.0.0"))
	writeChart(t, filepath.Join(v1v, "charts", "redis"), "redis", "1.0.0")
	// Round 6, item 6.
	symlinked := parent("symlinked", redisDep("1.0.0"))
	if err := os.MkdirAll(filepath.Join(symlinked, "charts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "redis"), filepath.Join(symlinked, "charts", "redis")); err != nil {
		t.Fatal(err)
	}
	v2req := parent("v2-req", "")
	writeGateFile(t, filepath.Join(v2req, "requirements.yaml"), redisDep("1.0.0"))
	writeChart(t, filepath.Join(v2req, "charts", "redis"), "redis", "1.0.0")
	writeGateFile(t, filepath.Join(parent("v2-req-missing", ""), "requirements.yaml"), redisDep("1.0.0"))
	writeChart(t, filepath.Join(src, "rb", "redis-bar"), "redis-bar", "2.0.0")
	fooBar := parent("dir-and-foo-bar", redisDep("1.0.0"))
	writeChart(t, filepath.Join(fooBar, "charts", "redis"), "redis", "1.0.0")
	packChart(t, filepath.Join(src, "rb", "redis-bar"), filepath.Join(fooBar, "charts", "redis-bar-2.0.0.tgz"))
	tgzDir := parent("tgz-and-dir", redisDep("1.0.0"))
	packChart(t, filepath.Join(src, "redis"), filepath.Join(tgzDir, "charts", "redis-1.0.0.tgz"))
	writeChart(t, filepath.Join(tgzDir, "charts", "redis"), "redis", "1.0.0")
	twoDirs := parent("two-dirs", redisDep("1.0.0"))
	writeChart(t, filepath.Join(twoDirs, "charts", "redis"), "redis", "1.0.0")
	writeChart(t, filepath.Join(twoDirs, "charts", "redis2"), "redis", "1.0.0")
	noVersion := parent("parent-no-version", "")
	writeGateFile(t, filepath.Join(noVersion, "Chart.yaml"), "apiVersion: v2\nname: parent-no-version\n"+redisDep("1.0.0"))
	writeChart(t, filepath.Join(noVersion, "charts", "redis"), "redis", "1.0.0")
	subNoVersion := parent("sub-no-version", redisDep("1.0.0"))
	writeChart(t, filepath.Join(subNoVersion, "charts", "redis"), "redis", "1.0.0")
	writeGateFile(t, filepath.Join(subNoVersion, "charts", "redis", "Chart.yaml"), "apiVersion: v2\nname: redis\n")
	depNoVersion := parent("dep-no-version", "dependencies:\n  - name: redis\n    repository: https://charts.example.test\n")
	writeChart(t, filepath.Join(depNoVersion, "charts", "redis"), "redis", "1.0.0")

	for _, tc := range []struct {
		chart string
		want  bool
	}{
		{"plain", false},
		{"vendored-tgz", false},
		{"range", false},
		{"vendored-dir", false},
		{"v1-vendored", false},
		{"missing", true},
		{"missing-no-version", true},
		{"helmignored", true},
		{"misnamed", true},
		{"wrong-version", true},
		{"two-archives", true},
		{"corrupt", true},
		{"filedep", true},
		{"filedep-vendored", true},
		{"v1", true},
		{"symlinked", false},
		{"v2-req", false},
		{"v2-req-missing", true},
		{"dir-and-foo-bar", true},
		{"tgz-and-dir", false},
		{"two-dirs", false},
		{"parent-no-version", true},
		{"sub-no-version", true},
		{"dep-no-version", true},
	} {
		dir := filepath.Join(base, tc.chart)
		// Every fixture but these loads: a refusal is for the dependency,
		// not for a chart Helm cannot read.
		unloadable := tc.chart == "corrupt" || tc.chart == "parent-no-version" || tc.chart == "sub-no-version"
		if _, err := loader.Load(dir); (err != nil) != unloadable {
			t.Errorf("%s: the fixture loads with %v", tc.chart, err)
		}
		if helm := helmSays(t, dir); helm != tc.want {
			t.Errorf("%s: the fixture is wrong: Helm says %v, the table %v", tc.chart, helm, tc.want)
		}
		for _, config := range []string{"chart: ./" + tc.chart + "\n", "chart: " + dir + "\n"} {
			_, got, err := ChartDepsNotVendored([]byte(config), base)
			if err != nil || got != tc.want {
				t.Errorf("%q: unvendored=%v err=%v, want %v", config, got, err, tc.want)
			}
		}
	}
	for _, config := range []string{
		"repository: https://charts.example.test\nchart: missing\n", // a remote chart: khelm pulls it into its cache
		"chart: ./does-not-exist\n",                                 // not a directory: a repo shorthand to khelm
	} {
		if _, got, err := ChartDepsNotVendored([]byte(config), base); err != nil || got {
			t.Errorf("%q: unvendored=%v err=%v, want false", config, got, err)
		}
	}
	// The gate reads only: no fixture gained a lock file or a tmpcharts dir.
	for _, name := range []string{"missing", "helmignored", "filedep"} {
		for _, f := range []string{"Chart.lock", "tmpcharts"} {
			if _, err := os.Stat(filepath.Join(base, name, f)); err == nil {
				t.Errorf("%s: the gate wrote %s", name, f)
			}
		}
	}
}

// Review round 6, item 6: Helm's loader logs to the standard logger about
// a symlinked subchart (and about a v2 chart with requirements.yaml). The
// gate keeps it off stderr, as the bash twin keeps helm's stderr out of its
// output; the refusal line and a real chart render are what the user sees.
func TestChartGateKeepsHelmsLogOffStderr(t *testing.T) {
	base := t.TempDir()
	writeChart(t, filepath.Join(base, "src", "redis"), "redis", "1.0.0")
	dir := filepath.Join(base, "linked")
	writeChart(t, dir, "linked", "0.1.0")
	writeGateFile(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v2\nname: linked\nversion: 0.1.0\ndependencies:\n  - name: redis\n    version: 1.0.0\n    repository: https://charts.example.test\n")
	if err := os.MkdirAll(filepath.Join(dir, "charts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "src", "redis"), filepath.Join(dir, "charts", "redis")); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	// The fixture makes the loader log (else this test proves nothing).
	if _, err := loader.Load(dir); err != nil || !strings.Contains(logged.String(), "found symbolic link") {
		t.Fatalf("the fixture does not make Helm's loader log: err %v, log %q", err, logged.String())
	}
	logged.Reset()
	t.Chdir(base)
	t.Setenv("KUSTOMIZE_PLUGIN_CONFIG_STRING", "chart: ./linked\n")
	var stderr strings.Builder
	refused, err := chartGate([]string{"ChartRenderer"}, &stderr)
	if err != nil || refused {
		t.Fatalf("the symlinked subchart was refused (%v, %v): %s", refused, err, stderr.String())
	}
	if logged.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("the gate printed Helm's log: log %q, stderr %q", logged.String(), stderr.String())
	}
	if log.Writer() != &logged {
		t.Error("the gate did not restore the standard logger's output")
	}
}
