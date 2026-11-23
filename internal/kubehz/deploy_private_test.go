package kubehz

// deploy_private_test.go — each kubectl apply of the deploy keeps the
// kubectl output private (kapply.RunPrivate): the output of an apply can
// quote a value of the manifest, and the bind secret holds a credential.
// lo prints fixed lines; the kubectl text prints only under
// LO_SHOW_SENSITIVE_OUTPUT=1. The bats twin is "deploy_apply: each kubectl
// apply keeps the kubectl output private, in both directions" in
// tests/unit/kubehz_live_agent_test.bats.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
)

const (
	// applyCanary stands for a value of the manifest that kubectl quotes on
	// its stdout, applyErrCanary for one on its stderr. Both are made up.
	applyCanary    = "zq7DeployCanary3vQs"
	applyErrCanary = "zq7DeployStderrCanary8nTb"
)

// leakingApplies makes each kubectl apply of h write one object line and
// the canaries to its stdout and stderr. A stream that is nil, or the
// stream of the process, is the terminal: the canary then goes to term.
func leakingApplies(h *harness, term *bytes.Buffer) {
	inner := h.runner.handler
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		var err error
		if inner != nil {
			err = inner(c, stdin)
		}
		if c.Name == "kubectl" && len(c.Args) > 0 && c.Args[0] == "apply" {
			io.WriteString(visible(c.Stdout, term), "configmap/x configured\nInvalid value: \""+applyCanary+"\"\n")
			io.WriteString(visible(c.Stderr, term), "Warning: the value \""+applyErrCanary+"\" is deprecated\n")
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

// Each apply of the deploy (the CronJob agent, the live agent, the
// namespace and the bind secret of the stage) prints its summary line, and
// no canary reaches the output, in both directions. Under the opt-in, the
// output shows the canaries.
func TestDeployApplyKeepsTheKubectlOutputPrivate(t *testing.T) {
	for _, mode := range []struct{ owner, access string }{{"operator", "managed"}, {"cronjob", "registered"}} {
		t.Run(mode.owner, func(t *testing.T) {
			for _, show := range []bool{false, true} {
				t.Setenv(kapply.ShowSensitiveEnv, "")
				if show {
					t.Setenv(kapply.ShowSensitiveEnv, "1")
				}
				h := newHarness(t)
				work := renderInto(t, h, mode.owner, mode.access)
				writeBindSecret(t, h, "acme.example.com", testBindSecret)
				_ = bindStageLogger(h, "")
				var term bytes.Buffer
				leakingApplies(h, &term)
				mustOK(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", mode.owner, mode.access), h.output())
				all := h.output() + term.String()
				if show {
					mustContain(t, all, applyCanary)
					mustContain(t, all, applyErrCanary)
					mustContain(t, all, "[warn] kapply: "+kapply.ShowSensitiveText("the kubectl output for the bind secret"))
					continue
				}
				mustNotContain(t, all, applyCanary)
				mustNotContain(t, all, applyErrCanary)
				labels := []string{"the namespace kubehz-system", "the bind secret", "the CronJob agent"}
				if mode.owner == "operator" {
					labels = append(labels, "the live agent")
				}
				for _, l := range labels {
					mustContain(t, all, "✓ "+l+" · 1 resource\n")
				}
				if strings.Contains(all, "kapply: kubectl apply failed") {
					t.Fatalf("a clean deploy printed a fixed failure line:\n%s", all)
				}
			}
		})
	}
}

// A failed apply prints the fixed line of its label, and the error of the
// step, never the kubectl output.
func TestDeployApplyFailedApplyPrintsTheFixedLine(t *testing.T) {
	t.Setenv(kapply.ShowSensitiveEnv, "")
	h := newHarness(t)
	work := renderInto(t, h, "operator", "managed")
	_ = bindStageLogger(h, "apply -k "+work+"/live-agent")
	var term bytes.Buffer
	leakingApplies(h, &term)
	mustErr(t, h.ctx.deployApply(t.Context(), work, "acme.example.com", "operator", "managed"))
	all := h.output() + term.String()
	mustContain(t, all, "[error] "+kapply.ApplyFailedText("the live agent", 1))
	mustContain(t, all, "kubehz: could not apply the live agent.")
	mustNotContain(t, all, applyCanary)
	mustNotContain(t, all, applyErrCanary)
}
