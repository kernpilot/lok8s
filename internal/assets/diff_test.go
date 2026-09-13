package assets

// diff_test.go covers the three-way classification and the report line.

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
	files := slices.Sorted(maps.Keys(embedded))
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
