package kubehz

// The workflows that run `lo provision` with HCLOUD_TOKEN
// (.github/workflows/spinup.yml and provision.yml) hide the one-time claim
// ticket, the fingerprint of the claim key, in the job log: a public repo
// shows its job logs to everyone, and any signed-in kubehz user who has the
// ticket can claim the cluster. The filter is the shell function
// hide_claim_ticket, inline in each workflow (a reusable workflow runs in the
// checkout of the calling repo, where no lok8s script exists). These tests
// read the workflows, run the function on the register output of this
// implementation, and run the steps with a stub lo. Before a step runs, the
// runner puts the value of each ${{ }} expression into the text of the step;
// resolveExpr does the same from a test context. The bats twin
// (tests/unit/kubehz_register_test.bats) runs the function on the output of
// the bash implementation.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// testTicket is an obvious fake claim ticket.
	testTicket = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
	// testTicketUpper is a second fake ticket, in capital hex digits.
	testTicketUpper = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:01"
	// testTicketOtherShape is a fake ticket without the MD5 shape: only the
	// exact match of the two register lines hides it.
	testTicketOtherShape = "SHA256:fake+claim/ticket=without-the-md5-shape"
	// testServerKeyFP is a fake server-key fingerprint, as the legacy
	// announce prints it.
	testServerKeyFP = "12:34:56:78:9a:bc:de:f0:12:34:56:78:9a:bc:de:f0"
	// ticketHidden is what the filter writes in place of a ticket.
	ticketHidden = "<hidden: one-time claim ticket>"
	// fpTail is the end of the "fingerprint:" register line.
	fpTail     = "   (also visible in Hetzner Console -> Security -> SSH keys)"
	testDomain = "test.kubehz.dev"
	// domainRuleError is the start of the error line of a refused domain.
	domainRuleError = "::error::The domain input does not match the lo domain rule"
)

// testValues are the ticket-like values that no filtered output may hold.
var testValues = []string{testTicket, testTicketUpper, testTicketOtherShape, testServerKeyFP}

// claimTicketWorkflows are the workflows that run `lo provision`.
var claimTicketWorkflows = []string{"spinup.yml", "provision.yml"}

// claimTicketSteps are the steps that run `lo provision` with HCLOUD_TOKEN.
var claimTicketSteps = []struct{ file, job, step string }{
	{"spinup.yml", "spinup", "Provision & register"},
	{"provision.yml", "provision", "Provision"},
}

type workflowStep struct {
	Name  string            `yaml:"name"`
	ID    string            `yaml:"id"`
	If    string            `yaml:"if"`
	Uses  string            `yaml:"uses"`
	With  map[string]string `yaml:"with"`
	Shell string            `yaml:"shell"`
	Env   map[string]string `yaml:"env"`
	Run   string            `yaml:"run"`
}

type workflowDefaults struct {
	Run struct {
		Shell string `yaml:"shell"`
	} `yaml:"run"`
}

type workflowJob struct {
	Defaults workflowDefaults  `yaml:"defaults"`
	Env      map[string]string `yaml:"env"`
	Steps    []workflowStep    `yaml:"steps"`
}

