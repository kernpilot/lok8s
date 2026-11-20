package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// The rook-ceph-csi-kms check of `lo doctor` (cmd_doctor_csikms.go). The
// cases are shared with the bats twin (tests/unit/doctor_csi_kms_test.bats)
// under tests/fixtures/doctor-csi-kms/cases/<case>/:
//
//	cluster.lok8s.yaml   the domain's spec
//	no-kubeconfig        present: the domain has no kubeconfig
//	render               the answer of the render (kustomize build): `yaml
//	                     <file in render/>`, or `fail`. Absent: the render
//	                     must not run.
//	values               lines that the merged values of the render must
//	                     hold (the values of the spec.bootstrap entry)
//	clusterroles         the answer of that read: `json <file in json/>`, or
//	clusterrolebindings  `fail <rc> <stderr>`. Absent: the read must not run.
//	roles
//	rolebindings
//	want                 the exact output of the check; @BASE@ stands for the
//	                     project directory (the kubeconfig in a command)
//
// The files in render/ are real renders of the addon (TestDoctorKMSCasesWithTheRealRender
// checks them on lo-full). Every failed read or render, and every name that
// the check must not print, carries the word CANARY: the check must never
// print it. The runner is a fake: no kubectl and no kustomize runs, and no
// cluster is reached.

const kmsFixtures = "../../tests/fixtures/doctor-csi-kms"

// kmsResources are the reads of the check, in order.
var kmsResources = []string{"clusterroles", "clusterrolebindings", "roles", "rolebindings"}

// kmsCase sets up one case in a synthetic project and returns the project,
// the PATH that holds the (never executed) kubectl, and the fake runner.
// With realRender, the render is the in-process render of lo-full and the
// runner refuses a kustomize; else LO_RENDER=exec sends the render through
// the runner, which answers it from the case.
func kmsCase(t *testing.T, name string, realRender bool) (*config.Paths, string, *scriptRunner) {
	t.Helper()
	dir := filepath.Join(kmsFixtures, "cases", name)
	if _, err := os.Stat(filepath.Join(dir, "want")); err != nil {
		t.Fatalf("no case %s: %v", name, err)
	}
	p := synthProject(t)
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	if realRender {
		t.Setenv(render.ModeEnv, "")
	} else {
		t.Setenv(render.ModeEnv, "exec")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cluster.lok8s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(p.Clusters, rbacTestDomain, "cluster.lok8s.yaml"), string(raw))
	kubeconfig := filepath.Join(p.Base, ".kubeconfig", rbacTestName+".yaml")
	if _, err := os.Stat(filepath.Join(dir, "no-kubeconfig")); err != nil {
		testutil.WriteFile(t, kubeconfig, "apiVersion: v1\nkind: Config\n")
	}
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	testutil.WriteFile(t, kubectl, "#!/bin/sh\nexit 99\n")
	if err := os.Chmod(kubectl, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scriptRunner{handler: func(c execx.Cmd) error {
		switch {
		case c.Name == "kustomize" && !realRender:
			return kmsFakeRender(t, dir, c)
		case c.Name == kubectl && len(c.Args) > 3:
			return kmsFakeRead(t, dir, kubeconfig, c)
		}
		t.Errorf("unexpected exec: %s %v", c.Name, c.Args)
		return errors.New("unexpected exec")
	}}
	return p, bin, r
}

// kmsFakeRender answers `kustomize build` from the case's render file. It
// checks the flags of the bootstrap's render and that the merged values of
// the build directory hold the case's values lines.
func kmsFakeRender(t *testing.T, dir string, c execx.Cmd) error {
	t.Helper()
	if len(c.Args) != 4 || !slices.Equal(c.Args[:3], []string{"build", "--enable-alpha-plugins", "--enable-exec"}) {
		t.Errorf("kustomize args = %q", c.Args)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "render"))
	if err != nil {
		t.Errorf("unexpected render")
		return errors.New("unexpected render")
	}
	if want, err := os.ReadFile(filepath.Join(dir, "values")); err == nil {
		merged, err := os.ReadFile(filepath.Join(c.Args[3], "values.merged.yaml"))
		if err != nil {
			t.Errorf("the render has no merged values: %v", err)
		}
		var lines []string
		for l := range strings.SplitSeq(string(merged), "\n") {
			lines = append(lines, strings.TrimSpace(l))
		}
		for l := range strings.SplitSeq(strings.TrimRight(string(want), "\n"), "\n") {
			if !slices.Contains(lines, strings.TrimSpace(l)) {
				t.Errorf("the merged values have no line %q:\n%s", l, merged)
			}
		}
	}
	kind, rest, _ := strings.Cut(strings.TrimRight(string(raw), "\n"), " ")
	switch kind {
	case "yaml":
		body, err := os.ReadFile(filepath.Join(kmsFixtures, "render", rest))
		if err != nil {
			t.Fatal(err)
		}
		c.Stdout.Write(body)
		return nil
	case "fail":
		c.Stderr.Write([]byte("Error: CANARY render\n"))
		return rbacExit(1)
	}
	t.Fatalf("bad render answer %q", raw)
	return nil
}

