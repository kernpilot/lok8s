package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustTemplate renders a template the test expects to be valid.
func mustTemplate(t *testing.T, o TemplateOptions) string {
	t.Helper()
	tpl, err := Template(o)
	if err != nil {
		t.Fatalf("Template(%+v): %v", o, err)
	}
	return tpl
}

func TestTemplateRejectsUnknownGroup(t *testing.T) {
	t.Parallel()
	if _, err := Template(TemplateOptions{Name: "demo", LoVersion: "0.3.0", Variant: "core", Groups: []string{"kustomize"}}); err == nil {
		t.Fatal("an unknown group rendered a template instead of failing")
	}
}

func TestTemplateGroupsAndMarker(t *testing.T) {
	t.Parallel()
	tpl := mustTemplate(t, TemplateOptions{Name: "demo", LoVersion: "v0.3.0", Variant: "core"})
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
	// The bash runtime is carried commented out by default (opt-in).
	for _, off := range []string{"  # yq:\n  #   groups: [bash]\n", "  # jq:\n  #   groups: [bash]\n", "  # sops:\n  #   groups: [bash]\n", "  # ssh-to-age:\n  #   groups: [bash]\n", "  # renvsubst:\n  #   alias: envsubst\n  #   groups: [bash]\n", "  # github.com/arg-sh/argsh:\n  #   asset: argsh\n  #   onPost: \"${B_BIN} builtin ${B_EVENT}\"\n  #   groups: [bash]\n"} {
		if !strings.Contains(tpl, off) {
			t.Errorf("bash entry not commented out: %q", off)
		}
	}
	for _, absent := range []string{"\n  yq:", "\n  jq:", "\n  sops:", "\n  ssh-to-age:", "\n  github.com/arg-sh/argsh:", "\n  renvsubst:"} {
		if strings.Contains(tpl, absent) {
			t.Errorf("consumer template must not activate %q by default", absent)
		}
	}
	// --groups bash: every bash entry uncommented, each checked on its own
	// with its real key text; the non-selected cloud group stays commented
	// with its real keys.
	withBash := mustTemplate(t, TemplateOptions{Name: "demo", LoVersion: "0.3.0", Variant: "core", Groups: []string{"core", "local", "bash"}})
	for _, on := range []string{
		"  github.com/arg-sh/argsh:\n    asset: argsh\n    onPost: \"${B_BIN} builtin ${B_EVENT}\"\n    groups: [bash]\n",
		"  yq:\n    groups: [bash]\n",
		"  jq:\n    groups: [bash]\n",
		"  renvsubst:\n    alias: envsubst\n    groups: [bash]\n",
		"  sops:\n    groups: [bash]\n",
		"  ssh-to-age:\n    groups: [bash]\n",
	} {
		if !strings.Contains(withBash, on) {
			t.Errorf("--groups bash did not activate %q:\n%s", on, withBash)
		}
	}
	for _, off := range []string{"  # github.com/kubermatic/kubeone:\n  #   groups: [cloud]\n", "  # hcloud:\n  #   groups: [cloud]\n"} {
		if !strings.Contains(withBash, off) {
			t.Errorf("--groups bash uncommented a cloud entry, want %q:\n%s", off, withBash)
		}
	}

	cloud := mustTemplate(t, TemplateOptions{Name: "demo", LoVersion: "0.3.0", Variant: "full", Groups: []string{"core", "local", "cloud"}})
	if !strings.Contains(cloud, "  github.com/kubermatic/kubeone:\n    groups: [cloud]\n") || !strings.Contains(cloud, "  hcloud:\n    groups: [cloud]\n") {
		t.Fatalf("--groups cloud did not activate the cloud entries:\n%s", cloud)
	}
	coreOnly := mustTemplate(t, TemplateOptions{Name: "demo", LoVersion: "0.3.0", Variant: "core", Groups: []string{"core"}})
	if !strings.Contains(coreOnly, "  # kind:\n  #   groups: [local]\n") {
		t.Fatalf("--groups core left local active:\n%s", coreOnly)
	}
}

func TestNormalizeGroups(t *testing.T) {
	t.Parallel()
	g, err := NormalizeGroups([]string{"cloud", " LOCAL ", ""})
	if err != nil || strings.Join(g, ",") != "core,local,cloud" {
		t.Fatalf("got %v, %v", g, err)
	}
	if g, err := NormalizeGroups([]string{"bash", "core"}); err != nil || strings.Join(g, ",") != "core,bash" {
		t.Fatalf("bash group: %v, %v", g, err)
	}
	if _, err := NormalizeGroups([]string{"kustomize"}); err == nil {
		t.Fatal("unknown group accepted")
	}
}

func TestWriteNeverOverwrites(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), ".bin")
	content := mustTemplate(t, TemplateOptions{Name: "p", LoVersion: "0.3.0", Variant: "core"})

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
	// A file `lo init toolchain` wrote before WP9 carries the legacy line
	// and is still ours; a foreign header is not.
	legacy := filepath.Join(t.TempDir(), "b.yaml")
	if err := os.WriteFile(legacy, []byte("# p\n"+legacyMarker+"\nbinaries: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasMarker(legacy) {
		t.Fatal("the legacy marker is not recognised")
	}
	foreign := filepath.Join(t.TempDir(), "b.yaml")
	if err := os.WriteFile(foreign, []byte("# lo-toolchain: managed pins\nbinaries: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasMarker(foreign) {
		t.Fatal("a foreign header counts as the marker")
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
	t.Parallel()
	d := unifiedDiff("a", "b", "x\ny\nz\n", "x\nY\nz\nw\n")
	want := "--- a\n+++ b\n x\n-y\n+Y\n z\n+w\n"
	if d != want {
		t.Fatalf("diff = %q, want %q", d, want)
	}
}
