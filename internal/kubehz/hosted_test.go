package kubehz

// hosted_test.go ports tests/unit/kubehz_hosted_test.bats (the library
// half; the driver-branch cases live in the kubeone/capi packages' own
// hook tests and in hooks_test.go here).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestBuildClusterPayloadMatchesBashGolden(t *testing.T) {
	t.Parallel()
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(spec, []byte(`kind: KubeOne
metadata:
  name: hosted
spec:
  cluster:
    domain: test.kubehz.dev
  hcloud:
    region: nbg1
  kubernetes:
    version: v1.31.10
  controlPlane:
    replicas: 3
  kubehz:
    hosting: hosted
    access: managed
    apiUrl: https://api.kubehz.dev
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildClusterPayload(spec)
	// The compare is semantic (jq pretty-printed the golden), so -update
	// rewrites the file only when the meaning changed.
	path := filepath.Join("testdata", "golden", "cluster-payload.json")
	want := readFile(t, path)
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("payload not JSON: %s", got)
	}
	_ = json.Unmarshal([]byte(want), &w)
	if !reflect.DeepEqual(g, w) {
		if *update {
			testutil.WriteFile(t, path, string(got))
		} else {
			t.Fatalf("payload drift (run: go test ./internal/kubehz/ -update):\n got %s\nwant %s", got, want)
		}
	}
	// Key order is the jq object order (bash: jq -n '{domain, kind, …}').
	if string(got) != `{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"nbg1","kubernetesVersion":"v1.31.10","controlPlaneReplicas":3,"hosting":"hosted","access":"managed"}` {
		t.Fatalf("key order: %s", got)
	}
}

func TestBuildClusterPayloadDefaults(t *testing.T) {
	t.Parallel()
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	_ = os.WriteFile(spec, []byte("kind: Capi\nspec:\n  cluster:\n    domain: default.kubehz.dev\n  kubernetes:\n    version: v1.30.0\n"), 0o644)
	var g map[string]any
	_ = json.Unmarshal(buildClusterPayload(spec), &g)
	if g["provider"] != "hetzner" || g["controlPlaneReplicas"] != float64(1) || g["region"] != "fsn1" || g["kind"] != "Capi" {
		t.Fatalf("%v", g)
	}
}

// Issue #272: an unset spec.kubernetes.version (no key, a YAML null, false,
// an empty string) is left out of the payload, not sent as the string
// "null" or "false"; a set version is sent as written, a number as its
// text in a JSON string. Bash twin: the build_cluster_payload version cases
// in tests/unit/kubehz_hosted_test.bats. Bash prints the payload pretty, so
// that test compares the compact form (`jq -c`) with the same strings.
func TestBuildClusterPayloadKubernetesVersion(t *testing.T) {
	t.Parallel()
	const unset = `{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","controlPlaneReplicas":1,"hosting":"hosted","access":"none"}`
	withVersion := func(v string) string {
		return `{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","kubernetesVersion":"` + v + `","controlPlaneReplicas":1,"hosting":"hosted","access":"none"}`
	}
	cases := []struct {
		name, block string
		version     string // the kubernetesVersion sent; "" = the key is left out
	}{
		{"no key", "", ""},
		{"null", "  kubernetes:\n    version: null\n", ""},
		{"tilde", "  kubernetes:\n    version: ~\n", ""},
		{"no value", "  kubernetes:\n    version:\n", ""},
		{"empty string", "  kubernetes:\n    version: \"\"\n", ""},
		{"false", "  kubernetes:\n    version: false\n", ""},
		{"zero", "  kubernetes:\n    version: 0\n", "0"},
		{"a number", "  kubernetes:\n    version: 1.30\n", "1.30"},
		{"set", "  kubernetes:\n    version: v1.31.10\n", "v1.31.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := filepath.Join(t.TempDir(), "spec.yaml")
			testutil.WriteFile(t, spec, "kind: KubeOne\nspec:\n  cluster:\n    domain: test.kubehz.dev\n"+tc.block+"  kubehz:\n    hosting: hosted\n")
			got := buildClusterPayload(spec)
			want := unset
			if tc.version != "" {
				want = withVersion(tc.version)
			}
			if string(got) != want {
				t.Fatalf("payload:\n got %s\nwant %s", got, want)
			}
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("payload not JSON: %s", got)
			}
			v, present := m["kubernetesVersion"]
			if tc.version != "" {
				if !present || v != tc.version {
					t.Fatalf("kubernetesVersion = %v (present %v), want the string %q", v, present, tc.version)
				}
			} else if present {
				t.Fatalf("kubernetesVersion is present (%v), want it left out", v)
			}
		})
	}
}

// A hosted spec with no spec.kubernetes.version is refused before any
// request: the platform needs the version (it answers HTTP 400, a missing
// field). No request reaches the api, and no kubeconfig is written. Bash
// twin: the provision_hosted refusal case in
// tests/unit/kubehz_hosted_test.bats.
func TestProvisionHostedRefusesASpecWithNoVersion(t *testing.T) {
	t.Parallel()
	for name, block := range map[string]string{
		"no key":       "",
		"null":         "  kubernetes:\n    version: null\n",
		"false":        "  kubernetes:\n    version: false\n",
		"empty string": "  kubernetes:\n    version: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := h.writeSpec("test.kubehz.dev", "kind: KubeOne\nmetadata:\n  name: test.kubehz.dev\nspec:\n  cluster:\n    domain: test.kubehz.dev\n"+block+"  kubehz:\n    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n")
			// The routes of a create that works: without the refusal, the
			// provision would succeed and write the kubeconfig.
			hostedKubeconfigRoutes(h, 200, "new\n")
			mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
			want := "[error] kubehz: spec.kubernetes.version is not set in " + spec + ". A hosted cluster needs a Kubernetes version, so no cluster was created. Set it, then run the command again.\n"
			if got := h.output(); got != want {
				t.Fatalf("output:\n got %q\nwant %q", got, want)
			}
			if reqs := h.reqs(); len(reqs) != 0 {
				t.Fatalf("requests reached the api: %+v", reqs)
			}
			if _, err := os.Stat(filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")); !os.IsNotExist(err) {
				t.Fatalf("a kubeconfig was written (stat: %v)", err)
			}
		})
	}
}

func TestWaitForCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.handle("GET /api/clusters/cl-001", 200, `{"id":"cl-001","status":"Running"}`)
	mustOK(t, h.ctx.waitForCluster(t.Context(), h.apiURL(), "cl-001", 30), h.output())
	h.reset()
	h.handle("GET /api/clusters/cl-002", 200, `{"id":"cl-002","status":"Failed"}`)
	mustErr(t, h.ctx.waitForCluster(t.Context(), h.apiURL(), "cl-002", 30))
	mustContain(t, h.output(), "failed")
	h.reset()
	h.handle("GET /api/clusters/cl-003", 200, `{"data":{"status":"Creating"}}`)
	mustErr(t, h.ctx.waitForCluster(t.Context(), h.apiURL(), "cl-003", 30))
	mustContain(t, h.output(), "Timed out waiting for hosted cluster cl-003 after 30s")
	// A status read that fails is "Unknown": the wait goes on.
	h.reset()
	h.handle("GET /api/clusters/cl-004", 500, `{"ok":false}`)
	mustErr(t, h.ctx.waitForCluster(t.Context(), h.apiURL(), "cl-004", 30))
	mustContain(t, h.output(), "Timed out waiting for hosted cluster cl-004 after 30s")
}

