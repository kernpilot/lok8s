package lo

// nodeimage_test.go — spec.kubernetes.version → the kind node image. An
// unset version (missing, null, "", the literal "null") once rendered
// `image: "kindest/node:null"`, and `lo provision` failed at `kind create`
// AFTER it had created the docker network and the registry containers. The
// bash twin is tests/unit/lo_node_version_test.bats.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

const testDigest = "@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2"

func TestNodeVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, versionYAML, want, wantErr string
	}{
		{name: "missing key", versionYAML: ""},
		{name: "yaml null", versionYAML: "version: null"},
		{name: "tilde null", versionYAML: "version: ~"},
		{name: "empty value", versionYAML: "version:"},
		{name: "empty string", versionYAML: `version: ""`},
		{name: "literal null string", versionYAML: `version: "null"`},
		{name: "plain", versionYAML: "version: v1.35.0", want: "v1.35.0"},
		{name: "quoted", versionYAML: `version: "v1.31.4"`, want: "v1.31.4"},
		{name: "digest", versionYAML: "version: v1.31.12" + testDigest, want: "v1.31.12" + testDigest},
		{name: "pre-release", versionYAML: "version: v1.36.0-rc.1", want: "v1.36.0-rc.1"},
		{name: "no v prefix", versionYAML: `version: "1.31.0"`, wantErr: "spec.kubernetes.version '1.31.0' is not a kind node version."},
		{name: "minor only", versionYAML: `version: "1.31"`, wantErr: "spec.kubernetes.version '1.31' is not a kind node version."},
		{name: "float", versionYAML: "version: 1.31", wantErr: "spec.kubernetes.version '1.31' is not a kind node version."},
		{name: "bool", versionYAML: "version: false", wantErr: "spec.kubernetes.version 'false' is not a kind node version."},
		{name: "latest", versionYAML: "version: latest", wantErr: "spec.kubernetes.version 'latest' is not a kind node version."},
		{name: "short digest", versionYAML: "version: v1.35.0@sha256:abc", wantErr: "spec.kubernetes.version 'v1.35.0@sha256:abc' is not a kind node version."},
		{name: "map", versionYAML: "version: {a: b}", wantErr: "spec.kubernetes.version is a map, not a kind node version."},
		{name: "list", versionYAML: "version: [v1.35.0]", wantErr: "spec.kubernetes.version is a list, not a kind node version."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
			testutil.WriteFile(t, spec, "kind: Lo\nspec:\n  kubernetes:\n    "+tc.versionYAML+"\n")
			got, err := NodeVersion(yqsem.LoadNode(spec))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NodeVersion = %q, nil; want the error %q", got, tc.wantErr)
				}
				if !strings.HasPrefix(err.Error(), tc.wantErr) || !strings.HasSuffix(err.Error(), nodeVersionHint) {
					t.Fatalf("error = %q, want %q + the hint", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NodeVersion = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestRenderKindConfigUnsetVersionNamesNoImage(t *testing.T) {
	// The multinode golden (3 control planes, 2 workers) minus its five
	// image lines: an unset version changes nothing else in the config.
	want := strings.ReplaceAll(readFileT(t, filepath.Join("testdata", "kindconfig_multinode.golden")),
		"\n    image: \"kindest/node:v1.31.4\"", "")
	for _, version := range []string{"", "null"} {
		t.Run("version="+version, func(t *testing.T) {
			d := goldenDriver(t)
			setNodeEnv(t, "3", "2", "false", "0")
			got := d.renderKindConfig("test-shared", version, "lok8s", "")
			if strings.Contains(got, "image:") || strings.Contains(got, "kindest/node") {
				t.Fatalf("an unset version still names a node image:\n%s", got)
			}
			if got != want {
				t.Fatalf("render differs from the multinode golden without its image lines:\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
}

// specWithVersion is the provision fixture's spec with spec.kubernetes
// carrying the given version line ("" leaves the key out).
func specWithVersion(versionYAML string) string {
	if versionYAML == "" {
		return specShared
	}
	return strings.Replace(specShared, "spec:\n", "spec:\n  kubernetes:\n    "+versionYAML+"\n", 1)
}

// kindCreateCapture wraps the provision fixture's runner: it records the
// argv of `kind create cluster` and the content of the config file it got.
func kindCreateCapture(t *testing.T, runner *fakeRunner) (argv *[]string, config *string) {
	t.Helper()
	argv, config = new([]string), new(string)
	base := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "create" && c.Args[1] == "cluster" {
			*argv = slices.Clone(c.Args)
			if i := slices.Index(c.Args, "--config"); i >= 0 && i+1 < len(c.Args) {
				raw, err := os.ReadFile(c.Args[i+1])
				if err != nil {
					t.Errorf("read the kind config %s: %v", c.Args[i+1], err)
				}
				*config = string(raw)
			}
		}
		return base(c)
	}
	return argv, config
}

func TestProvisionNodeImage(t *testing.T) {
	for _, tc := range []struct {
		name, versionYAML, wantImage string
	}{
		{name: "no version", versionYAML: ""},
		{name: "yaml null", versionYAML: "version: null"},
		{name: "tilde null", versionYAML: "version: ~"},
		{name: "literal null string", versionYAML: `version: "null"`},
		{name: "empty string", versionYAML: `version: ""`},
		{name: "given version", versionYAML: "version: v1.35.0", wantImage: "kindest/node:v1.35.0"},
		{name: "given digest", versionYAML: "version: v1.31.12" + testDigest, wantImage: "kindest/node:v1.31.12" + testDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, runner, _, errBuf, p := provisionFixture(t)
			testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specWithVersion(tc.versionYAML))
			argv, config := kindCreateCapture(t, runner)

			if err := d.Provision(t.Context(), "test.lok8s.dev"); err != nil {
				t.Fatalf("Provision: %v\nstderr:\n%s", err, errBuf.String())
			}
			if len(*argv) == 0 {
				t.Fatal("kind create cluster never ran")
			}
			if slices.ContainsFunc(*argv, func(a string) bool { return strings.HasPrefix(a, "--image") }) {
				t.Fatalf("kind create carries an --image flag: %v", *argv)
			}
			if strings.Contains(*config, "kindest/node:null") || strings.Contains(*config, "kindest/node:~") {
				t.Fatalf("the kind config names a null image:\n%s", *config)
			}
			images := strings.Count(*config, "\n    image: ")
			if tc.wantImage == "" {
				if images != 0 {
					t.Fatalf("unset version: want no image line (kind's default node image), got %d:\n%s", images, *config)
				}
				return
			}
			if want := "\n    image: \"" + tc.wantImage + "\""; images != 1 || !strings.Contains(*config, want) {
				t.Fatalf("want exactly one node with %q, got %d image lines:\n%s", want, images, *config)
			}
		})
	}
}

func TestProvisionMalformedVersionStopsBeforeAnyToolRuns(t *testing.T) {
	// A value kind cannot pull must stop the provision before the first
	// docker network, registry container or VM — not at `kind create`.
	for _, tc := range []struct{ name, versionYAML, wantErr string }{
		{"minor only", `version: "1.31"`, "spec.kubernetes.version '1.31' is not a kind node version."},
		{"no v prefix", "version: 1.35.0", "spec.kubernetes.version '1.35.0' is not a kind node version."},
		{"map", "version: {minor: 31}", "spec.kubernetes.version is a map, not a kind node version."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, runner, _, errBuf, p := provisionFixture(t)
			testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specWithVersion(tc.versionYAML))
			// A remote provider must not get a VM either.
			provider := &fakeLoProvider{output: []byte(`{"nodes":[]}`)}
			d.deps.ProviderName = "hetzner"
			d.deps.Provider = provider

			if err := d.Provision(t.Context(), "test.lok8s.dev"); err == nil {
				t.Fatal("Provision accepted a malformed spec.kubernetes.version")
			}
			if !strings.Contains(errBuf.String(), tc.wantErr) {
				t.Fatalf("stderr does not name the problem %q:\n%s", tc.wantErr, errBuf.String())
			}
			if len(runner.calls) != 0 {
				t.Fatalf("tools ran before the version was rejected:\n%s", strings.Join(runner.calls, "\n"))
			}
			if _, err := os.Stat(filepath.Join(p.Clusters, "test.lok8s.dev", ".provider")); err == nil {
				t.Fatal("the remote provision started before the version was rejected")
			}
		})
	}
}

func TestExportTreatsLiteralNullVersionAsUnset(t *testing.T) {
	for _, tc := range []struct{ versionYAML, want string }{
		{`version: "null"`, ""},
		{"version: ~", ""},
		{"", ""},
		{"version: v1.35.0", "v1.35.0"},
	} {
		d, _, errBuf, p := testDriver(t)
		testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specWithVersion(tc.versionYAML))
		if err := d.Export(t.Context(), "test.lok8s.dev"); err != nil {
			t.Fatalf("Export: %v\n%s", err, errBuf.String())
		}
		if got := os.Getenv("LOK8S_SPEC_KUBERNETES_VERSION"); got != tc.want {
			t.Errorf("%q: LOK8S_SPEC_KUBERNETES_VERSION = %q, want %q", tc.versionYAML, got, tc.want)
		}
	}
}
