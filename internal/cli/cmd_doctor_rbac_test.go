package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// The RBAC migration check of `lo doctor` (cmd_doctor_rbac.go). The cases
// are shared with the bats twin (tests/unit/doctor_rbac_test.bats) under
// tests/fixtures/doctor-rbac/cases/<case>/:
//
//	cluster.lok8s.yaml   the domain's spec (absent: the domain has none)
//	deploy.lok8s.yaml    the domain's deploy spec (absent: none)
//	kubeconfig-name      the name of the kubeconfig file (absent: rbac-test)
//	no-kubeconfig        present: the domain has no kubeconfig
//	clusterrolebindings  the answer of that read: `json <file in json/>`, or
//	clusterroles         `fail <rc> <stderr>`. Absent: the read must not run.
//	want                 the exact output of the check; @BASE@ stands for the
//	                     project directory (the kubeconfig in a command)
//
// Every failed read carries the word CANARY in its stderr (or its stdout):
// the check must never print it. The runner is a fake: nothing runs and no
// cluster is reached.

const (
	rbacFixtures   = "../../tests/fixtures/doctor-rbac"
	rbacTestDomain = "rbac.example"
	rbacTestName   = "rbac-test" // metadata.name in every case spec
)

// rbacExit is the error of a fake kubectl that exits non-zero.
type rbacExit int

func (e rbacExit) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e rbacExit) ExitCode() int { return int(e) }

// rbacCase sets up one case in a synthetic project and returns the project,
// the PATH that holds the (never executed) kubectl, and the fake runner.
func rbacCase(t *testing.T, name string) (*config.Paths, string, *scriptRunner) {
	t.Helper()
	dir := filepath.Join(rbacFixtures, "cases", name)
	if _, err := os.Stat(filepath.Join(dir, "want")); err != nil {
		t.Fatalf("no case %s: %v", name, err)
	}
	p := synthProject(t)
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	for _, f := range []string{"cluster.lok8s.yaml", "deploy.lok8s.yaml"} {
		if raw, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			testutil.WriteFile(t, filepath.Join(p.Clusters, rbacTestDomain, f), string(raw))
		}
	}
	kcName := rbacTestName
	if raw, err := os.ReadFile(filepath.Join(dir, "kubeconfig-name")); err == nil {
		kcName = strings.TrimSpace(string(raw))
	}
	kubeconfig := filepath.Join(p.Base, ".kubeconfig", kcName+".yaml")
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
		if c.Name != kubectl || len(c.Args) < 4 {
			t.Errorf("unexpected exec: %s %v", c.Name, c.Args)
			return errors.New("unexpected exec")
		}
		res := c.Args[3]
		want := []string{"--kubeconfig", kubeconfig, "get", res, "-o", "json", "--request-timeout=10s"}
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
			body, err := os.ReadFile(filepath.Join(rbacFixtures, "json", rest))
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
		t.Fatalf("case %s: bad answer %q", name, raw)
		return nil
	}}
	return p, bin, r
}

// rbacWant is the want file of the case, with the project directory for
// @BASE@. The directory has no single quote, so the case's quoting of the
// kubeconfig path is the check's own.
func rbacWant(t *testing.T, p *config.Paths, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rbacFixtures, "cases", name, "want"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(p.Base, "'@") {
		t.Fatalf("the project directory %q has a quote or an @", p.Base)
	}
	return strings.ReplaceAll(string(raw), "@BASE@", p.Base)
}

func rbacWantReads(t *testing.T, name string) []string {
	t.Helper()
	var reads []string
	for _, res := range []string{"clusterrolebindings", "clusterroles"} {
		if _, err := os.Stat(filepath.Join(rbacFixtures, "cases", name, res)); err == nil {
			reads = append(reads, res)
		}
	}
	return reads
}

