package cli

// cmd_provision_test.go covers the command-tree wiring of provision,
// destroy and bootstrap: flag and positional errors in the argsh shape, and
// the refusals that need no driver (deploy domains, missing specs). The
// exec seam is a fake for every test. Nothing here may reach
// docker, kind, tilt or bash.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestOrchestrateArgshShapedParseErrors(t *testing.T) {
	p, _, _ := orchestrateProject(t)
	for _, argv := range [][]string{
		{"provision", "extra"}, {"destroy", "extra"}, {"bootstrap", "extra"}, {"status", "extra"}, {"clean", "extra"}, {"up", "extra"},
	} {
		_, stderr, err := runLo(t, NewRoot(p), argv...)
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: too many arguments: extra") || !strings.Contains(stderr, `Run "lo -h" for more information.`) {
			t.Errorf("%v: err=%v stderr=%q", argv, err, stderr)
		}
	}
	for _, argv := range [][]string{{"provision", "--bogus"}, {"up", "--bogus"}, {"clean", "--bogus"}, {"status", "--bogus"}} {
		_, stderr, err := runLo(t, NewRoot(p), argv...)
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: unknown flag: --bogus\n\n  Run \"lo -h\"") {
			t.Errorf("%v: err=%v stderr=%q", argv, err, stderr)
		}
	}
	// bash: main::down has no :args — positionals AND unknown flags are
	// dropped; registry::* likewise.
	_, stderr, err := runLo(t, NewRoot(p), "registry", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: Invalid command: bogus") {
		t.Errorf("registry bogus: err=%v stderr=%q", err, stderr)
	}
}

func TestProvisionDeployDomainRefusal(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--domain", "gamma.app")
	if !errors.Is(err, ErrHandled) || len(*exits) != 0 {
		t.Fatalf("err=%v exits=%v", err, *exits)
	}
	for _, want := range []string{"lo provision: domain gamma.app\n", "Cannot provision a deployment domain. Use 'lo deploy gamma.app' instead.", "Deployment domains reference a cluster via spec.clusterRef.domain."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q: %q", want, stderr)
		}
	}
	_, stderr, err = runLo(t, NewRoot(p), "p", "--domain", "nope.dev")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "No cluster.lok8s.yaml or deploy.lok8s.yaml found in clusters/nope.dev/") {
		t.Errorf("alias/no spec: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "provision", "--domain", "nokind.dev")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "cluster spec has no .kind:") {
		t.Errorf("no kind: err=%v stderr=%q", err, stderr)
	}
}

// The real-infrastructure gate refuses non-interactively with the rc-3
// sentinel — passed through as the process exit (bash: dispatch returns 3,
// main::provision's status IS main's).
func TestProvisionGateDeclineExitsThree(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--domain", "beta.cloud")
	if !errors.Is(err, ErrHandled) || len(*exits) != 1 || (*exits)[0] != 3 {
		t.Fatalf("err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
	if !strings.Contains(stderr, "targets \033[1mreal infrastructure\033[0m") || !strings.Contains(stderr, "refusing to reconcile 'beta.cloud' non-interactively — re-run with --force") {
		t.Errorf("stderr = %q", stderr)
	}
	*exits = nil
	_, stderr, _ = runLo(t, NewRoot(p), "provision", "--bootstrap", "--domain", "beta.cloud")
	if len(*exits) != 1 || (*exits)[0] != 3 || !strings.Contains(stderr, "next: re-apply 1 bootstrap addons on the LIVE cluster") {
		t.Errorf("bootstrap gate: exits=%v stderr=%q", *exits, stderr)
	}
	*exits = nil
	_, stderr, _ = runLo(t, NewRoot(p), "destroy", "--domain", "beta.cloud")
	if len(*exits) != 1 || (*exits)[0] != 3 || !strings.Contains(stderr, "refusing to destroy 'beta.cloud' non-interactively") {
		t.Errorf("destroy gate: exits=%v stderr=%q", *exits, stderr)
	}
}

func TestDestroyAndBootstrapRefusals(t *testing.T) {
	p, _, _ := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "destroy", "--domain", "gamma.app")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Cannot destroy a deployment domain. Destroy the cluster domain instead.") {
		t.Errorf("destroy deploy: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "bootstrap", "--domain", "gamma.app")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "cluster spec not found:") {
		t.Errorf("bootstrap deploy: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "bootstrap", "--domain", "../evil")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "invalid domain name: ../evil") {
		t.Errorf("bootstrap traversal: err=%v stderr=%q", err, stderr)
	}
}

// The run-mode flags: --render-dir/--no-diff need --dry-run, and on
// provision all four need --bootstrap (bash: the same error lines, rc 1 —
// rc 2 when the command line asks for --dry-run: a dry run's every failure
// outside the per-entry diff is rc 2).
func TestBootstrapModeFlagDependencies(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--dry-run", "--domain", "alpha.dev")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "[error] --dry-run requires --bootstrap") || len(*exits) != 1 || (*exits)[0] != 2 {
		t.Errorf("provision --dry-run: err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
	*exits = nil
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"provision", "--only", "cilium", "--domain", "alpha.dev"}, "--only requires --bootstrap"},
		{[]string{"provision", "--render-dir", "x", "--domain", "alpha.dev"}, "--render-dir requires --bootstrap"},
		{[]string{"provision", "--allow-generators", "--domain", "alpha.dev"}, "--allow-generators requires --bootstrap"},
		{[]string{"provision", "-b", "--no-diff", "--domain", "alpha.dev"}, "--no-diff requires --dry-run"},
		{[]string{"provision", "-b", "--allow-generators", "--domain", "alpha.dev"}, "--allow-generators requires --dry-run"},
		{[]string{"bootstrap", "--render-dir", "x", "--domain", "alpha.dev"}, "--render-dir requires --dry-run"},
		{[]string{"bootstrap", "--no-diff", "--domain", "alpha.dev"}, "--no-diff requires --dry-run"},
		{[]string{"bootstrap", "--allow-generators", "--domain", "alpha.dev"}, "--allow-generators requires --dry-run"},
	} {
		_, stderr, err := runLo(t, NewRoot(p), tc.argv...)
		if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "[error] "+tc.want) || len(*exits) != 0 {
			t.Errorf("%v: err=%v exits=%v stderr=%q", tc.argv, err, *exits, stderr)
		}
	}
}

