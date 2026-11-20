package kubehz

// The workflows that run `lo provision` with HCLOUD_TOKEN
// (.github/workflows/spinup.yml and provision.yml) hide the one-time claim
// ticket, the fingerprint of the claim key, in the job log: a public repo
// shows its job logs to everyone, and any signed-in kubehz user who has the
// ticket can claim the cluster. The filter is the shell function
// hide_claim_ticket, inline in each workflow (a reusable workflow runs in the
// checkout of the calling repo, where no lok8s script exists). These tests
// run it on the register output of this implementation. The bats twin
// (tests/unit/kubehz_register_test.bats) runs it on the output of the bash
// implementation.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// testTicket is an obvious fake claim ticket.
	testTicket = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
	// ticketHidden is what the filter writes in place of the ticket.
	ticketHidden = "<hidden: one-time claim ticket>"
)

// claimTicketSteps are the steps that run `lo provision` with HCLOUD_TOKEN.
var claimTicketSteps = []struct{ file, job, step string }{
	{"spinup.yml", "spinup", "Provision & register"},
	{"provision.yml", "provision", "Provision"},
}

type workflowFile struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string            `yaml:"name"`
			If   string            `yaml:"if"`
			Env  map[string]string `yaml:"env"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, file string) (string, workflowFile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", file))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return string(raw), wf
}

// stepRun is the run script of the named step.
func stepRun(t *testing.T, file, job, step string) string {
	t.Helper()
	_, wf := readWorkflow(t, file)
	for _, s := range wf.Jobs[job].Steps {
		if s.Name == step {
			return s.Run
		}
	}
	t.Fatalf("%s: no step %q in job %q", file, step, job)
	return ""
}

// hideClaimTicketFunc is the definition of hide_claim_ticket in a run
// script: from its first line to the first line that is only "}".
func hideClaimTicketFunc(t *testing.T, run string) string {
	t.Helper()
	lines := strings.Split(run, "\n")
	for i, l := range lines {
		if l != "hide_claim_ticket() {" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n") + "\n"
			}
		}
	}
	t.Fatal("no hide_claim_ticket function in the run script")
	return ""
}

// runHideClaimTicket runs the function on input in a UTF-8 locale, so the
// function's own LC_ALL=C is what makes it compare bytes.
func runHideClaimTicket(t *testing.T, fn, input string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", fn+"hide_claim_ticket")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hide_claim_ticket: %v", err)
	}
	return string(out)
}

// registerOutput is the output of RegisterCluster for a Lo cluster: with
// HCLOUD_TOKEN the claim-key registration (its answer carries testTicket),
// without it the fingerprint announce.
func registerOutput(t *testing.T, claimKey bool) string {
	t.Helper()
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	if claimKey {
		h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
		h.handle("POST /api/clusters/register", 200,
			`{"id":"cl-5","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"`+testTicket+`","name":"kubehz-claim-test.kubehz.dev"}}`)
		h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[]}`)
		h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
	} else {
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-001","registered":true}`)
	}
	cfg := &Config{APIURL: h.apiURL(), Access: "registered"}
	mustOK(t, h.ctx.RegisterCluster(t.Context(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
	return h.output()
}

// nearMisses are lines that look like the ticket lines but are not: the
// filter must pass them unchanged. A filter with a looser match changes them.
const nearMisses = "  fingerprint: 11:22   (also visible in Hetzner Console -> Security -> SSH keys) and more\n" +
	"   fingerprint: 11:22   (also visible in Hetzner Console -> Security -> SSH keys)\n" +
	"x       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"11:22\"}'\n" +
	"       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"11:22\"}' and more\n" +
	"a line without a fingerprint\n"

// Every step that runs `lo provision` with HCLOUD_TOKEN has the same
// filter, and pipes every `lo provision` through it, stdout and stderr,
// under pipefail. spinup.yml tees the filtered stream.
func TestClaimTicketWorkflowsFilterEveryProvision(t *testing.T) {
	var fns []string
	for _, s := range claimTicketSteps {
		run := stepRun(t, s.file, s.job, s.step)
		fns = append(fns, hideClaimTicketFunc(t, run))
		if !strings.HasPrefix(run, "set -o pipefail\n") {
			t.Errorf("%s: the run script of %q does not start with set -o pipefail", s.file, s.step)
		}
		provisions := 0
		for _, l := range strings.Split(run, "\n") {
			if !strings.Contains(l, "lo provision") {
				continue
			}
			provisions++
			if !strings.Contains(l, " 2>&1 | hide_claim_ticket") {
				t.Errorf("%s: %q does not pipe stdout and stderr through hide_claim_ticket", s.file, strings.TrimSpace(l))
			}
		}
		if provisions == 0 {
			t.Errorf("%s: no lo provision in the step %q", s.file, s.step)
		}
	}
	if fns[0] != fns[1] {
		t.Errorf("hide_claim_ticket differs between %s and %s:\n%s\n%s", claimTicketSteps[0].file, claimTicketSteps[1].file, fns[0], fns[1])
	}
	run := stepRun(t, "spinup.yml", "spinup", "Provision & register")
	if !strings.Contains(run, `lo provision --force --domain "${DOMAIN}" 2>&1 | hide_claim_ticket | tee /tmp/provision.log`) {
		t.Error("spinup.yml: the provision does not tee the filtered stream")
	}
}

// No step output or env value of the workflows carries the fingerprint:
// the only step outputs read are the registration mode and the name of the
// claim key (spinup.yml) and the changed domains (provision.yml), and no
// env name or value names a fingerprint.
func TestClaimTicketWorkflowsPassNoFingerprint(t *testing.T) {
	ref := regexp.MustCompile(`steps\.[a-z_-]+\.outputs\.([A-Za-z0-9_-]+)`)
	for _, s := range claimTicketSteps {
		raw, wf := readWorkflow(t, s.file)
		for _, m := range ref.FindAllStringSubmatch(raw, -1) {
			if s.file == "spinup.yml" && (m[1] == "registration" || m[1] == "claim_key") {
				continue
			}
			if s.file == "provision.yml" && m[1] == "domains" {
				continue
			}
			t.Errorf("%s: the step output %q is read", s.file, m[1])
		}
		for job, j := range wf.Jobs {
			for _, st := range j.Steps {
				for k, v := range st.Env {
					if strings.Contains(strings.ToLower(k+v), "fingerprint") {
						t.Errorf("%s: job %s, step %q: env %s=%s", s.file, job, st.Name, k, v)
					}
				}
			}
		}
	}
}

// The filter on the register output of this implementation: the ticket is
// gone from both lines that carry it, every other line stays byte for byte
// (the announce line with its public fingerprint, the near misses, a line
// without a fingerprint), and a ticket line with a byte that is not UTF-8
// is hidden too.
func TestHideClaimTicketOnTheRegisterOutput(t *testing.T) {
	claim := registerOutput(t, true)
	if n := strings.Count(claim, testTicket); n != 2 {
		t.Fatalf("the claim-key registration prints the ticket %d times, want 2 (the curl hint and the fingerprint line):\n%s", n, claim)
	}
	announce := registerOutput(t, false)
	mustContain(t, announce, "\n  fingerprint: lo:test.kubehz.dev\n")
	notUTF8 := "  fingerprint: \xff" + testTicket + "   (also visible in Hetzner Console -> Security -> SSH keys)\n"
	input := claim + announce + nearMisses + notUTF8 + "the last line without a newline"
	want := strings.ReplaceAll(input, "\xff"+testTicket, ticketHidden)
	want = strings.ReplaceAll(want, testTicket, ticketHidden)
	for _, s := range claimTicketSteps {
		t.Run(s.file, func(t *testing.T) {
			got := runHideClaimTicket(t, hideClaimTicketFunc(t, stepRun(t, s.file, s.job, s.step)), input)
			if strings.Contains(got, testTicket) {
				t.Errorf("the ticket is in the filtered output:\n%s", got)
			}
			if got != want {
				t.Errorf("filtered output:\n%q\nwant:\n%q", got, want)
			}
			if n := strings.Count(got, ticketHidden); n != 3 {
				t.Errorf("%d hidden lines, want 3", n)
			}
		})
	}
}

// runStep runs a run script the way the runner does (bash -e), in dir, with
// env added to a small base environment. It returns stdout and stderr
// together, and the exit code.
func runStep(t *testing.T, dir, run string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-e", "-c", run)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8"}, env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("run: %v", err)
	}
	return string(out), ee.ExitCode()
}