// TestDoctorRBACCases runs every shared case: (a) a leftover SUC binding
// under another name than the chart's, with another subject (no delete
// command), and the account as the User name of its token or through a
// group of its token (a shared grant, no delete command); (b) a narrowed
// cluster passes; (c)
// no kubeconfig skips; (d) neither addon in spec.bootstrap skips; (e) each
// failed read prints its fixed reason and never kubectl's output, one case
// per classifier word; (f) each redis-operator form (the old binding, the
// old role, its aggregation labels, no pods/exec, each wildcard arm of the
// pods/exec check), and the guards around them; (g) a binding name that is
// not a DNS-1123 subdomain prints no name and no command; (h) an opt-in
// addon as a path entry does not count; (i) a deploy domain, a spec without
// metadata.name, subjects: null and rules: null; (j) a list of another
// shape (a malformed field, a label that is not a string, a key in
// another case, a second JSON document, a null item) prints the fixed
// reason in both twins; (k) each of the four addons as a bare entry with a
// trailing "/" or "/." counts; (l) the two opt-ins without their addons
// skip as neither addon (the name of an addon is not a prefix match).
func TestDoctorRBACCases(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(rbacFixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 20 {
		t.Fatalf("only %d cases under %s", len(entries), rbacFixtures)
	}
	for _, e := range entries {
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			p, bin, r := rbacCase(t, name)
			var out strings.Builder
			doctorRBACLeftovers(context.Background(), r, p, rbacTestDomain, bin, bootstrapEntries(p), &out)

			want := rbacWant(t, p, name)
			if out.String() != want {
				t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
			}
			if strings.Contains(out.String(), "CANARY") {
				t.Errorf("the output carries kubectl's output:\n%s", out.String())
			}
			var reads []string
			for _, call := range r.calls {
				if f := strings.Fields(call); len(f) > 4 {
					reads = append(reads, f[4])
				}
			}
			if want := rbacWantReads(t, name); !slices.Equal(reads, want) {
				t.Errorf("reads = %q, want %q", reads, want)
			}
		})
	}
}

// No active domain: no line (the domain section says it). No kubectl on the
// PATH: one line. No read in either.
func TestDoctorRBACSkipsWithoutDomainOrKubectl(t *testing.T) {
	p, _, r := rbacCase(t, "narrowed")
	var out strings.Builder
	doctorRBACLeftovers(context.Background(), r, p, "", t.TempDir(), bootstrapEntries(p), &out)
	if got := out.String(); got != "" {
		t.Errorf("no domain: %q, want no line", got)
	}

	out.Reset()
	doctorRBACLeftovers(context.Background(), r, p, rbacTestDomain, t.TempDir(), bootstrapEntries(p), &out)
	if got, want := out.String(), "  ! RBAC migration leftovers not checked: kubectl not found\n"; got != want {
		t.Errorf("no kubectl: %q, want %q", got, want)
	}
	if len(r.calls) != 0 {
		t.Errorf("reads without a domain or kubectl: %q", r.calls)
	}
}

// rbacHangRunner is a kubectl that does not end (an exec credential plugin
// that waits for a login): each run blocks until its context ends. If the
// context never ends, the run gives up after fallback with an error of the
// class "other", so that a lost bound fails the test and does not hang it.
type rbacHangRunner struct {
	fallback time.Duration
	calls    int
}

func (r *rbacHangRunner) Run(ctx context.Context, c execx.Cmd) error {
	r.calls++
	select {
	case <-ctx.Done():
		return errors.New("signal: interrupt")
	case <-time.After(r.fallback):
		c.Stderr.Write([]byte("error: CANARY\n"))
		return rbacExit(1)
	}
}

// The bound (rbacReadBound) stops a read that does not end, and the check
// prints its fixed reason. The parent context never ends: only the bound
// of rbacRead can stop the run.
func TestDoctorRBACReadBoundStopsAHungRead(t *testing.T) {
	p, bin, _ := rbacCase(t, "narrowed")
	defer func(d time.Duration) { rbacReadBound = d }(rbacReadBound)
	rbacReadBound = 5 * time.Millisecond
	r := &rbacHangRunner{fallback: 5 * time.Second}
	var out strings.Builder
	doctorRBACLeftovers(context.Background(), r, p, rbacTestDomain, bin, bootstrapEntries(p), &out)
	want := "  ! RBAC migration leftovers not checked: " + rbacReasonNoAnswer + "\n"
	if got := out.String(); got != want {
		t.Errorf("a hung read: %q, want %q", got, want)
	}
	if r.calls != 1 {
		t.Errorf("reads after a hung read: %d, want 1", r.calls)
	}
}