// kmsFakeRead answers one read from the case.
func kmsFakeRead(t *testing.T, dir, kubeconfig string, c execx.Cmd) error {
	t.Helper()
	res := c.Args[3]
	want := []string{"--kubeconfig", kubeconfig, "get", res, "--all-namespaces", "--selector", "lok8s.dev/name=rook-ceph-csi-kms", "-o", "json", "--request-timeout=10s"}
	if !slices.Equal(c.Args, want) {
		t.Errorf("kubectl args = %q, want %q", c.Args, want)
	}
	raw, err := os.ReadFile(filepath.Join(dir, res))
	if err != nil {
		t.Errorf("unexpected read of %s", res)
		return errors.New("unexpected read")
	}
	kind, rest, _ := strings.Cut(strings.TrimRight(string(raw), "\n"), " ")
	switch kind {
	case "json":
		body, err := os.ReadFile(filepath.Join(kmsFixtures, "json", rest))
		if err != nil {
			t.Fatal(err)
		}
		c.Stdout.Write(body)
		return nil
	case "fail":
		code, text, _ := strings.Cut(rest, " ")
		rc, err := strconv.Atoi(code)
		if err != nil {
			t.Fatal(err)
		}
		c.Stderr.Write([]byte(text + "\n"))
		return rbacExit(rc)
	}
	t.Fatalf("bad answer %q", raw)
	return nil
}

func kmsWant(t *testing.T, p *config.Paths, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(kmsFixtures, "cases", name, "want"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(p.Base, "'@") {
		t.Fatalf("the project directory %q has a quote or an @", p.Base)
	}
	return strings.ReplaceAll(string(raw), "@BASE@", p.Base)
}

// kmsCaseCalls are the calls that the case expects: "kustomize" when it has
// a render answer (and not realRender), then each read that it answers.
func kmsCaseCalls(t *testing.T, name string, realRender bool) []string {
	t.Helper()
	dir := filepath.Join(kmsFixtures, "cases", name)
	var calls []string
	if _, err := os.Stat(filepath.Join(dir, "render")); err == nil && !realRender {
		calls = append(calls, "kustomize")
	}
	for _, res := range kmsResources {
		if _, err := os.Stat(filepath.Join(dir, res)); err == nil {
			calls = append(calls, res)
		}
	}
	return calls
}

// kmsGotCalls names the calls of the runner the same way.
func kmsGotCalls(r *scriptRunner) []string {
	var calls []string
	for _, call := range r.calls {
		f := strings.Fields(call)
		switch {
		case f[0] == "kustomize":
			calls = append(calls, "kustomize")
		case len(f) > 4:
			calls = append(calls, f[4])
		}
	}
	return calls
}

// runKMSCases runs every case and checks the output, that no CANARY is
// printed, and the calls.
func runKMSCases(t *testing.T, realRender bool) {
	entries, err := os.ReadDir(filepath.Join(kmsFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 15 {
		t.Fatalf("only %d cases under %s", len(entries), kmsFixtures)
	}
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			if raw, err := os.ReadFile(filepath.Join(kmsFixtures, "cases", name, "render")); realRender && err == nil && strings.TrimSpace(string(raw)) == "fail" {
				t.Skip("a case of a failed render: the real render of its values does not fail")
			}
			p, bin, r := kmsCase(t, name, realRender)
			var out strings.Builder
			doctorKMSLeftovers(context.Background(), r, p, rbacTestDomain, bin, bootstrapEntries(p), &out)
			if want := kmsWant(t, p, name); out.String() != want {
				t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
			}
			if strings.Contains(out.String(), "CANARY") || strings.Contains(out.String(), "canary") {
				t.Errorf("the output carries a CANARY:\n%s", out.String())
			}
			if got, want := kmsGotCalls(r), kmsCaseCalls(t, name, realRender); !slices.Equal(got, want) {
				t.Errorf("calls = %q, want %q", got, want)
			}
		})
	}
}