// A bootstrap dry run is not gated: on a cloud domain without --force it
// passes the real-infrastructure gate and stops at the next guard (no
// kubeconfig yet) with rc 2 (a failure outside the per-entry diff), never
// the decline's rc 3.
func TestProvisionBootstrapDryRunIsNotGated(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--bootstrap", "--dry-run", "--domain", "beta.cloud")
	if !errors.Is(err, ErrHandled) || len(*exits) != 1 || (*exits)[0] != 2 {
		t.Fatalf("err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
	if strings.Contains(stderr, "real infrastructure") || !strings.Contains(stderr, "--bootstrap needs an existing cluster") {
		t.Errorf("stderr = %q", stderr)
	}
}

// --only GIVEN but blank (every value whitespace/empty after a comma split)
// is an error, never "no filter" — the latter is what an ABSENT --only
// means, and the two must not silently collapse into the same full,
// unfiltered apply.
func TestBootstrapOnlyBlankIsAnError(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	// "" is the review round-2 escape (item E): pflag's StringSliceVar
	// parses an EMPTY value through a CSV reader that returns ZERO
	// elements for "" — len(f.only) alone cannot tell "--only ''" apart
	// from "--only never given" at all; only cmd.Flags().Changed can.
	for _, only := range []string{"", " ", ",", " , "} {
		_, stderr, err := runLo(t, NewRoot(p), "bootstrap", "--only", only, "--domain", "alpha.dev")
		if !errors.Is(err, ErrHandled) || len(*exits) != 0 || !strings.Contains(stderr, "[error] --only: no entry names given") {
			t.Errorf("--only %q: err=%v exits=%v stderr=%q", only, err, *exits, stderr)
		}
	}
}

// --with-restore requires --only (and, transitively, --bootstrap on
// `lo provision`) — review round 2, item H's flag.
func TestWithRestoreRequiresOnly(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "bootstrap", "--with-restore", "--domain", "alpha.dev")
	if !errors.Is(err, ErrHandled) || len(*exits) != 0 || !strings.Contains(stderr, "[error] --with-restore requires --only") {
		t.Errorf("err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
}

func TestWithRestoreRequiresBootstrapOnProvision(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	_, stderr, err := runLo(t, NewRoot(p), "provision", "--only", "cilium", "--with-restore", "--domain", "alpha.dev")
	if !errors.Is(err, ErrHandled) || len(*exits) != 0 || !strings.Contains(stderr, "[error] --only requires --bootstrap") {
		t.Errorf("err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
}

// --only reaches the engine: an unknown name lists the valid entries.
func TestBootstrapOnlyUnknownNameListsEntries(t *testing.T) {
	p, _, exits := orchestrateProject(t)
	testutil.WriteFile(t, filepath.Join(p.Base, ".kubeconfig", "onlyc.yaml"), "")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "only.lok8s.dev", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: onlyc\nspec:\n  runtime: kind\n  network:\n    name: onlynet\n    cidr: 10.99.12.0/24\n  registries:\n    tls: false\n  bootstrap:\n    - cilium\n    - ccm: {name: cloud}\n")
	_, stderr, err := runLo(t, NewRoot(p), "bootstrap", "--only", "cilium,nope", "--domain", "only.lok8s.dev")
	if !errors.Is(err, ErrHandled) || len(*exits) != 0 || !strings.Contains(stderr, "bootstrap: --only: unknown entry 'nope' (valid: cilium, cloud)") {
		t.Errorf("err=%v exits=%v stderr=%q", err, *exits, stderr)
	}
}
