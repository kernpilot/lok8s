package scaffold

// cluster_test.go — the first cluster spec: one file per driver, the
// domain rule, the keep-unless-force rule, `lo init project --cluster`,
// and the gate that matters: `lo lint` accepts every rendered spec.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/lint"
)

func TestClusterSpecPerDriver(t *testing.T) {
	for _, d := range Drivers {
		got, err := ClusterSpec("demo.dev", d.Name)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		for _, want := range []string{
			"apiVersion: cluster.lok8s.dev/v1beta1\n",
			"kind: " + d.Kind + "\n",
			"metadata:\n  name: demo\n",
			"spec:\n",
			"  cluster:\n    domain: demo.dev\n",
			"  bootstrap:",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in:\n%s", d.Name, want, got)
			}
		}
		if d.Name == "lo" && !strings.Contains(got, "  bootstrap:                         # addons, applied in order (lo addons lists them)\n    - cilium\n") {
			t.Errorf("lo: the explicit cilium default missing:\n%s", got)
		}
		if d.Name != "lo" && !strings.Contains(got, "  bootstrap: []") {
			t.Errorf("%s: bootstrap not the empty list:\n%s", d.Name, got)
		}
		if has := strings.Contains(got, "hosting: hosted"); has != (d.Name == "kubehz-hosted") {
			t.Errorf("%s: kubehz block present=%v", d.Name, has)
		}
		// The KubeOne kinds carry the skill's minimal fields as commented
		// stubs with the documented example values and the fill-in header;
		// the others carry none of it.
		wantStubs := d.Kind == "KubeOne"
		for _, stub := range []string{
			"# Fill in before lo provision: spec.kubernetes.version and spec.provider",
			"  # kubernetes:\n  #   version: \"v1.31.12\"",
			"  # provider:\n  #   name: hetzner\n  #   configRef: hetzner.json",
		} {
			if strings.Contains(got, stub) != wantStubs {
				t.Errorf("%s: stub %q present=%v, want %v", d.Name, stub, !wantStubs, wantStubs)
			}
		}
		// The kind the readers resolve is the driver's.
		file := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
		os.WriteFile(file, []byte(got), 0o600)
		kind, err := domain.SpecDriver(file, "")
		if err != nil || kind != strings.ToLower(d.Kind) {
			t.Errorf("%s: SpecDriver = %q, %v", d.Name, kind, err)
		}
	}
	if _, err := ClusterSpec("demo.dev", "docker"); err == nil || !strings.Contains(err.Error(), "--driver must be one of lo|kubeone|capi|kkp|kubehz-hosted") {
		t.Errorf("unknown driver: %v", err)
	}
	if _, ok := DriverFor("nope"); ok {
		t.Error("DriverFor(nope)")
	}
}