// hostedSpec writes a hosted spec for test.kubehz.dev. It also answers
// the cluster list with no cluster: ProvisionHosted reads it before the
// create. A test registers its own list to replace it.
func hostedSpec(h *harness, name string) string {
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[],"meta":{"pagination":{"page":1,"perPage":500,"total":0}}}`)
	return h.writeSpec("test.kubehz.dev", "kind: KubeOne\nmetadata:\n  name: "+name+"\nspec:\n  cluster:\n    domain: test.kubehz.dev\n  kubernetes:\n    version: v1.31.10\n  kubehz:\n    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n")
}

func TestProvisionHostedCreatesWaitsDownloads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["KUBEHZ_TOKEN"] = "test-token"
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("POST /api/clusters", 201, `{"ok":true,"data":{"id":"cl-hosted-001","status":"Creating"}}`)
	h.handle("GET /api/clusters/cl-hosted-001", 200, `{"id":"cl-hosted-001","status":"Running"}`)
	h.handle("GET /api/clusters/cl-hosted-001/kubeconfig", 200, "apiVersion: v1\nkind: Config\n")
	cfg := &Config{APIURL: h.apiURL()}
	mustOK(t, h.ctx.ProvisionHosted(t.Context(), cfg, "test.kubehz.dev", spec), h.output())
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	if readFile(t, kc) != "apiVersion: v1\nkind: Config\n" {
		t.Fatal("kubeconfig not written")
	}
	if info, _ := os.Stat(kc); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if r := h.lastReq("POST", "/api/clusters"); r.Auth != "Bearer test-token" {
		t.Fatalf("create auth: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(h.base, ".kubeconfig", "shortname.yaml")); err == nil {
		t.Fatal("no mirror when metadata.name equals the domain")
	}
}

func TestProvisionHostedMirrorsToMetadataName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "shortname")
	h.handle("POST /api/clusters", 201, `{"data":{"id":"cl-1","status":"Running"}}`)
	h.handle("GET /api/clusters/cl-1", 200, `{"data":{"status":"Running"}}`)
	h.handle("GET /api/clusters/cl-1/kubeconfig", 200, "kc-bytes\n")
	mustOK(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	if readFile(t, filepath.Join(h.base, ".kubeconfig", "shortname.yaml")) != "kc-bytes\n" {
		t.Fatal("metadata.name mirror missing")
	}
}

// The kubeconfig writes (writePrivateFile). The fixtures are the same as
// in kubehz_hosted_test.bats (kubehz::_write_private).

// hostedTemps lists the temp files of writePrivateFile for file in dir
// (the name <file>.<random>).
func hostedTemps(t *testing.T, dir, file string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, file+".*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// hostedKubeconfigRoutes answers the create, the wait and the download
// (code and body of the download).
func hostedKubeconfigRoutes(h *harness, code int, body string) {
	h.handle("POST /api/clusters", 201, `{"data":{"id":"cl-1","status":"Running"}}`)
	h.handle("GET /api/clusters/cl-1", 200, `{"data":{"status":"Running"}}`)
	h.handle("GET /api/clusters/cl-1/kubeconfig", code, body)
}

// A failed download leaves the old kubeconfig as it was, and no temp file.
func TestProvisionHostedFailedDownloadKeepsTheOldKubeconfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	hostedKubeconfigRoutes(h, 500, "partial")
	dir := filepath.Join(h.base, ".kubeconfig")
	kc := filepath.Join(dir, "test.kubehz.dev.yaml")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kc, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "Failed to download kubeconfig for hosted cluster cl-1")
	if got := readFile(t, kc); got != "old\n" {
		t.Fatalf("kubeconfig = %q, want the old one", got)
	}
	if tmp := hostedTemps(t, dir, "test.kubehz.dev.yaml"); len(tmp) != 0 {
		t.Fatalf("temp files left: %q", tmp)
	}
}

// The mirror is never written in place (cp truncated the old file and
// wrote into it). The mirror here is a link to a file that the user can
// write, in a directory without write permission: no temp file can go
// beside it, so the old file stays, with a warning, and the provision
// succeeds.
func TestProvisionHostedMirrorIsNotWrittenInPlace(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root can write into a directory without write permission")
	}
	h := newHarness(t)
	spec := hostedSpec(h, "shortname")
	hostedKubeconfigRoutes(h, 200, "new\n")
	dir := filepath.Join(h.base, ".kubeconfig")
	ro := filepath.Join(h.base, "ro")
	for _, d := range []string{dir, ro} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(ro, "shortname.yaml")
	if err := os.WriteFile(old, []byte("old-mirror\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "ro", "shortname.yaml"), filepath.Join(dir, "shortname.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil { // #nosec G302 -- a directory without write permission is the fixture
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) }) // #nosec G302 -- so that the temp dir can go
	mustOK(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	mustContain(t, h.output(), "could not mirror the kubeconfig to .kubeconfig/shortname.yaml")
	if got := readFile(t, old); got != "old-mirror\n" {
		t.Fatalf("mirror = %q, want the old one", got)
	}
	if tmp := hostedTemps(t, ro, "shortname.yaml"); len(tmp) != 0 {
		t.Fatalf("temp files left: %q", tmp)
	}
	if got := readFile(t, filepath.Join(dir, "test.kubehz.dev.yaml")); got != "new\n" {
		t.Fatalf("kubeconfig = %q", got)
	}
}

// A kubeconfig that is a symlink keeps the link: the new content goes to
// the file that the link names, with mode 0600, and no temp file stays.
func TestProvisionHostedSymlinkedKubeconfigKeepsTheLink(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	hostedKubeconfigRoutes(h, 200, "new\n")
	dir := filepath.Join(h.base, ".kubeconfig")
	realDir := filepath.Join(h.base, "real")
	for _, d := range []string{dir, realDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(realDir, "test.kubehz.dev.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil { // #nosec G306 -- the old mode that the write must not keep
		t.Fatal(err)
	}
	kc := filepath.Join(dir, "test.kubehz.dev.yaml")
	link := filepath.Join("..", "real", "test.kubehz.dev.yaml")
	if err := os.Symlink(link, kc); err != nil {
		t.Fatal(err)
	}
	mustOK(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	if got, err := os.Readlink(kc); err != nil || got != link {
		t.Fatalf("the link is gone: %q, %v", got, err)
	}
	if got := readFile(t, target); got != "new\n" {
		t.Fatalf("target = %q", got)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode: %v, %v", info, err)
	}
	for _, d := range []string{dir, realDir} {
		if tmp := hostedTemps(t, d, "test.kubehz.dev.yaml"); len(tmp) != 0 {
			t.Fatalf("temp files left: %q", tmp)
		}
	}
}

// writePrivateFile refuses a link to nothing (also when the directory of
// its target exists) and a directory, with the line that bash prints, and
// leaves no temp file.
func TestWritePrivateFileRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling.yaml")
	if err := os.Symlink(filepath.Join(dir, "none.yaml"), dangling); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub.yaml")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dest, want string }{
		{dangling, "Cannot resolve the symlink " + dangling},
		{sub, sub + " is a directory, not a file"},
	} {
		err := writePrivateFile(t.Context(), tc.dest, []byte("x"))
		var refused privateFileError
		if !errors.As(err, &refused) || refused.Error() != tc.want {
			t.Fatalf("%s: err = %v, want %q", tc.dest, err, tc.want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %v", entries)
	}
}

// A kubeconfig path that the write refuses stops the provision before the
// create: the refusal creates no cluster (bash: kubehz::_private_target
// before the POST).
func TestProvisionHostedRefusedKubeconfigCreatesNoCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	hostedKubeconfigRoutes(h, 200, "new\n")
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	if err := os.MkdirAll(kc, 0o755); err != nil {
		t.Fatal(err)
	}
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), kc+" is a directory, not a file")
	if h.anyReq("POST", "/api/clusters") {
		t.Fatalf("the create ran: %+v", h.reqs())
	}
}

// A rename that fails after the temp file exists: the old file stays as it
// was, and no temp file is left. In the first case another operation puts
// a directory at the path between the write and the rename (the old file
// goes aside): os.Rename fails on it, nothing lands in it, and the error
// names it. In the second the rename fails for another cause. Not
// parallel: it replaces renameFile.
func TestWritePrivateFileRenameFailureLeavesNoTempFile(t *testing.T) {
	t.Cleanup(func() { renameFile = os.Rename })
	for _, tc := range []struct {
		name   string
		rename func(dir, dest string) func(string, string) error
		want   func(dest string) string
	}{
		{
			name: "a directory takes the place of the file",
			rename: func(dir, dest string) func(string, string) error {
				return func(from, to string) error {
					if err := os.Rename(dest, filepath.Join(dir, "moved-old.yaml")); err != nil {
						return err
					}
					if err := os.Mkdir(dest, 0o700); err != nil {
						return err
					}
					return os.Rename(from, to)
				}
			},
			want: func(dest string) string { return dest + " is a directory, not a file" },
		},
		{
			name: "the rename fails",
			rename: func(_, _ string) func(string, string) error {
				return func(_, _ string) error { return errors.New("injected") }
			},
			want: func(dest string) string { return "Cannot rename the temp file to " + dest },
		},
	} {
		dir := t.TempDir()
		dest := filepath.Join(dir, "kc.yaml")
		if err := os.WriteFile(dest, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		renameFile = tc.rename(dir, dest)
		err := writePrivateFile(t.Context(), dest, []byte("new\n"))
		renameFile = os.Rename
		var refused privateFileError
		if !errors.As(err, &refused) || refused.Error() != tc.want(dest) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want(dest))
		}
		if tmp := hostedTemps(t, dir, "kc.yaml"); len(tmp) != 0 {
			t.Fatalf("%s: temp files left: %q", tc.name, tmp)
		}
		old := dest
		if info, serr := os.Stat(dest); serr == nil && info.IsDir() {
			entries, rerr := os.ReadDir(dest)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(entries) != 0 {
				t.Fatalf("%s: the new file landed in the directory: %v", tc.name, entries)
			}
			old = filepath.Join(dir, "moved-old.yaml")
		}
		if got := readFile(t, old); got != "old\n" {
			t.Fatalf("%s: old file = %q, want the old one", tc.name, got)
		}
	}
}

func TestProvisionHostedNoClusterID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("POST /api/clusters", 200, `{"ok":true,"data":{}}`)
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "did not return a cluster ID")
}

func TestProvisionHostedCapacityEnvelope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	// The shape gate: the envelope the api builds since the metered shape
	// (kubehz-api server/domains/capacity atCapacityError).
	h.handle("POST /api/clusters", 503, `{"ok":false,"data":{"code":"AT_CAPACITY","message":"at capacity","detail":{"replicas":3,"used":40,"limit":40,"retryAfter":3600}}}`)
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	out := h.output()
	mustContain(t, out, "at capacity for a control plane with 3 apiserver replicas right now (40/40 in use, 0 free).")
	mustContain(t, out, "pick a smaller shape: fewer apiserver replicas (spec.controlPlane.replicas, 1 to 3)")
	mustContain(t, out, "check the live capacity per shape")
	mustContain(t, out, "hosting: self")
	mustContain(t, out, h.apiURL()+"/api/capacity")
	mustContain(t, out, "~60 min")
	mustNotContain(t, out, "~3600s")
	mustNotContain(t, out, "spec.kubehz.plan")
	mustNotMentionPlan(t, out)
	mustNotContain(t, out, "tier")

	// A pretty-printed body, and the legacy `tier` envelope of an older api:
	// the tier names the subject, the plan words stay gone.
	h.reset()
	h.handle("POST /api/clusters", 503, "{\n  \"ok\": false,\n  \"data\": {\n    \"code\": \"AT_CAPACITY\",\n    \"detail\": {\n      \"tier\": \"starter\",\n      \"used\": 20,\n      \"limit\": 20,\n      \"retryAfter\": 1800\n    }\n  }\n}")
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "at capacity for the 'starter' shape right now (20/20 in use, 0 free).")
	mustContain(t, h.output(), "~30 min")
	mustNotMentionPlan(t, h.output())
}

// mustNotMentionPlan asserts the plan wording of the frozen bash is gone
// ("control-plane" holds "plan" as a substring, so the phrases are matched).
func mustNotMentionPlan(t *testing.T, out string) {
	t.Helper()
	for _, phrase := range []string{"' plan", "plan '", "this plan", "per-plan", "plan right now", "spec.kubehz.plan"} {
		mustNotContain(t, out, phrase)
	}
}

func TestRenderCapacityRejectionHints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cfg := &Config{APIURL: "https://api.kubehz.dev"}
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"replicas":1,"used":3,"limit":3,"retryAfter":45}}}`))
	mustContain(t, h.output(), "with 1 apiserver replica right now (3/3 in use, 0 free).")
	mustContain(t, h.output(), "~45s")
	mustNotContain(t, h.output(), "min")
	// The shape gate counts replica slots, not a pool.
	mustNotContain(t, h.output(), "pool")

	// No retryAfter: no wait line. A bare (un-enveloped) body reads too.
	h.reset()
	h.ctx.renderCapacityRejection(cfg, []byte(`{"detail":{"replicas":2}}`))
	mustContain(t, h.output(), "at capacity for a control plane with 2 apiserver replicas right now.")
	mustNotContain(t, h.output(), "suggested wait")
	mustNotContain(t, h.output(), "in use")
	mustNotMentionPlan(t, h.output())

	// The legacy tier fallback without counts.
	h.reset()
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"tier":"dev"}}}`))
	mustContain(t, h.output(), "at capacity for the 'dev' shape right now.")
	mustNotContain(t, h.output(), "suggested wait")
	mustNotMentionPlan(t, h.output())

	// An empty detail: the generic subject, the shape hint stays.
	h.reset()
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"code":"AT_CAPACITY"}}`))
	mustContain(t, h.output(), "at capacity for this shape right now.")
	mustContain(t, h.output(), "fewer apiserver replicas")

	// Counts that do not fit an int: print what the api sent, no free
	// count. Atoi clamps and reports the overflow, so the subtraction
	// would otherwise invent a number.
	h.reset()
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"replicas":1,"used":99999999999999999999,"limit":99999999999999999999}}}`))
	mustContain(t, h.output(), "(99999999999999999999/99999999999999999999 in use)")
	mustNotContain(t, h.output(), " free)")

	// A server string reaches a terminal: control characters are stripped
	// and a long one is bounded, the way the rest of the package treats
	// anything the api sends.
	h.reset()
	long := strings.Repeat("z", 400)
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"tier":"de\u001b[2Jv\u0007","used":"`+long+`","limit":"3"}}}`))
	out := h.output()
	mustNotContain(t, out, "\x1b")
	mustNotContain(t, out, "\a")
	mustContain(t, out, "the 'de[2Jv' shape")
	mustContain(t, out, "…")
	if strings.Contains(out, long) {
		t.Fatalf("a 400-rune server value was not clipped:\n%s", out)
	}
}

