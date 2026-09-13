package cli

// cmd_provision_test.go covers the command-tree wiring of provision,
// destroy and bootstrap: flag and positional errors in the argsh shape, and
// the refusals that need no driver (deploy domains, missing specs). The
// exec seam is a fake for every test. Nothing here may reach
// docker, kind, tilt or bash.

import (
	"errors"
	"strings"
	"testing"
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
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "No cluster.lok8s.yaml or deploy.lok8s.yaml found in .lok8s/nope.dev/") {
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