func TestValidateDomain(t *testing.T) {
	for _, ok := range []string{"demo.dev", "a", "my-cluster.example.com", "1.2"} {
		if err := ValidateDomain(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../evil", ".hidden", "a b", "-x", "x/y"} {
		if err := ValidateDomain(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ClusterName("shop.example.com") != "shop" || ClusterName("flat") != "flat" {
		t.Error("ClusterName")
	}
}

func TestWriteClusterSpecKeepsUnlessForce(t *testing.T) {
	clusters := t.TempDir()
	var out, stderr bytes.Buffer
	if err := WriteClusterSpec(clusters, "demo.dev", "lo", false, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(clusters, "demo.dev", "cluster.lok8s.yaml")
	if !strings.Contains(out.String(), "Scaffolded "+file+"\n") {
		t.Errorf("stdout: %s", out.String())
	}
	os.WriteFile(file, []byte("kind: Mine\n"), 0o600)
	out.Reset()
	if err := WriteClusterSpec(clusters, "demo.dev", "lo", false, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != "kind: Mine\n" || !strings.Contains(out.String(), "Kept "+file+" (exists; --force overwrites)\n") {
		t.Errorf("kept: %q stdout %s", raw, out.String())
	}
	if err := WriteClusterSpec(clusters, "demo.dev", "lo", true, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(file)
	if !strings.Contains(string(raw), "kind: Lo\n") {
		t.Errorf("--force did not overwrite: %q", raw)
	}

	// The domain rule: the error is printed the bash way and the handled
	// sentinel returned; nothing is written.
	stderr.Reset()
	if err := WriteClusterSpec(clusters, "../evil", "lo", false, &out, &stderr); !errors.Is(err, ErrHandled) || !strings.Contains(stderr.String(), "invalid domain name: ../evil") {
		t.Errorf("bad domain: err=%v stderr=%s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(clusters, "..", "evil")); err == nil {
		t.Error("a traversal domain wrote outside clusters/")
	}
}

// Every rendered spec passes `lo lint` in a project whose clusters/ holds
// it: the readers, the schema check and the bootstrap-entry resolution
// accept the minimal file.
func TestClusterSpecPassesLint(t *testing.T) {
	for _, d := range Drivers {
		base := t.TempDir()
		var out, stderr bytes.Buffer
		if err := Project(base, ProjectOptions{Name: "acme", Env: "none", Domain: "demo.dev", Driver: d.Name}, &out, &stderr); err != nil {
			t.Fatalf("%s: project: %v\n%s", d.Name, err, stderr.String())
		}
		p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
		var lintOut, lintErr bytes.Buffer
		l := &lint.Linter{Paths: p, Out: &lintOut, ErrOut: &lintErr}
		if err := l.Run("demo.dev"); err != nil {
			t.Errorf("%s: lo lint rejected the spec: %v\n%s%s", d.Name, err, lintOut.String(), lintErr.String())
		}
		if strings.Contains(lintErr.String(), "[error]") {
			t.Errorf("%s: lint errors:\n%s", d.Name, lintErr.String())
		}
	}
}

// --implementation: the project file is created with the block on a fresh
// directory; on an existing file the comments and the other keys survive.
func TestProjectImplementation(t *testing.T) {
	base := t.TempDir()
	var out, stderr bytes.Buffer
	if err := Project(base, ProjectOptions{Name: "acme", Env: "none", Implementation: "bash"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "lok8s.yaml")
	if !strings.Contains(out.String(), "Set spec.implementation.default: bash in "+file+"\n") {
		t.Errorf("stdout:\n%s", out.String())
	}
	if impl, err := config.LoadImplementation(base); err != nil || impl.Default != config.ImplBash {
		t.Errorf("fresh dir: %+v %v", impl, err)
	}

	os.WriteFile(file, []byte("# my header\napiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: keep # me\nspec:\n  implementation:\n    default: bash\n    bash:\n      commands: [registry] # routed\n"), 0o600)
	out.Reset()
	if err := Project(base, ProjectOptions{Env: "none", Implementation: "go"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	for _, want := range []string{"# my header\n", "name: keep # me\n", "    default: go\n", "commands: [registry] # routed\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in:\n%s", want, raw)
		}
	}
	if !strings.Contains(out.String(), "Kept "+file+" (exists; --force overwrites)\n") {
		t.Errorf("the existing file was not reported kept:\n%s", out.String())
	}

	// An unknown value is refused before anything is written.
	fresh := t.TempDir()
	stderr.Reset()
	if err := Project(fresh, ProjectOptions{Env: "none", Implementation: "python"}, &out, &stderr); !errors.Is(err, ErrHandled) || !strings.Contains(stderr.String(), "--implementation must be go or bash") {
		t.Errorf("python: %v %s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(fresh, "lok8s.yaml")); err == nil {
		t.Error("a refused value wrote the project file")
	}
}

func TestProjectWithCluster(t *testing.T) {
	base := t.TempDir()
	var out, stderr bytes.Buffer
	if err := Project(base, ProjectOptions{Env: "none", Domain: "demo.dev"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(base, "clusters", "demo.dev", "cluster.lok8s.yaml")
	raw, err := os.ReadFile(spec)
	if err != nil || !strings.Contains(string(raw), "kind: Lo\n") {
		t.Errorf("driver not defaulted to lo: %v %q", err, raw)
	}
	if !strings.Contains(out.String(), "Scaffolded "+spec+"\n") || !strings.Contains(out.String(), "  lo use demo.dev   # then lo up\n") {
		t.Errorf("stdout:\n%s", out.String())
	}
	if strings.Contains(out.String(), "lo use <domain>") {
		t.Error("the placeholder next step printed beside a real domain")
	}

	// Without --cluster nothing changes: no clusters/<x>, the placeholder
	// next step.
	base2 := t.TempDir()
	out.Reset()
	if err := Project(base2, ProjectOptions{Env: "none"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(base2, "clusters"))
	if len(entries) != 1 || entries[0].Name() != ".gitkeep" || !strings.Contains(out.String(), "lo use <domain>") {
		t.Errorf("clusters entries %v stdout:\n%s", entries, out.String())
	}

	// A bad driver stops before anything else is reported wrong.
	if err := Project(t.TempDir(), ProjectOptions{Env: "none", Domain: "x.dev", Driver: "nope"}, &out, &stderr); !errors.Is(err, ErrHandled) {
		t.Errorf("bad driver: %v", err)
	}
}
