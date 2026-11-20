package cli

// The one-time claim ticket of a claim-key registration goes to stdout only
// on a terminal, or with --print-token. These tests run each command that
// can register a cluster (lo kubehz register, lo provision, lo up) against
// a TLS stub of the platform api and of the Hetzner Cloud api, in a project
// in the test's temp dir. lo provision and lo up run only the register hook
// of their dispatcher, so no driver runs.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/ui"
)

// cliTestTicket is an obvious fake claim ticket.
const cliTestTicket = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"

// The ticket lines on a terminal or with --print-token, and the lines in
// their place on a stdout that is not a terminal.
const (
	ticketShownCurl  = "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"" + cliTestTicket + "\"}'\n"
	ticketShownFP    = "  fingerprint: " + cliTestTicket + "   (also visible in Hetzner Console -> Security -> SSH keys)\n"
	ticketHiddenCurl = "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"<hidden: one-time claim ticket>\"}'\n"
	ticketHiddenFP   = "  fingerprint: <hidden: one-time claim ticket>   (in Hetzner Console -> Security -> SSH keys, key 'kubehz-claim-t.dev'; --print-token shows it here)\n"
)

// cliTicketShapeRe is a string with the shape of an MD5 fingerprint.
var cliTicketShapeRe = regexp.MustCompile(`[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){15,}`)

// claimTicketProject starts the stubs and writes a project with the domain
// t.dev (access registered, apiUrl the stub). It sets the environment of a
// claim-key registration and the client of the kubehz commands. It returns
// the paths of the project and the path of the spec. Not for parallel
// tests: it sets the environment and kubehzHTTP.
func claimTicketProject(t *testing.T) (*config.Paths, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/clusters/register":
			_, _ = io.WriteString(w, `{"id":"cl-5","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"`+cliTestTicket+`","name":"kubehz-claim-t.dev"}}`)
		case "GET /v1/ssh_keys":
			_, _ = io.WriteString(w, `{"ssh_keys":[]}`)
		case "POST /v1/ssh_keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ssh_key":{"id":43}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prev := kubehzHTTP
	kubehzHTTP = srv.Client()
	t.Cleanup(func() { kubehzHTTP = prev })

	base := t.TempDir()
	dir := filepath.Join(base, "clusters", "t.dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	body := "kind: Lo\nspec:\n  cluster:\n    domain: t.dev\n  kubehz:\n    access: registered\n    apiUrl: " + srv.URL + "\n"
	if err := os.WriteFile(spec, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOMAIN_NAME", "")
	t.Setenv("KUBEHZ_TOKEN", "")
	t.Setenv("HCLOUD_TOKEN", "hc_test")
	t.Setenv("HCLOUD_API_BASE", srv.URL)
	paths := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	return paths, spec
}

// checkTicketHidden checks the output of a registration on a stdout that
// is not a terminal: no ticket-shaped string on stdout or stderr, and the
// two lines in place of the ticket lines.
func checkTicketHidden(t *testing.T, name, out, errOut string) {
	t.Helper()
	for stream, s := range map[string]string{"stdout": out, "stderr": errOut} {
		if m := cliTicketShapeRe.FindString(s); m != "" {
			t.Errorf("%s: %s holds the ticket-shaped string %q:\n%s", name, stream, m, s)
		}
	}
	if !strings.Contains(out, ticketHiddenCurl+ticketHiddenFP) {
		t.Errorf("%s: stdout has no placeholder lines:\n%s", name, out)
	}
}

// checkTicketShown checks that stdout holds both ticket lines.
func checkTicketShown(t *testing.T, name, out string) {
	t.Helper()
	if !strings.Contains(out, ticketShownCurl+ticketShownFP) {
		t.Errorf("%s: stdout has no ticket lines:\n%s", name, out)
	}
	if strings.Contains(out, "<hidden: one-time claim ticket>") {
		t.Errorf("%s: stdout holds the placeholder:\n%s", name, out)
	}
}

// lo kubehz register: a buffer (not a terminal) gets no ticket, and
// --print-token shows it there.
func TestKubehzRegisterKeepsTheClaimTicketOffAPipe(t *testing.T) {
	paths, _ := claimTicketProject(t)
	out, errOut, err := runKubehzLo(t, paths.Base, "kubehz", "register", "--domain", "t.dev")
	if err != nil {
		t.Fatalf("register: %v\n%s%s", err, out, errOut)
	}
	checkTicketHidden(t, "register", out, errOut)
}

func TestKubehzRegisterPrintTokenShowsTheClaimTicketOnAPipe(t *testing.T) {
	paths, _ := claimTicketProject(t)
	out, errOut, err := runKubehzLo(t, paths.Base, "kubehz", "register", "--domain", "t.dev", "--print-token")
	if err != nil {
		t.Fatalf("register --print-token: %v\n%s%s", err, out, errOut)
	}
	checkTicketShown(t, "register --print-token", out)
}

// On a terminal (the repo's terminal override, ui.ForceTTY), lo kubehz
// register shows the ticket without the flag.
func TestKubehzRegisterShowsTheClaimTicketOnATerminal(t *testing.T) {
	t.Cleanup(ui.ForceTTY(true))
	t.Cleanup(ui.ForceColor(false))
	paths, _ := claimTicketProject(t)
	out, errOut, err := runKubehzLo(t, paths.Base, "kubehz", "register", "--domain", "t.dev")
	if err != nil {
		t.Fatalf("register: %v\n%s%s", err, out, errOut)
	}
	checkTicketShown(t, "register on a terminal", out)
}

// lo provision and lo up take --print-token to the register of their
// dispatch.
func TestProvisionAndUpPassPrintTokenToTheRegister(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*config.Paths, commandSpec) *cobra.Command
	}{
		{"provision", newProvisionCommand},
		{"up", newUpCommand},
	} {
		for _, flag := range []bool{false, true} {
			paths, spec := claimTicketProject(t)
			cmd := tc.make(paths, commandSpec{})
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			args := []string{}
			if flag {
				args = append(args, "--print-token")
			}
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatalf("%s %v: %v", tc.name, args, err)
			}
			disp := newDispatcher(cmd, paths)
			if err := disp.Hooks.KubehzRegister(t.Context(), "t.dev", spec); err != nil {
				t.Fatalf("%s %v: register hook: %v\n%s%s", tc.name, args, err, out.String(), errOut.String())
			}
			if flag {
				checkTicketShown(t, tc.name+" --print-token", out.String())
			} else {
				checkTicketHidden(t, tc.name, out.String(), errOut.String())
			}
		}
	}
}
