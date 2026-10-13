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

// gateFixture is one row of testdata/chartgate-fixtures.txt, the table the
// bats twin reads too.
type gateFixture struct {
	name, verdict, cause string
}

// readGateFixtures parses testdata/chartgate-fixtures.txt.
func readGateFixtures(t *testing.T) []gateFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "chartgate-fixtures.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []gateFixture
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) != 3 || (f[1] != "render" && f[1] != "refuse") || (f[1] == "render") != (f[2] == "-") {
			t.Fatalf("chartgate-fixtures.txt: bad row %q", line)
		}
		rows = append(rows, gateFixture{f[0], f[1], f[2]})
	}
	return rows
}

// causeText is what the refusal of each cause says (both implementations).
var causeText = map[string]string{
	"deps": "Vendor them into charts/, or pass --allow-generators.",
	"file": "is a file:// chart. khelm builds it into the chart directory on each render. Pass --allow-generators to render it.",
	"load": "Fix the chart so that helm can load it.",
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
//
// Review round 7, items 2 and 4: the fixtures and their verdicts come from
// testdata/chartgate-fixtures.txt, the table the bats twin reads, so the two
// suites cannot drift; every row has a builder here and every builder a
// row. The cause column pins each refusal's own message: only a dependency
// that is not vendored gets the vendor advice.
func TestChartRefusal(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard) // Helm's loader logs about the symlink and the v2 requirements.yaml
	t.Cleanup(func() { log.SetOutput(prev) })
	base := t.TempDir()
	src := t.TempDir()
	writeChart(t, filepath.Join(src, "redis"), "redis", "1.0.0")
	writeChart(t, filepath.Join(src, "other", "redis"), "other", "1.0.0") // an archive named redis-*.tgz that holds chart "other"
	redis11 := filepath.Join(src, "v11", "redis")
	writeChart(t, redis11, "redis", "1.1.0")
	writeChart(t, filepath.Join(src, "rb", "redis-bar"), "redis-bar", "2.0.0")
	writeChart(t, filepath.Join(base, "common"), "common", "0.1.0") // the file:// target

	parent := func(dir, deps string) {
		name := filepath.Base(dir)
		writeChart(t, dir, name, "0.1.0")
		if deps != "" {
			writeGateFile(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v2\nname: "+name+"\nversion: 0.1.0\n"+deps)
		}
	}
	redisDep := func(version string) string {
		return "dependencies:\n  - name: redis\n    version: \"" + version + "\"\n    repository: https://charts.example.test\n"
	}
	const noVersionDep = "dependencies:\n  - name: redis\n    repository: https://charts.example.test\n"
	const fileDep = "dependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../common\n"
	tgz := func(chart, dir, file string) { packChart(t, chart, filepath.Join(dir, "charts", file)) }
	sub := func(dir, name, version string) { writeChart(t, filepath.Join(dir, "charts", name), "redis", version) }
	v1 := func(dir string) {
		writeGateFile(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v1\nname: "+filepath.Base(dir)+"\nversion: 0.1.0\n")
		writeGateFile(t, filepath.Join(dir, "requirements.yaml"), redisDep("1.0.0"))
	}
	builders := map[string]func(dir string){
		"plain":              func(d string) { parent(d, "") },
		"vendored-tgz":       func(d string) { parent(d, redisDep("1.0.0")); tgz(filepath.Join(src, "redis"), d, "redis-1.0.0.tgz") },
		"vendored-dir":       func(d string) { parent(d, redisDep("1.0.0")); sub(d, "redis", "1.0.0") },
		"range":              func(d string) { parent(d, redisDep("~1.0.0")); tgz(filepath.Join(src, "redis"), d, "redis-1.0.0.tgz") },
		"v1-vendored":        func(d string) { v1(d); sub(d, "redis", "1.0.0") },
		"missing":            func(d string) { parent(d, redisDep("1.0.0")) },
		"missing-no-version": func(d string) { parent(d, noVersionDep) },
		"helmignored": func(d string) {
			parent(d, redisDep("1.0.0"))
			tgz(filepath.Join(src, "redis"), d, "redis-1.0.0.tgz")
			writeGateFile(t, filepath.Join(d, ".helmignore"), "*.tgz\n")
		},
		"misnamed": func(d string) {
			parent(d, redisDep("1.0.0"))
			tgz(filepath.Join(src, "other", "redis"), d, "redis-1.0.0.tgz")
		},
		"wrong-version": func(d string) { parent(d, redisDep("2.0.0")); tgz(filepath.Join(src, "redis"), d, "redis-1.0.0.tgz") },
		"two-archives": func(d string) {
			parent(d, redisDep(">=1.0.0"))
			tgz(filepath.Join(src, "redis"), d, "redis-1.0.0.tgz")
			tgz(redis11, d, "redis-1.1.0.tgz")
		},
		"corrupt": func(d string) {
			parent(d, redisDep("1.0.0"))
			writeGateFile(t, filepath.Join(d, "charts", "redis-1.0.0.tgz"), "x")
		},
		"filedep": func(d string) { parent(d, fileDep) },
		"filedep-vendored": func(d string) {
			parent(d, fileDep)
			writeChart(t, filepath.Join(d, "charts", "common"), "common", "0.1.0")
		},
		"v1": v1,
		"symlinked": func(d string) {
			parent(d, redisDep("1.0.0"))
			if err := os.MkdirAll(filepath.Join(d, "charts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(src, "redis"), filepath.Join(d, "charts", "redis")); err != nil {
				t.Fatal(err)
			}
		},
		"v2-req": func(d string) {
			parent(d, "")
			writeGateFile(t, filepath.Join(d, "requirements.yaml"), redisDep("1.0.0"))
			sub(d, "redis", "1.0.0")
		},
		"v2-req-missing": func(d string) {
			parent(d, "")
			writeGateFile(t, filepath.Join(d, "requirements.yaml"), redisDep("1.0.0"))
		},
		"dir-and-foo-bar": func(d string) {
			parent(d, redisDep("1.0.0"))
			sub(d, "redis", "1.0.0")
			tgz(filepath.Join(src, "rb", "redis-bar"), d, "redis-bar-2.0.0.tgz")
		},
		"tgz-and-dir": func(d string) {
			parent(d, redisDep("1.0.0"))
			tgz(filepath.Join(src, "redis"), d, "redis-1.0.0.tgz")
			sub(d, "redis", "1.0.0")
		},
		"two-dirs": func(d string) { parent(d, redisDep("1.0.0")); sub(d, "redis", "1.0.0"); sub(d, "redis2", "1.0.0") },
		"parent-no-version": func(d string) {
			parent(d, "")
			writeGateFile(t, filepath.Join(d, "Chart.yaml"), "apiVersion: v2\nname: parent-no-version\n"+redisDep("1.0.0"))
			sub(d, "redis", "1.0.0")
		},
		"sub-no-version": func(d string) {
			parent(d, redisDep("1.0.0"))
			sub(d, "redis", "1.0.0")
			writeGateFile(t, filepath.Join(d, "charts", "redis", "Chart.yaml"), "apiVersion: v2\nname: redis\n")
		},
		"dep-no-version": func(d string) { parent(d, noVersionDep); sub(d, "redis", "1.0.0") },
	}

	rows := readGateFixtures(t)
	inTable := map[string]bool{}
	for _, row := range rows {
		inTable[row.name] = true
	}
	for name := range builders {
		if !inTable[name] {
			t.Errorf("fixture %s has a builder but no row in testdata/chartgate-fixtures.txt", name)
		}
	}
	for _, row := range rows {
		build, ok := builders[row.name]
		if !ok {
			t.Errorf("testdata/chartgate-fixtures.txt: no builder for fixture %s", row.name)
			continue
		}
		dir := filepath.Join(base, row.name)
		build(dir)
		want := row.verdict == "refuse"
		// A fixture that does not load is refused for that, not for a
		// dependency.
		if _, err := loader.Load(dir); (err != nil) != (row.cause == "load") {
			t.Errorf("%s: the fixture loads with %v", row.name, err)
		}
		if helm := helmSays(t, dir); helm != want {
			t.Errorf("%s: the fixture is wrong: Helm says %v, the table %s", row.name, helm, row.verdict)
		}
		for _, config := range []string{"chart: ./" + row.name + "\n", "chart: " + dir + "\n"} {
			_, refusal, err := ChartRefusal([]byte(config), base)
			if err != nil || (refusal != "") != want {
				t.Errorf("%q: refusal %q err=%v, want %s", config, refusal, err, row.verdict)
			}
			if want && !strings.Contains(refusal, causeText[row.cause]) {
				t.Errorf("%q: refusal %q, want the %s message", config, refusal, row.cause)
			}
			if row.cause != "deps" && strings.Contains(refusal, causeText["deps"]) {
				t.Errorf("%q: refusal %q gives the vendor advice", config, refusal)
			}
		}
	}
	for _, config := range []string{
		"repository: https://charts.example.test\nchart: missing\n", // a remote chart: khelm pulls it into its cache
		"chart: ./does-not-exist\n",                                 // not a directory: a repo shorthand to khelm
	} {
		if _, refusal, err := ChartRefusal([]byte(config), base); err != nil || refusal != "" {
			t.Errorf("%q: refusal %q err=%v, want none", config, refusal, err)
		}
	}
	// The gate reads only: no fixture gained a lock file or a tmpcharts dir.
	for _, row := range rows {
		for _, f := range []string{"Chart.lock", "tmpcharts"} {
			if _, err := os.Stat(filepath.Join(base, row.name, f)); err == nil {
				t.Errorf("%s: the gate wrote %s", row.name, f)
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