// The two steps of spinup.yml end to end, with a stub lo that prints the
// register output of this implementation and exits with a given status:
// the ticket reaches no output of the provision step (its log, the tee
// file, GITHUB_OUTPUT), and the summary names the claim key but never the
// ticket. A provision that fails after the registration still writes the
// outputs, and the step still fails with the status of lo.
func TestSpinupStepsKeepTheTicketOut(t *testing.T) {
	provision := stepRun(t, "spinup.yml", "spinup", "Provision & register")
	if n := strings.Count(provision, "/tmp/provision.log"); n != 2 {
		t.Fatalf("the provision step names /tmp/provision.log %d times, want 2 (tee and read)", n)
	}
	summary := stepRun(t, "spinup.yml", "spinup", "Claim instructions")
	_, wf := readWorkflow(t, "spinup.yml")
	for _, s := range wf.Jobs["spinup"].Steps {
		if s.Name == "Claim instructions" && s.If != "always() && steps.provision.outputs.registration != ''" {
			t.Errorf("the summary step runs if %q", s.If)
		}
	}
	for _, c := range []struct {
		name, mode, outputs, mustSay string
		claimKey                     bool
		rc                           int
	}{
		{"claim key", "claim-key", "registration=claim-key\nclaim_key=kubehz-claim-test.kubehz.dev\n", "the SSH key `kubehz-claim-test.kubehz.dev`", true, 0},
		{"claim key, then the provision fails", "claim-key", "registration=claim-key\nclaim_key=kubehz-claim-test.kubehz.dev\n", "the SSH key `kubehz-claim-test.kubehz.dev`", true, 3},
		{"fingerprint announce", "announce", "registration=announce\nclaim_key=\n", "`lo kubehz claim-code`", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			testWrite(t, filepath.Join(dir, "lo.out"), registerOutput(t, c.claimKey))
			testWrite(t, filepath.Join(dir, "bin", "lo"), "#!/bin/sh\ncat \"$LO_OUTPUT\"\nexit \"$LO_RC\"\n")
			if err := os.Chmod(filepath.Join(dir, "bin", "lo"), 0o755); err != nil {
				t.Fatal(err)
			}
			logFile := filepath.Join(dir, "provision.log")
			out, rc := runStep(t, dir, strings.ReplaceAll(provision, "/tmp/provision.log", logFile),
				"DOMAIN=test.kubehz.dev", "GITHUB_OUTPUT="+filepath.Join(dir, "outputs"),
				"LO_OUTPUT="+filepath.Join(dir, "lo.out"), "LO_RC="+strconv.Itoa(c.rc))
			if rc != c.rc {
				t.Fatalf("the provision step exits %d, want %d:\n%s", rc, c.rc, out)
			}
			outputs := testRead(t, filepath.Join(dir, "outputs"))
			log := testRead(t, logFile)
			if outputs != c.outputs {
				t.Errorf("GITHUB_OUTPUT = %q, want %q", outputs, c.outputs)
			}
			for name, s := range map[string]string{"the step log": out, "the tee file": log, "GITHUB_OUTPUT": outputs} {
				if strings.Contains(s, testTicket) {
					t.Errorf("the ticket is in %s:\n%s", name, s)
				}
			}
			if c.claimKey && strings.Count(out, ticketHidden) != 2 {
				t.Errorf("the step log has %d hidden lines, want 2:\n%s", strings.Count(out, ticketHidden), out)
			}

			key := strings.TrimPrefix(strings.Split(c.outputs, "\n")[1], "claim_key=")
			_, rc = runStep(t, dir, summary, "DOMAIN=test.kubehz.dev", "REGISTRATION="+c.mode,
				"CLAIM_KEY="+key, "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
			if rc != 0 {
				t.Fatalf("the summary step exits %d", rc)
			}
			sum := testRead(t, filepath.Join(dir, "summary"))
			if strings.Contains(sum, testTicket) {
				t.Errorf("the ticket is in the job summary:\n%s", sum)
			}
			mustContain(t, sum, "Your cluster `test.kubehz.dev` is registered with kubehz as **pending**.")
			mustContain(t, sum, c.mustSay)
		})
	}
}

func testWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
