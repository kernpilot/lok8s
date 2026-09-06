package cli

// cmd_assets_test.go — the Go-only eject-model surface: `lo assets
// {list,show,eject,diff,update}`, the --no-eject flag, `lo init project`,
// the origin columns of `lo addons`/`lo drivers --list` and the doctor
// line. No twin in the frozen tree, so the tests here ARE the gate (see
// internal/assets for the resolver's own tests).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
)

func quietAssets(t *testing.T) {
	t.Helper()
	prev := assets.Stderr
	assets.Stderr = io.Discard
	t.Cleanup(func() {
		assets.Stderr = prev
		assets.SetPolicy(assets.PolicyEject)
		assets.Cleanup()
	})
}

func TestAssetsEjectDiffUpdateRoundTrip(t *testing.T) {
	quietAssets(t)
	t.Setenv("SOURCE_DATE_EPOCH", "0")
	p := synthProject(t)
	writeFile(t, filepath.Join(p.Clusters, "a.dev", "cluster.lok8s.yaml"), "kind: Lo\nspec:\n  bootstrap:\n    - cilium\n    - metallb\n    - ./targets/glue\n")

	// --check on a fresh project: exit 1, nothing written.
	stdout, stderr, err := runLo(t, NewRoot(p), "assets", "eject", "--check")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stdout, "would eject addons/cilium\n") || !strings.Contains(stdout, "would eject drivers/lo/cluster\n") || !strings.Contains(stderr, "4 asset(s) would be ejected") {
		t.Fatalf("eject --check: err=%v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "addons")); err == nil {
		t.Fatal("--check wrote into the project")
	}

	// eject (referenced set): cilium, metallb, the lo templates, the CRD.
	stdout, _, err = runLo(t, NewRoot(p), "assets", "eject")
	if err != nil || !strings.Contains(stdout, "ejected 4 asset(s) into .lok8s") {
		t.Fatalf("eject: err=%v stdout=%s", err, stdout)
	}
	for _, rel := range []string{"addons/cilium", "addons/metallb", "drivers/lo/cluster", "libs/inventory/manifests"} {
		if _, err := os.Stat(filepath.Join(p.Lok8s, filepath.FromSlash(rel), assets.MarkerFile)); err != nil {
			t.Errorf("%s: not ejected with a marker", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "addons", "ccm")); err == nil {
		t.Error("an unreferenced addon was ejected")
	}
	// Idempotent + --check now clean (rc 0).
	if stdout, _, err := runLo(t, NewRoot(p), "assets", "eject", "--check"); err != nil || !strings.Contains(stdout, "nothing to eject") {
		t.Errorf("eject --check after eject: err=%v stdout=%s", err, stdout)
	}

	// diff: clean.
	stdout, _, err = runLo(t, NewRoot(p), "assets", "diff", "--check")
	if err != nil || !strings.Contains(stdout, "addons/cilium                   addon       local ") || !strings.Contains(stdout, "addons/ccm                      addon       builtin ") {
		t.Fatalf("diff clean: err=%v\n%s", err, stdout)
	}

	// Edit a file → local modified; diff --check exits 1; update refuses.
	chart := filepath.Join(p.Lok8s, "addons", "cilium", "chart.yaml")
	writeFile(t, chart, "kind: ChartRenderer\nversion: 0.0.0-mine\n")
	stdout, stderr, err = runLo(t, NewRoot(p), "assets", "diff", "--check")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "1 asset(s) drifted") || !strings.Contains(stdout, "local (modified)    0.0.0-mine") {
		t.Fatalf("diff --check drift: err=%v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	stdout, _, err = runLo(t, NewRoot(p), "assets", "diff", "addons/cilium")
	if err != nil || !strings.Contains(stdout, "chart.yaml                                local modified") {
		t.Fatalf("diff rel: err=%v\n%s", err, stdout)
	}
	stdout, stderr, err = runLo(t, NewRoot(p), "assets", "update", "addons/cilium")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "1 locally modified file(s)") {
		t.Fatalf("update on conflict: err=%v stderr=%s", err, stderr)
	}
	if raw, _ := os.ReadFile(chart); !strings.Contains(string(raw), "0.0.0-mine") {
		t.Fatal("update overwrote a locally modified file without --force")
	}
	if !strings.Contains(stdout, "local modified") {
		t.Errorf("update did not show the diff first:\n%s", stdout)
	}
	// --force applies and the diff is clean again.
	if _, _, err := runLo(t, NewRoot(p), "assets", "update", "addons/cilium", "--force"); err != nil {
		t.Fatalf("update --force: %v", err)
	}
	if _, _, err := runLo(t, NewRoot(p), "assets", "diff", "--check"); err != nil {
		t.Fatal("still drifted after update --force")
	}

	// --json: stable shape.
	stdout, _, err = runLo(t, NewRoot(p), "assets", "diff", "--json", "addons/cilium")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Lo     string `json:"lo"`
		Assets []struct {
			Rel     string `json:"rel"`
			Origin  string `json:"origin"`
			Drifted bool   `json:"drifted"`
			Version struct{ Local, Embedded string }
			Marker  *struct{ Lo, EjectedAt string }
			Files   []struct{ Path, State string }
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if doc.Lo != assets.Version() || len(doc.Assets) != 1 || doc.Assets[0].Rel != "addons/cilium" || doc.Assets[0].Origin != "local" ||
		doc.Assets[0].Marker == nil || doc.Assets[0].Marker.EjectedAt != "1970-01-01T00:00:00Z" || len(doc.Assets[0].Files) == 0 || doc.Assets[0].Files[0].State != "unchanged" {
		t.Errorf("json shape: %+v", doc)
	}

	// show + list + unknown rel.
	stdout, _, err = runLo(t, NewRoot(p), "assets", "show", "addons/cilium")
	if err != nil || !strings.Contains(stdout, "origin:   local\n") || !strings.Contains(stdout, "ejected:  by lo "+assets.Version()+" at 1970-01-01T00:00:00Z\n") {
		t.Errorf("show: err=%v\n%s", err, stdout)
	}
	if _, stderr, err := runLo(t, NewRoot(p), "assets", "show", "addons/nope"); !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "not an embedded asset: addons/nope") {
		t.Errorf("show unknown: err=%v stderr=%s", err, stderr)
	}
	stdout, _, err = runLo(t, NewRoot(p), "assets", "list")
	if err != nil || !strings.HasPrefix(stdout, "ASSET                           KIND        ORIGIN") || !strings.Contains(stdout, "chat                            chat        builtin") {
		t.Errorf("list: err=%v\n%s", err, stdout)
	}

	// --all ejects the rest.
	if _, _, err := runLo(t, NewRoot(p), "assets", "eject", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "chat", assets.MarkerFile)); err != nil {
		t.Error("--all did not eject chat")
	}
}

