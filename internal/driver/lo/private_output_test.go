package lo

// private_output_test.go — each kubectl apply of the Lo driver (the CoreDNS
// files, the coredns-custom ConfigMap, the local-registry-hosting and the
// lok8s-registries ConfigMaps) keeps the kubectl output private
// (applyPrivate, kapply.RunPrivate): the output of an apply can quote a
// value of the manifest. lo prints a summary line; the kubectl text prints
// only under LO_SHOW_SENSITIVE_OUTPUT=1. The bats twins are the "private:"
// cases of tests/unit/coredns_spec_test.bats and
// tests/unit/network_config_test.bats.

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
	"github.com/kernpilot/lok8s/internal/testutil"
)

const (
	// loCanary stands for a value of the manifest that kubectl quotes on
	// its stdout, loErrCanary for one on its stderr. Both are made up.
	loCanary    = "zq7LoCanary5hRd"
	loErrCanary = "zq7LoStderrCanary2wXe"
)

// leakingApplies wraps the handler of runner: each kubectl apply writes one
// object line and the canaries to its stdout and stderr. A stream that is
// nil, or a stream of the process, is the terminal: the canary then goes to
// term.
func leakingApplies(runner *fakeRunner, term *bytes.Buffer) {
	inner := runner.handler
	runner.handler = func(c execx.Cmd) error {
		var err error
		if inner != nil {
			err = inner(c)
		}
		if c.Name == "kubectl" && slices.Contains(c.Args, "apply") {
			io.WriteString(visible(c.Stdout, term), "configmap/x configured\nInvalid value: \""+loCanary+"\"\n")
			io.WriteString(visible(c.Stderr, term), "Warning: the value \""+loErrCanary+"\" is deprecated\n")
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

func TestDriverAppliesKeepTheKubectlOutputPrivate(t *testing.T) {
	type site struct {
		name   string
		labels []string
		run    func(t *testing.T, out, errOut io.Writer, term *bytes.Buffer)
	}
	sites := []site{
		{"coredns", []string{"the CoreDNS Corefile", "the CoreDNS external Service", "the CoreDNS custom ConfigMap"},
			func(t *testing.T, out, errOut io.Writer, term *bytes.Buffer) {
				d, runner, _ := corednsFixture(t, "  coredns:\n    hosts:\n      - name: kubehz.dev\n        target: gateway\n")
				leakingApplies(runner, term)
				if err := d.coredns(t.Context(), out, errOut, "test.lok8s.dev"); err != nil {
					t.Fatal(err)
				}
			}},
		{"registries", []string{"the registries ConfigMap", "the local registry hosting ConfigMap"},
			func(t *testing.T, out, errOut io.Writer, term *bytes.Buffer) {
				d, runner, _, _, p, cy := lifecycleDriver(t)
				testutil.WriteFile(t, filepath.Join(p.Base, ".kubeconfig", "test-lifecycle.yaml"), "apiVersion: v1\n")
				leakingApplies(runner, term)
				if err := d.registryConfigmap(t.Context(), out, errOut, "test.lok8s.dev", cy); err != nil {
					t.Fatal(err)
				}
				if err := d.applyLocalRegistryHosting(t.Context(), out, errOut, "test.lok8s.dev"); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, s := range sites {
		for _, show := range []bool{false, true} {
			t.Run(s.name+map[bool]string{false: "/private", true: "/opt-in"}[show], func(t *testing.T) {
				t.Setenv(kapply.ShowSensitiveEnv, "")
				if show {
					t.Setenv(kapply.ShowSensitiveEnv, "1")
				}
				var out, errOut, term bytes.Buffer
				s.run(t, &out, &errOut, &term)
				all := out.String() + errOut.String() + term.String()
				if show {
					for _, want := range []string{loCanary, loErrCanary, "[warn] kapply: " + kapply.ShowSensitiveText("the kubectl output for "+s.labels[0])} {
						if !strings.Contains(all, want) {
							t.Errorf("the opt-in output lacks %q:\n%s", want, all)
						}
					}
					return
				}
				for _, leak := range []string{loCanary, loErrCanary} {
					if strings.Contains(all, leak) {
						t.Errorf("the output holds %q:\n%s", leak, all)
					}
				}
				for _, l := range s.labels {
					if !strings.Contains(out.String(), "✓ "+l+" · 1 resource\n") {
						t.Errorf("stdout lacks the summary of %s:\n%s", l, out.String())
					}
				}
			})
		}
	}
}