func TestRenderCapacityRejectionOptionAndPool(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cfg := &Config{APIURL: "https://api.kubehz.dev"}

	// The metal gate: the option is the subject, the hint is to create
	// without it. No smaller-shape hint.
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"option":"dedicated","metalNodes":2,"metalRequired":3,"retryAfter":3600}}}`))
	out := h.output()
	mustContain(t, out, "at capacity for the 'dedicated' option right now (2/3 metal nodes present).")
	mustNotContain(t, out, " free)")
	mustNotContain(t, out, "in use")
	mustContain(t, out, "create the cluster without the 'dedicated' option now")
	mustNotContain(t, out, "fewer apiserver replicas")
	// Only the free-pool gate is about a pool: the metal gate counts nodes.
	mustNotContain(t, out, "pool")
	mustNotMentionPlan(t, out)

	// The free pool: `cap` is the limit, the hint is the payment method.
	h.reset()
	h.ctx.renderCapacityRejection(cfg, []byte(`{"data":{"detail":{"pool":"free","used":25,"cap":25,"retryAfter":3600}}}`))
	out = h.output()
	mustContain(t, out, "at capacity for the free hosted control-plane pool right now (25/25 in use, 0 free).")
	mustContain(t, out, "add a payment method")
	mustNotContain(t, out, "fewer apiserver replicas")
	mustNotMentionPlan(t, out)
}

func TestProvisionHostedSurfacesAPIMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("POST /api/clusters", 502, `{"ok":false,"data":{"code":"HOSTED_BACKEND_ERROR","message":"Failed to schedule the hosted control plane","help":"try later"}}`)
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "HTTP 502")
	mustContain(t, h.output(), "Failed to schedule the hosted control plane")
	mustContain(t, h.output(), "  try later")
}

// hostedCount counts the requests of one method to one exact path.
func hostedCount(h *harness, method, path string) int {
	n := 0
	for _, r := range h.reqs() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// hostedListBody is a list answer with the rows of the list and a total
// that equals their number.
func hostedListBody(rows ...string) string {
	return `{"ok":true,"data":[` + strings.Join(rows, ",") + `],"meta":{"pagination":{"total":` + strconv.Itoa(len(rows)) + `}}}`
}

// The other row of a list: a cluster of another domain, which an api that
// ignores ?domain= sends too.
const hostedOtherRow = `{"id":"cl-other","domain":"other.example.com","hosting":"hosted","status":"Running","createdAt":"2026-01-01T00:00:00Z"}`

// One cluster for one domain: one cluster of the domain that lo does not
// use stops the create. lo names its id, its hosting and its status, and
// why it stops. The create, the wait and the download are answered:
// without the stop, the provision would succeed.
func TestProvisionHostedStopsOnAClusterOfTheDomain(t *testing.T) {
	t.Parallel()
	const confirmLines = "  kubehz is still checking the create of this cluster.\n" +
		"  Usually, kubehz keeps or removes the cluster in about 3 minutes.\n" +
		"  When the check fails, kubehz tries again about 15 minutes later.\n" +
		"  Run the command again after that.\n"
	const (
		getLine  = "  See the cluster with:  lo kubehz cluster get cl-1a\n"
		anyStep  = "  lo downloads the kubeconfig only when the status is Running, Paused or Updating.\n"
		creating = "  The cluster is not ready yet. Run the command again when its status is Running.\n"
		failed   = "  kubehz reports an error for the cluster. Contact support with the cluster id.\n"
		unread   = "  lo could not read the status of the cluster. Run the command again.\n"
	)
	stop := func(state string) string {
		return "[error] kubehz: your account already has cluster cl-1a for test.kubehz.dev (" + state + "), so lo did not create another one.\n"
	}
	live := func(status string) string { return `{"ok":true,"data":{"id":"cl-1a","status":"` + status + `"}}` }
	const creatingRow = `{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":null}`
	for name, tc := range map[string]struct {
		row    string
		status int    // the answer of GET /api/clusters/cl-1a; 0: no read expected
		live   string // its body
		want   string
	}{
		// The list keeps the status of the create: lo reads the live one.
		"a hosted cluster that kubehz deletes": {
			creatingRow, 200, live("Deleting"),
			stop("hosted, Deleting") +
				"  kubehz deletes the cluster. After it leaves your cluster list, run the command again: lo then creates a new cluster.\n" +
				getLine,
		},
		// The live status decides, not the status of the list.
		"the list says Running, the live status is Creating": {
			`{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Running","createUnconfirmedAt":null}`,
			200, live("Creating"),
			stop("hosted, Creating") + creating + getLine,
		},
		"a cluster that waits for capacity": {creatingRow, 200, live("Pending-Capacity"), stop("hosted, Pending-Capacity") + creating + getLine},
		"a failed cluster":                  {creatingRow, 200, live("Failed"), stop("hosted, Failed") + failed + getLine},
		"a cluster with an error":           {creatingRow, 200, live("Error"), stop("hosted, Error") + failed + getLine},
		"a status that lo has no step for":  {creatingRow, 200, live("Starting"), stop("hosted, Starting") + anyStep + getLine},
		"a hosted cluster whose create is not confirmed": {
			`{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":"2026-11-26T10:00:00Z"}`,
			200, live("Running"),
			stop("hosted, Running, create not confirmed") + confirmLines + getLine,
		},
		"a create that is not confirmed and a status read that fails": {
			`{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":"2026-11-26T10:00:00Z"}`,
			500, `{"ok":false}`,
			stop("hosted, status not read, create not confirmed") + confirmLines + getLine,
		},
		"a self-hosted cluster": {
			`{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"self","status":"Running"}`,
			0, "",
			"[error] kubehz: your account already has cluster cl-1a for test.kubehz.dev (self-hosted, Running), so lo did not create another one.\n" +
				"  It is not a hosted cluster, so lo does not download its kubeconfig.\n" +
				"  See the cluster with:  lo kubehz cluster get cl-1a\n",
		},
		"a cluster without a hosting": {
			`{"id":"cl-1a","domain":"test.kubehz.dev","status":"Running"}`,
			0, "",
			"[error] kubehz: your account already has cluster cl-1a for test.kubehz.dev (unknown, Running), so lo did not create another one.\n" +
				"  It is not a hosted cluster, so lo does not download its kubeconfig.\n" +
				"  See the cluster with:  lo kubehz cluster get cl-1a\n",
		},
		// lo cannot read the status: the api refuses the read, or the
		// answer holds no status or is not JSON.
		"the status read fails":       {creatingRow, 500, `{"ok":false}`, stop("hosted, status not read") + unread + getLine},
		"an answer without a status":  {creatingRow, 200, `{"ok":true,"data":{"id":"cl-1a"}}`, stop("hosted, status not read") + unread + getLine},
		"an answer that is not JSON":  {creatingRow, 200, `<html>proxy</html>`, stop("hosted, status not read") + unread + getLine},
		"a status of an empty string": {creatingRow, 200, live(""), stop("hosted, unknown") + anyStep + getLine},
		// A status that is not one word is a server string that lo does
		// not repeat.
		"a status lo does not show": {
			creatingRow, 200, `{"ok":true,"data":{"status":"Running\u001b[2J"}}`,
			stop("hosted, unknown") + anyStep + getLine,
		},
		// An id in another shape is a server string that lo does not repeat,
		// and lo reads no status for it.
		"an id lo does not show": {
			`{"id":"cl-\u001b[2J","domain":"test.kubehz.dev","hosting":"hosted","status":"Running"}`,
			0, "",
			"[error] kubehz: your account already has a cluster for test.kubehz.dev (hosted, Running), so lo did not create another one.\n" +
				"  See your clusters with:  lo kubehz cluster list\n",
		},
		"no id, no hosting and no status": {
			`{"domain":"test.kubehz.dev"}`,
			0, "",
			"[error] kubehz: your account already has a cluster for test.kubehz.dev (unknown, unknown), so lo did not create another one.\n" +
				"  See your clusters with:  lo kubehz cluster list\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := hostedSpec(h, "test.kubehz.dev")
			hostedKubeconfigRoutes(h, 200, "new\n")
			h.handle("GET /api/clusters", 200, hostedListBody(hostedOtherRow, tc.row))
			if tc.status != 0 {
				h.handle("GET /api/clusters/cl-1a", tc.status, tc.live)
			}
			mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
			if got := h.output(); got != tc.want {
				t.Fatalf("output:\n got %q\nwant %q", got, tc.want)
			}
			if h.anyReq("POST", "/api/clusters") {
				t.Fatal("a create was sent")
			}
			want := 0
			if tc.status != 0 {
				want = 1
			}
			if n := hostedCount(h, "GET", "/api/clusters/cl-1a"); n != want {
				t.Fatalf("the status was read %d times, want %d", n, want)
			}
			if h.anyReq("GET", "/kubeconfig") {
				t.Fatal("a kubeconfig was downloaded")
			}
			r := h.lastReq("GET", "/api/clusters")
			if r == nil || r.Query != "perPage=500&domain=test.kubehz.dev" || r.Auth != "Bearer khz_test_token" {
				t.Fatalf("the list request: %+v", r)
			}
			if _, err := os.Stat(filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")); !os.IsNotExist(err) {
				t.Fatalf("a kubeconfig was written (stat: %v)", err)
			}
		})
	}
}

