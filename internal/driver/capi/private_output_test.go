package capi

// private_output_test.go: each kubectl apply of the capi driver (the
// namespace and the resources on the management cluster, the credentials
// Secret, the resources on the bootstrap cluster and the operator deploy
// dir) keeps the kubectl output private (kapply.RunPrivate): the output of
// an apply can quote a value of the manifest, and the credentials Secret
// holds the provider credentials. The kubectl text prints only under
// LO_SHOW_SENSITIVE_OUTPUT=1. The bats twins are the "private:" cases of
// tests/unit/capi_test.bats and tests/unit/capi_bootstrap_test.bats.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
)

const (
	// mgmtCanary stands for a value of the manifest that kubectl quotes on
	// its stdout, mgmtErrCanary for one on its stderr. Both are made up.
	mgmtCanary    = "zq7MgmtCanary6pLn"
	mgmtErrCanary = "zq7MgmtStderrCanary1cVy"
)

// leakingApplies wraps the handler of runner: each kubectl apply writes one
// object line and the canaries to its stdout and stderr, and the argv of
// the apply goes to applies. A stream that is nil, or a stream of the
// process, is the terminal: the canary then goes to term.
func leakingApplies(runner *fakeRunner, term *bytes.Buffer, applies *[]string) {
	inner := runner.handler
	runner.handler = func(c execx.Cmd, stdin string) error {
		var err error
		if inner != nil {
			err = inner(c, stdin)
		}
		if c.Name == "kubectl" && slices.Contains(c.Args, "apply") {
			*applies = append(*applies, argvLine(c))
			io.WriteString(visible(c.Stdout, term), "secret/x configured\nInvalid value: \""+mgmtCanary+"\"\n")
			io.WriteString(visible(c.Stderr, term), "Warning: the value \""+mgmtErrCanary+"\" is deprecated\n")
		}
		return err
	}
}

// visible is w, or term when w is nil or a stream of the process.
func visible(w io.Writer, term *bytes.Buffer) io.Writer {
	if w == nil || w == os.Stdout || w == os.Stderr {
		return term
	}
	return w
}

func TestKubectlApplyOutputStaysPrivate(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "test-token")
	t.Setenv("HROBOT_USER", "")
	t.Setenv("HROBOT_PASSWORD", "")
	for name, run := range map[string]struct {
		applies int
		run     func(t *testing.T, term *bytes.Buffer, applies *[]string) *bytes.Buffer
	}{
		// capi.go: the namespace and the resources on the management
		// cluster, and the credentials Secret (generate.go).
		"provision": {3, func(t *testing.T, term *bytes.Buffer, applies *[]string) *bytes.Buffer {
			d, runner, stderr := provisionSetup(t)
			leakingApplies(runner, term, applies)
			if err := d.Provision(t.Context(), "test.dev"); err != nil {
				t.Fatalf("Provision: %v\n%s", err, stderr.String())
			}
			return stderr
		}},
		// capi.go: the resources on the bootstrap cluster, the credentials
		// Secret on it, and the operator deploy dir on the management
		// cluster.
		"bootstrap": {3, func(t *testing.T, term *bytes.Buffer, applies *[]string) *bytes.Buffer {
			d, runner, stderr := testDriver(t)
			bootstrapSpec(t, d)
			if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, "operator", "deploy"), 0o755); err != nil {
				t.Fatal(err)
			}
			runner.handler = happyHandler
			leakingApplies(runner, term, applies)
			if err := d.Bootstrap(t.Context(), "mgmt.lok8s.dev"); err != nil {
				t.Fatalf("Bootstrap: %v\n%s", err, stderr.String())
			}
			return stderr
		}},
		"credentials": {1, func(t *testing.T, term *bytes.Buffer, applies *[]string) *bytes.Buffer {
			d, runner, stderr := testDriver(t)
			leakingApplies(runner, term, applies)
			if err := d.EnsureCredentialsSecret(t.Context(), hetznerFixture(t), "hetzner", "/tmp/kubeconfig.yaml"); err != nil {
				t.Fatal(err)
			}
			return stderr
		}},
	} {
		for _, show := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/private", true: "/opt-in"}[show], func(t *testing.T) {
				t.Setenv(kapply.ShowSensitiveEnv, "")
				if show {
					t.Setenv(kapply.ShowSensitiveEnv, "1")
				}
				var term bytes.Buffer
				var applies []string
				stderr := run.run(t, &term, &applies)
				if len(applies) != run.applies {
					t.Fatalf("%d kubectl applies ran, want %d:\n%s", len(applies), run.applies, strings.Join(applies, "\n"))
				}
				all := stderr.String() + term.String()
				if show {
					if !strings.Contains(all, mgmtCanary) || !strings.Contains(all, "[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1") {
						t.Errorf("the opt-in did not print the kubectl output:\n%s", all)
					}
					return
				}
				for _, leak := range []string{mgmtCanary, mgmtErrCanary} {
					if strings.Contains(all, leak) {
						t.Errorf("the output holds %q:\n%s", leak, all)
					}
				}
			})
		}
	}
}
