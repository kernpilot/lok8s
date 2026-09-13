package assets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// withCache points the cache at a fresh temp dir (t.Setenv: the test must
// not be parallel) and returns the tree dir BashTree would fill.
func withCache(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(EnvCacheHome, root)
	return filepath.Join(root, "lok8s", Version(), "lok8s")
}

func TestBashTreeChecoutWinsOverCache(t *testing.T) {
	cache := withCache(t)
	p := project(t)

	// PATH_LOK8S (p.Lok8s) holding lo: served as is, nothing extracted.
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
	tree, err := BashTree(p)
	if err != nil || tree.Origin != OriginLocal || tree.Dir != p.Lok8s {
		t.Fatalf("checkout: %+v %v", tree, err)
	}
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("a checkout must not trigger a cache extract")
	}
	if got := FindBashTree(p); got != tree {
		t.Fatalf("FindBashTree = %+v, want %+v", got, tree)
	}

	// PATH_LOK8S elsewhere (no lo there), the project's .lok8s holds lo.
	other := t.TempDir()
	q := *p
	q.Lok8s = other
	tree, err = BashTree(&q)
	if err != nil || tree.Origin != OriginLocal || tree.Dir != p.Lok8s {
		t.Fatalf("project tree: %+v %v", tree, err)
	}
	// Neither: the cache.
	os.Remove(filepath.Join(p.Lok8s, "lo"))
	tree, err = BashTree(&q)
	if err != nil || tree.Origin != OriginCache || tree.Dir != cache {
		t.Fatalf("cache: %+v %v", tree, err)
	}
}

func TestBashTreeExtractsOnceAndRedoesAPartialExtract(t *testing.T) {
	cache := withCache(t)
	p := project(t)

	if got := FindBashTree(p); got.Origin != OriginNone || got.Dir != cache {
		t.Fatalf("before extract: %+v", got)
	}
	tree, err := BashTree(p)
	if err != nil || tree.Origin != OriginCache || tree.Dir != cache {
		t.Fatalf("%+v %v", tree, err)
	}
	// The whole tree, byte-identical, executable bits restored.
	onDisk := testutil.ReadDir(t, "cache", cache, func(rel string) bool { return rel == MarkerFile })
	embedded := testutil.ReadFS(t, "embed", FS(), nil)
	testutil.Drift{Want: embedded, Got: onDisk, Sync: "BashTree"}.Check(t)
	for _, f := range []string{"lo", "libs/build", "utils/provider.sh"} {
		info, err := os.Stat(filepath.Join(cache, filepath.FromSlash(f)))
		if err != nil || info.Mode()&0o100 == 0 {
			t.Errorf("%s: not executable (%v, %v)", f, info, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(cache, "libs", "doctor")); info.Mode()&0o100 != 0 {
		t.Error("libs/doctor is executable in the cache but not in the mirror")
	}
	m, err := ReadMarker(filepath.Join(cache, MarkerFile))
	if err != nil || m == nil || m.Lo != Version() || len(m.Files) != len(embedded.Files) {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	// No stage dir left beside the tree.
	entries, _ := os.ReadDir(filepath.Dir(cache))
	if len(entries) != 1 {
		t.Errorf("cache version dir holds %d entries", len(entries))
	}
	if got := FindBashTree(p); got != tree {
		t.Fatalf("FindBashTree after extract = %+v", got)
	}

	// Idempotent: a second call leaves the extracted tree alone (a stray
	// file survives, so nothing was re-extracted).
	stray := filepath.Join(cache, "stray")
	testutil.WriteFile(t, stray, "x")
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("a valid cache was re-extracted")
	}

	// Partial: a file missing → redone (the stray goes with the stale copy).
	os.Remove(filepath.Join(cache, "libs", "build"))
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "libs", "build")); err != nil {
		t.Fatal("missing file not restored")
	}
	if _, err := os.Stat(stray); err == nil {
		t.Fatal("a partial cache was patched instead of replaced")
	}
	// Corrupt: a file with other content → redone.
	os.WriteFile(filepath.Join(cache, "lo"), []byte("tampered\n"), 0o755)
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(cache, "lo")); strings.HasPrefix(string(raw), "tampered") {
		t.Fatal("a tampered file was kept")
	}
	// Lost mode → redone.
	os.Chmod(filepath.Join(cache, "lo"), 0o644)
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(cache, "lo")); info.Mode()&0o100 == 0 {
		t.Fatal("lost executable bit not restored")
	}
	// A manifest from another version → redone.
	os.WriteFile(filepath.Join(cache, MarkerFile), []byte("lo: 0.0.0\nfiles: {}\n"), 0o644)
	if got := FindBashTree(p); got.Origin != OriginNone {
		t.Fatalf("stale manifest still valid: %+v", got)
	}
	if tree, err := BashTree(p); err != nil || tree.Origin != OriginCache || !cacheValid(cache) {
		t.Fatalf("stale manifest: %+v %v", tree, err)
	}
}

