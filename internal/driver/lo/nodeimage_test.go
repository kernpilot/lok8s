package lo

// nodeimage_test.go — spec.kubernetes.version → the kind node image. An
// unset version (missing, null, "", the literal "null") once rendered
// `image: "kindest/node:null"`, and `lo provision` failed at `kind create`
// AFTER it had created the docker network and the registry containers. The
// bash twin is tests/unit/lo_node_version_test.bats.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

const testDigest = "@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2"

// nodeVersionFixtures lists tests/fixtures/kind-node-version/<case>.yaml.
// Each <case>.want holds one line: "unset", "tag <tag>", or "error
// <message>". The same files drive the bats twin
// (tests/unit/lo_node_version_test.bats).
func nodeVersionFixtures(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(t), "tests", "fixtures", "kind-node-version", "*.yaml"))
	if err != nil || len(files) < 50 {
		t.Fatalf("kind node version fixtures missing (%d found, err %v)", len(files), err)
	}
	out := map[string]string{}
	for _, f := range files {
		want, err := os.ReadFile(strings.TrimSuffix(f, ".yaml") + ".want")
		if err != nil {
			t.Fatalf("%s has no .want file: %v", filepath.Base(f), err)
		}
		out[f] = strings.TrimSuffix(string(want), "\n")
	}
	return out
}

// nodeVersionVerdict renders NodeVersion in the .want format.
func nodeVersionVerdict(path string) string {
	v, err := NodeVersion(path)
	switch {
	case err != nil:
		return "error " + err.Error()
	case v == "":
		return "unset"
	}
	return "tag " + v
}

func TestNodeVersionFixtures(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for path, want := range nodeVersionFixtures(t) {
		if got := nodeVersionVerdict(path); got != want {
			t.Errorf("%s:\n got  %q\n want %q", filepath.Base(path), got, want)
		}
		verdict, _, _ := strings.Cut(want, " ")
		if prefix, _, _ := strings.Cut(filepath.Base(path), "--"); prefix != verdict {
			t.Errorf("%s: the name says %q, the .want file says %q", filepath.Base(path), prefix, verdict)
		}
		seen[verdict] = true
	}
	for _, v := range []string{"unset", "tag", "error"} {
		if !seen[v] {
			t.Errorf("no fixture for the verdict %q", v)
		}
	}
}

// bashLocale is the locale the bash twin runs under in
// TestNodeVersionMatchesBash: en_US.UTF-8, where bash matches 'é' with
// [A-Za-z] (the C.UTF-8 of a CI runner does not), so the `local LC_ALL=C` of
// spec::_is_image_tag is under test. Without that locale the test runs in the
// inherited one; under CI=true a missing locale fails the test (the go-tests
// job generates it).
func bashLocale(t *testing.T) []string {
	t.Helper()
	out, _ := exec.CommandContext(t.Context(), "locale", "-a").Output()
	for l := range strings.FieldsSeq(strings.ToLower(string(out))) {
		if l == "en_us.utf8" || l == "en_us.utf-8" {
			return []string{"LC_ALL=en_US.UTF-8"}
		}
	}
	if os.Getenv("CI") == "true" {
		t.Fatal("the locale en_US.UTF-8 is missing under CI (locale-gen en_US.UTF-8)")
	}
	t.Log("no en_US.UTF-8 locale: the bash twin runs in the inherited locale")
	return nil
}

// TestNodeVersionMatchesBash runs the bash twin (spec::kind_node_version) on
// every fixture and compares its answer with the Go answer. It needs bash
// and the pinned yq (.bin/yq); it skips without them, and fails without
// them under CI=true. The merge cycles run too: the reads of the twin are
// bounded (spec::_kver_yq: 10 s, 256 MiB).
func TestNodeVersionMatchesBash(t *testing.T) {
	root := testutil.RepoRoot(t)
	if _, err := os.Stat(filepath.Join(root, ".bin", "yq")); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("the pinned yq is missing under CI: %v", err)
		}
		t.Skip("no .bin/yq (b install)")
	}
	locale := bashLocale(t)
	utils := filepath.Join(root, ".lok8s", "utils")
	// Source the twin and its domain util (domain::spec_layout) with the
	// argsh import stubbed: only the reader runs.
	script := `import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"
if out=$(spec::kind_node_version "$2"); then
  if [[ -n "${out}" ]]; then printf 'tag %s\n' "${out}"; else echo unset; fi
else
  printf 'error %s\n' "${out}"
fi`
	for path := range nodeVersionFixtures(t) {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", utils, path)
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, ".bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Env = append(cmd.Env, locale...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: the bash reader failed: %v\n%s", filepath.Base(path), err, stderr.String())
		}
		if bash, goVerdict := strings.TrimSuffix(string(out), "\n"), nodeVersionVerdict(path); bash != goVerdict {
			t.Errorf("%s:\n Go   %q\n bash %q", filepath.Base(path), goVerdict, bash)
		}
		if stderr.Len() != 0 {
			t.Errorf("%s: the bash reader wrote to stderr:\n%s", filepath.Base(path), stderr.String())
		}
	}
}

