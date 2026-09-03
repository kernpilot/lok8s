package assets

// drift_test.go pins the embedded mirror (internal/assets/lok8s/**) to the
// frozen bash tree (.lok8s/**) byte for byte, in BOTH directions — the
// same gate internal/kubehz/manifests_test.go and the scaffold template
// test apply to their embeds. The embedded copy is canonical; a file edited
// on either side without hack/sync-legacy-assets.sh fails here.

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"testing"
)

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

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEmbeddedMirrorMatchesLegacyTree(t *testing.T) {
	legacy := filepath.Join(repoRoot(t), ".lok8s")
	if _, err := os.Stat(filepath.Join(legacy, "lo")); err != nil {
		t.Skipf("frozen tree not present: %v", err)
	}
	embedded := map[string][]byte{}
	if err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(FS(), p)
		if err != nil {
			return err
		}
		embedded[p] = data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	onDisk := map[string][]byte{}
	for _, sub := range mirrored {
		root := filepath.Join(legacy, filepath.FromSlash(sub))
		info, err := os.Stat(root)
		if err != nil {
			t.Fatalf("%s: missing from .lok8s: %v", sub, err)
		}
		if !info.IsDir() {
			data, _ := os.ReadFile(root)
			onDisk[sub] = data
			continue
		}
		if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			// An ejected marker never belongs to the frozen tree; ignore one
			// left behind by a local experiment rather than fail on it.
			if path.Base(filepath.ToSlash(p)) == MarkerFile {
				return nil
			}
			rel, _ := filepath.Rel(legacy, p)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			onDisk[filepath.ToSlash(rel)] = data
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]bool{}
	for k := range embedded {
		keys[k] = true
	}
	for k := range onDisk {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		e, inE := embedded[k]
		d, inD := onDisk[k]
		switch {
		case !inE:
			t.Errorf("%s: in .lok8s but not in internal/assets/lok8s (run: hack/sync-legacy-assets.sh --from-legacy)", k)
		case !inD:
			t.Errorf("%s: embedded but missing from .lok8s (run: hack/sync-legacy-assets.sh)", k)
		case string(e) != string(d):
			t.Errorf("%s: embedded bytes differ from .lok8s (run: hack/sync-legacy-assets.sh)", k)
		}
	}
	if len(embedded) < 100 {
		t.Fatalf("embedded mirror suspiciously small: %d files", len(embedded))
	}
	for _, must := range []string{"addons/cilium/chart.yaml", "drivers/lo/cluster/registry/mirror.yaml", "drivers/kubeone/cluster/core/kubeone.yaml", "drivers/capi/cluster/core/cluster.yaml", "libs/inventory/manifests/clusterinventory.crd.yaml", "chat/defaults.json", "VERSION"} {
		if _, ok := embedded[must]; !ok {
			t.Errorf("%s missing from the embed", must)
		}
	}
}

func TestUnitsCoverEveryEmbeddedFile(t *testing.T) {
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