// TestDoctorKMSCases runs every shared case with the fake render: (a) an
// object of an earlier option set of each kind warns with its delete
// command; (b) an equal set prints nothing; (c) a ClusterRoleBinding of the
// option after a change to tenantNamespaces warns; (d) without the addon
// in spec.bootstrap but with rook-ceph, each object of the addon is a
// leftover, and none prints nothing; (e) without either addon, or with the
// addon as a path entry, there is no read; (f) a failed read is one
// warning, the reads stop, and no finding of an earlier read prints; (g) a
// name or namespace that is not DNS-1123 is counted and never printed, and
// an option label that is not a DNS-1123 label is not printed; (h) an
// object without the addon's label is never a finding; (i) a failed render
// is one warning, with no read, and so is an env name that the bootstrap
// refuses (no render runs); (j) no kubeconfig: one line for the addon, none
// without it.
func TestDoctorKMSCases(t *testing.T) { runKMSCases(t, false) }

// TestDoctorKMSCasesWithTheRealRender runs the cases with the in-process
// render of the real addon (lo-full): the values of the spec.bootstrap
// entry reach the render, the render holds the objects of the render files
// of the cases, and the output is the same. lo core renders with the
// kustomize of the toolchain: there the fake render runs only (CI runs the
// Go tests with -tags inprocess too).
func TestDoctorKMSCasesWithTheRealRender(t *testing.T) {
	if !render.InProcessAvailable() {
		t.Skip("lo core: the real render of the addon runs in the lo-full tests (-tags inprocess)")
	}
	runKMSCases(t, true)
}

// No kubectl on the PATH: one line when spec.bootstrap has the addon, none
// without it. No read and no render in either.
func TestDoctorKMSSkipsWithoutKubectl(t *testing.T) {
	for name, want := range map[string]string{
		"equal":              "  ! rook-ceph-csi-kms RBAC leftovers not checked: kubectl not found\n",
		"disabled-leftovers": "",
	} {
		t.Run(name, func(t *testing.T) {
			p, _, r := kmsCase(t, name, false)
			var out strings.Builder
			doctorKMSLeftovers(context.Background(), r, p, rbacTestDomain, t.TempDir(), bootstrapEntries(p), &out)
			if got := out.String(); got != want {
				t.Errorf("no kubectl: %q, want %q", got, want)
			}
			if len(r.calls) != 0 {
				t.Errorf("calls without kubectl: %q", r.calls)
			}
		})
	}
}

// The reads have the bound of the RBAC migration check (rbacRead): a read
// that does not end is stopped, and the check prints the fixed reason once.
func TestDoctorKMSReadBoundStopsAHungRead(t *testing.T) {
	p, bin, _ := kmsCase(t, "disabled-leftovers", false)
	defer func(d time.Duration) { rbacReadBound = d }(rbacReadBound)
	rbacReadBound = 5 * time.Millisecond
	r := &rbacHangRunner{fallback: 5 * time.Second}
	var out strings.Builder
	doctorKMSLeftovers(context.Background(), r, p, rbacTestDomain, bin, bootstrapEntries(p), &out)
	if got, want := out.String(), "  ! rook-ceph-csi-kms RBAC leftovers not checked: "+rbacReasonNoAnswer+"\n"; got != want {
		t.Errorf("a hung read: %q, want %q", got, want)
	}
	if r.calls != 1 {
		t.Errorf("reads after a hung read: %d, want 1", r.calls)
	}
}

