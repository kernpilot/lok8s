package kubehz

// The claim-key registration prints the one-time claim ticket only with
// --print-token (Context.PrintToken), or when stdout is a terminal and the
// environment is not a CI run (ui.NonInteractive). Any signed-in user who
// has the ticket can claim the cluster, so a pipe or a file (a CI log, a
// captured output) gets a placeholder and the place to read it. The bats
// twin is in tests/unit/kubehz_register_test.bats.
//
// The fixture is a real key and its real MD5 fingerprint
// (testutil.ClaimKeyPublic, testutil.ClaimTicket). A hidden ticket is in no
// form (testutil.NoClaimTicket) on stdout, on stderr, in the error or in a
// file of the project. The terminal is the stdout writer only
// (ui.Styled), not the global override.

import (
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// The lines of a claim-key registration on a stdout that is not a
// terminal, after the curl line: the placeholder in the curl hint, and the
// place to read the ticket.
const (
	ticketHiddenCurl = "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"<hidden: one-time claim ticket>\"}'\n"
	ticketHiddenFP   = "  fingerprint: <hidden: one-time claim ticket>   (in Hetzner Console -> Security -> SSH keys, key 'kubehz-claim-test.kubehz.dev'; 'lo kubehz register --domain test.kubehz.dev --print-token' registers again and shows a new ticket)\n"
)

// ticketCase is one claim-key registration: the flag, which writer is a
// terminal, and the environment on top of the harness's.
type ticketCase struct {
	printToken     bool
	outTTY, errTTY bool
	env            map[string]string
}

// ticketRun runs the claim-key registration against the stubs, with a
// register answer that carries the fixture key, its ticket and a bind
// secret (so the run writes a file of its own).
func ticketRun(t *testing.T, k ticketCase) (*harness, error) {
	t.Helper()
	h := newHarness(t)
	h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
	for key, v := range k.env {
		h.env[key] = v
	}
	h.handle("POST /api/clusters/register", 200,
		`{"id":"cl-5","bindSecret":"`+bindSecretFixture+`","claimKey":{"publicKey":"`+testutil.ClaimKeyPublic+`","fingerprint":"`+testutil.ClaimTicket+`","name":"kubehz-claim-test.kubehz.dev"}}`)
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[]}`)
	h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
	if k.outTTY {
		h.ctx.Out = ui.Styled(&h.out, ui.Style{TTY: true})
	}
	if k.errTTY {
		h.ctx.ErrOut = ui.Styled(&h.errOut, ui.Style{TTY: true})
	}
	h.ctx.PrintToken = k.printToken
	return h, h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL())
}

// checkTicketHidden: the run ended well, uploaded the key, and holds the
// ticket in no form anywhere. The curl hint has the placeholder, and the
// last line names the key in the Hetzner Console and the re-run.
func checkTicketHidden(t *testing.T, h *harness, err error) {
	t.Helper()
	if err != nil {
		testutil.NoClaimTicket(t, "the error", err.Error())
		t.Fatalf("ensureClaimKey: %v\n%s", err, h.output())
	}
	testutil.NoClaimTicket(t, "stdout", h.out.String())
	testutil.NoClaimTicket(t, "stderr", h.errOut.String())
	testutil.NoClaimTicketInFiles(t, h.base)
	mustContain(t, h.out.String(), "/api/claims/verify -H 'Authorization: Bearer <khzt_ token (clusters:write)>' \\\n"+ticketHiddenCurl+ticketHiddenFP)
	if !h.anyReq("POST", "/v1/ssh_keys") {
		t.Fatalf("the claim key was not uploaded: %+v", h.reqs())
	}
}

// checkTicketShown: the ticket is whole on the curl hint and on the
// "fingerprint:" line of stdout, and nowhere on stderr.
func checkTicketShown(t *testing.T, h *harness, err error) {
	t.Helper()
	mustOK(t, err, h.output())
	if err := ticketWhole(h.out.String(), testutil.ClaimTicket); err != nil {
		t.Errorf("%v:\n%s", err, h.out.String())
	}
	mustNotContain(t, h.out.String(), "<hidden: one-time claim ticket>")
	testutil.NoClaimTicket(t, "stderr", h.errOut.String())
}

// The harness writes to a buffer, which is not a terminal.
func TestEnsureClaimKeyKeepsTheTicketOffAStdoutThatIsNotATerminal(t *testing.T) {
	h, err := ticketRun(t, ticketCase{})
	checkTicketHidden(t, h, err)
	mustContain(t, h.out.String(), "uploaded to your Hetzner Cloud account.\n")
}

// Only stdout counts: a terminal on stderr does not show the ticket on a
// stdout that is not one.
func TestEnsureClaimKeyHidesTheTicketWhenOnlyStderrIsATerminal(t *testing.T) {
	h, err := ticketRun(t, ticketCase{errTTY: true})
	checkTicketHidden(t, h, err)
}

// A terminal on stdout gets the ticket.
func TestEnsureClaimKeyShowsTheTicketOnATerminal(t *testing.T) {
	h, err := ticketRun(t, ticketCase{outTTY: true})
	checkTicketShown(t, h, err)
}

// A CI run (CI set and not "false") or LOK8S_NONINTERACTIVE hides the
// ticket on a terminal too: a runner can give the job a pty.
func TestEnsureClaimKeyHidesTheTicketOnATerminalInACIRun(t *testing.T) {
	for _, env := range []map[string]string{
		{"CI": "true"},
		{"CI": "1"},
		{"LOK8S_NONINTERACTIVE": "1"},
		{"LOK8S_NONINTERACTIVE": "1", "CI": "false"},
	} {
		h, err := ticketRun(t, ticketCase{outTTY: true, env: env})
		checkTicketHidden(t, h, err)
	}
	h, err := ticketRun(t, ticketCase{outTTY: true, env: map[string]string{"CI": "false"}})
	checkTicketShown(t, h, err)
}

// With --print-token, a stdout that is not a terminal gets the ticket, and
// so does a CI run.
func TestEnsureClaimKeyPrintTokenShowsTheTicketOnAPipe(t *testing.T) {
	h, err := ticketRun(t, ticketCase{printToken: true})
	checkTicketShown(t, h, err)
	h, err = ticketRun(t, ticketCase{printToken: true, env: map[string]string{"CI": "true"}})
	checkTicketShown(t, h, err)
}
