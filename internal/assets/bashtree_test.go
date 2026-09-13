package assets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
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

	// The project's .lok8s holding lo: served as is, nothing extracted.
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
	tree, err := BashTree(p)
	if err != nil || tree.Source != TreeProject || tree.Dir != p.Lok8s {
		t.Fatalf("project: %+v %v", tree, err)
	}
	// PATH_LOK8S pointing at another tree that holds lo: a checkout.
	checkout := t.TempDir()
	testutil.WriteFile(t, filepath.Join(checkout, "lo"), "#!/usr/bin/env bash\n")
	c := *p
	c.Lok8s = checkout
	if tree, err := BashTree(&c); err != nil || tree.Source != TreeCheckout || tree.Dir != checkout || !tree.Source.Local() {
		t.Fatalf("checkout: %+v %v", tree, err)
	}
	if TreeCache.Local() || TreeTemp.Local() || TreeNone.Local() {
		t.Fatal("a non-local source reports Local")
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
	if err != nil || tree.Source != TreeProject || tree.Dir != p.Lok8s {
		t.Fatalf("project tree: %+v %v", tree, err)
	}
	// Neither: the cache.
	os.Remove(filepath.Join(p.Lok8s, "lo"))
	tree, err = BashTree(&q)
	if err != nil || tree.Source != TreeCache || tree.Dir != cache {
		t.Fatalf("cache: %+v %v", tree, err)
	}
}

