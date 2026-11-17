package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// hcloudStub is the hcloud of the test: the first one on the children's
// PATH (.bin), so no real hcloud runs. It logs each call to calls.log and
// answers the server list with servers.json, both in the directory that
// replaces @DIR@. The other lists are empty, a delete does nothing, and
// any other call fails.
const hcloudStub = `#!/usr/bin/env bash
set -euo pipefail
dir='@DIR@'
echo "$*" >> "${dir}/calls.log"
case "${1:-} ${2:-}" in
  "server list") cat "${dir}/servers.json" ;;
  *" list") echo '[]' ;;
  *" delete") ;;
  *) echo "hcloud stub: hcloud $* is not expected" >&2; exit 1 ;;
esac
`

// requireTool skips the test when a tool is not on PATH, except under CI,
// where the toolchain must be there.
func requireTool(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath(path); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is missing under CI: %v", path, err)
		}
		t.Skipf("no %s (b install)", path)
	}
}

// The Go CLI runs the hetzner destroy through the bridge: the real bash
// provider of the embedded tree, with a stub hcloud that does not apply
// the -l selector. The plan names server 46 with the cluster label of this
// cluster. At the destroy, the list gives 46 with another label. The
// destroy does not delete 46, and the last lines name it with the reason,
// before the line that counts and names the kept parts (47 has no owner
// mark), which stays the last line. These lines go to stderr, not to stdout.
// hack/parity-orchestrate.sh runs the same case in both CLIs.
func TestHetznerDestroyNamesAPartOfThePlanThatTheListGivesWithAnotherClusterLabel(t *testing.T) {
	root := testutil.RepoRoot(t)
	yq := filepath.Join(root, ".bin", "yq")
	for _, tool := range []string{"bash", "jq", "sha256sum", yq} {
		requireTool(t, tool)
	}
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	stubDir := filepath.Join(base, "hcloud-stub")
	if strings.ContainsRune(stubDir, '\'') {
		t.Skip("the temporary directory has a single quote")
	}
	argsh, err := os.ReadFile(filepath.Join(root, ".bin", "argsh"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(p.Bin, "argsh"), string(argsh))
	testutil.WriteFile(t, filepath.Join(p.Bin, "hcloud"), strings.ReplaceAll(hcloudStub, "@DIR@", stubDir))
	if err := os.Chmod(filepath.Join(p.Bin, "hcloud"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(yq, filepath.Join(p.Bin, "yq")); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(p.Clusters, "prod.cloud", "hetzner.json")
	work := filepath.Join(p.Clusters, "prod.cloud", ".kubeone")
	testutil.WriteFile(t, cfg, `{"cluster_name": "prod-a"}`+"\n")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	// No checkout in the project: the children source the embedded tree.
	t.Setenv(assets.EnvCacheHome, t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("PROVIDER_DOMAIN", "prod.cloud")
	t.Setenv("LOK8S_DESTROY_PLAN", "")
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("HCLOUD_CONFIG", filepath.Join(base, "no-hcloud-config.toml"))

	sum := sha256.Sum256([]byte("prod.cloud\nprod-a\n"))
	mark := hex.EncodeToString(sum[:])[:32]
	servers := func(label46 string) {
		testutil.WriteFile(t, filepath.Join(stubDir, "servers.json"),
			`[{"id":42,"name":"cp-0","labels":{"lok8s.dev/cluster":"prod-a","lok8s.dev/owner":"`+mark+`"}},`+
				`{"id":46,"name":"moved-0","labels":{"lok8s.dev/cluster":"`+label46+`","lok8s.dev/owner":"`+mark+`"}},`+
				`{"id":47,"name":"old-0","labels":{"lok8s.dev/cluster":"prod-a"}}]`+"\n")
		testutil.WriteFile(t, filepath.Join(stubDir, "calls.log"), "")
	}

	var outBuf, errBuf strings.Builder
	l := &Loader{Paths: p, Stdout: &outBuf, Stderr: &errBuf}
	loaded, err := l.Load(t.Context(), "hetzner")
	if err != nil {
		t.Fatalf("load: %v\n%s", err, errBuf.String())
	}
	prov := loaded.(*Provider)

	servers("prod-a")
	plan, ok, err := prov.DestroyPlan(t.Context(), cfg, work)
	if err != nil || !ok {
		t.Fatalf("destroy plan: ok = %v, err = %v\n%s", ok, err, errBuf.String())
	}
	if want := "cluster\tprod-a\nserver\t42\tcp-0\nserver\t46\tmoved-0\n"; plan != want {
		t.Fatalf("plan = %q, want %q", plan, want)
	}

	servers("prod-b")
	t.Setenv("LOK8S_DESTROY_PLAN", strings.TrimSuffix(plan, "\n"))
	outBuf.Reset()
	errBuf.Reset()
	if err := prov.Destroy(t.Context(), cfg, work); err != nil {
		t.Fatalf("destroy: %v\n%s", err, errBuf.String())
	}
	out := errBuf.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	want := []string{
		"[warn] lo destroy: skipped server 46 moved-0: the plan named it, and now it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it.",
		"[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod-a that does not have the owner mark of this cluster (1 server): server 47 old-0. Delete them by hand only if they are yours and you want them gone.",
	}
	if len(lines) < 2 || lines[len(lines)-2] != want[0] || lines[len(lines)-1] != want[1] {
		t.Errorf("the last lines of stderr are not the skipped part and then the kept line:\n%s", out)
	}
	if strings.Contains(outBuf.String(), "lo destroy:") {
		t.Errorf("the skipped or the kept line is on stdout, want stderr only:\n%s", outBuf.String())
	}
	// Two lines name part 46: the warning that the list gave it (the plan
	// named 46, so it showed no warning for it) and the skipped line. Not
	// the line of a plan row that is not live.
	if n := strings.Count(out, "server 46"); n != 2 {
		t.Errorf("server 46 is named %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, "[warn] hetzner: skipped server 46 moved-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b."); n != 1 {
		t.Errorf("the warning that the list gave server 46 is printed %d times, want 1:\n%s", n, out)
	}
	calls, err := os.ReadFile(filepath.Join(stubDir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	var deletes []string
	for c := range strings.SplitSeq(strings.TrimSuffix(string(calls), "\n"), "\n") {
		if strings.Contains(c, " delete ") {
			deletes = append(deletes, c)
		}
	}
	if strings.Join(deletes, ";") != "server delete 42" {
		t.Errorf("deletes = %q, want only server delete 42", deletes)
	}
}
