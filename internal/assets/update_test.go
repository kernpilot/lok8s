package assets

// update_test.go covers `lo assets update`: the clean update, the in-sync
// vendored copy and the marker refusal.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// A vendored copy with no marker that is byte-identical to the embedded
// unit is already up to date: no --force demanded, nothing written.
func TestUpdateIdenticalVendoredCopyIsInSync(t *testing.T) {
	withPolicy(t, PolicyEject)
	p := project(t)
	dir := filepath.Join(p.Lok8s, "addons", "metallb")
	if err := writeUnit(Unit{Rel: "addons/metallb", Kind: "addon"}, dir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r, err := Update(p, "addons/metallb", false, &out)
	if err != nil {
		t.Fatalf("identical vendored copy refused: %v", err)
	}
	if r.Marker != nil || r.Drifted {
		t.Fatalf("report: marker=%v drifted=%v", r.Marker, r.Drifted)
	}
	if !strings.Contains(out.String(), "addons/metallb: already in sync") {
		t.Fatalf("output: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err == nil {
		t.Fatal("an in-sync report must not write a marker")
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
