package provision

// provider_test.go covers WriteProviderConfig (bash: provider::write_config).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// The domain of each row of testdata/spec-domain-cases.txt: ".", "..", "/",
// a trailing slash, "a/../b" and more. The bats test "provider::_spec_domain
// gives the domain of each row of the shared case table" reads the same
// table, and its expected values come from the bash function, so Go and
// bash give one domain for one path. No row names a spec file: the domain
// is set before the spec is read.
func TestWriteProviderConfigDomainCases(t *testing.T) {
	t.Setenv("PROVIDER_DOMAIN", "")
	t.Setenv("PROVIDER_CONFIG_FILE", "")

	b, err := os.ReadFile(filepath.Join("testdata", "spec-domain-cases.txt"))
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for line := range strings.SplitSeq(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		spec, want, ok := strings.Cut(line, "\t")
		if !ok || spec == "" || want == "" || strings.Contains(want, "\t") {
			t.Fatalf("bad row %q: want <spec path> TAB <domain>", line)
		}
		rows++
		os.Setenv("PROVIDER_DOMAIN", "not-set")
		_, cleanup, err := WriteProviderConfig(spec, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		if cleanup != nil {
			cleanup()
		}
		if got := os.Getenv("PROVIDER_DOMAIN"); got != want {
			t.Errorf("%q: PROVIDER_DOMAIN = %q, want %q (bash provider::_spec_domain)", spec, got, want)
		}
	}
	if rows < 10 {
		t.Fatalf("the case table has %d rows, want at least 10", rows)
	}
}
