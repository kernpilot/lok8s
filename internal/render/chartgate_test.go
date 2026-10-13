package render

import (
	"os"
	"path/filepath"
	"testing"
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

// khelm's rule, per chart shape: a local chart builds its dependencies (and
// writes Chart.lock/charts/ into the chart directory) when one is a file://
// chart or charts/ lacks one. A remote chart, a local chart without
// dependencies, and a vendored one render without writing.
func TestChartDepsNotVendored(t *testing.T) {
	base := t.TempDir()
	chart := func(name, deps string) string {
		dir := filepath.Join(base, name)
		writeGateFile(t, filepath.Join(dir, "Chart.yaml"), "apiVersion: v2\nname: "+name+"\nversion: 0.1.0\n"+deps)
		return dir
	}
	remoteDep := "dependencies:\n  - name: redis\n    version: 1.0.0\n    repository: https://charts.example.test\n"
	chart("plain", "")
	chart("unvendored", remoteDep)
	vendored := chart("vendored", remoteDep)
	writeGateFile(t, filepath.Join(vendored, "charts", "redis-1.0.0.tgz"), "x")
	vendoredDir := chart("vendored-dir", remoteDep)
	writeGateFile(t, filepath.Join(vendoredDir, "charts", "redis", "Chart.yaml"), "name: redis\n")
	chart("common", "")
	chart("filedep", "dependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../common\n")
	v1 := filepath.Join(base, "v1")
	writeGateFile(t, filepath.Join(v1, "Chart.yaml"), "apiVersion: v1\nname: v1\nversion: 0.1.0\n")
	writeGateFile(t, filepath.Join(v1, "requirements.yaml"), remoteDep)
	for _, tc := range []struct {
		config string
		want   bool
	}{
		{"chart: ./plain\n", false},
		{"chart: ./unvendored\n", true},
		{"chart: " + filepath.Join(base, "unvendored") + "\n", true}, // an absolute path in the project
		{"chart: ./vendored\n", false},
		{"chart: ./vendored-dir\n", false},
		{"chart: ./filedep\n", true},
		{"chart: ./v1\n", true}, // requirements.yaml (apiVersion v1)
		{"repository: https://charts.example.test\nchart: unvendored\n", false},
		{"chart: ./missing\n", false}, // not a directory: a repo shorthand to khelm
	} {
		_, got, err := ChartDepsNotVendored([]byte(tc.config), base)
		if err != nil || got != tc.want {
			t.Errorf("%q: unvendored=%v err=%v, want %v", tc.config, got, err, tc.want)
		}
	}
}
