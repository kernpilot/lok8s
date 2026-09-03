package assets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
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

// TestClassificationMatrix drives every one of the six classes through a
// real ejected unit.
func TestClassificationMatrix(t *testing.T) {
	withPolicy(t, PolicyEject)
	quiet(t)
	p := project(t)
	if _, _, err := Resolve(p, "addons/cilium"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.Lok8s, "addons", "cilium")
	embedded, _ := EmbeddedFiles(Unit{Rel: "addons/cilium", Kind: "addon"})
	files := sortedKeys(embedded)
	if len(files) < 4 {
		t.Fatalf("cilium ships only %d files; the matrix needs 4", len(files))
	}
	// local modified: edit one file.
	os.WriteFile(filepath.Join(dir, files[0]), []byte("edited locally\n"), 0o644)
	// lo updated: pretend lo ships a new copy of files[1] by rewriting the
	// marker's hash for it to what is on disk AND changing the local file to
	// match the marker — i.e. origin == local != embedded.
	m, _ := ReadMarker(filepath.Join(dir, MarkerFile))
	os.WriteFile(filepath.Join(dir, files[1]), []byte("older shipped copy\n"), 0o644)
	m.Files[files[1]] = hashBytes([]byte("older shipped copy\n"))
	// both: origin, local and embedded all differ.
	os.WriteFile(filepath.Join(dir, files[2]), []byte("local edit of an old copy\n"), 0o644)
	m.Files[files[2]] = hashBytes([]byte("some other old copy\n"))
	// builtin-only: delete a shipped file. local-only: add one.
	os.Remove(filepath.Join(dir, files[3]))
	os.WriteFile(filepath.Join(dir, "my-extra.yaml"), []byte("x: 1\n"), 0o644)
	if err := m.write(filepath.Join(dir, MarkerFile)); err != nil {
		t.Fatal(err)
	}

	reports, err := Report(p, []string{"addons/cilium"})
	if err != nil {
		t.Fatal(err)
	}
	r := reports[0]
	got := map[string]FileState{}
	for _, f := range r.Files {
		got[f.Path] = f.State
	}
	want := map[string]FileState{
		files[0]:        StateLocalModified,
		files[1]:        StateLoUpdated,
		files[2]:        StateBoth,
		files[3]:        StateBuiltinOnly,
		"my-extra.yaml": StateLocalOnly,
	}
	for f, s := range want {
		if got[f] != s {
			t.Errorf("%s: %s, want %s", f, got[f], s)
		}
	}
	unchanged := 0
	for _, f := range r.Files {
		if f.State == StateUnchanged {
			unchanged++
		}
	}
	if unchanged != len(files)-4 {
		t.Errorf("unchanged = %d, want %d", unchanged, len(files)-4)
	}
	if !r.Drifted || r.Origin != OriginColLocalModified {
		t.Errorf("unit verdict: drifted=%v origin=%s", r.Drifted, r.Origin)
	}
	if !strings.Contains(r.Summary(), "1 lo updated") || !strings.Contains(r.Summary(), "1 local modified") || !strings.Contains(r.Summary(), "1 both") {
		t.Errorf("summary: %s", r.Summary())
	}
	if r.Marker == nil || r.Version.Embedded == "-" {
		t.Errorf("report header: marker=%v version=%+v", r.Marker, r.Version)
	}

	// Update refuses on the conflicts, writes nothing.
	var out bytes.Buffer
	before, _ := os.ReadFile(filepath.Join(dir, files[0]))
	if _, err := Update(p, "addons/cilium", false, &out); !errors.Is(err, ErrConflict) {
		t.Fatalf("update on conflict: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, files[0]))
	if !bytes.Equal(before, after) {
		t.Fatal("update wrote despite the conflict")
	}
	if !strings.Contains(out.String(), string(StateBoth)) {
		t.Errorf("update did not show the diff first: %s", out.String())
	}
	// --force applies the embed, keeps the local-only file, rewrites the marker.
	out.Reset()
	if _, err := Update(p, "addons/cilium", true, &out); err != nil {
		t.Fatal(err)
	}
	reports, _ = Report(p, []string{"addons/cilium"})
	if reports[0].Drifted {
		t.Errorf("after --force: %s", reports[0].Summary())
	}
	if _, err := os.Stat(filepath.Join(dir, "my-extra.yaml")); err != nil {
		t.Error("--force removed the local-only file")
	}
	if _, err := os.Stat(filepath.Join(dir, files[3])); err != nil {
		t.Error("--force did not restore the builtin-only file")
	}
}

func TestUpdateAppliesCleanLoUpdate(t *testing.T) {
	withPolicy(t, PolicyEject)
	quiet(t)
	p := project(t)
	Resolve(p, "addons/metallb")
	dir := filepath.Join(p.Lok8s, "addons", "metallb")
	// Simulate "lo shipped a new chart.yaml": local == origin != embedded.
	m, _ := ReadMarker(filepath.Join(dir, MarkerFile))
	old := []byte("kind: ChartRenderer\nversion: 0.0.0-old\n")
	os.WriteFile(filepath.Join(dir, "chart.yaml"), old, 0o644)
	m.Files["chart.yaml"] = hashBytes(old)
	m.write(filepath.Join(dir, MarkerFile))

	reports, _ := Report(p, []string{"addons/metallb"})
	if reports[0].Version.Local != "0.0.0-old" || reports[0].Version.Embedded == "0.0.0-old" {
		t.Fatalf("headline: %+v", reports[0].Version)
	}
	var out bytes.Buffer
	if _, err := Update(p, "addons/metallb", false, &out); err != nil {
		t.Fatalf("clean update refused: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	emb, _ := readEmbedded("addons/metallb/chart.yaml")
	if !bytes.Equal(got, emb) {
		t.Fatal("update did not apply the embedded copy")
	}
	m2, _ := ReadMarker(filepath.Join(dir, MarkerFile))
	if m2.Files["chart.yaml"] != hashBytes(emb) {
		t.Fatal("marker not rewritten")
	}
}

func TestUpdateRefusesWithoutMarker(t *testing.T) {
	withPolicy(t, PolicyEject)
	p := project(t)
	dir := filepath.Join(p.Lok8s, "addons", "metallb")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "chart.yaml"), []byte("vendored\n"), 0o644)
	var out bytes.Buffer
	if _, err := Update(p, "addons/metallb", false, &out); !errors.Is(err, ErrConflict) {
		t.Fatalf("update without marker: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	if string(got) != "vendored\n" {
		t.Fatal("update overwrote a vendored copy without --force")
	}
}

func TestReportOriginsAndDoctorLine(t *testing.T) {
	withPolicy(t, PolicyEject)
	quiet(t)
	p := project(t)
	line, warn := DoctorLine(p)
	if warn || !strings.Contains(line, "none ejected") {
		t.Errorf("empty project: %q %v", line, warn)
	}
	Resolve(p, "addons/cilium")
	Resolve(p, "drivers/kubeone/cluster")
	os.MkdirAll(filepath.Join(p.Lok8s, "addons", "mine"), 0o755)
	os.WriteFile(filepath.Join(p.Lok8s, "addons", "mine", "kustomization.yaml"), []byte("resources: []\n"), 0o644)

	reports, err := Report(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	origins := map[string]string{}
	for _, r := range reports {
		origins[r.Rel] = r.Origin
	}
	for rel, want := range map[string]string{
		"addons/cilium":           OriginColLocal,
		"addons/metallb":          OriginColBuiltin,
		"drivers/kubeone/cluster": OriginColLocal,
		"drivers/capi/cluster":    OriginColBuiltin,
		"addons/mine":             OriginColLocalOnly,
	} {
		if origins[rel] != want {
			t.Errorf("%s: origin %q, want %q", rel, origins[rel], want)
		}
	}
	if reports[len(reports)-1].Rel != "addons/mine" {
		t.Errorf("local-only unit not listed last: %s", reports[len(reports)-1].Rel)
	}
	line, warn = DoctorLine(p)
	if warn || line != "assets: 2 local, all in sync with the binary" {
		t.Errorf("in sync: %q %v", line, warn)
	}
	os.WriteFile(filepath.Join(p.Lok8s, "addons", "cilium", "chart.yaml"), []byte("edited\n"), 0o644)
	line, warn = DoctorLine(p)
	if !warn || line != "assets: 1 of 2 local assets drifted (lo assets diff)" {
		t.Errorf("drift: %q %v", line, warn)
	}
	reports, _ = Report(p, nil)
	if !AnyDrift(reports) {
		t.Error("AnyDrift missed the edit")
	}

	var table bytes.Buffer
	WriteTable(&table, reports, false)
	if !strings.Contains(table.String(), "addons/cilium                   addon       local (modified)") {
		t.Errorf("table:\n%s", table.String())
	}
}

func TestMarkerRoundTrip(t *testing.T) {
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

func TestVersionFallsBackToEmbedded(t *testing.T) {
	prev := BuildVersion
	t.Cleanup(func() { BuildVersion = prev })
	BuildVersion = ""
	emb, _ := readEmbedded("VERSION")
	if Version() != strings.TrimSpace(string(emb)) {
		t.Errorf("Version() = %q, embedded VERSION = %q", Version(), emb)
	}
	BuildVersion = "9.9.9"
	if Version() != "9.9.9" {
		t.Errorf("stamped version ignored: %s", Version())
	}
}