// The bound with the real runner (execx.NewRunner) and real processes: the
// kubectl starts a child that ignores TERM (as a credential plugin can) and
// ignores TERM itself, or ends on it. Each read runs as its own process
// group: at the bound the group gets SIGTERM, and SIGKILL after the grace
// (rbacReadGrace). The check prints its fixed reason, and when it returns,
// neither the kubectl nor its child exists: kill(pid, 0) fails with ESRCH
// (a zombie that its new parent has not reaped yet also counts as gone).
// The child keeps the kubectl's stdout, so the check returns after the
// grace and before the bound plus the grace plus 1 s.
func TestDoctorRBACReadBoundStopsARealKubectl(t *testing.T) {
	for _, tc := range []struct{ name, trap string }{
		{"a kubectl that ignores TERM", "trap '' INT TERM\n"},
		{"a kubectl that ends on TERM", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, bin, _ := rbacCase(t, "narrowed")
			defer func(d time.Duration) { rbacReadBound = d }(rbacReadBound)
			rbacReadBound = 500 * time.Millisecond
			dir := t.TempDir()
			pidFile, childFile := filepath.Join(dir, "kubectl.pid"), filepath.Join(dir, "child.pid")
			testutil.WriteFile(t, filepath.Join(bin, "kubectl"), "#!/bin/sh\n"+tc.trap+
				"echo $$ > '"+pidFile+"'\n"+
				"sh -c 'trap \"\" TERM; echo $$ > \""+childFile+"\"; exec sleep 30' &\n"+
				"sleep 30\n")
			var out strings.Builder
			start := time.Now()
			doctorRBACLeftovers(context.Background(), execx.NewRunner(p), p, rbacTestDomain, bin, bootstrapEntries(p), &out)
			took := time.Since(start)
			want := "  ! RBAC migration leftovers not checked: " + rbacReasonNoAnswer + "\n"
			if got := out.String(); got != want {
				t.Errorf("output: %q, want %q", got, want)
			}
			if limit := rbacReadBound + rbacReadGrace + time.Second; took >= limit {
				t.Errorf("the check took %s, want less than %s", took, limit)
			}
			if took < rbacReadBound+rbacReadGrace {
				t.Errorf("the check took %s: the child ended before the KILL of the grace", took)
			}
			for _, f := range []string{pidFile, childFile} {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatalf("a process did not start: %v", err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				if !rbacProcessGone(pid) {
					t.Errorf("process %d (%s) still exists after the check", pid, filepath.Base(f))
				}
			}
		})
	}
}

// rbacProcessGone reports that pid does not exist (kill(pid, 0) fails with
// ESRCH), or is a zombie that its new parent has not reaped yet.
func rbacProcessGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the name, which ends at the last ")".
	i := strings.LastIndexByte(string(raw), ')')
	return i >= 0 && strings.HasPrefix(strings.TrimLeft(string(raw[i+1:]), " "), "Z")
}

// rbacTermOKRunner is a kubectl that handles the SIGTERM of the bound and
// exits 0 with a valid list: the run ends with the context and returns nil.
type rbacTermOKRunner struct{ body []byte }

func (r *rbacTermOKRunner) Run(ctx context.Context, c execx.Cmd) error {
	<-ctx.Done()
	c.Stdout.Write(r.body)
	return nil
}