// More than one cluster of the domain stops the create: lo cannot tell
// which one to use. It names them, the oldest first, at most ten, and
// reads no status.
func TestProvisionHostedStopsOnMoreThanOneCluster(t *testing.T) {
	t.Parallel()
	row := func(id, hosting, at string) string {
		return `{"id":"` + id + `","domain":"test.kubehz.dev","hosting":"` + hosting + `","status":"Running","createdAt":"` + at + `"}`
	}
	head := func(n int) string {
		return "[error] kubehz: your account has " + strconv.Itoa(n) + " clusters for test.kubehz.dev, so lo did not create another one.\n" +
			"  lo downloads a kubeconfig only when the account has one cluster for the domain.\n"
	}
	const foot = "  See your clusters with:  lo kubehz cluster list\n"
	var twelve []string
	for i := range 12 {
		twelve = append(twelve, row("cl-"+strconv.Itoa(10+i), "hosted", "2026-02-"+strconv.Itoa(10+i)+"T00:00:00Z"))
	}
	var twelveWant strings.Builder
	twelveWant.WriteString(head(12))
	for i := range 10 {
		twelveWant.WriteString("  - cluster cl-" + strconv.Itoa(10+i) + " (hosted)\n")
	}
	twelveWant.WriteString("  - and 2 more\n" + foot)
	for name, tc := range map[string]struct {
		rows []string
		want string
	}{
		"three clusters": {
			[]string{
				hostedOtherRow,
				row("cl-new", "hosted", "2026-03-01T00:00:00Z"),
				`{"id":"../x","domain":"test.kubehz.dev","hosting":"x\u001b[2J","createdAt":"2026-02-15T00:00:00Z"}`,
				row("cl-old", "self", "2026-02-01T00:00:00Z"),
			},
			head(3) +
				"  - cluster cl-old (self-hosted)\n" +
				"  - a cluster (unknown)\n" +
				"  - cluster cl-new (hosted)\n" + foot,
		},
		"twelve clusters": {twelve, twelveWant.String()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := hostedSpec(h, "test.kubehz.dev")
			hostedKubeconfigRoutes(h, 200, "new\n")
			h.handle("GET /api/clusters", 200, hostedListBody(tc.rows...))
			mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
			if got := h.output(); got != tc.want {
				t.Fatalf("output:\n got %q\nwant %q", got, tc.want)
			}
			if h.anyReq("POST", "/api/clusters") || h.anyReq("GET", "/api/clusters/") {
				t.Fatalf("a create, a status read or a download was sent: %+v", h.reqs())
			}
		})
	}
}

