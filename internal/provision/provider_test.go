package provision

// provider_test.go covers WriteProviderConfig (bash: provider::write_config).

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// bats: "provider::write_config exports PROVIDER_DOMAIN, the directory of
// the spec, in both modes". The hetzner provider keys the owner mark of the
// cluster's parts on this domain.
func TestWriteProviderConfigExportsTheDomain(t *testing.T) {
	// Registers the restore of the values WriteProviderConfig sets.
	t.Setenv("PROVIDER_DOMAIN", "")
	t.Setenv("PROVIDER_CONFIG_FILE", "")

	dir := t.TempDir()
	refSpec := filepath.Join(dir, "clusters", "ref.example.test", "cluster.lok8s.yaml")
	testutil.WriteFile(t, filepath.Join(filepath.Dir(refSpec), "hetzner.json"), `{"cluster_name":"ref"}`)
	testutil.WriteFile(t, refSpec, "spec:\n  provider:\n    name: hetzner\n    configRef: hetzner.json\n")
	inlineSpec := filepath.Join(dir, "clusters", "inline.example.test", "cluster.lok8s.yaml")
	testutil.WriteFile(t, inlineSpec, "spec:\n  provider:\n    name: hetzner\n    config:\n      cluster_name: inline\n")

	// A path with "." or ".." parts: the domain of the cleaned path, the
	// same as bash provider::_spec_domain.
	testutil.WriteFile(t, filepath.Join(dir, "clusters", "inline.example.test", "child", ".keep"), "")
	dotSpec := filepath.Join(dir, "clusters", "inline.example.test") + "/./cluster.lok8s.yaml"
	upSpec := filepath.Join(dir, "clusters", "inline.example.test") + "/child/../cluster.lok8s.yaml"

	for _, c := range []struct{ spec, want string }{
		{refSpec, "ref.example.test"},
		{inlineSpec, "inline.example.test"},
		{dotSpec, "inline.example.test"},
		{upSpec, "inline.example.test"},
	} {
		_, cleanup, err := WriteProviderConfig(c.spec, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if cleanup != nil {
			cleanup()
		}
		if got := os.Getenv("PROVIDER_DOMAIN"); got != c.want {
			t.Errorf("%s: PROVIDER_DOMAIN = %q, want %q", c.spec, got, c.want)
		}
	}
}