func TestBashTreeCacheLocationAndTempFallback(t *testing.T) {
	p := project(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvCacheHome, "")
	os.Unsetenv(EnvCacheHome)
	tree, err := BashTree(p)
	want := filepath.Join(home, ".cache", "lok8s", Version(), "lok8s")
	if err != nil || tree.Origin != OriginCache || tree.Dir != want {
		t.Fatalf("HOME cache: %+v %v (want %s)", tree, err, want)
	}

	// Neither HOME nor XDG_CACHE_HOME: the per-run temp dir, gone on Cleanup.
	withPolicy(t, PolicyEject)
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	if got := FindBashTree(p); got.Origin != OriginNone || got.Dir != "" {
		t.Fatalf("no cache dir: %+v", got)
	}
	tree, err = BashTree(p)
	if err != nil || tree.Origin != OriginEmbedded {
		t.Fatalf("temp fallback: %+v %v", tree, err)
	}
	for _, f := range []string{"lo", "addons/cilium/chart.yaml", "tilt/Tiltfile", "VERSION"} {
		if _, err := os.Stat(filepath.Join(tree.Dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("temp tree incomplete: %s: %v", f, err)
		}
	}
	if strings.HasPrefix(tree.Dir, p.Base) || strings.HasPrefix(tree.Dir, home) {
		t.Fatalf("temp tree inside the project or home: %s", tree.Dir)
	}
	Cleanup()
	if _, err := os.Stat(tree.Dir); err == nil {
		t.Fatal("Cleanup left the temp tree")
	}
}

func TestEjectBashWritesTheCodeHalfBesideDataUnits(t *testing.T) {
	withPolicy(t, PolicyEject)
	notices := quiet(t)
	t.Setenv("SOURCE_DATE_EPOCH", "0")
	p := project(t)

	// A data unit first, then the bash unit lands beside it.
	if _, _, err := Resolve(p, "drivers/lo/cluster"); err != nil {
		t.Fatal(err)
	}
	cilium := filepath.Join(p.Lok8s, "addons", "cilium")
	os.MkdirAll(cilium, 0o755)
	os.WriteFile(filepath.Join(cilium, "chart.yaml"), []byte("version: 0.0.1-local\n"), 0o644)
	// A pre-existing code file: kept, never overwritten.
	custom := filepath.Join(p.Lok8s, "libs", "build")
	testutil.WriteFile(t, custom, "# my build\n")
	notices.Reset()

	u, err := Eject(p, BashRel)
	if err != nil || u.Kind != KindBash {
		t.Fatalf("Eject bash: %+v %v", u, err)
	}
	if !strings.Contains(notices.String(), "[assets] ejected bash -> .lok8s; 1 existing file(s) kept (review with: lo assets diff bash)") {
		t.Errorf("notice: %q", notices.String())
	}
	for _, f := range []string{"lo", "libs/deploy", "utils/domain.sh", "drivers/lo/main", "drivers/lo/utils/config.sh", "providers/hetzner/main", "VERSION", MarkerFile} {
		if _, err := os.Stat(filepath.Join(p.Lok8s, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s not ejected: %v", f, err)
		}
	}
	if raw, _ := os.ReadFile(custom); string(raw) != "# my build\n" {
		t.Fatal("an existing file was overwritten")
	}
	if raw, _ := os.ReadFile(filepath.Join(cilium, "chart.yaml")); string(raw) != "version: 0.0.1-local\n" {
		t.Fatal("a data unit was touched by the bash eject")
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "drivers", "lo", "cluster", MarkerFile)); err != nil {
		t.Fatal("the driver unit's marker vanished")
	}
	if info, _ := os.Stat(filepath.Join(p.Lok8s, "lo")); info.Mode()&0o100 == 0 {
		t.Error("lo not executable")
	}
	// Data units are NOT part of the bash unit: chat was never ejected.
	if _, err := os.Stat(filepath.Join(p.Lok8s, "chat")); err == nil {
		t.Error("the bash eject wrote a data unit (chat)")
	}
	// No stage dir left behind.
	entries, _ := os.ReadDir(p.Lok8s)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bash.lo-eject-") {
			t.Errorf("stage dir left: %s", e.Name())
		}
	}
	// The marker at the tree root lists the code half only.
	m, err := ReadMarker(filepath.Join(p.Lok8s, MarkerFile))
	if err != nil || m == nil || m.Lo != Version() {
		t.Fatalf("marker: %+v %v", m, err)
	}
	embedded, _ := EmbeddedFiles(bashUnit)
	if len(m.Files) != len(embedded) || m.Files["addons/cilium/chart.yaml"] != "" || m.Files["lo"] == "" {
		t.Errorf("marker files: %d (embed %d)", len(m.Files), len(embedded))
	}

	// Precedence now: the project tree serves bash mode; a second eject refuses.
	if tree, err := BashTree(p); err != nil || tree.Origin != OriginLocal || tree.Dir != p.Lok8s {
		t.Fatalf("BashTree after eject: %+v %v", tree, err)
	}
	if _, err := Eject(p, BashRel); !errors.Is(err, ErrExists) {
		t.Fatalf("second eject: %v", err)
	}
	if _, o, _ := Resolve(p, "utils/domain.sh"); o != OriginLocal {
		t.Errorf("Resolve inside the bash unit: %s", o)
	}

	// The report: the kept file is local modified, the rest unchanged; the
	// data units report themselves.
	reports, err := Report(p, []string{BashRel})
	if err != nil || len(reports) != 1 {
		t.Fatalf("report: %v %v", reports, err)
	}
	r := reports[0]
	if r.Origin != OriginColLocalModified || r.Path != p.Lok8s || r.Kind != string(KindBash) || r.Marker == nil {
		t.Fatalf("report: %+v", r)
	}
	c := r.Counts()
	if c[StateLocalModified] != 1 || c[StateUnchanged] != len(embedded)-1 || c[StateLocalOnly] != 0 {
		t.Errorf("counts: %v", c)
	}
	for _, f := range r.Files {
		if strings.HasPrefix(f.Path, "addons/") || strings.HasPrefix(f.Path, "drivers/lo/cluster/") {
			t.Errorf("data-unit file in the bash report: %s", f.Path)
		}
	}
	// update --force restores the kept file and rewrites the marker.
	var out strings.Builder
	if _, err := Update(p, BashRel, true, &out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(custom); string(raw) == "# my build\n" {
		t.Fatal("update --force did not restore libs/build")
	}
	if !strings.Contains(out.String(), "bash: updated 1 file(s)") {
		t.Errorf("update output: %q", out.String())
	}
	if after, _ := Report(p, []string{BashRel}); after[0].Drifted || after[0].Origin != OriginColLocal {
		t.Errorf("bash unit after update: %s %s", after[0].Origin, after[0].Summary())
	}
}