// One hosted cluster of the domain with a confirmed create and a live
// status of Running, Paused or Updating: lo creates no cluster. It
// downloads the kubeconfig and writes the mirror, as after a create, and
// replaces the old kubeconfig. The list keeps the status of the create
// ("Creating"): lo reads the live one once. For Paused and Updating, a
// note follows the download.
func TestProvisionHostedUsesTheClusterOfTheDomain(t *testing.T) {
	t.Parallel()
	for status, note := range map[string]string{
		"Running": "",
		"Paused": "  The cluster is Paused. The download asks kubehz to resume it.\n" +
			"  A suspended account, an ended lease or an ended contract keeps it paused. See it with:  lo kubehz cluster get cl-1a\n",
		"Updating": "  The cluster is Updating: kubectl can fail for a short time, until the update ends.\n",
	} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := hostedSpec(h, "mirror-name")
			h.handle("GET /api/clusters", 200, hostedListBody(hostedOtherRow,
				`{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Creating","createUnconfirmedAt":null}`))
			h.handle("GET /api/clusters/cl-1a", 200, `{"ok":true,"data":{"id":"cl-1a","status":"`+status+`"}}`)
			h.handle("GET /api/clusters/cl-1a/kubeconfig", 200, "apiVersion: v1\nkind: Config\n")
			h.handle("POST /api/clusters", 201, `{"data":{"id":"cl-2b"}}`)
			dir := filepath.Join(h.base, ".kubeconfig")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			kc := filepath.Join(dir, "test.kubehz.dev.yaml")
			if err := os.WriteFile(kc, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			mustOK(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
			want := "kubehz: your account already has cluster cl-1a for test.kubehz.dev (hosted, " + status + ").\n" +
				"  lo downloads its kubeconfig and does not create another cluster.\n" + note
			if got := h.output(); got != want {
				t.Fatalf("output:\n got %q\nwant %q", got, want)
			}
			if got := h.out.String(); got != want {
				t.Fatalf("stdout:\n got %q\nwant %q", got, want)
			}
			if h.anyReq("POST", "/api/clusters") {
				t.Fatal("a create was sent")
			}
			if n := hostedCount(h, "GET", "/api/clusters/cl-1a"); n != 1 {
				t.Fatalf("the status was read %d times, want 1", n)
			}
			for _, f := range []string{kc, filepath.Join(dir, "mirror-name.yaml")} {
				if got := readFile(t, f); got != "apiVersion: v1\nkind: Config\n" {
					t.Fatalf("%s: %q", f, got)
				}
			}
			if r := h.lastReq("GET", "/api/clusters/cl-1a/kubeconfig"); r == nil || r.Auth != "Bearer khz_test_token" {
				t.Fatalf("the download: %+v", r)
			}
		})
	}
}