// rbacDeleteCommandIn checks the namespace again where it builds the
// command: no command for a namespace that is not a DNS-1123 label of at
// most 63 characters, also when a caller did not check it.
func TestDoctorKMSDeleteCommandChecksTheNamespace(t *testing.T) {
	for _, ns := range []string{"a b", "a;b", "A", "a.b", "-a", "a\nb", "a$(x)", strings.Repeat("a", 64)} {
		if cmd, ok := rbacDeleteCommandIn("/k", "role", ns, "a"); ok || cmd != "" {
			t.Errorf("namespace %q: command %q", ns, cmd)
		}
	}
	if cmd, ok := rbacDeleteCommandIn("/k", "role", "rook-ceph", "a;b"); ok || cmd != "" {
		t.Errorf("name a;b: command %q", cmd)
	}
	for _, ns := range []string{"a", "rook-ceph", strings.Repeat("a", 63)} {
		if cmd, ok := rbacDeleteCommandIn("/k", "role", ns, "a.b"); !ok || cmd != "kubectl --kubeconfig '/k' delete role a.b --namespace "+ns {
			t.Errorf("namespace %q: command %q, %v", ns, cmd, ok)
		}
	}
}

// The names and labels that the check uses are the ones of the embedded
// addon: a rename in the addon fails here, not as a silent miss.
func TestDoctorKMSConstantsMatchTheAddon(t *testing.T) {
	for _, name := range []string{kmsAddon, kmsParentAddon} {
		if !slices.Contains(assets.AddonNames(), name) {
			t.Errorf("no embedded addon %s", name)
		}
	}
	read := func(rel string) string {
		t.Helper()
		raw, err := fs.ReadFile(assets.FS(), "addons/"+kmsAddon+"/"+rel)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if k := read("kustomization.yaml"); !strings.Contains(k, "      "+kmsNameLabel+": "+kmsAddon+"\n") {
		t.Errorf("the kustomization of %s does not set %s: %s", kmsAddon, kmsNameLabel, kmsAddon)
	}
	if h := read("chart/templates/_helpers.tpl"); !strings.Contains(h, kmsOptionLabel+": {{ $b.option }}") {
		t.Errorf("the bindings of %s do not carry %s", kmsAddon, kmsOptionLabel)
	}
	// The render's RBAC kinds are the kinds that the check reads.
	set := map[kmsID]bool{}
	body, err := os.ReadFile(filepath.Join(kmsFixtures, "render", "metadata.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !kmsRenderedIDs(string(body), set) || len(set) != 4 {
		t.Errorf("the render of kms [metadata] gives %d objects of the read kinds, want 4: %v", len(set), set)
	}
}

// The real command: the lines of the check come after the RBAC migration
// lines, in the domain section.
func TestDoctorKMSLinesFollowTheRBACLines(t *testing.T) {
	p, _, _ := kmsCase(t, "no-kubeconfig", false)
	t.Setenv("PATH", t.TempDir())
	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", rbacTestDomain)
	want := "  ℹ RBAC migration check skipped: spec.bootstrap has no system-upgrade-controller and no redis-operator\n" +
		"  ℹ rook-ceph-csi-kms RBAC check skipped: no kubeconfig at .kubeconfig/rbac-test.yaml\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("want %q in:\n%s", want, stdout)
	}
}

// The entry resolver hands the check the values and the env of the entry,
// as the bootstrap renders it.
func TestDoctorKMSEntryCarriesValuesAndEnv(t *testing.T) {
	p := synthProject(t)
	spec := filepath.Join(p.Clusters, rbacTestDomain, "cluster.lok8s.yaml")
	testutil.WriteFile(t, spec, "apiVersion: lok8s.dev/v1\nkind: KubeOne\nmetadata:\n  name: rbac-test\nspec:\n  bootstrap:\n"+
		"    - rook-ceph-csi-kms:\n        values: {kms: [kmip]}\n        env: {LOK8S_USER_X: \"1\"}\n")
	var got []addons.Resolved
	for _, e := range bootstrapEntries(p)(spec, "kubeone", rbacTestDomain) {
		if e.Builtin && filepath.Base(e.Dir) == kmsAddon {
			got = append(got, e)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Inline, "kmip") || got[0].Env["LOK8S_USER_X"] != "1" {
		t.Errorf("entries = %+v", got)
	}
}
