package recover

// bridge_test.go covers the bash bridge argv.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBashBridgeArgv(t *testing.T) {
	h := newHarness(t)
	rec := &argvRecorder{probe: "rebuild\ndoctor\n"}
	h.r.Exec = rec
	prov, err := h.r.bashProvider(t.Context(), "hetzner")
	if err != nil {
		t.Fatal(err)
	}
	if !prov.HasRebuild() || !prov.HasDoctor() {
		t.Error("probe not parsed")
	}
	_ = prov.Doctor(t.Context(), "/cfg")
	_ = prov.Rebuild(t.Context(), "/cfg", "/wd")
	_, _ = prov.Output(t.Context(), "/cfg")
	_ = h.r.bashProvision(t.Context(), "test.dom")

	wantArgs := [][]string{
		{"-c", providerProbeScript, "lo-recover-provider", "hetzner"},
		{"-c", providerCallScript, "lo-recover-provider", "hetzner", "provider::doctor", "/cfg"},
		{"-c", providerCallScript, "lo-recover-provider", "hetzner", "provider::rebuild", "/cfg", "/wd"},
		{"-c", providerCallScript, "lo-recover-provider", "hetzner", "provider::output", "/cfg"},
		{"-c", provisionScript, "lo-recover-provision", "test.dom"},
	}
	if len(rec.cmds) != len(wantArgs) {
		t.Fatalf("%d children, want %d", len(rec.cmds), len(wantArgs))
	}
	p := h.r.Paths
	for i, c := range rec.cmds {
		if c.Name != "bash" || strings.Join(c.Args, "\x00") != strings.Join(wantArgs[i], "\x00") {
			t.Errorf("child %d: %s %q", i, c.Name, c.Args)
		}
		if c.Dir != p.Base {
			t.Errorf("child %d: dir = %q", i, c.Dir)
		}
		env := strings.Join(c.Env, "\n")
		for _, want := range []string{"PATH_BASE=" + p.Base, "PATH_BIN=" + p.Bin, "PATH_LOK8S=" + p.Lok8s, "PATH_CLUSTERS=" + p.Clusters, "PATH_SCRIPTS=" + p.Lok8s, "PATH_SECRETS=" + filepath.Join(p.Base, ".secrets")} {
			if !strings.Contains(env, want) {
				t.Errorf("child %d: env missing %q", i, want)
			}
		}
		// shimEnv order: .bin ends up first, then .lok8s, then the inherited PATH.
		if !strings.HasPrefix(c.Env[0], "PATH="+p.Bin+string(os.PathListSeparator)+p.Lok8s+string(os.PathListSeparator)) {
			t.Errorf("child %d: PATH = %q", i, c.Env[0])
		}
	}
	// The scripts hold the load-bearing pieces verbatim.
	for _, want := range []string{`provider::load "${1}" >/dev/null`, "declare -F provider::rebuild", "declare -F provider::doctor"} {
		if !strings.Contains(providerProbeScript, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	for _, want := range []string{"import ^libs/provision", "import ^libs/bootstrap", "import ^libs/kubehz/main", "import ^libs/inventory/main", "import ^libs/gitops", "force=1\nprovision::dispatch \"${1}\""} {
		if !strings.Contains(provisionScript, want) {
			t.Errorf("provision script missing %q", want)
		}
	}
	// A failing probe is a failed load.
	h.r.Exec = &argvRecorder{fail: true}
	if _, err := h.r.bashProvider(t.Context(), "nosuch"); err == nil {
		t.Error("failed probe must fail the load")
	}
}