// A refused download names the cause and, for a code that lo knows, the
// next step. The old kubeconfig stays.
func TestHostedKubeconfigNamesTheCause(t *testing.T) {
	t.Parallel()
	const head = "[error] Failed to download kubeconfig for hosted cluster cl-1 "
	for _, tc := range []struct {
		name, body, want string
		code             int
	}{
		{"an api that refuses each token", `{"ok":false,"data":{"code":"TOKEN_NOT_ALLOWED","message":"no"}}`, head + "(HTTP 403 TOKEN_NOT_ALLOWED)\n" +
			"  This api gives the kubeconfig only to a dashboard sign-in, not to an API token. Download it in the dashboard.\n" +
			"  The cluster exists: do not create it again.\n", 403},
		{"a token without the scope", `{"code":"TOKEN_SCOPE_MISSING"}`, head + "(HTTP 403 TOKEN_SCOPE_MISSING)\n" +
			"  KUBEHZ_TOKEN needs the read scope and the clusters:write scope. Mint a token with both in the dashboard (Access -> API Tokens).\n", 403},
		{"not ready", `{"ok":false,"data":{"code":"KUBECONFIG_NOT_READY"}}`, head + "(HTTP 409 KUBECONFIG_NOT_READY)\n" +
			"  Run the command again when the status is Running.\n", 409},
		{"a code of another shape", `{"ok":false,"data":{"code":"no\u001b[2Jcode"}}`, head + "(HTTP 500)\n", 500},
		{"another code", `{"ok":false,"data":{"code":"KUBECONFIG_BACKEND_UNAVAILABLE"}}`, head + "(HTTP 503 KUBECONFIG_BACKEND_UNAVAILABLE)\n", 503},
		{"not JSON", `<html>`, head + "(HTTP 404)\n", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := hostedSpec(h, "test.kubehz.dev")
			h.handle("GET /api/clusters/cl-1/kubeconfig", tc.code, tc.body)
			kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
			_ = os.MkdirAll(filepath.Dir(kc), 0o700)
			_ = os.WriteFile(kc, []byte("old\n"), 0o600)
			mustErr(t, h.ctx.hostedKubeconfig(t.Context(), h.apiURL(), "cl-1", "test.kubehz.dev", kc, spec))
			if h.output() != tc.want {
				t.Fatalf("output:\n%s\nwant:\n%s", h.output(), tc.want)
			}
			if got := readFile(t, kc); got != "old\n" {
				t.Fatalf("kubeconfig = %q, want the old one", got)
			}
		})
	}
	// No answer: the connection fails.
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	mustErr(t, h.ctx.hostedKubeconfig(t.Context(), "https://127.0.0.1:1", "cl-1", "test.kubehz.dev", kc, spec))
	if want := head + "(no answer)\n"; h.output() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", h.output(), want)
	}
}

// A download that fails after lo found the cluster fails the run and
// keeps the old kubeconfig, as after a create. The note of a Paused
// cluster follows a download only.
func TestProvisionHostedUsedClusterFailedDownload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("GET /api/clusters", 200, hostedListBody(`{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted"}`))
	h.handle("GET /api/clusters/cl-1a", 200, `{"ok":true,"data":{"status":"Paused"}}`)
	h.handle("GET /api/clusters/cl-1a/kubeconfig", 403, `{"ok":false,"data":{"code":"TOKEN_NOT_ALLOWED"}}`)
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "[error] Failed to download kubeconfig for hosted cluster cl-1a (HTTP 403 TOKEN_NOT_ALLOWED)\n")
	if strings.Contains(h.output(), "The cluster is Paused") {
		t.Fatalf("the note of a Paused cluster after a failed download:\n%s", h.output())
	}
	if h.anyReq("POST", "/api/clusters") {
		t.Fatal("a create was sent")
	}
	if _, err := os.Stat(filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a kubeconfig was written (stat: %v)", err)
	}
}

