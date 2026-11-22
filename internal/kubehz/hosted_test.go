package kubehz

// hosted_test.go ports tests/unit/kubehz_hosted_test.bats (the library
// half; the driver-branch cases live in the kubeone/capi packages' own
// hook tests and in hooks_test.go here).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
}

func hostedSpec(h *harness, name string) string {
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

func TestDestroyHostedResolvesOldestAndDeletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := hostedSpec(h, "test.kubehz.dev")
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
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
	if h.anyReq("DELETE", "/api/clusters/cl-new") {
		t.Fatal("deleted the NEWEST cluster")
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
