package provision

// destroy_guard_test.go: the cluster_name guard of `lo destroy` (bats twin:
// tests/unit/destroy_guard_test.bats). The provider answers its destroy
// plan in the provider::destroy_plan format: "cluster\t<name>", then
// "<resource>\t<id>\t<name>" lines.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/testutil"
)

const guardSpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: test-prod
spec:
  kubernetes:
    version: "v1.35.5"
  cluster:
    domain: test.prod
  provider:
    name: hetzner
    configRef: hetzner.json
`

const guardPlan = "cluster\tprod-a\nload-balancer\t11\tapi-lb\nserver\t42\tcp-0\n"

// planProvider is a loaded provider with the destroy_plan hook.
type planProvider struct {
	fakeProvider
	plan string
	has  bool
	err  error
	log  *[]string
}

func (p *planProvider) DestroyPlan(_ context.Context, configFile, workDir string) (string, bool, error) {
	*p.log = append(*p.log, "plan:"+filepath.Base(configFile)+":"+workDir)
	return p.plan, p.has, p.err
}

type planLoader struct{ p *planProvider }

func (l *planLoader) Load(context.Context, string) (driver.Provider, error) { return l.p, nil }

// guardHarness: clusters/test.prod (KubeOne on hetzner, cluster_name
// prod-a in its hetzner.json), a fake kubeone driver and a provider with
// the destroy_plan hook, in remote mode (where destroy loads the provider).
func guardHarness(t *testing.T, input string, interactive bool) (*Dispatcher, *bytes.Buffer, *[]string, *planProvider) {
	t.Helper()
	return guardHarnessSpec(t, guardSpecYAML, input, interactive)
}

// guardHarnessSpec is guardHarness over another spec; the fake driver
// answers for every kind and records the destroy plan it sees.
func guardHarnessSpec(t *testing.T, spec, input string, interactive bool) (*Dispatcher, *bytes.Buffer, *[]string, *planProvider) {
	t.Helper()
	t.Cleanup(func() { os.Unsetenv(driver.DestroyPlanEnv) })
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.prod", "cluster.lok8s.yaml"), spec)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.prod", "hetzner.json"), `{"cluster_name": "prod-a"}`)
	log := &[]string{}
	fd := &planDriver{fakeDriver{log: log}}
	prov := &planProvider{fakeProvider: fakeProvider{name: "hetzner"}, plan: guardPlan, has: true, log: log}
	var errBuf bytes.Buffer
	d := &Dispatcher{
		Paths:       p,
		Stderr:      &errBuf,
		Stdout:      &bytes.Buffer{},
		In:          strings.NewReader(input),
		Interactive: func() bool { return interactive },
		Remote:      true,
		Providers:   &planLoader{p: prov},
		Drivers: func(name string) (driver.Factory, bool) {
			return func(deps *driver.Deps) (driver.Driver, error) { return fd, nil }, true
		},
		Hooks: Hooks{
			KubehzDeregister: func(ctx context.Context, domain, yaml string) error {
				*log = append(*log, "DEREGISTERED")
				return nil
			},
		},
	}
	return d, &errBuf, log, prov
}

// planDriver is a fake driver whose destroy records the confirmed plan the
// provider::destroy of the driver would read (LOK8S_DESTROY_PLAN).
type planDriver struct{ fakeDriver }

func (f *planDriver) Destroy(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "destroy:"+domain, "plan-env:"+strings.ReplaceAll(os.Getenv(driver.DestroyPlanEnv), "\t", " "))
	return f.destroyErr
}

func assertDeclinedNothingRan(t *testing.T, err error, log *[]string) {
	t.Helper()
	if !errors.Is(err, driver.ErrDeclined) || driver.ExitCode(err) != 3 {
		t.Fatalf("expected a decline (rc 3), got %v", err)
	}
	joined := strings.Join(*log, ",")
	refuteContains(t, joined, "DEREGISTERED")
	refuteContains(t, joined, "destroy:")
}

func TestDestroyGuardRefusesAClusterNameAnotherDomainUses(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	testutil.WriteFile(t, filepath.Join(d.Paths.Clusters, "copy.prod", "hetzner.json"), `{"cluster_name": "prod-a"}`)
	// Neither --force nor the right --confirm-cluster gets past it.
	d.Force, d.ConfirmCluster = true, "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "cluster_name 'prod-a' of 'test.prod' is also the cluster_name of: copy.prod. The provider deletes only the resources with this cluster_name and the owner mark of 'test.prod'. The resources of those domains have the same cluster label, so only the mark keeps them apart: a resource of another domain with a wrong mark would be deleted.")
	refuteContains(t, errBuf.String(), "deletes every resource")
	assertContains(t, errBuf.String(), "Nothing was deleted. Give each domain its own cluster_name, then run lo destroy again.")
}

// Another domain whose provider config has a merge key (<<) on the path to
// its cluster_name: the scan cannot tell the name that its provider uses,
// so the destroy is refused (fail closed), with --force and the right
// --confirm-cluster too.
func TestDestroyGuardRefusesAMergeKeyInAnotherDomain(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	testutil.WriteFile(t, filepath.Join(d.Paths.Clusters, "merge.prod", "hetzner.json"), "base: &b {cluster_name: prod-a}\n<<: *b\n")
	d.Force, d.ConfirmCluster = true, "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "[error] a domain has a merge key (<<) on the path to the cluster_name of its provider config: merge.prod. The provider can take the cluster_name from the merged mapping, so lo destroy cannot compare it with the cluster_name 'prod-a' of 'test.prod'. Nothing was deleted. Write cluster_name as a literal key, without a merge key, in the provider config of that domain, then run lo destroy again.")
	refuteContains(t, errBuf.String(), "is also the cluster_name of")
}

// A clusters dir that the scan cannot list (no read permission): the guard
// cannot tell the other domains, so the destroy is refused (fail closed).
// The bash twin is the test "destroy guard: an unreadable clusters dir stops
// the destroy" in destroy_guard_test.bats.
func TestDestroyGuardRefusesWhenTheClustersDirCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a dir without the read permission")
	}
	d, errBuf, log, _ := guardHarness(t, "", false)
	if err := os.Chmod(d.Paths.Clusters, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d.Paths.Clusters, 0o755) })
	d.Force, d.ConfirmCluster = true, "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "[error] could not check if another domain has the cluster_name 'prod-a' of 'test.prod': lo cannot read the directory "+d.Paths.Clusters+
		". Nothing was deleted. Make "+d.Paths.Clusters+" a directory that you can read and search, then run lo destroy again.")
	refuteContains(t, errBuf.String(), "destroy deletes these resources")
}

func TestDestroyGuardListsTheResourcesAndRefusesOffATerminal(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	d.Force = true // --force does not replace the cluster_name
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	out := errBuf.String()
	assertContains(t, out, "destroy deletes these resources of cluster_name \033[1mprod-a\033[0m:\n    load-balancer api-lb (11)\n    server cp-0 (42)\n")
	assertContains(t, out, "there is no terminal to ask for the cluster_name of 'test.prod'. Nothing was deleted. To confirm, run: lo destroy --domain test.prod -r --confirm-cluster prod-a")
	refuteContains(t, out, "type yes to continue")
}

// A name in the plan comes from the provider's API. The plan shows it as
// ui.Shown gives it: each control character (C0, DEL, C1), each backslash
// and each bidi or zero-width character is '?', and a multibyte name
// stays. The confirmed plan that the driver gets stays as the provider gave
// it (the provider checks it by kind and id). The bash twin is the test
// "destroy guard: the plan shows each name ..." in destroy_guard_test.bats.
func TestDestroyGuardShowsEachNameOfThePlanSafely(t *testing.T) {
	d, errBuf, log, prov := guardHarness(t, "", false)
	prov.plan = "cluster\tprod-a\n" +
		"server\t70\tö-測試\n" +
		"server\t71\ta\x7fb\u0085c\n" +
		"server\t72\t\\\n" +
		"server\t73\tx\u202ey\u200bz\n" +
		"server\t74\te\x1b[31mvil\n"
	d.ConfirmCluster = "prod-a"
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatal(err)
	}
	assertContains(t, errBuf.String(), "destroy deletes these resources of cluster_name \033[1mprod-a\033[0m:\n"+
		"    server ö-測試 (70)\n"+
		"    server a?b?c (71)\n"+
		"    server ? (72)\n"+
		"    server x?y?z (73)\n"+
		"    server e?[31mvil (74)\n")
	refuteContains(t, errBuf.String(), "\x1b[31m")
	refuteContains(t, errBuf.String(), "\u202e")
	assertContains(t, strings.Join(*log, "\n"), "plan-env:cluster prod-a\nserver 70 ö-測試\nserver 71 a\x7fb\u0085c\nserver 72 \\\nserver 73 x\u202ey\u200bz\nserver 74 e\x1b[31mvil")
}

// The kind and the id of a plan line come from the provider too (a
// third-party provider can give any text), so the plan shows them through
// ui.Shown as well. A last row without a name shows an empty name. The
// confirmed plan stays as the provider gave it. The bash twin is the test
// "destroy guard: the plan shows each kind and id safely too".
func TestDestroyGuardShowsEachKindAndIDSafely(t *testing.T) {
	d, errBuf, log, prov := guardHarness(t, "", false)
	prov.plan = "cluster\tprod-a\nser\x1b[2Kver\t7\u202e6\tcp-0\nnetwork\t9\n"
	d.ConfirmCluster = "prod-a"
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatal(err)
	}
	assertContains(t, errBuf.String(), "cluster_name \033[1mprod-a\033[0m:\n"+
		"    ser?[2Kver cp-0 (7?6)\n"+
		"    network  (9)\n")
	refuteContains(t, errBuf.String(), "\x1b[2K")
	refuteContains(t, errBuf.String(), "\u202e")
	assertContains(t, strings.Join(*log, "\n"), "plan-env:cluster prod-a\nser\x1b[2Kver 7\u202e6 cp-0\nnetwork 9")
}

// bash reads a plan line with `IFS=$'\t' read -r kind id name`: the name
// keeps the tabs in it, so two tabs in a row show as two '?'. The bash twin
// is the test "destroy guard: a name with two tabs in a row shows two '?'".
func TestDestroyGuardShowsANameWithTwoTabsAsBashReadsIt(t *testing.T) {
	d, errBuf, _, prov := guardHarness(t, "", false)
	prov.plan = "cluster\tprod-a\n\tserver\t\t75\t\ta\t\tb\t\n"
	d.ConfirmCluster = "prod-a"
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatal(err)
	}
	assertContains(t, errBuf.String(), "cluster_name \033[1mprod-a\033[0m:\n    server a??b (75)\n")
}

// assertClusterNameRefused: a plan whose cluster line names name is refused
// (rc 3) before any message shows the name: with the matching
// --confirm-cluster, on a terminal with the name typed, and off a terminal.
// Nothing runs.
func assertClusterNameRefused(t *testing.T, name string) {
	t.Helper()
	const want = "the destroy plan of 'test.prod' names a cluster_name with a character that a message cannot show as it is (a control, separator, bidi or invisible character, a backslash, U+FFFD, or a byte that is not UTF-8). Nothing was deleted. Correct the cluster_name in the provider config or the destroy_plan hook of the provider, then run lo destroy again."
	check := func(how string, errBuf *bytes.Buffer, log *[]string, err error) {
		t.Helper()
		if !errors.Is(err, driver.ErrDeclined) || driver.ExitCode(err) != 3 {
			t.Fatalf("%q, %s: expected a refusal (rc 3), got %v", name, how, err)
		}
		joined := strings.Join(*log, ",")
		refuteContains(t, joined, "DEREGISTERED")
		refuteContains(t, joined, "destroy:")
		out := errBuf.String()
		assertContains(t, out, want)
		// Nothing shows the name: no header, no prompt, no hint.
		refuteContains(t, out, name)
		refuteContains(t, out, "destroy deletes these resources")
		refuteContains(t, out, "type the cluster_name")
	}
	plan := "cluster\t" + strings.Replace(name, "\t", "\t\t", 1) + "\nserver\t42\tcp-0\n"

	d, errBuf, log, prov := guardHarness(t, "", false)
	prov.plan, d.ConfirmCluster = plan, name
	check("--confirm-cluster", errBuf, log, d.DispatchDestroy(t.Context(), "test.prod"))

	d, errBuf, log, prov = guardHarness(t, name+"\n", true)
	prov.plan = plan
	check("typed", errBuf, log, d.DispatchDestroy(t.Context(), "test.prod"))

	d, errBuf, log, prov = guardHarness(t, "", false)
	prov.plan = plan
	check("off a terminal", errBuf, log, d.DispatchDestroy(t.Context(), "test.prod"))
}

// The cluster_name is shown in the guard's messages and typed as the
// answer, so a shown form must be the name itself. A name with a character
// of ui.Unshown is refused before any message shows it. The bash twin is
// the test "destroy guard: a cluster_name with a character that cannot be
// shown is refused before it is shown".
func TestDestroyGuardRefusesAClusterNameThatCannotBeShown(t *testing.T) {
	for _, name := range []string{
		"prod\x1b[2J-a",   // ESC
		"prod\u202e-a",    // bidi override
		"prod\u200b-a",    // zero-width space
		"prod\t-a",        // a tab: the cluster line "cluster\tprod\t\t-a"
		"prod-a\r",        // a CR (a plan with CRLF line ends)
		"prod\u0085-a",    // C1 (NEL)
		"prod\U000e0041a", // a tag character
	} {
		assertClusterNameRefused(t, name)
	}
}

// A backslash is refused. The rule dates from when bash's error() printed
// with echo -e, which read "\e[2J" as an ESC; it stays. The bash twin is
// the test "destroy guard: a cluster_name with a backslash is refused".
func TestDestroyGuardRefusesAClusterNameWithABackslash(t *testing.T) {
	for _, name := range []string{`prod\e[2J-a`, `a\b`, `prod\`} {
		assertClusterNameRefused(t, name)
	}
}

// A byte that is not UTF-8: a terminal shows it as another character, and
// jq (the bash twin's check) reads it as U+FFFD. The bash twin is the test
// "destroy guard: a cluster_name that is not UTF-8 is refused".
func TestDestroyGuardRefusesAClusterNameThatIsNotUTF8(t *testing.T) {
	for _, name := range []string{
		"prod\xff-a",             // a byte that never starts a character
		"prod\xe2\x80-a",         // a sequence without its last byte
		"prod\xc0\x80-a",         // an overlong form
		"prod\xed\xa0\x80-a",     // a surrogate
		"prod\xf4\x90\x80\x80-a", // above U+10FFFF
		"prod\xf0\x9f\x98-a",     // a four-byte sequence without its last byte
	} {
		assertClusterNameRefused(t, name)
	}
}

// U+FFFD: the bash twin cannot tell it from a byte that is not UTF-8 (jq
// reads each such sequence as U+FFFD), so both twins refuse it. The bash
// twin is the test "destroy guard: a cluster_name with U+FFFD is refused".
func TestDestroyGuardRefusesAClusterNameWithUFFFD(t *testing.T) {
	for _, name := range []string{"prod\uFFFD-a", "\uFFFD"} {
		assertClusterNameRefused(t, name)
	}
}

// withoutHeaderColors: out without the fixed colour codes of the guard
// header (bold yellow, dim, bold, reset), on the header line only. The same
// codes on any other line stay, so a value that becomes one of them fails
// the check.
func withoutHeaderColors(out string) string {
	codes := strings.NewReplacer("\x1b[1;33m", "", "\x1b[0m", "", "\x1b[2m", "", "\x1b[1m", "")
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.Contains(line, "destroy deletes these resources of cluster_name") {
			lines[i] = codes.Replace(line)
		}
	}
	return strings.Join(lines, "\n")
}

// A --confirm-cluster value and a typed answer that hold the text "\e[2J":
// the messages show the value as it is, and no ESC byte (besides the colour
// codes of the header) is in the output. The guard does not show the typed
// answer. The bash twin is the test "destroy guard: a backslash sequence in
// --confirm-cluster or in the answer stays text".
func TestDestroyGuardShowsABackslashSequenceAsText(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	d.ConfirmCluster = `\e[2J`
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), `--confirm-cluster '\e[2J' does not match the cluster_name 'prod-a' of 'test.prod'. Nothing was deleted.`)
	refuteContains(t, withoutHeaderColors(errBuf.String()), "\x1b")

	d, errBuf, log, _ = guardHarness(t, `\e[2J`+"\n", true)
	err = d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "the answer is not the cluster_name 'prod-a'. Nothing was deleted.")
	refuteContains(t, withoutHeaderColors(errBuf.String()), "\x1b")
	refuteContains(t, errBuf.String(), `\e[2J`)
}

// A NUL byte in the plan: no environment variable can hold it, so the
// confirmed plan could not reach the provider's destroy, and bash's command
// substitution would drop it (the plan would change: "4<NUL>2" becomes
// "42"). The whole plan is refused before anything is shown, also with the
// matching --confirm-cluster. The bash twin is the test "destroy guard: a
// plan with a NUL byte is refused, and nothing is deleted".
func TestDestroyGuardRefusesAPlanWithANulByte(t *testing.T) {
	for _, plan := range []string{
		"cluster\tprod-a\nserver\t4\x002\tcp-0\n",
		"cluster\tprod-a\nserver\t42\tcp\x00-0\n",
		"cluster\tprod\x00-a\nserver\t42\tcp-0\n",
		"\x00",
	} {
		d, errBuf, log, prov := guardHarness(t, "", false)
		prov.plan, d.ConfirmCluster = plan, "prod-a"
		err := d.DispatchDestroy(t.Context(), "test.prod")
		assertDeclinedNothingRan(t, err, log)
		out := errBuf.String()
		assertContains(t, out, "the destroy plan of 'test.prod' has a NUL byte, and the confirmed plan cannot carry it to the provider. Nothing was deleted. Correct the destroy_plan hook of the provider, then run lo destroy again.")
		refuteContains(t, out, "destroy deletes these resources")
		refuteContains(t, out, "\x00")
	}
}

// When the confirmed plan cannot be set, the provider's destroy would run
// without it and delete what its own list gives. The guard stops instead.
// Go only: in bash, export of a string cannot fail.
func TestDestroyGuardStopsWhenTheConfirmedPlanCannotBeSet(t *testing.T) {
	orig := setenv
	t.Cleanup(func() { setenv = orig })
	setenv = func(key, value string) error {
		return errors.New("setenv: invalid argument")
	}
	d, errBuf, log, _ := guardHarness(t, "", false)
	d.ConfirmCluster = "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "could not give the confirmed plan of 'test.prod' to the provider (setenv: invalid argument). Nothing was deleted. Run lo destroy again.")
}

func TestDestroyGuardRefusesAConfirmClusterThatDoesNotMatch(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	d.ConfirmCluster = "prod-b"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "--confirm-cluster 'prod-b' does not match the cluster_name 'prod-a' of 'test.prod'. Nothing was deleted.")
}

func TestDestroyGuardRunsTheDestroyWithTheMatchingConfirmCluster(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	d.ConfirmCluster = "prod-a"
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatalf("expected the destroy to run, got %v", err)
	}
	work := filepath.Join(d.Paths.Clusters, "test.prod", ".kubeone")
	// The driver's destroy sees exactly the confirmed plan.
	want := "plan:hetzner.json:" + work + ",DEREGISTERED,destroy:test.prod,plan-env:cluster prod-a\nload-balancer 11 api-lb\nserver 42 cp-0"
	if got := strings.Join(*log, ","); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if v, ok := os.LookupEnv(driver.DestroyPlanEnv); ok {
		t.Errorf("%s is still set after the destroy: %q", driver.DestroyPlanEnv, v)
	}
	// One question per run: the yes-prompt of ConfirmInfra is not asked.
	refuteContains(t, errBuf.String(), "type yes to continue")
	refuteContains(t, errBuf.String(), "[error]")
}

func TestDestroyGuardTakesTheTypedClusterName(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "prod-a\n", true)
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatalf("expected the destroy to run, got %v", err)
	}
	assertContains(t, errBuf.String(), "type the cluster_name (prod-a) to delete these resources: ")
	assertContains(t, strings.Join(*log, ","), "destroy:test.prod")

	// "yes" is not the cluster_name.
	d, errBuf, log, _ = guardHarness(t, "yes\n", true)
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "the answer is not the cluster_name 'prod-a'. Nothing was deleted.")

	// A last line without a newline and with a CR counts (bash twin:
	// provision::_read_answer).
	d, _, log, _ = guardHarness(t, " prod-a\r", true)
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatalf("an answer without a newline: %v", err)
	}
	assertContains(t, strings.Join(*log, ","), "destroy:test.prod")
}

func TestDestroyGuardStopsWhenThePlanFails(t *testing.T) {
	d, _, log, prov := guardHarness(t, "", false)
	prov.err = errors.New("exit status 1")
	d.ConfirmCluster = "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	if err == nil || errors.Is(err, driver.ErrDeclined) {
		t.Fatalf("expected the plan failure, got %v", err)
	}
	refuteContains(t, strings.Join(*log, ","), "destroy:")
}

func TestDestroyGuardRefusesAPlanWithoutAClusterName(t *testing.T) {
	d, errBuf, log, prov := guardHarness(t, "", false)
	prov.plan = "server\t42\tcp-0\n"
	d.ConfirmCluster = "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	assertDeclinedNothingRan(t, err, log)
	assertContains(t, errBuf.String(), "the provider gave no cluster_name for 'test.prod'. Nothing was deleted.")
}

// A provider without the hook keeps the yes-prompt (ConfirmInfra).
func TestDestroyGuardLeavesAProviderWithoutTheHookToTheYesGate(t *testing.T) {
	d, errBuf, log, prov := guardHarness(t, "yes\n", true)
	prov.has, prov.plan = false, ""
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatalf("expected the destroy to run, got %v", err)
	}
	assertContains(t, errBuf.String(), "type yes to continue")
	refuteContains(t, errBuf.String(), "type the cluster_name")
	assertContains(t, strings.Join(*log, ","), "destroy:test.prod")
}

func TestParseDestroyPlan(t *testing.T) {
	name, res := parseDestroyPlan("cluster\tprod-a\n\nserver\t42\tcp-0\nvolume\t7\t\nnetwork\t9\tnet\n")
	if name != "prod-a" {
		t.Fatalf("name = %q", name)
	}
	want := []destroyResource{{"server", "42", "cp-0"}, {"volume", "7", ""}, {"network", "9", "net"}}
	if len(res) != len(want) {
		t.Fatalf("resources = %+v", res)
	}
	for i := range want {
		if res[i] != want[i] {
			t.Errorf("resource %d = %+v, want %+v", i, res[i], want[i])
		}
	}
	// A cluster line after a resource is a resource, not the name.
	if name, _ := parseDestroyPlan("server\t42\tcp-0\ncluster\tx\n"); name != "" {
		t.Errorf("late cluster line read as the name: %q", name)
	}
}

// readPlanLine splits a line as bash's `IFS=$'\t' read -r kind id rest`
// does (each want is what bash 5 gives for the line).
func TestReadPlanLineAsBashRead(t *testing.T) {
	for _, c := range []struct{ line, kind, id, rest string }{
		{"server\t7\ta\t\tb", "server", "7", "a\t\tb"},
		{"\t\tserver\t\t7\t\ta\t\t\tb\t", "server", "7", "a\t\t\tb"},
		{"x\ty\tz\tw\t\t", "x", "y", "z\tw"},
		{"server", "server", "", ""},
		{"server\t\t", "server", "", ""},
		{"volume\t7\t", "volume", "7", ""},
		{" a \t b \t c ", " a ", " b ", " c "},
	} {
		kind, id, rest := readPlanLine(c.line)
		if kind != c.kind || id != c.id || rest != c.rest {
			t.Errorf("readPlanLine(%q) = %q, %q, %q, want %q, %q, %q", c.line, kind, id, rest, c.kind, c.id, c.rest)
		}
	}
	// The cluster line keeps the tabs of the name too: id, one tab, rest.
	if name, _ := parseDestroyPlan("cluster\tp\t\tx\t\ty\n"); name != "p\tx\t\ty" {
		t.Errorf("cluster name = %q, want %q", name, "p\tx\t\ty")
	}
}

// Without -r no provider is loaded and no guard runs: a --confirm-cluster
// must not let the destroy run as if it were confirmed (a forced kubeone
// destroy resets the nodes and leaves the servers up).
func TestDestroyGuardRefusesConfirmClusterWhenNoGuardRuns(t *testing.T) {
	d, errBuf, log, _ := guardHarness(t, "", false)
	d.Remote, d.Force, d.ConfirmCluster = false, true, "prod-a"
	err := d.DispatchDestroy(t.Context(), "test.prod")
	if err == nil || errors.Is(err, driver.ErrDeclined) || driver.ExitCode(err) != 1 {
		t.Fatalf("expected a refusal with rc 1, got %v (rc %d)", err, driver.ExitCode(err))
	}
	assertContains(t, errBuf.String(), "--confirm-cluster 'prod-a' is set, but the destroy of 'test.prod' checks no cluster_name: it runs without -r (--remote), so no provider is loaded. Nothing was deleted. Add -r for a provider cluster, or remove --confirm-cluster.")
	refuteContains(t, strings.Join(*log, ","), "destroy:")

	// Without --confirm-cluster the forced destroy runs as before.
	d, _, log, _ = guardHarness(t, "", false)
	d.Remote, d.Force = false, true
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatalf("expected the forced destroy to run, got %v", err)
	}
	assertContains(t, strings.Join(*log, ","), "destroy:test.prod")
}

// A driver that does not destroy through the provider gets no plan print and
// no guard: the yes-gate as before, and --confirm-cluster is refused.
func TestDestroyGuardSkipsDriversThatDoNotDeleteThroughTheProvider(t *testing.T) {
	hosted := strings.Replace(guardSpecYAML, "spec:\n", "spec:\n  kubehz:\n    hosting: hosted\n    apiUrl: https://api.example.com\n", 1)
	capi := strings.Replace(guardSpecYAML, "kind: KubeOne", "kind: Capi", 1)
	for _, tc := range []struct{ name, spec, reason string }{
		{"hosted kubeone", hosted, "a hosted cluster is destroyed through the kubehz API, not the provider"},
		{"capi", capi, "the capi driver does not destroy through the provider"},
	} {
		d, errBuf, log, _ := guardHarnessSpec(t, tc.spec, "yes\n", true)
		if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
			t.Fatalf("%s: expected the yes-gate to pass, got %v", tc.name, err)
		}
		refuteContains(t, errBuf.String(), "destroy deletes these resources")
		assertContains(t, errBuf.String(), "type yes to continue")
		refuteContains(t, strings.Join(*log, ","), "plan:")

		d, errBuf, log, _ = guardHarnessSpec(t, tc.spec, "", false)
		d.ConfirmCluster = "prod-a"
		if err := d.DispatchDestroy(t.Context(), "test.prod"); err == nil {
			t.Fatalf("%s: expected --confirm-cluster to be refused", tc.name)
		}
		assertContains(t, errBuf.String(), "checks no cluster_name: "+tc.reason+".")
		refuteContains(t, strings.Join(*log, ","), "destroy:")
	}
}

// An inherited LOK8S_DESTROY_PLAN must never reach a destroy that the guard
// did not confirm (bats twin: "an inherited plan is cleared on a guard-less
// destroy"). Without -r no guard runs.
func TestDestroyGuardClearsAnInheritedPlan(t *testing.T) {
	d, _, log, _ := guardHarness(t, "", false)
	t.Setenv(driver.DestroyPlanEnv, "cluster\tprod-a\nserver\t42\tcp-0")
	d.Remote, d.Force = false, true
	if err := d.DispatchDestroy(t.Context(), "test.prod"); err != nil {
		t.Fatalf("expected the forced destroy to run, got %v", err)
	}
	if got := strings.Join(*log, ","); got != "DEREGISTERED,destroy:test.prod,plan-env:" {
		t.Fatalf("calls = %q: the driver saw an inherited plan", got)
	}
}