// lo sends ?domain= only for a domain of the shape that the api takes;
// such a value needs no escape. Another domain gets the list without the
// filter, and lo still compares the domain of each row.
func TestHostedListedDomainFilter(t *testing.T) {
	t.Parallel()
	label63 := strings.Repeat("a", 63)
	long253 := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("b", 61)
	for domain, want := range map[string]string{
		"test.kubehz.dev":          "perPage=500&domain=test.kubehz.dev",
		"a-1.b2.example":           "perPage=500&domain=a-1.b2.example",
		long253:                    "perPage=500&domain=" + long253,
		long253 + "b":              "perPage=500",
		"Test.kubehz.dev":          "perPage=500",
		"localhost":                "perPage=500",
		"-a.example.com":           "perPage=500",
		"a-.example.com":           "perPage=500",
		"a..example.com":           "perPage=500",
		"a.example.com.":           "perPage=500",
		"a&perPage=1.example.com":  "perPage=500",
		"a.example.com\nb.example": "perPage=500",
		label63 + "a.example.com":  "perPage=500",
		"null":                     "perPage=500",
		"":                         "perPage=500",
	} {
		h := newHarness(t)
		h.handle("GET /api/clusters", 200, hostedListBody(hostedOtherRow))
		l := h.ctx.hostedListed(t.Context(), h.apiURL(), domain)
		if l.cause != "" || len(l.rows) != 0 {
			t.Fatalf("%q: %+v", domain, l)
		}
		if r := h.lastReq("GET", "/api/clusters"); r == nil || r.Query != want {
			t.Fatalf("%q: the list request %+v, want the query %q", domain, r, want)
		}
	}
}

// The domain of the create is spec.cluster.domain: a cluster of the lo
// domain with another spec.cluster.domain does not stop the create, and a
// full list without a cluster of the domain does not either.
func TestProvisionHostedCreatesWhenTheListShowsNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := h.writeSpec("test.kubehz.dev", "kind: KubeOne\nmetadata:\n  name: test.kubehz.dev\nspec:\n  cluster:\n    domain: api.test.kubehz.dev\n  kubernetes:\n    version: v1.31.10\n  kubehz:\n    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev"},{"id":"cl-2b","domain":"other.example.com"}],"meta":{"pagination":{"total":2}}}`)
	hostedKubeconfigRoutes(h, 200, "new\n")
	mustOK(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	if !h.anyReq("POST", "/api/clusters") {
		t.Fatal("no create was sent")
	}
	if r := h.lastReq("POST", "/api/clusters"); !strings.Contains(r.Body, `"domain":"api.test.kubehz.dev"`) {
		t.Fatalf("the create body: %s", r.Body)
	}
}

// hostedNoAnswer ends the connection of a request without an answer: the
// request reached the server, the client gets no status.
func hostedNoAnswer(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}
}

// A list that lo cannot read stops the create: lo cannot tell whether the
// account has a cluster of the domain.
func TestProvisionHostedStopsWhenTheListCannotBeRead(t *testing.T) {
	t.Parallel()
	const again = "  Run the command again when lo can read the list.\n"
	line := func(cause string) string {
		return "[error] kubehz: lo cannot read the cluster list (" + cause + "), so it cannot check for a cluster of test.kubehz.dev. No cluster was created.\n"
	}
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"a refusal": {500, `{"ok":false,"data":{"code":"INTERNAL_ERROR","message":"boom"}}`, line("HTTP 500 INTERNAL_ERROR") + again},
		"a missing scope": {403, `{"ok":false,"data":{"code":"TOKEN_SCOPE_MISSING","message":"This endpoint requires the 'read' scope"}}`,
			line("HTTP 403 TOKEN_SCOPE_MISSING") + "  KUBEHZ_TOKEN needs the read scope and the clusters:write scope. Mint a token with both in the dashboard (Access -> API Tokens).\n"},
		"a code lo does not repeat": {502, `{"data":{"code":"x\u001b[2J"}}`, line("HTTP 502") + again},
		"no list":                   {200, `{"ok":true,"data":{"id":"cl-1"}}`, line("an answer without a list") + again},
		"a null list":               {200, `{"ok":true,"data":null}`, line("an answer without a list") + again},
		"not JSON":                  {200, `<html>proxy</html>`, line("an answer without a list") + again},
		"a part of the clusters": {200, `{"ok":true,"data":[{"id":"cl-2b","domain":"other.example.com"}],"meta":{"pagination":{"total":612}}}`,
			line("it shows 1 of 612 clusters") + again},
		// lo cannot tell that the cluster on the page is the only one.
		"a part of the clusters with a cluster of the domain": {200,
			`{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev","hosting":"hosted","status":"Running"}],"meta":{"pagination":{"total":2}}}`,
			line("it shows 1 of 2 clusters") + again},
		"no answer": {0, "", line("no answer") + again},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := hostedSpec(h, "test.kubehz.dev")
			hostedKubeconfigRoutes(h, 200, "new\n")
			if tc.status == 0 {
				h.handleFunc("GET /api/clusters", hostedNoAnswer(t))
			} else {
				h.handle("GET /api/clusters", tc.status, tc.body)
			}
			mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
			if got := h.output(); got != tc.want {
				t.Fatalf("output:\n got %q\nwant %q", got, tc.want)
			}
			if h.anyReq("POST", "/api/clusters") {
				t.Fatal("a create was sent")
			}
		})
	}
}