// A read that the bound stopped gives the fixed reason of the bound, also
// when kubectl exited 0 with a valid list after the SIGTERM (bash: the
// timeout mark comes before the exit code).
func TestDoctorRBACReadBoundWinsOverASuccessAfterTheStop(t *testing.T) {
	p, bin, _ := rbacCase(t, "narrowed")
	defer func(d time.Duration) { rbacReadBound = d }(rbacReadBound)
	rbacReadBound = 5 * time.Millisecond
	body, err := os.ReadFile(filepath.Join(rbacFixtures, "json", "crb-narrowed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	doctorRBACLeftovers(context.Background(), &rbacTermOKRunner{body: body}, p, rbacTestDomain, bin, bootstrapEntries(p), &out)
	if got, want := out.String(), "  ! RBAC migration leftovers not checked: "+rbacReasonNoAnswer+"\n"; got != want {
		t.Errorf("a read that exits 0 after the stop: %q, want %q", got, want)
	}
}

// rbacDeleteCommand checks the name again where it builds the command: no
// command for a name that is not a DNS-1123 subdomain of at most 253
// characters, also when a caller did not check it.
func TestDoctorRBACDeleteCommandChecksTheName(t *testing.T) {
	for _, name := range []string{"", "a b", "a;rm -rf x", "a\nb", "a\tb", "A", "a$(x)", "-a", "a.", strings.Repeat("a", 254), "a\x1b[31m"} {
		if cmd, ok := rbacDeleteCommand("/k", "clusterrolebinding", name); ok || cmd != "" {
			t.Errorf("name %q: command %q", name, cmd)
		}
	}
	for _, name := range []string{"a", "redis-operator", "a.b-c", strings.Repeat("a", 253), rbacRedisOldRole} {
		if cmd, ok := rbacDeleteCommand("/k", "clusterrolebinding", name); !ok || cmd != "kubectl --kubeconfig '/k' delete clusterrolebinding "+name {
			t.Errorf("name %q: command %q, %v", name, cmd, ok)
		}
	}
}

// The names, ServiceAccounts and namespaces that the check looks for are
// the ones that the embedded addons apply: the narrowed bindings of both
// addons, the pods/exec grant, and the bindings of the two opt-in addons.
// A rename in a manifest fails here, not as a silent false finding.
func TestDoctorRBACConstantsMatchTheAddons(t *testing.T) {
	for _, name := range []string{rbacSUCAddon, rbacSUCAdminAddon, rbacRedisAddon, rbacRedisChartAddon} {
		if !slices.Contains(assets.AddonNames(), name) {
			t.Errorf("no embedded addon %s", name)
		}
	}
	read := func(rel string) ([]rbacBinding, []rbacRole, []string) {
		t.Helper()
		raw, err := fs.ReadFile(assets.FS(), rel)
		if err != nil {
			t.Fatal(err)
		}
		var bindings []rbacBinding
		var roles []rbacRole
		var namespaces []string
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("%s: %v", rel, err)
			}
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("%s: %v", rel, err)
			}
			// The parsers of the check read the manifest, as JSON.
			var item map[string]any
			if err := json.Unmarshal(body, &item); err != nil {
				t.Fatalf("%s: %v", rel, err)
			}
			switch doc["kind"] {
			case "ClusterRoleBinding":
				b, ok := rbacParseBinding(item)
				if !ok {
					t.Fatalf("%s: the check cannot read a ClusterRoleBinding", rel)
				}
				bindings = append(bindings, b)
			case "ClusterRole":
				r, ok := rbacParseRole(item)
				if !ok {
					t.Fatalf("%s: the check cannot read a ClusterRole", rel)
				}
				roles = append(roles, r)
			case "Namespace":
				var n struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(body, &n); err != nil {
					t.Fatalf("%s: %v", rel, err)
				}
				namespaces = append(namespaces, n.Metadata.Name)
			}
		}
		return bindings, roles, namespaces
	}
	bound := func(rel string, bindings []rbacBinding, sa rbacSubject, role string) {
		t.Helper()
		grants := rbacGrantsTo(bindings, sa, []string{role})
		if len(grants) == 0 || !grants[0].direct || grants[0].others != 0 || grants[0].group != "" {
			t.Errorf("%s: no ClusterRoleBinding binds %+v alone to %s", rel, sa, role)
		}
	}

	const sucRBAC = "addons/" + rbacSUCAddon + "/rbac.yaml"
	b, _, _ := read(sucRBAC)
	bound(sucRBAC, b, rbacSUCAccount, rbacSUCNewRole)
	if _, _, ns := read("addons/" + rbacSUCAddon + "/namespace.yaml"); !slices.Equal(ns, []string{rbacSUCAccount.Namespace}) {
		t.Errorf("%s namespace.yaml: %q, want %q", rbacSUCAddon, ns, rbacSUCAccount.Namespace)
	}
	const sucAdmin = "addons/" + rbacSUCAdminAddon + "/clusterrolebinding.yaml"
	b, _, _ = read(sucAdmin)
	bound(sucAdmin, b, rbacSUCAccount, rbacClusterAdmin)

	const redisRBAC = "addons/" + rbacRedisAddon + "/rbac.yaml"
	b, r, _ := read(redisRBAC)
	bound(redisRBAC, b, rbacRedisAccount, rbacRedisNewRole)
	bound(redisRBAC, b, rbacRedisAccount, rbacRedisExecRole)
	if !rbacGrantsExec(b, r, rbacRedisAccount) {
		t.Errorf("%s: the check does not see the pods/exec grant", redisRBAC)
	}
	if _, _, ns := read("addons/" + rbacRedisAddon + "/namespace.yaml"); !slices.Equal(ns, []string{rbacRedisAccount.Namespace}) {
		t.Errorf("%s namespace.yaml: %q, want %q", rbacRedisAddon, ns, rbacRedisAccount.Namespace)
	}
	const chartRBAC = "addons/" + rbacRedisChartAddon + "/rbac.yaml"
	b, _, _ = read(chartRBAC)
	bound(chartRBAC, b, rbacRedisAccount, rbacRedisChartRole)
}

// The real command: the lines close the domain section, after the active
// line, and before the provider section.
func TestDoctorRBACLinesCloseTheDomainSection(t *testing.T) {
	p, _, _ := rbacCase(t, "no-kubeconfig")
	t.Setenv("PATH", t.TempDir())
	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", rbacTestDomain)
	want := "--- domain ---\n  ✓ active: rbac.example (kind kubeone)\n  ℹ RBAC migration check skipped: no kubeconfig at .kubeconfig/rbac-test.yaml\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("want %q in:\n%s", want, stdout)
	}
}