// The --no-eject flag on a flag-parsing command: the addon is served from
// the binary, the project stays untouched.
func TestNoEjectFlag(t *testing.T) {
	quietAssets(t)
	p := synthProject(t)
	stdout, _, err := runLo(t, NewRoot(p), "--no-eject", "addons", "cilium")
	if err != nil || !strings.Contains(stdout, "name:    cilium\n") {
		t.Fatalf("addons cilium: err=%v\n%s", err, stdout)
	}
	if strings.Contains(stdout, "path:    "+p.Lok8s) {
		t.Errorf("--no-eject served from the project path:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "addons")); err == nil {
		t.Fatal("--no-eject wrote into the project")
	}
	// Without the flag: ejected.
	if _, _, err := runLo(t, NewRoot(p), "addons", "cilium"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Lok8s, "addons", "cilium", assets.MarkerFile)); err != nil {
		t.Fatal("addons <name> did not eject on first use")
	}
}

func TestAddonsAndDriversOriginColumns(t *testing.T) {
	quietAssets(t)
	p := synthProject(t)
	writeFile(t, filepath.Join(p.Lok8s, "addons", "mine", "kustomization.yaml"), "resources: []\n")
	writeFile(t, filepath.Join(p.Clusters, "a.dev", "cluster.lok8s.yaml"), "kind: Lo\nspec:\n  bootstrap:\n    - cilium\n    - mine\n    - ./targets/glue\n")
	if _, _, err := runLo(t, NewRoot(p), "assets", "eject", "addons/metallb"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p.Lok8s, "addons", "metallb", "values.yaml"), "edited: true\n")

	stdout, _, err := runLo(t, NewRoot(p), "addons", "--origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NAME                  TYPE      VERSION       ORIGIN              CHART/REPO\n",
		"mine                  raw       -             local-only          -\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("addons --origin missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "   builtin             cilium (") || !strings.Contains(stdout, "   local (modified)    metallb (") {
		t.Errorf("addons --origin verdicts:\n%s", stdout)
	}
	// Default output: no column (byte-parity with the frozen tree).
	stdout, _, _ = runLo(t, NewRoot(p), "addons")
	if strings.Contains(stdout, "ORIGIN") {
		t.Error("origin column shown without --origin")
	}

	stdout, _, err = runLo(t, NewRoot(p), "addons", "--detail", "--origin", "--domain", "a.dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "NAME                      CATEGORY        TYPE      VERSION     ORIGIN              CONFIGURE\n") ||
		!strings.Contains(stdout, "  builtin             ") || !strings.Contains(stdout, "  target              per-cluster glue in") {
		t.Errorf("addons --detail --origin:\n%s", stdout)
	}

	stdout, _, err = runLo(t, NewRoot(p), "drivers", "--list", "--origin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "- lo           builtin\n") || !strings.Contains(stdout, "- kubehz       builtin\n") {
		t.Errorf("drivers --list --origin:\n%s", stdout)
	}
	if _, _, err := runLo(t, NewRoot(p), "assets", "eject", "drivers/kubeone/cluster"); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ = runLo(t, NewRoot(p), "drivers", "--list", "--origin")
	if !strings.Contains(stdout, "- kubeone      local\n") {
		t.Errorf("drivers --list --origin after eject:\n%s", stdout)
	}
	stdout, _, _ = runLo(t, NewRoot(p), "drivers", "--list")
	if strings.Contains(stdout, "builtin") {
		t.Error("origin shown without --origin")
	}
}