// A create that reached the api and got no answer, or got the answer of a
// gateway in front of the api, can have made the cluster. lo reads the
// list again and names what it shows before it reports anything.
func TestProvisionHostedLostAnswerReadsTheListAgain(t *testing.T) {
	t.Parallel()
	const tail = "  If kubehz is still checking the create, it usually keeps or removes the cluster in about 3 minutes.\n" +
		"  When the check fails, kubehz tries again about 15 minutes later.\n" +
		"  Do not run lo provision again before kubehz keeps or removes the cluster.\n" +
		"  Then run it again. lo downloads the kubeconfig of a kept cluster, or creates a new one.\n"
	head := func(cause string) string {
		return "[error] kubehz: the result of the create for test.kubehz.dev is unknown (" + cause + "). Possibly, kubehz made the cluster.\n"
	}
	const row = `{"ok":true,"data":[{"id":"cl-1a2b3c4d","domain":"test.kubehz.dev","createdAt":"2026-02-01T00:00:00Z"}]}`
	type answer struct {
		status int
		body   string
	}
	for name, tc := range map[string]struct {
		create answer // status 0: no answer
		second answer // the second list read
		want   string
	}{
		"no answer, the list shows the cluster": {answer{}, answer{200, row},
			head("no answer") + "  The cluster list shows cluster cl-1a2b3c4d for test.kubehz.dev:  lo kubehz cluster get cl-1a2b3c4d\n" + tail},
		"no answer, the list shows an id lo does not show": {answer{}, answer{200, `{"ok":true,"data":[{"id":"../x","domain":"test.kubehz.dev"}]}`},
			head("no answer") + "  The cluster list shows a cluster for test.kubehz.dev:  lo kubehz cluster list\n" + tail},
		"no answer, the list shows two clusters": {answer{}, answer{200, `{"ok":true,"data":[{"id":"cl-1a","domain":"test.kubehz.dev"},{"id":"cl-2b","domain":"test.kubehz.dev"}]}`},
			head("no answer") + "  The cluster list shows 2 clusters for test.kubehz.dev:  lo kubehz cluster list\n" + tail},
		"no answer, the list shows none": {answer{}, answer{200, `{"ok":true,"data":[]}`},
			head("no answer") + "  The cluster list shows no cluster for test.kubehz.dev now.\n" + tail},
		"no answer, the list fails": {answer{}, answer{500, `{"ok":false}`},
			head("no answer") + "  lo cannot read the cluster list either (HTTP 500).\n" + tail},
		"a gateway 504": {answer{504, `upstream request timeout`}, answer{200, row},
			head("HTTP 504") + "  The cluster list shows cluster cl-1a2b3c4d for test.kubehz.dev:  lo kubehz cluster get cl-1a2b3c4d\n" + tail},
		"a gateway 502 with a JSON body and no code": {answer{502, `{"ok":false}`}, answer{200, `{"ok":true,"data":[]}`},
			head("HTTP 502") + "  The cluster list shows no cluster for test.kubehz.dev now.\n" + tail},
		"a gateway 503": {answer{503, `no healthy upstream`}, answer{200, `{"ok":true,"data":[]}`},
			head("HTTP 503") + "  The cluster list shows no cluster for test.kubehz.dev now.\n" + tail},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			spec := hostedSpec(h, "test.kubehz.dev")
			var lists atomic.Int32
			h.handleFunc("GET /api/clusters", func(w http.ResponseWriter, _ *http.Request) {
				if lists.Add(1) == 1 {
					_, _ = io.WriteString(w, `{"ok":true,"data":[]}`)
					return
				}
				w.WriteHeader(tc.second.status)
				_, _ = io.WriteString(w, tc.second.body)
			})
			if tc.create.status == 0 {
				h.handleFunc("POST /api/clusters", hostedNoAnswer(t))
			} else {
				h.handle("POST /api/clusters", tc.create.status, tc.create.body)
			}
			mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
			if got := h.output(); got != tc.want {
				t.Fatalf("output:\n got %q\nwant %q", got, tc.want)
			}
			if n := lists.Load(); n != 2 {
				t.Fatalf("the list was read %d times, want 2", n)
			}
			if h.anyReq("GET", "/api/clusters/") {
				t.Fatalf("lo waited for a cluster: %+v", h.reqs())
			}
		})
	}
}

// A create that did not leave this machine made no cluster: lo says so,
// and does not read the list again. The list answer closes the connection
// and the server stops listening, so the create cannot connect.
func TestProvisionHostedCreateNotSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	var lists atomic.Int32
	h.handleFunc("GET /api/clusters", func(w http.ResponseWriter, _ *http.Request) {
		lists.Add(1)
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, `{"ok":true,"data":[]}`)
		_ = h.srv.Listener.Close()
	})
	mustErr(t, h.ctx.ProvisionHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	want := "[error] Failed to create hosted cluster via kubehz API (network error)\n" +
		"  The request did not reach the api, so no cluster was created.\n"
	if got := h.output(); got != want {
		t.Fatalf("output:\n got %q\nwant %q", got, want)
	}
	if h.anyReq("POST", "/api/clusters") {
		t.Fatal("the create reached the server")
	}
	if n := lists.Load(); n != 1 {
		t.Fatalf("the list was read %d times, want 1", n)
	}
}

func TestDestroyHostedResolvesTheClusterAndDeletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[
  {"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
]}`)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true,"data":{"deleted":true,"id":"cl-old"}}`)
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	_ = os.MkdirAll(filepath.Dir(kc), 0o755)
	_ = os.WriteFile(kc, []byte("kc"), 0o600)
	mustOK(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	if _, err := os.Stat(kc); err == nil {
		t.Fatal("kubeconfig not cleaned up")
	}
	if !h.anyReq("DELETE", "/api/clusters/cl-old") || h.anyReq("DELETE", "/api/clusters/cl-other") {
		t.Fatalf("deletes: %+v", h.reqs())
	}
	if r := h.lastReq("GET", "/api/clusters"); r == nil || r.Query != "perPage=500&domain=test.kubehz.dev" {
		t.Fatalf("list: %+v", r)
	}
}

// More than one cluster for the domain: lo deletes none of them and keeps
// the local kubeconfigs.
func TestDestroyHostedSeveralClustersDeletesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
]}`)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
	h.handle("DELETE /api/clusters/cl-new", 200, `{"ok":true}`)
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	_ = os.MkdirAll(filepath.Dir(kc), 0o755)
	_ = os.WriteFile(kc, []byte("kc"), 0o600)
	mustErr(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "[error] kubehz: your account has 2 clusters for test.kubehz.dev, so lo deleted none of them.\n"+
		"  lo deletes a cluster only when the account has one cluster for the domain. The local kubeconfigs stay.\n"+
		"  - cluster cl-old\n"+
		"  - cluster cl-new\n"+
		"  Select the cluster in the dashboard and delete it there.\n")
	mustNotContain(t, h.output(), "cannot read the cluster registry")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run with more than one cluster for the domain")
	}
	if _, err := os.Stat(kc); err != nil {
		t.Fatal("the local kubeconfig must stay")
	}
}

func TestDestroyHostedRefusesOnLookupFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("GET /api/clusters", 500, `{}`)
	kc := filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml")
	_ = os.MkdirAll(filepath.Dir(kc), 0o755)
	_ = os.WriteFile(kc, []byte("kc"), 0o600)
	mustErr(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "nothing was deleted")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run when the lookup failed")
	}
	if _, err := os.Stat(kc); err != nil {
		t.Fatal("kubeconfig must survive the refusal")
	}
}

func TestDestroyHostedNoRowIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","createdAt":"2026-03-01T00:00:00Z"}]}`)
	mustOK(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	mustContain(t, h.output(), "nothing to destroy")
}

func TestDestroyHostedRefusedDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "x")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-1","domain":"test.kubehz.dev","createdAt":"2026-01-01T00:00:00Z"}]}`)
	h.handle("DELETE /api/clusters/cl-1", 403, `{"ok":false,"data":{"message":"forbidden"}}`)
	mustErr(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec))
	mustContain(t, h.output(), "HTTP 403: forbidden")
}

func TestDestroyHostedRemovesMirror(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "shortname")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-1","domain":"test.kubehz.dev","createdAt":"2026-01-01T00:00:00Z"}]}`)
	h.handle("DELETE /api/clusters/cl-1", 200, `{"ok":true}`)
	dir := filepath.Join(h.base, ".kubeconfig")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "test.kubehz.dev.yaml"), []byte("kc"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "shortname.yaml"), []byte("kc"), 0o600)
	mustOK(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
	for _, f := range []string{"test.kubehz.dev.yaml", "shortname.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s leaked", f)
		}
	}
}

func TestDestroyHostedRefusesPlainHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustErr(t, h.ctx.DestroyHosted(t.Context(), &Config{APIURL: "http://api"}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "must use HTTPS")
}
