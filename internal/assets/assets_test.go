package assets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io/fs"
	"path"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// project is a fresh, empty lok8s project (no .lok8s at all).
func project(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	return &config.Paths{Base: base, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters"), Bin: filepath.Join(base, ".bin")}
}

func withPolicy(t *testing.T, p Policy) {
	t.Helper()
	mu.Lock()
	prev, prevSet := policy, policySet
	mu.Unlock()
	SetPolicy(p)
	t.Cleanup(func() {
		mu.Lock()
		policy, policySet = prev, prevSet
		mu.Unlock()
		Cleanup()
	})
}

func quiet(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := Stderr
	Stderr = &buf
	t.Cleanup(func() { Stderr = prev })
	return &buf
}

func TestResolveEjectsOnFirstUseWithMarker(t *testing.T) {
	withPolicy(t, PolicyEject)
	notices := quiet(t)
	t.Setenv("SOURCE_DATE_EPOCH", "0")
	p := project(t)

	path, origin, err := Resolve(p, "addons/cilium")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(p.Lok8s, "addons", "cilium")
	if path != want || origin != OriginEjected {
		t.Fatalf("path=%s origin=%s", path, origin)
	}
	if _, err := os.Stat(filepath.Join(want, "chart.yaml")); err != nil {
		t.Fatalf("chart.yaml not ejected: %v", err)
	}
	// Byte-identical to the embed.
	got, _ := os.ReadFile(filepath.Join(want, "chart.yaml"))
	emb, _ := readEmbedded("addons/cilium/chart.yaml")
	if !bytes.Equal(got, emb) {
		t.Fatal("ejected bytes differ from the embed")
	}
	// The marker: version, deterministic timestamp, one hash per file.
	m, err := ReadMarker(filepath.Join(want, MarkerFile))
	if err != nil || m == nil {
		t.Fatalf("marker: %v %v", m, err)
	}
	if m.Lo != Version() || m.EjectedAt != "1970-01-01T00:00:00Z" {
		t.Errorf("marker header: %+v", m)
	}
	embedded, _ := EmbeddedFiles(Unit{Rel: "addons/cilium", Kind: "addon"})
	if len(m.Files) != len(embedded) {
		t.Errorf("marker lists %d files, embed has %d", len(m.Files), len(embedded))
	}
	for f, h := range embedded {
		if m.Files[f] != h {
			t.Errorf("%s: marker hash %s, embed %s", f, m.Files[f], h)
		}
	}
	if !strings.Contains(notices.String(), "[assets] ejected addons/cilium -> .lok8s/addons/cilium") {
		t.Errorf("notice: %q", notices.String())
	}
	// No stray temp dirs left beside the unit.
	entries, _ := os.ReadDir(filepath.Join(p.Lok8s, "addons"))
	if len(entries) != 1 {
		t.Errorf("addons dir holds %d entries", len(entries))
	}

	// Second call: the local copy wins, nothing more is written or logged.
	notices.Reset()
	path2, origin2, _ := Resolve(p, "addons/cilium/values.yaml")
	if origin2 != OriginLocal || path2 != filepath.Join(want, "values.yaml") {
		t.Errorf("second resolve: %s %s", path2, origin2)
	}
	if notices.Len() != 0 {
		t.Errorf("second resolve logged: %q", notices.String())
	}
}

func TestPrecedenceLocalWinsAndIsNeverOverwritten(t *testing.T) {
	withPolicy(t, PolicyEject)
	quiet(t)
	p := project(t)
	dir := filepath.Join(p.Lok8s, "addons", "cilium")
	os.MkdirAll(dir, 0o755)
	// A local copy that differs from the embed, and lacks files the embed has.
	os.WriteFile(filepath.Join(dir, "chart.yaml"), []byte("kind: ChartRenderer\nversion: 0.0.1-local\n"), 0o644)

	path, origin, err := Resolve(p, "addons/cilium")
	if err != nil || origin != OriginLocal || path != dir {
		t.Fatalf("path=%s origin=%s err=%v", path, origin, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	if !strings.Contains(string(got), "0.0.1-local") {
		t.Fatal("local chart.yaml was overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "values.yaml")); err == nil {
		t.Fatal("values.yaml was written next to a local copy (partial eject)")
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err == nil {
		t.Fatal("a marker was written into a pre-existing local copy")
	}
	// Explicit eject onto a local copy refuses.
	if _, err := Eject(p, "addons/cilium"); !errors.Is(err, ErrExists) {
		t.Fatalf("Eject over local: %v", err)
	}
}

func TestPolicyNeverServesFromTempDir(t *testing.T) {
	withPolicy(t, PolicyNever)
	quiet(t)
	p := project(t)
	path, origin, err := Resolve(p, "drivers/lo/cluster/registry")
	if err != nil || origin != OriginEmbedded {
		t.Fatalf("path=%s origin=%s err=%v", path, origin, err)
	}
	if strings.HasPrefix(path, p.Base) {
		t.Fatalf("PolicyNever wrote into the project: %s", path)
	}
	if _, err := os.Stat(filepath.Join(path, "mirror.yaml")); err != nil {
		t.Fatalf("temp copy incomplete: %v", err)
	}
	if _, err := os.Stat(p.Lok8s); err == nil {
		t.Fatal(".lok8s was created under PolicyNever")
	}
	// Peek behaves the same under either policy.
	SetPolicy(PolicyEject)
	pp, po, _ := Peek(p, "chat/defaults.json")
	if po != OriginEmbedded || strings.HasPrefix(pp, p.Base) {
		t.Fatalf("Peek: %s %s", pp, po)
	}
	if _, err := os.Stat(p.Lok8s); err == nil {
		t.Fatal("Peek wrote into the project")
	}
	Cleanup()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Cleanup left the temp dir")
	}
}

func TestConfigureReadsEnv(t *testing.T) {
	withPolicy(t, PolicyEject)
	t.Setenv(EnvEject, "never")
	Configure(false)
	if CurrentPolicy() != PolicyNever {
		t.Fatal("LO_ASSETS_EJECT=never not honored")
	}
	t.Setenv(EnvEject, "")
	Configure(true)
	if CurrentPolicy() != PolicyNever {
		t.Fatal("--no-eject not honored")
	}
	Configure(false)
	if CurrentPolicy() != PolicyEject {
		t.Fatal("default policy is not eject")
	}
}

func TestResolveUnknownAndInvalid(t *testing.T) {
	withPolicy(t, PolicyEject)
	p := project(t)
	path, origin, err := Resolve(p, "addons/nope")
	if err != nil || origin != OriginNone || path != filepath.Join(p.Lok8s, "addons", "nope") {
		t.Errorf("unknown: %s %s %v", path, origin, err)
	}
	for _, bad := range []string{"", "../x", "addons/../../etc", "/abs"} {
		if _, _, err := Resolve(p, bad); !errors.Is(err, ErrInvalidRel) {
			t.Errorf("%q: err=%v", bad, err)
		}
	}
	if _, err := Eject(p, "libs/nothing"); !errors.Is(err, ErrNotAsset) {
		t.Errorf("Eject unknown: %v", err)
	}
}

// Two processes ejecting the same unit: the loser's rename lands on a
// populated directory (EEXIST/ENOTEMPTY) and must be a no-op success, not
// an error — the unit is there, precedence holds.
func TestEjectRenameRaceLoserIsANoop(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dest := filepath.Join(parent, "cilium")
	os.MkdirAll(dest, 0o755)
	os.WriteFile(filepath.Join(dest, "chart.yaml"), []byte("winner\n"), 0o644)
	tmp := filepath.Join(parent, ".cilium.lo-eject-loser")
	os.MkdirAll(tmp, 0o755)
	os.WriteFile(filepath.Join(tmp, "chart.yaml"), []byte("loser\n"), 0o644)

	won, err := renameInto(tmp, dest)
	if err != nil || won {
		t.Fatalf("raced rename: won=%v err=%v", won, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "chart.yaml")); string(got) != "winner\n" {
		t.Fatalf("the loser overwrote the winner: %q", got)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("the loser's stage was not removed")
	}
	// A rename that fails for another reason is still an error.
	if _, err := renameInto(filepath.Join(parent, "never-staged"), filepath.Join(parent, "x")); err == nil {
		t.Fatal("a missing stage must fail")
	}
	// The winner's path: an absent dest is claimed.
	stage := filepath.Join(parent, ".fresh.lo-eject-")
	os.MkdirAll(stage, 0o755)
	if won, err := renameInto(stage, filepath.Join(parent, "fresh")); err != nil || !won {
		t.Fatalf("fresh rename: won=%v err=%v", won, err)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := &Marker{Lo: "1.2.3", EjectedAt: "2026-09-03T00:00:00Z", Files: map[string]string{
		"chart.yaml":         "sha256:aa",
		"weird: name.yaml":   "sha256:bb",
		"sub/values.lo.yaml": "sha256:cc",
	}}
	file := filepath.Join(dir, MarkerFile)
	if err := m.write(file); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMarker(file)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lo != m.Lo || got.EjectedAt != m.EjectedAt || len(got.Files) != 3 {
		t.Fatalf("round trip: %+v", got)
	}
	for k, v := range m.Files {
		if got.Files[k] != v {
			t.Errorf("%q: %q", k, got.Files[k])
		}
	}
	if none, err := ReadMarker(filepath.Join(dir, "absent")); none != nil || err != nil {
		t.Errorf("absent marker: %v %v", none, err)
	}
}

// The mirror gate pins the embedded mirror (internal/assets/lok8s/**) to
// the frozen bash tree (.lok8s/**) byte for byte, in BOTH directions. It is
// the same gate internal/kubehz/manifests_test.go and the scaffold template
// test apply to their embeds. The embedded copy is canonical; a file edited
// on either side without hack/sync-legacy-assets.sh fails here.

// mirrored lists the .lok8s subtrees the mirror carries (the sync script's
// list — keep the two in step).
var mirrored = []string{
	"addons",
	"drivers/lo/cluster",
	"drivers/kubeone/cluster",
	"drivers/capi/cluster",
	"libs/inventory/manifests",
	"chat",
	"VERSION",
}

func TestEmbeddedMirrorMatchesLegacyTree(t *testing.T) {
	t.Parallel()
	legacy := filepath.Join(testutil.RepoRoot(t), ".lok8s")
	if _, err := os.Stat(filepath.Join(legacy, "lo")); err != nil {
		t.Skipf("frozen tree not present: %v", err)
	}
	embedded := testutil.ReadFS(t, "internal/assets/lok8s", FS(), nil)
	onDisk := testutil.Tree{Name: ".lok8s", Files: map[string]string{}}
	for _, sub := range mirrored {
		root := filepath.Join(legacy, filepath.FromSlash(sub))
		info, err := os.Stat(root)
		if err != nil {
			t.Fatalf("%s: missing from .lok8s: %v", sub, err)
		}
		if !info.IsDir() {
			data, _ := os.ReadFile(root)
			onDisk.Files[sub] = string(data)
			continue
		}
		// An ejected marker never belongs to the frozen tree; ignore one
		// left behind by a local experiment rather than fail on it.
		tree := testutil.ReadDir(t, ".lok8s", root, func(rel string) bool { return path.Base(rel) == MarkerFile })
		for rel, data := range tree.Files {
			onDisk.Files[sub+"/"+rel] = data
		}
	}
	testutil.Drift{
		Want:     embedded,
		Got:      onDisk,
		Sync:     "hack/sync-legacy-assets.sh",
		SyncBack: "hack/sync-legacy-assets.sh --from-legacy",
	}.Check(t)
	if len(embedded.Files) < 100 {
		t.Fatalf("embedded mirror suspiciously small: %d files", len(embedded.Files))
	}
	for _, must := range []string{"addons/cilium/chart.yaml", "drivers/lo/cluster/registry/mirror.yaml", "drivers/kubeone/cluster/core/kubeone.yaml", "drivers/capi/cluster/core/cluster.yaml", "libs/inventory/manifests/clusterinventory.crd.yaml", "chat/defaults.json", "VERSION"} {
		if _, ok := embedded.Files[must]; !ok {
			t.Errorf("%s missing from the embed", must)
		}
	}
}

// `mirrored` above and SUBTREES in hack/sync-legacy-assets.sh are the same
// list kept in two places (Go cannot import a bash array); this pins them
// to each other so a subtree added on one side fails here.
func TestMirroredListMatchesSyncScript(t *testing.T) {
	t.Parallel()
	script := filepath.Join(testutil.RepoRoot(t), "hack", "sync-legacy-assets.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Skipf("sync script not present: %v", err)
	}
	var inScript []string
	inBlock := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "SUBTREES=("):
			inBlock = true
		case inBlock && trimmed == ")":
			inBlock = false
		case inBlock && trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			inScript = append(inScript, trimmed)
		}
	}
	if strings.Join(inScript, "\n") != strings.Join(mirrored, "\n") {
		t.Fatalf("hack/sync-legacy-assets.sh SUBTREES and assets_test.go `mirrored` differ:\nscript: %v\ngo:     %v", inScript, mirrored)
	}
}

func TestUnitsCoverEveryEmbeddedFile(t *testing.T) {
	t.Parallel()
	err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "VERSION" {
			return err
		}
		if _, ok := UnitFor(p); !ok {
			t.Errorf("%s: embedded but no unit covers it", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(AddonNames()); n < 20 {
		t.Fatalf("only %d embedded addons", n)
	}
}