func TestInitProject(t *testing.T) {
	quietAssets(t)
	base := t.TempDir()
	p := synthProject(t)
	dir := filepath.Join(base, "myproj")
	stdout, _, err := runLo(t, NewRoot(p), "init", "project", "--path", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"clusters/.gitkeep", "lok8s.yaml", ".gitignore", ".bin/b.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not scaffolded", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".lok8s")); err == nil {
		t.Error("init project wrote a .lok8s tree")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "lok8s.yaml"))
	if !strings.Contains(string(raw), "kind: Project\nmetadata:\n  name: myproj\n") {
		t.Errorf("lok8s.yaml:\n%s", raw)
	}
	ign, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	for _, want := range []string{".bin/*\n", "!.bin/b.yaml\n", ".kubeconfig/\n", ".secrets/\n"} {
		if !strings.Contains(string(ign), want) {
			t.Errorf(".gitignore missing %q", want)
		}
	}
	if !strings.Contains(stdout, "Scaffolded "+dir+"/lok8s.yaml\n") || !strings.Contains(stdout, "Done. Next:") {
		t.Errorf("stdout:\n%s", stdout)
	}

	// Re-run: existing files kept, .gitignore not duplicated.
	writeFile(t, filepath.Join(dir, "lok8s.yaml"), "keep: me\n")
	stdout, _, err = runLo(t, NewRoot(p), "init", "project", "--path", dir)
	if err != nil || !strings.Contains(stdout, "Kept "+dir+"/lok8s.yaml (exists; --force overwrites)\n") {
		t.Fatalf("re-run: err=%v\n%s", err, stdout)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "lok8s.yaml")); string(raw) != "keep: me\n" {
		t.Error("re-run overwrote lok8s.yaml")
	}
	if ign2, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(ign2) != string(ign) {
		t.Error("re-run changed .gitignore")
	}
	// --force overwrites; a bad name is refused.
	if _, _, err := runLo(t, NewRoot(p), "init", "project", "--path", dir, "-f"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "lok8s.yaml")); string(raw) == "keep: me\n" {
		t.Error("-f did not overwrite")
	}
	if _, stderr, err := runLo(t, NewRoot(p), "init", "project", "../evil", "--path", dir); !errors.Is(err, ErrHandled) || stderr == "" {
		t.Errorf("bad name accepted: err=%v", err)
	}
}

func TestDoctorAssetsLine(t *testing.T) {
	quietAssets(t)
	p := synthProject(t)
	// Fresh project: "none ejected".
	line := doctorAssetsLine(t, p)
	if !strings.Contains(line, "assets: none ejected") {
		t.Errorf("fresh: %q", line)
	}
	if _, _, err := runLo(t, NewRoot(p), "assets", "eject", "addons/cilium"); err != nil {
		t.Fatal(err)
	}
	if line = doctorAssetsLine(t, p); !strings.Contains(line, "✓\033[0m assets: 1 local, all in sync with the binary") {
		t.Errorf("in sync: %q", line)
	}
	writeFile(t, filepath.Join(p.Lok8s, "addons", "cilium", "chart.yaml"), "edited\n")
	if line = doctorAssetsLine(t, p); !strings.Contains(line, "!\033[0m assets: 1 of 1 local assets drifted (lo assets diff)") {
		t.Errorf("drift: %q", line)
	}
	// A complete vendored tree, no markers, in sync: the line is omitted so
	// doctor stays byte-identical to the frozen implementation.
	root := repoRootDir(t)
	p2 := synthProject(t)
	os.RemoveAll(p2.Lok8s)
	if err := os.Symlink(filepath.Join(root, ".lok8s"), p2.Lok8s); err != nil {
		t.Skip(err)
	}
	if line = doctorAssetsLine(t, p2); line != "" {
		t.Errorf("vendored tree printed %q", line)
	}
}

// doctorAssetsLine runs the doctor's assets section alone.
func doctorAssetsLine(t *testing.T, p *config.Paths) string {
	t.Helper()
	var buf bytes.Buffer
	doctorAssets(&buf, p)
	return buf.String()
}
