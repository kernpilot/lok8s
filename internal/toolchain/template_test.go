package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateGroupsAndMarker(t *testing.T) {
	tpl := Template(TemplateOptions{Name: "demo", LoVersion: "v0.3.0", Variant: "core"})
	if !strings.Contains(tpl, Marker+"\n") {
		t.Fatal("marker line missing")
	}
	if !strings.HasPrefix(tpl, "# demo — ") {
		t.Fatalf("header: %q", strings.SplitN(tpl, "\n", 2)[0])
	}
	// Default groups: core + local active, cloud commented out.
	for _, active := range []string{"  kubectl:\n    groups: [core]\n", "  kind:\n    groups: [local]\n", "  tilt:\n    groups: [local]\n", "  mkcert:\n    groups: [local]\n"} {
		if !strings.Contains(tpl, active) {
			t.Errorf("missing active entry %q", active)
		}
	}
	for _, off := range []string{"  # github.com/kubermatic/kubeone:\n  #   groups: [cloud]\n", "  # hcloud:\n  #   groups: [cloud]\n"} {
		if !strings.Contains(tpl, off) {
			t.Errorf("cloud entry not commented out: %q", off)
		}
	}
	// The contributor-only tools are absent as entries (mentioned in the
	// header only as an opt-in recipe).
	for _, absent := range []string{"\n  yq:", "\n  jq:", "\n  sops:", "\n  ssh-to-age:", "\n  github.com/arg-sh/argsh:", "\n  renvsubst:"} {
		if strings.Contains(tpl, absent) {
			t.Errorf("consumer template must not declare %q", absent)
		}
	}

	cloud := Template(TemplateOptions{Name: "demo", LoVersion: "0.3.0", Variant: "full", Groups: []string{"core", "local", "cloud"}})
	if !strings.Contains(cloud, "  github.com/kubermatic/kubeone:\n    groups: [cloud]\n") || !strings.Contains(cloud, "  hcloud:\n    groups: [cloud]\n") {
		t.Fatalf("--groups cloud did not activate the cloud entries:\n%s", cloud)
	}
	coreOnly := Template(TemplateOptions{Name: "demo", LoVersion: "0.3.0", Variant: "core", Groups: []string{"core"}})
	if !strings.Contains(coreOnly, "  # kind:\n  #   groups: [local]\n") {
		t.Fatalf("--groups core left local active:\n%s", coreOnly)
	}
}

func TestNormalizeGroups(t *testing.T) {
	g, err := NormalizeGroups([]string{"cloud", " LOCAL ", ""})
	if err != nil || strings.Join(g, ",") != "core,local,cloud" {
		t.Fatalf("got %v, %v", g, err)
	}
	if _, err := NormalizeGroups([]string{"kustomize"}); err == nil {
		t.Fatal("unknown group accepted")
	}
}

func TestWriteNeverOverwrites(t *testing.T) {
	bin := filepath.Join(t.TempDir(), ".bin")
	content := Template(TemplateOptions{Name: "p", LoVersion: "0.3.0", Variant: "core"})

	// Dry run creates nothing.
	res, err := Write(bin, content, true)
	if err != nil || res.Written || res.Same || res.Diff != "" {
		t.Fatalf("dry run: %+v, %v", res, err)
	}
	if _, err := os.Stat(res.Path); err == nil {
		t.Fatal("dry run wrote b.yaml")
	}

	res, err = Write(bin, content, false)
	if err != nil || !res.Written {
		t.Fatalf("first write: %+v, %v", res, err)
	}
	if !HasMarker(res.Path) {
		t.Fatal("written b.yaml has no marker")
	}

	// Identical content: reported as Same, nothing rewritten.
	res, err = Write(bin, content, false)
	if err != nil || !res.Same || res.Written {
		t.Fatalf("second write: %+v, %v", res, err)
	}

	// A user-edited file stays byte-for-byte; the diff names the change.
	edited := strings.Replace(content, "version: "+KustomizeCLI, "version: v5.0.0", 1)
	if err := os.WriteFile(res.Path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Write(bin, content, false)
	if err != nil || res.Written || res.Same {
		t.Fatalf("existing file: %+v, %v", res, err)
	}
	if !strings.Contains(res.Diff, "-    version: v5.0.0\n") || !strings.Contains(res.Diff, "+    version: "+KustomizeCLI+"\n") {
		t.Fatalf("diff does not show the pin change:\n%s", res.Diff)
	}
	raw, _ := os.ReadFile(res.Path)
	if string(raw) != edited {
		t.Fatal("existing b.yaml was modified")
	}
}

func TestUnifiedDiffShape(t *testing.T) {
	d := unifiedDiff("a", "b", "x\ny\nz\n", "x\nY\nz\nw\n")
	want := "--- a\n+++ b\n x\n-y\n+Y\n z\n+w\n"
	if d != want {
		t.Fatalf("diff = %q, want %q", d, want)
	}
}
