package addons

// Port of tests/unit/addons_detail_test.bats — the helper + list/show half;
// the --detail inventory tests live in internal/cli (they need the real
// bootstrap parser, which this package cannot import).

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

func frameworkLok8s(t *testing.T) string {
	t.Helper()
	fw, _ := filepath.Abs(filepath.Join("..", "..", ".lok8s"))
	if info, err := os.Stat(filepath.Join(fw, "addons")); err != nil || !info.IsDir() {
		t.Skip("framework addons tree not available")
	}
	return fw
}

func sandbox(t *testing.T, lok8s string) *config.Paths {
	t.Helper()
	base := t.TempDir()
	if lok8s == "" {
		lok8s = filepath.Join(base, ".lok8s")
		os.MkdirAll(filepath.Join(lok8s, "addons"), 0o755)
	}
	return &config.Paths{Base: base, Lok8s: lok8s, Clusters: filepath.Join(base, "clusters")}
}

func writeSpec(t *testing.T, p *config.Paths, d, body string) {
	t.Helper()
	dir := filepath.Join(p.Clusters, d)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "cluster.lok8s.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCategoryReadsLabel(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ns.yaml"), []byte("metadata:\n  labels:\n    lok8s.dev/category: storage\n"), 0o644)
	if got := Category(dir); got != "storage" {
		t.Errorf("Category = %q", got)
	}
	// The bytewise-first match wins across files.
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("lok8s.dev/category:   networking\n"), 0o644)
	if got := Category(dir); got != "networking" {
		t.Errorf("Category = %q, want the sorted-first winner", got)
	}
	if got := Category(t.TempDir()); got != "-" {
		t.Errorf("unlabelled = %q", got)
	}
}

// Every .lok8s/addons/ dir has a config-help entry (parity, fails on drift).
func TestEveryAddonHasConfigHint(t *testing.T) {
	fw := frameworkLok8s(t)
	var missing []string
	for _, dir := range addonDirs(filepath.Join(fw, "addons")) {
		n := filepath.Base(dir)
		hint := ConfigHint(n)
		if hint == "" {
			missing = append(missing, n)
		}
		if strings.Contains(hint, "\n") {
			t.Errorf("%s: hint is not a one-liner", n)
		}
	}
	if len(missing) > 0 {
		t.Errorf("addons without a config hint: %v", missing)
	}
	if ConfigHint("definitely-not-an-addon") != "" {
		t.Error("unknown addon has a hint")
	}
}

// The rc 2 contract — a malformed kind is never defaulted to "lo".
func TestDriverRefusesMalformedKind(t *testing.T) {
	p := sandbox(t, "")
	writeSpec(t, p, "bad", "kind: ../../evil\nmetadata: { name: bad }\n")
	var stderr bytes.Buffer
	if got, err := Driver(p, "bad", &stderr); err == nil || got == "lo" {
		t.Errorf("Driver = %q, %v", got, err)
	}
	if !strings.Contains(stderr.String(), "malformed kind") {
		t.Errorf("stderr: %s", stderr.String())
	}
	os.MkdirAll(filepath.Join(p.Clusters, "nospec"), 0o755)
	if got, err := Driver(p, "nospec", io.Discard); err != nil || got != "lo" {
		t.Errorf("no spec: Driver = %q, %v (want lo)", got, err)
	}
}

func TestListAndShow(t *testing.T) {
	p := sandbox(t, "")
	root := Dir(p)
	os.MkdirAll(filepath.Join(root, "khelm-one"), 0o755)
	os.WriteFile(filepath.Join(root, "khelm-one", "chart.yaml"), []byte("kind: ChartRenderer\nchart: thing\nversion: 1.2.3\nrepository: https://example.test\n"), 0o644)
	os.WriteFile(filepath.Join(root, "khelm-one", "values.yaml"), []byte("a: 1\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "raw-one"), 0o755)
	os.WriteFile(filepath.Join(root, "raw-one", "kustomization.yaml"), []byte("resources: []\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "empty-one"), 0o755)
	os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)

	var out bytes.Buffer
	if err := List(p, "any.dev", &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := "NAME                  TYPE      VERSION       CHART/REPO\n" +
		"----                  ----      -------       ----------\n" +
		"empty-one             empty     -             -\n" +
		"khelm-one             khelm     1.2.3         thing (https://example.test)\n" +
		"raw-one               raw       -             -\n"
	if out.String() != want {
		t.Errorf("list:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	if err := Show(p, "any.dev", "khelm-one", &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	want = "name:    khelm-one\ndriver:  lo\npath:    " + root + "/khelm-one\ntype:    khelm\n" +
		"chart:   thing\nversion: 1.2.3\nrepo:    https://example.test\n\nfiles:\n  - chart.yaml\n  - values.yaml\n"
	if out.String() != want {
		t.Errorf("show:\n%s\nwant:\n%s", out.String(), want)
	}

	var stderr bytes.Buffer
	if err := Show(p, "any.dev", "nope", io.Discard, &stderr); err == nil || !strings.Contains(stderr.String(), "addon 'nope' not found") {
		t.Errorf("missing addon: err=%v stderr=%s", err, stderr.String())
	}

	// No addons directory at all → warning, rc 0.
	p2 := &config.Paths{Base: t.TempDir(), Lok8s: t.TempDir() + "/none", Clusters: t.TempDir()}
	stderr.Reset()
	if err := List(p2, "x", io.Discard, &stderr); err != nil || !strings.Contains(stderr.String(), "No addons directory") {
		t.Errorf("err=%v stderr=%s", err, stderr.String())
	}
}