// A bash eject that died before `lo` landed does not count as local; the
// next eject completes it, keeping what is there.
func TestEjectBashCompletesAHalfWrittenTree(t *testing.T) {
	withPolicy(t, PolicyEject)
	notices := quiet(t)
	p := project(t)
	if _, err := Eject(p, BashRel); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(p.Lok8s, "lo"))
	if unitExists(p, bashUnit) || LocalExists(p, BashRel) {
		t.Fatal("a tree without lo counts as local")
	}
	if tree, _ := FindBashTree(p), 0; tree.Origin == OriginLocal {
		t.Fatal("FindBashTree honors a tree without lo")
	}
	notices.Reset()
	if _, err := Eject(p, BashRel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "lo")); err != nil {
		t.Fatal("lo not restored")
	}
	if !strings.Contains(notices.String(), "existing file(s) kept") {
		t.Errorf("notice: %q", notices.String())
	}
	if r, _ := Report(p, []string{BashRel}); r[0].Drifted {
		t.Errorf("completed tree drifted: %s", r[0].Summary())
	}
}

func TestBashUnitUnderPolicyNever(t *testing.T) {
	withPolicy(t, PolicyNever)
	p := project(t)
	dir, o, err := Resolve(p, BashRel)
	if err != nil || o != OriginEmbedded || strings.HasPrefix(dir, p.Base) {
		t.Fatalf("%s %s %v", dir, o, err)
	}
	for _, f := range []string{"lo", "libs/build"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("temp bash unit incomplete: %s", f)
		}
	}
	file, o2, _ := Resolve(p, "utils/domain.sh")
	if o2 != OriginEmbedded || file != filepath.Join(dir, "utils", "domain.sh") {
		t.Errorf("file inside the bash unit: %s %s", file, o2)
	}
	if _, err := os.Stat(p.Lok8s); err == nil {
		t.Fatal(".lok8s was created under PolicyNever")
	}
	// Not ejected: the report says builtin, update says nothing to do.
	reports, _ := Report(p, nil)
	found := false
	for _, r := range reports {
		if r.Rel == BashRel {
			found = true
			if r.Origin != OriginColBuiltin || r.Path != p.Lok8s {
				t.Errorf("bash report: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("bash unit missing from Report")
	}
	if len(MissingDataUnits(p)) != len(Units())-1 {
		t.Errorf("MissingDataUnits = %d, want every data unit", len(MissingDataUnits(p)))
	}
}