func TestBashTreeExtractsOnceAndRedoesAPartialExtract(t *testing.T) {
	cache := withCache(t)
	p := project(t)

	if got := FindBashTree(p); got.Source != TreeNone || got.Dir != cache {
		t.Fatalf("before extract: %+v", got)
	}
	tree, err := BashTree(p)
	if err != nil || tree.Source != TreeCache || tree.Dir != cache {
		t.Fatalf("%+v %v", tree, err)
	}
	// The whole tree, byte-identical, executable bits restored.
	onDisk := testutil.ReadDir(t, "cache", cache, func(rel string) bool { return rel == config.CacheMarker })
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
	m, err := ReadMarker(filepath.Join(cache, config.CacheMarker))
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

	// Idempotent: a second call leaves the extracted tree alone (the
	// manifest keeps its mtime, so nothing was re-extracted).
	before, _ := os.Stat(filepath.Join(cache, config.CacheMarker))
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(filepath.Join(cache, config.CacheMarker)); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a valid cache was re-extracted")
	}
	// An unlisted file makes the copy invalid: the tree is replaced, the
	// stray goes with it (the same for a symlink in place of a file).
	stray := filepath.Join(cache, "stray")
	testutil.WriteFile(t, stray, "x")
	if got := FindBashTree(p); got.Source != TreeNone {
		t.Fatalf("a cache with a stray file counts as valid: %+v", got)
	}
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err == nil {
		t.Fatal("a stray file survived the re-extract")
	}
	os.Remove(filepath.Join(cache, "libs", "doctor"))
	os.Symlink(filepath.Join(cache, "libs", "build"), filepath.Join(cache, "libs", "doctor"))
	if cacheValid(cache) {
		t.Fatal("a symlink in the tree counts as valid")
	}

	// Partial: a file missing → redone.
	os.Remove(filepath.Join(cache, "libs", "build"))
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "libs", "build")); err != nil {
		t.Fatal("missing file not restored")
	}
	if _, err := os.Stat(filepath.Join(cache, "libs", "build")); err != nil || !cacheValid(cache) {
		t.Fatal("re-extract left an invalid tree")
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
	os.WriteFile(filepath.Join(cache, config.CacheMarker), []byte("lo: 0.0.0\nfiles: {}\n"), 0o644)
	if got := FindBashTree(p); got.Source != TreeNone {
		t.Fatalf("stale manifest still valid: %+v", got)
	}
	if tree, err := BashTree(p); err != nil || tree.Source != TreeCache || !cacheValid(cache) {
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
	if err != nil || tree.Source != TreeCache || tree.Dir != want {
		t.Fatalf("HOME cache: %+v %v (want %s)", tree, err, want)
	}

	// A relative XDG_CACHE_HOME is ignored (XDG spec), not joined against
	// the working directory.
	t.Setenv(EnvCacheHome, "rel-cache")
	if got, err := cacheTreeDir(); err != nil || got != want {
		t.Fatalf("relative XDG_CACHE_HOME: %q %v (want %s)", got, err, want)
	}
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("rel-cache"); err == nil {
		t.Fatal("a relative XDG_CACHE_HOME landed a cache in the working directory")
	}
	os.Unsetenv(EnvCacheHome)

	// Neither HOME nor XDG_CACHE_HOME: the temp twin under
	// os.TempDir()/lok8s-<uid>/<version>, verified and reused like the cache.
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	if got := FindBashTree(p); got.Source != TreeNone || got.Dir != "" {
		t.Fatalf("no cache dir: %+v", got)
	}
	tree, err = BashTree(p)
	wantTmp := filepath.Join(tmpRoot, fmt.Sprintf("lok8s-%d", os.Getuid()), Version(), "lok8s")
	if err != nil || tree.Source != TreeTemp || tree.Dir != wantTmp {
		t.Fatalf("temp fallback: %+v %v (want %s)", tree, err, wantTmp)
	}
	for _, f := range []string{"lo", "addons/cilium/chart.yaml", "tilt/Tiltfile", "VERSION"} {
		if _, err := os.Stat(filepath.Join(tree.Dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("temp tree incomplete: %s: %v", f, err)
		}
	}
	if !cacheValid(tree.Dir) {
		t.Fatal("temp tree not verifiable")
	}
	if info, _ := os.Lstat(filepath.Dir(filepath.Dir(tree.Dir))); info.Mode().Perm() != 0o700 {
		t.Fatalf("temp base dir mode %o, want 0700", info.Mode().Perm())
	}
	// Reused: a second call keeps it (no per-run leak), and Cleanup does
	// not touch it.
	before, _ := os.Stat(filepath.Join(tree.Dir, config.CacheMarker))
	if again, err := BashTree(p); err != nil || again.Dir != tree.Dir {
		t.Fatalf("second temp resolve: %+v %v", again, err)
	}
	if after, _ := os.Stat(filepath.Join(tree.Dir, config.CacheMarker)); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the temp tree was re-extracted")
	}
	Cleanup()
	if _, err := os.Stat(tree.Dir); err != nil {
		t.Fatal("Cleanup removed the reusable temp tree")
	}
	// A base dir that is not ours (wrong mode) is refused.
	os.Chmod(filepath.Dir(filepath.Dir(tree.Dir)), 0o755)
	if _, err := tempCacheDir(); err == nil {
		t.Fatal("a world-readable temp base dir was accepted")
	}
}

// A cache that cannot be written (read-only HOME) falls back to the temp
// twin instead of failing.
func TestBashTreeFallsBackToTempOnUnwritableCache(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	notices := quiet(t)
	t.Setenv("DEBUG", "1")
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	ro := t.TempDir()
	os.Chmod(ro, 0o500)
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	t.Setenv(EnvCacheHome, ro)
	p := project(t)
	tree, err := BashTree(p)
	if err != nil || tree.Source != TreeTemp || !strings.HasPrefix(tree.Dir, tmpRoot) {
		t.Fatalf("read-only cache root: %+v %v", tree, err)
	}
	if !strings.Contains(notices.String(), "bash tree cache unavailable") {
		t.Errorf("no debug line: %q", notices.String())
	}
	if _, err := os.Stat(filepath.Join(ro, "lok8s")); err == nil {
		t.Fatal("something was written below the read-only root")
	}
}

// A cache tree is never local: PATH_LOK8S naming it (what the shim exports
// to its bash children) resolves to TreeCache, nothing in it counts as an
// ejected unit, and nothing is written into it.
func TestCacheTreeIsNeverLocal(t *testing.T) {
	withPolicy(t, PolicyEject)
	quiet(t)
	cache := withCache(t)
	p := project(t)
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	q := *p
	q.Lok8s = cache
	tree, err := BashTree(&q)
	if err != nil || tree.Source != TreeCache || tree.Dir != cache || tree.Source.Local() {
		t.Fatalf("PATH_LOK8S=cache: %+v %v", tree, err)
	}
	if got := FindBashTree(&q); got != tree {
		t.Fatalf("FindBashTree: %+v", got)
	}
	reports, err := Report(&q, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reports {
		if r.Origin != OriginColBuiltin {
			t.Errorf("%s reported %s from the cache", r.Rel, r.Origin)
		}
	}
	if _, err := Eject(&q, BashRel); !errors.Is(err, ErrCacheTree) {
		t.Fatalf("Eject into the cache: %v", err)
	}
	if _, err := Eject(&q, "addons/cilium"); !errors.Is(err, ErrCacheTree) {
		t.Fatalf("Eject a data unit into the cache: %v", err)
	}
	before, _ := os.Stat(filepath.Join(cache, config.CacheMarker))
	if dir, o, err := Resolve(&q, "addons/cilium"); err != nil || o != OriginEmbedded || strings.HasPrefix(dir, cache) {
		t.Fatalf("Resolve with PATH_LOK8S=cache: %s %s %v", dir, o, err)
	}
	var out strings.Builder
	if _, err := Update(&q, "addons/cilium", true, &out); err != nil || !strings.Contains(out.String(), "not ejected") {
		t.Fatalf("Update --force with PATH_LOK8S=cache: %v %q", err, out.String())
	}
	if !cacheValid(cache) {
		t.Fatal("the cache was modified")
	}
	if after, _ := os.Stat(filepath.Join(cache, config.CacheMarker)); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the cache was re-extracted by a read")
	}
}

// After a successful extract the sweep removes abandoned stage dirs (older
// than an hour; a fresh one may belong to a running extract) and other
// versions' trees.
func TestExtractSweepsStaleStagesAndOldVersions(t *testing.T) {
	cache := withCache(t)
	p := project(t)
	versionDir := filepath.Dir(cache)
	root := filepath.Dir(versionDir)
	old := filepath.Join(versionDir, ".lok8s.lo-extract-old")
	fresh := filepath.Join(versionDir, ".lok8s.lo-extract-fresh")
	older := filepath.Join(root, "0.0.1", "lok8s")
	for _, d := range []string{old, fresh, older} {
		testutil.WriteFile(t, filepath.Join(d, "f"), "x")
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("stale stage dir not swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a fresh stage dir was swept")
	}
	if _, err := os.Stat(older); err == nil {
		t.Error("an older version's tree not swept")
	}
	if !cacheValid(cache) {
		t.Fatal("current tree not valid after the sweep")
	}
	// A valid cache is not re-extracted, so the sweep does not run again.
	os.RemoveAll(fresh)
	testutil.WriteFile(t, filepath.Join(older, "f"), "x")
	if _, err := BashTree(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(older); err != nil {
		t.Error("a valid cache triggered a sweep")
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
	if tree, err := BashTree(p); err != nil || tree.Source != TreeProject || tree.Dir != p.Lok8s {
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
	if tree, _ := FindBashTree(p), 0; tree.Source.Local() {
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
