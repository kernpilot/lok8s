package operator

// private_output_test.go: each kubectl apply of the hooks (the CAPI
// manifests of capi-reconcile, the kubeconfig Secret of capi-status-sync and
// of lo-reconcile) keeps the kubectl output private (kube.applyPrivate,
// kapply.RunPrivate): the output of an apply can quote a value of the
// manifest, and a kubeconfig Secret holds a credential. The kubectl text
// prints only under LO_SHOW_SENSITIVE_OUTPUT=1.

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
)

const (
	// hookCanary stands for a value of the manifest that kubectl quotes on
	// its stdout, hookErrCanary for one on its stderr. Both are made up.
	hookCanary    = "zq7HookCanary4mJt"
	hookErrCanary = "zq7HookStderrCanary7bQz"
)

// leakingApplies wraps the handler of r: each kubectl apply writes one
// object line and the canaries to its stdout and stderr. A stream that is
// nil, or a stream of the process, is the terminal: the canary then goes to
// term. It returns the number of applies so far.
func leakingApplies(r *fakeRunner, term *bytes.Buffer) *int {
	n := 0
	inner := r.handler
	r.handler = func(c execx.Cmd) error {
		var err error
		if inner != nil {
			err = inner(c)
		}
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "apply" {
			n++
			io.WriteString(visible(c.Stdout, term), "secret/x configured\nInvalid value: \""+hookCanary+"\"\n")
			io.WriteString(visible(c.Stderr, term), "Warning: the value \""+hookErrCanary+"\" is deprecated\n")
		}
		return err
	}
	return &n
}

// visible is w, or term when w is nil or a stream of the process.
func visible(w io.Writer, term *bytes.Buffer) io.Writer {
	if w == nil || w == os.Stdout || w == os.Stderr {
		return term
	}
	return w
}

func TestHookAppliesKeepTheKubectlOutputPrivate(t *testing.T) {
	type run func(t *testing.T, term *bytes.Buffer) (applies *int, out, errOut *bytes.Buffer)
	for name, r := range map[string]run{
		"capi-reconcile": func(t *testing.T, term *bytes.Buffer) (*int, *bytes.Buffer, *bytes.Buffer) {
			f := newCapiFixture(t)
			f.stubTemplates(t)
			n := leakingApplies(f.runner, term)
			f.hook.Reconcile(t.Context(), []byte(capiCR))
			return n, f.stdout, f.stderr
		},
		"capi-status-sync": func(t *testing.T, term *bytes.Buffer) (*int, *bytes.Buffer, *bytes.Buffer) {
			f := newStatusFixture(t)
			f.runner.handler = func(c execx.Cmd) error {
				a := argv(c)
				switch {
				case strings.HasPrefix(a, "clusterctl get kubeconfig prod -n clusters"):
					writeOut(c, "apiVersion: v1\nkind: Config\n")
				case strings.Contains(a, "jsonpath={.spec.cluster.domain}"):
					writeOut(c, "prod.example.com")
				case strings.HasPrefix(a, "kubectl create secret"):
					writeOut(c, "kind: Secret\n")
				}
				return nil
			}
			f.hook.DeployApply = func(ctx context.Context, domain string) error { return nil }
			n := leakingApplies(f.runner, term)
			if err := f.hook.Trigger(t.Context(), mustEvents(t, strings.Replace(clusterEvent, "%s", "Provisioned", 1))); err != nil {
				t.Fatal(err)
			}
			return n, f.stdout, f.stderr
		},
		"lo-reconcile": func(t *testing.T, term *bytes.Buffer) (*int, *bytes.Buffer, *bytes.Buffer) {
			f := newLoFixture(t)
			n := leakingApplies(f.runner, term)
			if err := f.hook.Reconcile(t.Context(), []byte(loCR)); err != nil {
				t.Fatal(err)
			}
			return n, f.stdout, f.stderr
		},
	} {
		for _, show := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/private", true: "/opt-in"}[show], func(t *testing.T) {
				t.Setenv(kapply.ShowSensitiveEnv, "")
				if show {
					t.Setenv(kapply.ShowSensitiveEnv, "1")
				}
				var term bytes.Buffer
				n, out, errOut := r(t, &term)
				if *n != 1 {
					t.Fatalf("%d kubectl applies ran, want 1", *n)
				}
				all := out.String() + errOut.String() + term.String()
				// capi-status-sync drops the stderr of its apply (io.Discard, as
				// before), so the opt-in prints nothing there.
				if show && name != "capi-status-sync" {
					if !strings.Contains(all, "[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1") {
						t.Errorf("the opt-in printed no [warn] line:\n%s", all)
					}
					return
				}
				if show {
					return
				}
				for _, leak := range []string{hookCanary, hookErrCanary} {
					if strings.Contains(all, leak) {
						t.Errorf("the output holds %q:\n%s", leak, all)
					}
				}
			})
		}
	}
}