// TestNodeVersionMergeChainIsQuick: a doubling chain of 30 mappings would
// cost a yq path read 2^31 map reads. The merge-read count refuses it at
// once, in Go and (TestNodeVersionMatchesBash) in bash, before any read.
func TestNodeVersionMergeChainIsQuick(t *testing.T) {
	t.Parallel()
	path := filepath.Join(testutil.RepoRoot(t), "tests", "fixtures", "kind-node-version", "error--merge-doubling-chain.yaml")
	start := time.Now()
	_, err := NodeVersion(path)
	if err == nil || !strings.Contains(err.Error(), "need more than 10000 map reads") {
		t.Fatalf("NodeVersion: %v; want the map-read limit", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("NodeVersion took %v on a doubling chain", d)
	}
}

// TestNodeVersionGoOnlyProblems: the problems whose text the two readers do
// not share: the YAML parser messages of yaml.v3 and yq differ. Both readers
// refuse each one (the bats twin pins the bash side).
func TestNodeVersionGoOnlyProblems(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct{ name, src, want string }{
		{"parse error", "kind: Lo\nspec: [\n", "spec.kubernetes.version cannot be read: yaml: "},
		{"unknown anchor", "kind: Lo\nspec:\n  kubernetes:\n    version: *nope\n", "spec.kubernetes.version cannot be read: yaml: "},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
		testutil.WriteFile(t, path, tc.src)
		v, err := NodeVersion(path)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: NodeVersion = %q, %v; want the error %q…", tc.name, v, err, tc.want)
		}
	}
	if _, err := NodeVersion(filepath.Join(dir, "missing.yaml")); err == nil ||
		!strings.HasPrefix(err.Error(), "spec.kubernetes.version cannot be read: open ") ||
		!strings.HasSuffix(err.Error(), ". Correct the spec file.") {
		t.Errorf("a missing file: %v", err)
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
		{name: "custom tag", versionYAML: "version: latest", wantImage: "kindest/node:latest"},
		// The merge key once read as unset: kind's default Kubernetes
		// version instead of the one in the spec.
		{name: "merge key", versionYAML: "<<: {version: v1.31.4}", wantImage: "kindest/node:v1.31.4"},
		{name: "block scalar", versionYAML: "version: |\n      v1.31.4", wantImage: "kindest/node:v1.31.4"},
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
			// The image checks below read the captured config: without
			// --config they would pass on an empty capture.
			if !slices.Contains(*argv, "--config") {
				t.Fatalf("kind create carries no --config: %v", *argv)
			}
			if !strings.Contains(*config, "\nkind: Cluster\n") || strings.Count(*config, "\n  - role: control-plane") != 1 {
				t.Fatalf("the captured kind config is not the cluster config with one control-plane node:\n%s", *config)
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
			if want := "\n  - role: control-plane\n    image: \"" + tc.wantImage + "\""; images != 1 || !strings.Contains(*config, want) {
				t.Fatalf("want the control-plane node with %q, got %d image lines:\n%s", tc.wantImage, images, *config)
			}
		})
	}
}

func TestProvisionMalformedVersionStopsBeforeAnyToolRuns(t *testing.T) {
	// A version that cannot be read or is not an image tag must stop the
	// provision before the first docker network, registry container or VM,
	// not at `kind create`. It is never read as unset.
	for _, tc := range []struct{ name, versionYAML, wantErr string }{
		{"image reference", "version: kindest/node:v1.31.4", "[error] spec.kubernetes.version 'kindest/node:v1.31.4' is not a valid image tag. " + nodeVersionHint},
		{"map", "version: {minor: 31}", "[error] spec.kubernetes.version cannot be read: the value is a map, not a single value. Write spec.kubernetes.version as a single value."},
		{"merge of a scalar", "<<: v1.31.4", "[error] spec.kubernetes.version cannot be read: a merge key (<<) in the spec file does not refer to a map or a list of maps. Correct the merge key."},
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