type workflowFile struct {
	Defaults workflowDefaults       `yaml:"defaults"`
	Env      map[string]string      `yaml:"env"`
	Jobs     map[string]workflowJob `yaml:"jobs"`
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

// workflowStepNamed is the named step of a job.
func workflowStepNamed(t *testing.T, file, job, name string) workflowStep {
	t.Helper()
	_, wf := readWorkflow(t, file)
	for _, s := range wf.Jobs[job].Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%s: no step %q in job %q", file, name, job)
	return workflowStep{}
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

// hideFuncDefRe matches a definition of hide_claim_ticket in each form that
// bash accepts: "name()", "name ()" and "function name".
var hideFuncDefRe = regexp.MustCompile(`(^|[^A-Za-z0-9_])(function[ \t]+hide_claim_ticket([^A-Za-z0-9_]|$)|hide_claim_ticket[ \t]*\(\))`)

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
	mustOK(t, h.ctx.RegisterCluster(t.Context(), cfg, testDomain, loSpec(h)), h.output())
	return h.output()
}

// nearMisses are lines that look like ticket lines but are not: the filter
// must pass them unchanged. A filter with a looser match changes them.
var nearMisses = []string{
	"  fingerprint: 11:22   (also visible in Hetzner Console -> Security -> SSH keys) and more",
	"   fingerprint: 11:22   (also visible in Hetzner Console -> Security -> SSH keys)",
	"x       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"11:22\"}'",
	"       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"11:22\"}' and more",
	"a line without a fingerprint",
	"15 pairs: aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88",
	"one digit short: aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:9",
	"  fingerprint: lo:" + testDomain,
}

// hideCases are input lines, and the lines that the filter must write for
// them: the real register lines of this implementation (with LF and CRLF,
// with a ticket without the MD5 shape, behind a log prefix), the announce,
// a server-key fingerprint, tickets in other text, the near misses, and a
// last line without a newline that holds a ticket.
func hideCases(t *testing.T) (in, want []string) {
	t.Helper()
	claim := registerOutput(t, true)
	if n := strings.Count(claim, testTicket); n != 2 {
		t.Fatalf("the claim-key registration prints the ticket %d times, want 2 (the curl hint and the fingerprint line):\n%s", n, claim)
	}
	announce := registerOutput(t, false)
	mustContain(t, announce, "\n  fingerprint: lo:"+testDomain+"\n")
	add := func(i, w string) {
		in = append(in, i)
		want = append(want, w)
	}
	for l := range strings.SplitSeq(strings.TrimSuffix(claim, "\n"), "\n") {
		hidden := strings.ReplaceAll(l, testTicket, ticketHidden)
		other := strings.ReplaceAll(l, testTicket, testTicketOtherShape)
		add(l, hidden)
		add(l+"\r", hidden+"\r")
		add(other, hidden)
		add(other+"\r", hidden+"\r")
		add("  [info] "+l, "  [info] "+hidden)
	}
	for l := range strings.SplitSeq(strings.TrimSuffix(announce, "\n"), "\n") {
		add(l, l)
	}
	add("  fingerprint: "+testServerKeyFP, "  fingerprint: "+ticketHidden)
	add("warning: "+testTicket, "warning: "+ticketHidden)
	add("two: "+testTicket+" and "+testTicketUpper+".", "two: "+ticketHidden+" and "+ticketHidden+".")
	add("17 pairs: "+testTicket+":01", "17 pairs: "+ticketHidden)
	add("  fingerprint: \xff"+testTicket+fpTail, "  fingerprint: "+ticketHidden+fpTail)
	add("bytes: \xff"+testTicket, "bytes: \xff"+ticketHidden)
	for _, l := range nearMisses {
		add(l, l)
	}
	add("last: "+testTicket, "last: "+ticketHidden)
	return in, want
}

// Each workflow that runs `lo provision` defines hide_claim_ticket once, in
// the same form, and every line of a run script that names `provision`
// (other than as the log file of spinup.yml) pipes stdout and stderr
// through it, under pipefail. No step sets a shell other than bash, writes
// GITHUB_ENV, or has a ${{ }} expression in its run script (an expression
// there becomes shell source). spinup.yml tees the filtered stream.
func TestClaimTicketWorkflowsFilterEveryProvision(t *testing.T) {
	for _, file := range claimTicketWorkflows {
		raw, wf := readWorkflow(t, file)
		if n := len(hideFuncDefRe.FindAllStringIndex(raw, -1)); n != 1 {
			t.Errorf("%s defines hide_claim_ticket %d times, want 1", file, n)
		}
		if s := wf.Defaults.Run.Shell; s != "" && s != "bash" {
			t.Errorf("%s: the default shell is %q", file, s)
		}
		provisions := 0
		for job, j := range wf.Jobs {
			if s := j.Defaults.Run.Shell; s != "" && s != "bash" {
				t.Errorf("%s: job %s: the default shell is %q", file, job, s)
			}
			for _, st := range j.Steps {
				if st.Shell != "" && st.Shell != "bash" {
					t.Errorf("%s: step %q runs in the shell %q", file, st.Name, st.Shell)
				}
				if strings.Contains(st.Run, "GITHUB_ENV") {
					t.Errorf("%s: step %q writes GITHUB_ENV", file, st.Name)
				}
				if strings.Contains(st.Run, "${{") {
					t.Errorf("%s: step %q has a ${{ }} expression in its run script", file, st.Name)
				}
				for l := range strings.SplitSeq(st.Run, "\n") {
					code := strings.TrimSpace(l)
					if strings.HasPrefix(code, "#") || !strings.Contains(strings.ReplaceAll(code, "/tmp/provision.log", ""), "provision") {
						continue
					}
					provisions++
					if !strings.Contains(code, " 2>&1 | hide_claim_ticket") {
						t.Errorf("%s: step %q: %q does not pipe stdout and stderr through hide_claim_ticket", file, st.Name, code)
					}
					if !strings.HasPrefix(st.Run, "set -o pipefail\n") {
						t.Errorf("%s: the run script of %q does not start with set -o pipefail", file, st.Name)
					}
				}
			}
		}
		if provisions == 0 {
			t.Errorf("%s: no lo provision", file)
		}
	}
	var fns []string
	for _, s := range claimTicketSteps {
		fns = append(fns, hideClaimTicketFunc(t, workflowStepNamed(t, s.file, s.job, s.step).Run))
	}
	if fns[0] != fns[1] {
		t.Errorf("hide_claim_ticket differs between %s and %s:\n%s\n%s", claimTicketSteps[0].file, claimTicketSteps[1].file, fns[0], fns[1])
	}
	run := workflowStepNamed(t, "spinup.yml", "spinup", "Provision & register").Run
	if !strings.Contains(run, `lo provision --force --domain "${DOMAIN}" 2>&1 | hide_claim_ticket | tee /tmp/provision.log`) {
		t.Error("spinup.yml: the provision does not tee the filtered stream")
	}
}

// No step output or env value of the workflows carries the fingerprint:
// the only step outputs read are the registration mode and the name of the
// claim key (spinup.yml) and the changed domains (provision.yml), and no
// env name or value of the workflow, a job or a step names a fingerprint.
func TestClaimTicketWorkflowsPassNoFingerprint(t *testing.T) {
	ref := regexp.MustCompile(`steps\.[a-z_-]+\.outputs\.([A-Za-z0-9_-]+)`)
	noFingerprint := func(file, where string, env map[string]string) {
		for k, v := range env {
			if strings.Contains(strings.ToLower(k+v), "fingerprint") {
				t.Errorf("%s: %s: env %s=%s", file, where, k, v)
			}
		}
	}
	for _, file := range claimTicketWorkflows {
		raw, wf := readWorkflow(t, file)
		for _, m := range ref.FindAllStringSubmatch(raw, -1) {
			if file == "spinup.yml" && (m[1] == "registration" || m[1] == "claim_key") {
				continue
			}
			if file == "provision.yml" && m[1] == "domains" {
				continue
			}
			t.Errorf("%s: the step output %q is read", file, m[1])
		}
		noFingerprint(file, "the workflow", wf.Env)
		for job, j := range wf.Jobs {
			noFingerprint(file, "job "+job, j.Env)
			for _, st := range j.Steps {
				noFingerprint(file, "job "+job+", step "+strconv.Quote(st.Name), st.Env)
			}
		}
	}
}

// stepRefRe is a reference to a step by its id.
var stepRefRe = regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.`)

// Each steps.<id> reference of a step (in its if, env, with or run) names
// an earlier step of the same job: with a renamed id, the runner gives an
// empty value and the summary step does not run. No step uses an
// upload-artifact action, and no with value names the provision log: the
// log stays on the runner.
func TestClaimTicketWorkflowsStepRefsAndArtifacts(t *testing.T) {
	refs := 0
	for _, file := range claimTicketWorkflows {
		_, wf := readWorkflow(t, file)
		for job, j := range wf.Jobs {
			ids := map[string]bool{}
			for _, st := range j.Steps {
				texts := []string{st.If, st.Run}
				for _, v := range st.Env {
					texts = append(texts, v)
				}
				for _, v := range st.With {
					texts = append(texts, v)
				}
				for _, s := range texts {
					for _, m := range stepRefRe.FindAllStringSubmatch(s, -1) {
						refs++
						if !ids[m[1]] {
							t.Errorf("%s: job %s: the step %q reads the step %q, which is not an earlier step of the job", file, job, st.Name, m[1])
						}
					}
				}
				if strings.Contains(st.Uses, "upload-artifact") {
					t.Errorf("%s: job %s: the step %q uses %s", file, job, st.Name, st.Uses)
				}
				for k, v := range st.With {
					if strings.Contains(v, "provision.log") {
						t.Errorf("%s: job %s: the step %q names the provision log in with.%s", file, job, st.Name, k)
					}
				}
				if st.ID != "" {
					ids[st.ID] = true
				}
			}
		}
	}
	if refs == 0 {
		t.Error("no step reads another step: the step model does not see the references")
	}
}

// The filter of each workflow on the cases of hideCases: the output is
// exactly the wanted text, and it holds no ticket-like test value.
func TestHideClaimTicketOnTheRegisterOutput(t *testing.T) {
	in, want := hideCases(t)
	input, wantOut := strings.Join(in, "\n"), strings.Join(want, "\n")
	for _, s := range claimTicketSteps {
		t.Run(s.file, func(t *testing.T) {
			got := runHideClaimTicket(t, hideClaimTicketFunc(t, workflowStepNamed(t, s.file, s.job, s.step).Run), input)
			for _, v := range testValues {
				if strings.Contains(got, v) {
					t.Errorf("%q is in the filtered output", v)
				}
			}
			if got == wantOut {
				return
			}
			gotLines := strings.Split(got, "\n")
			for i := range want {
				if i >= len(gotLines) || gotLines[i] != want[i] {
					g := "<no line>"
					if i < len(gotLines) {
						g = gotLines[i]
					}
					t.Errorf("line %d: in %q\ngot  %q\nwant %q", i+1, in[i], g, want[i])
				}
			}
			if len(gotLines) != len(want) {
				t.Errorf("%d lines out, want %d", len(gotLines), len(want))
			}
			t.Errorf("the output differs from the wanted text (want no newline at the end): got %q", got[max(0, len(got)-40):])
		})
	}
}

// The filter is for text: bash read drops a NUL byte. A NUL in a ticket
// line does not let the ticket pass: the filter works on the line without
// the NUL.
func TestHideClaimTicketDropsANulByte(t *testing.T) {
	in := "nul: aa:bb:cc:dd\x00:ee:ff:00:11:22:33:44:55:66:77:88:99\n" +
		"  fingerprint: \x00" + testTicket + fpTail + "\n"
	want := "nul: " + ticketHidden + "\n" + "  fingerprint: " + ticketHidden + fpTail + "\n"
	for _, s := range claimTicketSteps {
		t.Run(s.file, func(t *testing.T) {
			got := runHideClaimTicket(t, hideClaimTicketFunc(t, workflowStepNamed(t, s.file, s.job, s.step).Run), in)
			if got != want {
				t.Errorf("got  %q\nwant %q", got, want)
			}
		})
	}
}

// ticketLines are the two register lines that carry the ticket, in the
// format that the filter matches exactly.
func ticketLines(ticket string) []string {
	return []string{
		"       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"" + ticket + "\"}'",
		"  fingerprint: " + ticket + fpTail,
	}
}

// ticketWhole checks a register output: the ticket is whole on exactly the
// two register lines, and the output has no other piece of it that a
// newline or a CR splits.
func ticketWhole(output, ticket string) error {
	var got []string
	for l := range strings.SplitSeq(output, "\n") {
		if strings.Contains(l, ticket) {
			got = append(got, l)
		}
	}
	if want := ticketLines(ticket); !slices.Equal(got, want) {
		return fmt.Errorf("the lines with the whole ticket are %q, want %q", got, want)
	}
	if n := strings.Count(strings.NewReplacer("\n", "", "\r", "").Replace(output), ticket); n != 2 {
		return fmt.Errorf("without line ends, the output holds the ticket %d times, want 2", n)
	}
	return nil
}

// splitTicketLine is a copy of a register output with the ticket on the
// "fingerprint:" line split by sep: a format that the filter does not cover.
func splitTicketLine(output, ticket, sep string) string {
	return strings.Replace(output, "  fingerprint: "+ticket, "  fingerprint: "+ticket[:23]+sep+ticket[23:], 1)
}

// The print lines of the fingerprint in kubehz::ensure_claim_key of the bash
// implementation, and its one other line that expands the fingerprint (a
// check that prints nothing).
const (
	bashCurlLine  = `  echo "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"${fingerprint}\"}'"`
	bashFPLine    = `  echo "  fingerprint: ${fingerprint}   (also visible in Hetzner Console -> Security -> SSH keys)"`
	bashCheckLine = `  [[ -n "${cluster_id}" && -n "${public_key}" && -n "${fingerprint}" && -n "${key_name}" ]] || return 1`
)

// fpExpandRe is an expansion of the bash variable fingerprint.
var fpExpandRe = regexp.MustCompile(`\$\{?fingerprint([^A-Za-z0-9_]|$)`)

// bashTicketLines checks the bash source of the register: in
// kubehz::ensure_claim_key, each line that expands the fingerprint is one of
// the two print lines (each one time, each an echo of one argument that
// holds the whole value) or the check line.
func bashTicketLines(src string) error {
	lines := strings.Split(src, "\n")
	start := slices.Index(lines, "kubehz::ensure_claim_key() {")
	if start < 0 {
		return errors.New("no kubehz::ensure_claim_key function")
	}
	end := slices.Index(lines[start:], "}")
	if end < 0 {
		return errors.New("kubehz::ensure_claim_key has no end")
	}
	seen := map[string]int{}
	for _, l := range lines[start : start+end] {
		if !fpExpandRe.MatchString(l) {
			continue
		}
		switch l {
		case bashCurlLine, bashFPLine, bashCheckLine:
			seen[l]++
		default:
			return fmt.Errorf("the line %q expands the fingerprint, and it is not a print line", l)
		}
	}
	for _, l := range []string{bashCurlLine, bashFPLine} {
		if seen[l] != 1 {
			return fmt.Errorf("the print line %q is there %d times, want 1", l, seen[l])
		}
	}
	return nil
}

// splitBashFPLine is a copy of the bash source with the "fingerprint:" print
// line split into two prints, each with a piece of the value.
func splitBashFPLine(src string) string {
	return strings.Replace(src, bashFPLine+"\n",
		`  printf '  fingerprint: %s\n' "${fingerprint:0:23}"`+"\n"+
			`  echo "${fingerprint:23}   (also visible in Hetzner Console -> Security -> SSH keys)"`+"\n", 1)
}

// The filter hides a ticket that is on one line. Both implementations of the
// register print the ticket that way: whole, on the curl hint and on the
// "fingerprint:" line. This implementation is checked on its output, the
// bash one on its source. The checks refuse copies with a split ticket line.
func TestClaimTicketProducersPrintTheTicketWhole(t *testing.T) {
	claim := registerOutput(t, true)
	if err := ticketWhole(claim, testTicket); err != nil {
		t.Errorf("the Go register output: %v:\n%s", err, claim)
	}
	src := testRead(t, filepath.Join("..", "..", ".lok8s", "libs", "kubehz", "main"))
	if err := bashTicketLines(src); err != nil {
		t.Errorf("the bash register: %v", err)
	}
	for name, bad := range map[string]string{
		"split by a newline":  splitTicketLine(claim, testTicket, "\n"),
		"split by a CR":       splitTicketLine(claim, testTicket, "\r"),
		"one more split copy": claim + "debug: " + testTicket[:23] + "\n" + testTicket[23:] + "\n",
	} {
		if bad == claim {
			t.Fatalf("Go copy %s: the copy did not change", name)
		}
		if ticketWhole(bad, testTicket) == nil {
			t.Errorf("Go copy %s: the check passes it", name)
		}
	}
	for name, bad := range map[string]string{
		"the fingerprint line split": splitBashFPLine(src),
		"one more print of a piece":  strings.Replace(src, bashFPLine+"\n", bashFPLine+"\n"+`  echo "${fingerprint:0:8}"`+"\n", 1),
	} {
		if bad == src {
			t.Fatalf("bash copy %s: the copy did not change", name)
		}
		if bashTicketLines(bad) == nil {
			t.Errorf("bash copy %s: the check passes it", name)
		}
	}
}

// exprRe is a ${{ }} expression of a workflow.
var exprRe = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

// resolveExpr puts the value of each ${{ }} expression into s, from ctx,
// the way the runner does before a step runs. A step output that ctx does
// not hold is empty, as on the runner. Another expression that ctx does
// not hold fails the test.
func resolveExpr(t *testing.T, s string, ctx map[string]string) string {
	t.Helper()
	return exprRe.ReplaceAllStringFunc(s, func(m string) string {
		e := exprRe.FindStringSubmatch(m)[1]
		if v, ok := ctx[e]; ok {
			return v
		}
		if strings.HasPrefix(e, "steps.") && strings.Contains(e, ".outputs.") {
			return ""
		}
		t.Errorf("the test context has no value for %s", m)
		return ""
	})
}

// stepEnv is the env block of a step, with ctx put in, as NAME=value.
func stepEnv(t *testing.T, st workflowStep, ctx map[string]string) []string {
	t.Helper()
	env := make([]string, 0, len(st.Env))
	for k, v := range st.Env {
		env = append(env, k+"="+resolveExpr(t, v, ctx))
	}
	sort.Strings(env)
	return env
}

// runStep runs a run script the way the runner does (bash -e), in dir, with
// env added to a small base environment. It returns stdout and stderr
// together (the job log), and the exit code.
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

// stubLo writes a lo into dir/bin. It adds its arguments as one line to
// $LO_CALLS, prints the file $LO_OUTPUT on stdout and "warning: $LO_TICKET"
// on stderr, and exits with $LO_RC. It returns the env for these values.
func stubLo(t *testing.T, dir, output string, rc int) []string {
	t.Helper()
	testWrite(t, filepath.Join(dir, "lo.out"), output)
	testWrite(t, filepath.Join(dir, "bin", "lo"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$LO_CALLS\"\ncat \"$LO_OUTPUT\"\necho \"warning: $LO_TICKET\" >&2\nexit \"$LO_RC\"\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "lo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"LO_CALLS=" + filepath.Join(dir, "lo.calls"), "LO_OUTPUT=" + filepath.Join(dir, "lo.out"),
		"LO_TICKET=" + testTicket, "LO_RC=" + strconv.Itoa(rc)}
}

// readOutputs reads a GITHUB_OUTPUT file of name=value lines.
func readOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for l := range strings.SplitSeq(strings.TrimSuffix(testRead(t, path), "\n"), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.Contains(k, "<<") {
			t.Fatalf("the GITHUB_OUTPUT line %q is not name=value", l)
		}
		out[k] = v
	}
	return out
}

// spinupContext is the expression context of a spinup run: the input, the
// secret and the outputs of the provision step.
func spinupContext(domain string, outputs map[string]string) map[string]string {
	ctx := map[string]string{"inputs.domain": domain, "secrets.HCLOUD_TOKEN": "hc_test"}
	for k, v := range outputs {
		ctx["steps.provision.outputs."+k] = v
	}
	return ctx
}

// badDomains are domain inputs that the workflows refuse before lo runs.
// pwned is a file that a command substitution in them would make.
func badDomains(pwned string) []string {
	return []string{"a$(touch " + pwned + ")", "a`touch " + pwned + "`", "a b", "a\nb", "a\r", "-x", "../x", ""}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// The two steps of spinup.yml end to end, with a stub lo that prints the
// register output of this implementation, a ticket on stderr, and exits
// with a given status. Each step gets the env of its own env block, with
// the outputs that the provision step wrote. The ticket reaches no output
// of the provision step (its log, the tee file, GITHUB_OUTPUT), and the
// summary names the claim key but never the ticket. A provision that fails
// after the registration still writes the outputs, and the step still
// fails with the status of lo.
func TestSpinupStepsKeepTheTicketOut(t *testing.T) {
	prov := workflowStepNamed(t, "spinup.yml", "spinup", "Provision & register")
	if n := strings.Count(prov.Run, "/tmp/provision.log"); n != 2 {
		t.Fatalf("the provision step names /tmp/provision.log %d times, want 2 (tee and read)", n)
	}
	sum := workflowStepNamed(t, "spinup.yml", "spinup", "Claim instructions")
	if sum.If != "always() && steps.provision.outputs.registration != ''" {
		t.Errorf("the summary step runs if %q", sum.If)
	}
	for _, c := range []struct {
		name     string
		claimKey bool
		rc       int
		outputs  map[string]string
		mustSay  []string
	}{
		{"claim key", true, 0, map[string]string{"registration": "claim-key", "claim_key": "kubehz-claim-test.kubehz.dev"},
			[]string{"the MD5 fingerprint of the SSH key `kubehz-claim-test.kubehz.dev`", "**Security → SSH keys**, the key `kubehz-claim-test.kubehz.dev`.",
				"The tickets of earlier runs keep working until someone claims them"}},
		{"claim key, then the provision fails", true, 3, map[string]string{"registration": "claim-key", "claim_key": "kubehz-claim-test.kubehz.dev"},
			[]string{"the MD5 fingerprint of the SSH key `kubehz-claim-test.kubehz.dev`"}},
		{"fingerprint announce", false, 0, map[string]string{"registration": "announce", "claim_key": ""},
			[]string{"This run did not confirm a claim-key upload", "That does not mean that no ticket exists", "`lo kubehz claim-code`"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			env := stubLo(t, dir, registerOutput(t, c.claimKey), c.rc)
			logFile, outFile := filepath.Join(dir, "provision.log"), filepath.Join(dir, "outputs")
			ctx := spinupContext(testDomain, nil)
			run := strings.ReplaceAll(resolveExpr(t, prov.Run, ctx), "/tmp/provision.log", logFile)
			out, rc := runStep(t, dir, run, append(append(stepEnv(t, prov, ctx), "GITHUB_OUTPUT="+outFile), env...)...)
			if rc != c.rc {
				t.Fatalf("the provision step exits %d, want %d:\n%s", rc, c.rc, out)
			}
			if calls := testRead(t, filepath.Join(dir, "lo.calls")); calls != "provision --force --domain "+testDomain+"\n" {
				t.Errorf("lo ran as %q", calls)
			}
			outputs := readOutputs(t, outFile)
			if len(outputs) != len(c.outputs) {
				t.Errorf("GITHUB_OUTPUT = %v, want %v", outputs, c.outputs)
			}
			for k, v := range c.outputs {
				if outputs[k] != v {
					t.Errorf("the output %s = %q, want %q", k, outputs[k], v)
				}
			}
			log := testRead(t, logFile)
			for name, s := range map[string]string{"the step log": out, "the tee file": log, "GITHUB_OUTPUT": testRead(t, outFile)} {
				if strings.Contains(s, testTicket) {
					t.Errorf("the ticket is in %s:\n%s", name, s)
				}
			}
			// The two register lines of a claim key, and the stderr line.
			wantHidden := 1
			if c.claimKey {
				wantHidden = 3
			}
			if n := strings.Count(out, ticketHidden); n != wantHidden {
				t.Errorf("the step log has %d hidden values, want %d:\n%s", n, wantHidden, out)
			}

			if outputs["registration"] == "" {
				t.Fatal("the summary step would not run")
			}
			sumFile := filepath.Join(dir, "summary")
			sctx := spinupContext(testDomain, outputs)
			sout, rc := runStep(t, dir, resolveExpr(t, sum.Run, sctx), append(stepEnv(t, sum, sctx), "GITHUB_STEP_SUMMARY="+sumFile)...)
			if rc != 0 {
				t.Fatalf("the summary step exits %d:\n%s", rc, sout)
			}
			summary := testRead(t, sumFile)
			if strings.Contains(summary, testTicket) {
				t.Errorf("the ticket is in the job summary:\n%s", summary)
			}
			mustContain(t, summary, "Your cluster `test.kubehz.dev` is registered with kubehz as **pending**.")
			for _, s := range c.mustSay {
				mustContain(t, summary, s)
			}
		})
	}
}

// Both steps of spinup.yml refuse a domain input that does not match the
// domain rule of lo: the step exits 1 with an error line, lo does not run,
// no output or summary is written, and no command in the input runs.
func TestSpinupStepsRefuseABadDomain(t *testing.T) {
	prov := workflowStepNamed(t, "spinup.yml", "spinup", "Provision & register")
	sum := workflowStepNamed(t, "spinup.yml", "spinup", "Claim instructions")
	for i, d := range badDomains("PWNED") {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()
			pwned := filepath.Join(dir, "PWNED")
			d = strings.ReplaceAll(d, "PWNED", pwned)
			env := stubLo(t, dir, registerOutput(t, true), 0)
			outFile, sumFile := filepath.Join(dir, "outputs"), filepath.Join(dir, "summary")
			ctx := spinupContext(d, nil)
			out, rc := runStep(t, dir, resolveExpr(t, prov.Run, ctx), append(append(stepEnv(t, prov, ctx), "GITHUB_OUTPUT="+outFile), env...)...)
			if rc != 1 || !strings.Contains(out, domainRuleError) {
				t.Errorf("domain %q: the provision step exits %d, want 1 with the error line:\n%s", d, rc, out)
			}
			sctx := spinupContext(d, map[string]string{"registration": "claim-key", "claim_key": "kubehz-claim-x"})
			out, rc = runStep(t, dir, resolveExpr(t, sum.Run, sctx), append(stepEnv(t, sum, sctx), "GITHUB_STEP_SUMMARY="+sumFile)...)
			if rc != 1 || !strings.Contains(out, domainRuleError) {
				t.Errorf("domain %q: the summary step exits %d, want 1 with the error line:\n%s", d, rc, out)
			}
			for _, f := range []string{filepath.Join(dir, "lo.calls"), outFile, sumFile, pwned} {
				if fileExists(f) {
					t.Errorf("domain %q: %s exists", d, filepath.Base(f))
				}
			}
		})
	}
}

// The provision step of spinup.yml passes a claim key name only when the
// name matches its rule: a register line with another name gives empty
// outputs, so the summary step does not run.
func TestSpinupStepRefusesABadKeyName(t *testing.T) {
	prov := workflowStepNamed(t, "spinup.yml", "spinup", "Provision & register")
	for i, name := range []string{"kubehz-claim-x`id`", "kubehz claim", "kubehz-claim-é", "kubehz-claim-x$(id)"} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()
			output := strings.Replace(registerOutput(t, true), "Claim key 'kubehz-claim-test.kubehz.dev'", "Claim key '"+name+"'", 1)
			if !strings.Contains(output, name) {
				t.Fatal("the register output has no claim key line")
			}
			env := stubLo(t, dir, output, 0)
			logFile, outFile := filepath.Join(dir, "provision.log"), filepath.Join(dir, "outputs")
			ctx := spinupContext(testDomain, nil)
			run := strings.ReplaceAll(resolveExpr(t, prov.Run, ctx), "/tmp/provision.log", logFile)
			out, rc := runStep(t, dir, run, append(append(stepEnv(t, prov, ctx), "GITHUB_OUTPUT="+outFile), env...)...)
			if rc != 0 {
				t.Fatalf("the provision step exits %d:\n%s", rc, out)
			}
			if got := testRead(t, outFile); got != "registration=\nclaim_key=\n" {
				t.Errorf("key name %q: GITHUB_OUTPUT = %q, want empty values", name, got)
			}
		})
	}
}

// provisionContext is the expression context of a provision.yml run.
func provisionContext(input, detected string) map[string]string {
	return map[string]string{
		"secrets.HCLOUD_TOKEN":         "hc_test",
		"github.event.inputs.domain":   input,
		"steps.detect.outputs.domains": detected,
	}
}

// The Provision step of provision.yml, with a stub lo: the ticket of each
// run is hidden in the log, lo runs once for the input domain or for each
// changed domain, and a failing lo fails the step on both paths.
func TestProvisionStepKeepsTheTicketOut(t *testing.T) {
	st := workflowStepNamed(t, "provision.yml", "provision", "Provision")
	claim := registerOutput(t, true)
	two := "provision --force --domain a.example.com\nprovision --force --domain b.example.com\n"
	for _, c := range []struct {
		name, input, detected string
		rc                    int
		calls                 string
	}{
		{"the input domain", testDomain, "", 0, "provision --force --domain " + testDomain + "\n"},
		{"the changed domains", "", "a.example.com b.example.com ", 0, two},
		{"the input domain, lo fails", testDomain, "", 3, "provision --force --domain " + testDomain + "\n"},
		{"the changed domains, lo fails", "", "a.example.com b.example.com ", 3, "provision --force --domain a.example.com\n"},
		{"no domain", "", "", 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			env := stubLo(t, dir, claim, c.rc)
			ctx := provisionContext(c.input, c.detected)
			out, rc := runStep(t, dir, resolveExpr(t, st.Run, ctx), append(stepEnv(t, st, ctx), env...)...)
			if rc != c.rc {
				t.Errorf("the step exits %d, want %d:\n%s", rc, c.rc, out)
			}
			calls := ""
			if fileExists(filepath.Join(dir, "lo.calls")) {
				calls = testRead(t, filepath.Join(dir, "lo.calls"))
			}
			if calls != c.calls {
				t.Errorf("lo ran as %q, want %q", calls, c.calls)
			}
			if strings.Contains(out, testTicket) {
				t.Errorf("the ticket is in the step log:\n%s", out)
			}
			if n, want := strings.Count(out, ticketHidden), 3*strings.Count(c.calls, "\n"); n != want {
				t.Errorf("the step log has %d hidden values, want %d:\n%s", n, want, out)
			}
		})
	}
}

// The Provision step of provision.yml refuses a bad input domain, and a
// list of changed domains with a bad name in it: the step exits 1 with an
// error line before lo runs, and no command in a name runs.
func TestProvisionStepRefusesABadDomain(t *testing.T) {
	st := workflowStepNamed(t, "provision.yml", "provision", "Provision")
	type tc struct{ input, detected string }
	var cases []tc
	for _, d := range badDomains("PWNED") {
		if d != "" {
			cases = append(cases, tc{input: d})
		}
	}
	for _, d := range []string{"a.example.com $(touch PWNED) b.example.com", "a.example.com `touch PWNED`", "a.example.com ../x", "-x a.example.com"} {
		cases = append(cases, tc{detected: d})
	}
	for i, c := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()
			pwned := filepath.Join(dir, "PWNED")
			ctx := provisionContext(strings.ReplaceAll(c.input, "PWNED", pwned), strings.ReplaceAll(c.detected, "PWNED", pwned))
			env := stubLo(t, dir, registerOutput(t, true), 0)
			out, rc := runStep(t, dir, resolveExpr(t, st.Run, ctx), append(stepEnv(t, st, ctx), env...)...)
			if rc != 1 || !strings.Contains(out, "::error::A domain does not match the lo domain rule") {
				t.Errorf("%+v: the step exits %d, want 1 with the error line:\n%s", c, rc, out)
			}
			for _, f := range []string{filepath.Join(dir, "lo.calls"), pwned} {
				if fileExists(f) {
					t.Errorf("%+v: %s exists", c, filepath.Base(f))
				}
			}
		})
	}
}

// fakeGit writes a git into dir/bin. It adds its arguments as one line to
// $GIT_CALLS. As git does, it prints the paths with a NUL after each when
// an argument is -z, else one path per line. It returns the env for these
// values.
func fakeGit(t *testing.T, dir string, paths []string) []string {
	t.Helper()
	nul, lf := "", ""
	for _, p := range paths {
		nul += p + "\x00"
		lf += p + "\n"
	}
	testWrite(t, filepath.Join(dir, "git.nul"), nul)
	testWrite(t, filepath.Join(dir, "git.lf"), lf)
	testWrite(t, filepath.Join(dir, "bin", "git"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_CALLS\"\nfor a in \"$@\"; do\n  if [ \"$a\" = -z ]; then\n    exec cat \"$GIT_NUL\"\n  fi\ndone\nexec cat \"$GIT_LF\"\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"GIT_CALLS=" + filepath.Join(dir, "git.calls"), "GIT_NUL=" + filepath.Join(dir, "git.nul"), "GIT_LF=" + filepath.Join(dir, "git.lf")}
}

// detectError is the error line of the detect step of provision.yml.
const detectError = "::error::A changed cluster directory does not match the lo domain rule ^[A-Za-z0-9][A-Za-z0-9._-]*$. No domain was passed on.\n"

// The detect step of provision.yml, with a fake git, and its output in the
// Provision step, with a stub lo. Each changed directory is one domain, also
// with a space or a newline in its name. A directory name that does not
// match the domain rule stops the detect step with a static error line,
// before it writes an output, so the job stops before lo runs.
func TestProvisionDetectorThroughTheProvisionStep(t *testing.T) {
	det := workflowStepNamed(t, "provision.yml", "provision", "Detect changed domains")
	prov := workflowStepNamed(t, "provision.yml", "provision", "Provision")
	if det.ID != "detect" || prov.If != "" {
		t.Fatalf("the detect step has the id %q, and the Provision step runs if %q: want detect, and no if", det.ID, prov.If)
	}
	claim := registerOutput(t, true)
	for _, c := range []struct {
		name    string
		dirs    []string
		domains string // the output of the detect step; "" when it refuses
		refused bool
	}{
		{"two good directories", []string{"a.example.com", "b.example.com"}, "a.example.com b.example.com", false},
		{"one good directory", []string{testDomain}, testDomain, false},
		{"no change", nil, "", false},
		{"a space in a directory name", []string{"a.example.com b.example.com"}, "", true},
		{"a newline in a directory name", []string{"a.example.com\nb.example.com"}, "", true},
		{"a tab in a directory name", []string{"a.example.com\tb.example.com"}, "", true},
		{"a command in a directory name", []string{"a$(touch PWNED)"}, "", true},
		{"a nested directory", []string{"a/b"}, "", true},
		{"a good and a bad directory", []string{"a.example.com", "-x"}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var paths []string
			for _, d := range c.dirs {
				paths = append(paths, ".lok8s/"+d+"/cluster.lok8s.yaml")
			}
			genv := fakeGit(t, dir, paths)
			outFile := filepath.Join(dir, "outputs")
			ctx := provisionContext("", "")
			out, rc := runStep(t, dir, resolveExpr(t, det.Run, ctx), append(append(stepEnv(t, det, ctx), "GITHUB_OUTPUT="+outFile), genv...)...)
			if calls := testRead(t, filepath.Join(dir, "git.calls")); calls != "diff -z --name-only HEAD~1 HEAD -- .lok8s/*/cluster.lok8s.yaml\n" {
				t.Errorf("git ran as %q", calls)
			}
			detected := ""
			if c.refused {
				if rc != 1 || out != detectError {
					t.Errorf("the detect step exits %d with %q, want 1 with only the error line", rc, out)
				}
				if fileExists(outFile) {
					t.Errorf("the detect step wrote GITHUB_OUTPUT: %q", testRead(t, outFile))
				}
			} else {
				if rc != 0 || out != "" {
					t.Fatalf("the detect step exits %d:\n%s", rc, out)
				}
				if got := testRead(t, outFile); got != "domains="+c.domains+"\n" {
					t.Errorf("GITHUB_OUTPUT = %q, want domains=%s", got, c.domains)
				}
				detected = readOutputs(t, outFile)["domains"]
			}
			if fileExists(filepath.Join(dir, "PWNED")) {
				t.Error("a command in a directory name ran")
			}

			env := stubLo(t, dir, claim, 0)
			pctx := provisionContext("", detected)
			pout, rc := runStep(t, dir, resolveExpr(t, prov.Run, pctx), append(stepEnv(t, prov, pctx), env...)...)
			if rc != 0 {
				t.Errorf("the Provision step exits %d:\n%s", rc, pout)
			}
			want := ""
			for d := range strings.FieldsSeq(c.domains) {
				want += "provision --force --domain " + d + "\n"
			}
			calls := ""
			if fileExists(filepath.Join(dir, "lo.calls")) {
				calls = testRead(t, filepath.Join(dir, "lo.calls"))
			}
			if calls != want {
				t.Errorf("lo ran as %q, want %q", calls, want)
			}
			if strings.Contains(pout, testTicket) {
				t.Errorf("the ticket is in the step log:\n%s", pout)
			}
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
