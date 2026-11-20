package kubehz

// The claim-key registration prints the one-time claim ticket only on a
// terminal, or with --print-token (Context.PrintToken). Any signed-in user
// who has the ticket can claim the cluster, so a pipe or a file (a CI log,
// a captured output) gets a placeholder and the place to read it. The bats
// twin is in tests/unit/kubehz_register_test.bats.

import (
	"regexp"
	"testing"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ticketShapeRe is a string with the shape of an MD5 fingerprint (16 pairs
// of hex digits, with a colon between each two), as the log filter of the
// provision workflows matches it.
var ticketShapeRe = regexp.MustCompile(`[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){15,}`)

// The lines of a claim-key registration on a stdout that is not a
// terminal, after the curl line: the placeholder in the curl hint, and the
// place to read the ticket.
const (
	ticketHiddenCurl = "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"<hidden: one-time claim ticket>\"}'\n"
	ticketHiddenFP   = "  fingerprint: <hidden: one-time claim ticket>   (in Hetzner Console -> Security -> SSH keys, key 'kubehz-claim-test.kubehz.dev'; --print-token shows it here)\n"
)

// claimKeyRegister runs the claim-key registration against the stubs, with
// a register answer that carries testTicket.
func claimKeyRegister(t *testing.T, printToken bool) *harness {
	t.Helper()
	h := newHarness(t)
	h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
	h.handle("POST /api/clusters/register", 200,
		`{"id":"cl-5","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"`+testTicket+`","name":"kubehz-claim-test.kubehz.dev"}}`)
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[]}`)
	h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
	h.ctx.PrintToken = printToken
	mustOK(t, h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL()), h.output())
	return h
}

// The harness writes to a buffer, which is not a terminal: no line of
// stdout or stderr holds the ticket or any string of its shape. The curl
// hint has the placeholder, and the last line names the key in the Hetzner
// Console and the flag. The key upload still happens.
func TestEnsureClaimKeyKeepsTheTicketOffAStdoutThatIsNotATerminal(t *testing.T) {
	h := claimKeyRegister(t, false)
	for name, s := range map[string]string{"stdout": h.out.String(), "stderr": h.errOut.String()} {
		if m := ticketShapeRe.FindString(s); m != "" {
			t.Errorf("%s holds the ticket-shaped string %q:\n%s", name, m, s)
		}
	}
	mustContain(t, h.out.String(), "uploaded to your Hetzner Cloud account.\n")
	mustContain(t, h.out.String(), "/api/claims/verify -H 'Authorization: Bearer <khzt_ token (clusters:write)>' \\\n"+ticketHiddenCurl+ticketHiddenFP)
	if !h.anyReq("POST", "/v1/ssh_keys") {
		t.Fatalf("the claim key was not uploaded: %+v", h.reqs())
	}
}

// On a terminal (the repo's terminal override, ui.ForceTTY), the ticket is
// whole on the curl hint and on the "fingerprint:" line. Not parallel: the
// override is global.
func TestEnsureClaimKeyShowsTheTicketOnATerminal(t *testing.T) {
	t.Cleanup(ui.ForceTTY(true))
	t.Cleanup(ui.ForceColor(false))
	h := claimKeyRegister(t, false)
	if err := ticketWhole(h.out.String(), testTicket); err != nil {
		t.Errorf("%v:\n%s", err, h.out.String())
	}
	mustNotContain(t, h.out.String(), "<hidden: one-time claim ticket>")
}

// With --print-token, a stdout that is not a terminal gets the ticket too.
func TestEnsureClaimKeyPrintTokenShowsTheTicketOnAPipe(t *testing.T) {
	h := claimKeyRegister(t, true)
	if err := ticketWhole(h.out.String(), testTicket); err != nil {
		t.Errorf("%v:\n%s", err, h.out.String())
	}
	mustNotContain(t, h.out.String(), "<hidden: one-time claim ticket>")
}
