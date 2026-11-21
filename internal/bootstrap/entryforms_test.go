package bootstrap

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// bareEntryFixtures holds the written forms of a cilium entry that the bats
// twin (tests/unit/bootstrap_test.bats) reads too.
const bareEntryFixtures = "../../tests/fixtures/bare-entry"

// bareEntryCase lays down one shared case: the addon at
// .lok8s/addons/cilium, the same dir at clusters/test.lok8s.dev/cilium (a
// `./cilium` path entry), the case's cluster spec and the kubeconfig. It
// returns the spec and the kubeconfig.
func bareEntryCase(t *testing.T, p *config.Paths, name string) (string, string) {
	t.Helper()
	for _, dst := range []string{
		filepath.Join(p.Lok8s, "addons", "cilium"),
		filepath.Join(p.Clusters, "test.lok8s.dev", "cilium"),
	} {
		for _, f := range []string{"chart.yaml", "values.yaml"} {
			raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "addon", f))
			if err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(dst, f), string(raw))
		}
	}
	raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "cases", name, "cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, spec, string(raw))
	return spec, writeKubeconfig(t, p)
}

// bareEntryCases lists the shared cases that have the want file <want>,
// with its content. A case without it is a form where the bootstrap stops
// before it reads the dir name (Go resolves `a/cilium/.` to the addon
// cilium and finds no addon for `cilium/..` or `.`).
func bareEntryCases(t *testing.T, want string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(bareEntryFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(bareEntryFixtures, "cases", e.Name(), want))
		if err != nil {
			continue
		}
		cases[e.Name()] = strings.TrimSuffix(string(raw), "\n")
	}
	if len(cases) < 9 {
		t.Fatalf("only %d cases with %s under %s", len(cases), want, bareEntryFixtures)
	}
	return cases
}

const kubeoneDeferLine = "[bootstrap] cilium is applied by the KubeOne driver on a full provision — skipping"

// TestKubeOneDeferEntryForms runs the shared cases on a KubeOne full
// provision: a bare cilium entry with a trailing "/", "/.", "/./" or "//"
// and a path entry with a trailing "/" are left to the driver (no render);
// a path entry with a trailing "/." or "/./" is not cilium and renders.
func TestKubeOneDeferEntryForms(t *testing.T) {
	for name, want := range bareEntryCases(t, "skip") {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LOK8S_BOOTSTRAP_ONLY", "0")
			e, f, out, errOut, p := testEngine(t)
			spec, kc := bareEntryCase(t, p, name)
			if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
				t.Fatalf("Apply: %v\n%s", err, errOut.String())
			}
			skipped := strings.Contains(out.String()+errOut.String(), kubeoneDeferLine)
			switch want {
			case "skip":
				if !skipped || len(f.buildDirs) != 0 {
					t.Errorf("want the defer and no render: skipped=%v renders=%v\n%s%s", skipped, f.buildDirs, out.String(), errOut.String())
				}
			case "apply":
				if skipped || len(f.buildDirs) == 0 {
					t.Errorf("want a render and no defer: skipped=%v renders=%v\n%s%s", skipped, f.buildDirs, out.String(), errOut.String())
				}
			default:
				t.Fatalf("bad want %q", want)
			}
		})
	}
}

// TestDryRunKubeOneDefersABareCiliumWithATrailingDot: the dry run skips a
// bare `cilium/.` on a KubeOne full provision, as the real run does (the
// bats twin: "bootstrap::apply --dry-run: a bare cilium/. is skipped on a
// KubeOne full provision").
func TestDryRunKubeOneDefersABareCiliumWithATrailingDot(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_ONLY", "0")
	e, f, out, errOut, p := testEngine(t)
	spec, kc := bareEntryCase(t, p, "trailing-dot")
	e.Options.DryRun = true
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "bootstrap dry-run: cilium/.: skipped\n") || len(f.buildDirs) != 0 {
		t.Errorf("want the entry skipped and no render: renders=%v\n%s%s", f.buildDirs, out.String(), errOut.String())
	}
}

// TestOnlyEntryForms runs the shared cases with --only cilium: the dir name
// of a bare entry with a trailing "/", "/.", "/./" or "//" is cilium, also
// of a path entry with a trailing "/"; a path entry with a trailing "/."
// or "/./" is not cilium.
func TestOnlyEntryForms(t *testing.T) {
	for name, want := range bareEntryCases(t, "only") {
		t.Run(name, func(t *testing.T) {
			e, _, _, errOut, p := testEngine(t)
			spec, kc := bareEntryCase(t, p, name)
			var mu sync.Mutex
			var applied []string
			e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
				mu.Lock()
				defer mu.Unlock()
				applied = append(applied, job.Name)
				return 0
			}
			e.Options.Only = []string{"cilium"}
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if want == "kept" {
				if err != nil || len(applied) != 1 {
					t.Errorf("want the entry kept: err=%v applied=%v\n%s", err, applied, errOut.String())
				}
				return
			}
			if err == nil || !strings.Contains(errOut.String(), want) || len(applied) != 0 {
				t.Errorf("want %q: err=%v applied=%v\n%s", want, err, applied, errOut.String())
			}
		})
	}
}
