package cli

// The one-time claim ticket of a claim-key registration goes to stdout only
// with --print-token, or when stdout is a terminal and the environment is
// not a CI run. These tests run each command that can register a cluster
// (lo kubehz register, lo provision, lo up) against a TLS stub of the
// platform api and of the Hetzner Cloud api, in a project in the test's
// temp dir. lo provision and lo up run only the register hook of their
// dispatcher, so no driver runs. The fixture is a real key and its real
// MD5 fingerprint (internal/testutil).

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/ui"
)

// The ticket lines with the ticket, and the lines in their place on a
// stdout that is not a terminal.
const (
	ticketShownCurl  = "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"" + testutil.ClaimTicket + "\"}'\n"
	ticketShownFP    = "  fingerprint: " + testutil.ClaimTicket + "   (also visible in Hetzner Console -> Security -> SSH keys)\n"
	ticketHiddenCurl = "       -H 'Content-Type: application/json' -d '{\"fingerprint\":\"<hidden: one-time claim ticket>\"}'\n"
	ticketHiddenFP   = "  fingerprint: <hidden: one-time claim ticket>   (in Hetzner Console -> Security -> SSH keys, key 'kubehz-claim-t.dev'; 'lo kubehz register --domain t.dev --print-token' registers again and shows a new ticket)\n"
)

// claimTicketProject starts the stubs and writes a project with the domain
// t.dev (access registered, apiUrl the stub). It sets the environment of a
// claim-key registration (CI and LOK8S_NONINTERACTIVE empty, so the run is
// the same on a CI runner) and the client of the kubehz commands. It
// returns the paths of the project and the path of the spec. Not for
// parallel tests: it sets the environment and kubehzHTTP.
func claimTicketProject(t *testing.T) (*config.Paths, string) {
	t.Helper()
	// 64 lowercase hex digits, the form that the register keeps. The test
	// builds it at run time, so the source holds no token-like literal.
	bind := strings.Repeat("9f1c2b3a", 8)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/clusters/register":
			_, _ = io.WriteString(w, `{"id":"cl-5","bindSecret":"`+bind+`","claimKey":{"publicKey":"`+testutil.ClaimKeyPublic+`","fingerprint":"`+testutil.ClaimTicket+`","name":"kubehz-claim-t.dev"}}`)
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
	tmp := filepath.Join(base, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	body := "kind: Lo\nspec:\n  cluster:\n    domain: t.dev\n  kubehz:\n    access: registered\n    apiUrl: " + srv.URL + "\n"
	if err := os.WriteFile(spec, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"DOMAIN_NAME": "", "KUBEHZ_TOKEN": "", "CI": "", "LOK8S_NONINTERACTIVE": "",
		"HCLOUD_TOKEN": "hc_test", "HCLOUD_API_BASE": srv.URL, "TMPDIR": tmp,
	} {
		t.Setenv(k, v)
	}
	paths := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	return paths, spec
}

// runKubehzLoTo is runKubehzLo with the writers of the caller, so a test
// can make stdout alone a terminal (ui.Styled).
func runKubehzLoTo(base string, out, errOut io.Writer, args ...string) error {
	paths := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	root := NewRoot(paths)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	return root.Execute()
}

// checkTicketHidden checks a registration that must not show the ticket:
// none of its five literal forms (testutil.NoClaimTicket) on stdout, on
// stderr, in the error or in a file of the project, and the two lines in
// place of the ticket lines.
func checkTicketHidden(t *testing.T, name, base, out, errOut string, err error) {
	t.Helper()
	if err != nil {
		testutil.NoClaimTicket(t, name+": the error", err.Error())
		t.Fatalf("%s: %v\n%s%s", name, err, out, errOut)
	}
	testutil.NoClaimTicket(t, name+": stdout", out)
	testutil.NoClaimTicket(t, name+": stderr", errOut)
	testutil.NoClaimTicketInFiles(t, base)
	if !strings.Contains(out, ticketHiddenCurl+ticketHiddenFP) {
		t.Errorf("%s: stdout has no placeholder lines:\n%s", name, out)
	}
}

// checkTicketShown checks that stdout holds both ticket lines.
func checkTicketShown(t *testing.T, name, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	if !strings.Contains(out, ticketShownCurl+ticketShownFP) {
		t.Errorf("%s: stdout has no ticket lines:\n%s", name, out)
	}
	if strings.Contains(out, "<hidden: one-time claim ticket>") {
		t.Errorf("%s: stdout holds the placeholder:\n%s", name, out)
	}
}

// lo kubehz register: a buffer (not a terminal) gets no ticket, also when
// stderr is a terminal.
func TestKubehzRegisterKeepsTheClaimTicketOffAPipe(t *testing.T) {
	for _, errTTY := range []bool{false, true} {
		paths, _ := claimTicketProject(t)
		var out, errOut bytes.Buffer
		var ew io.Writer = &errOut
		if errTTY {
			ew = ui.Styled(&errOut, ui.Style{TTY: true})
		}
		err := runKubehzLoTo(paths.Base, &out, ew, "kubehz", "register", "--domain", "t.dev")
		checkTicketHidden(t, "register", paths.Base, out.String(), errOut.String(), err)
	}
}

// --print-token shows the ticket on a buffer, also in a CI run.
func TestKubehzRegisterPrintTokenShowsTheClaimTicketOnAPipe(t *testing.T) {
	for _, ci := range []string{"", "true"} {
		paths, _ := claimTicketProject(t)
		t.Setenv("CI", ci)
		var out, errOut bytes.Buffer
		err := runKubehzLoTo(paths.Base, &out, &errOut, "kubehz", "register", "--domain", "t.dev", "--print-token")
		checkTicketShown(t, "register --print-token (CI="+ci+")", out.String(), err)
	}
}

// A terminal on stdout (ui.Styled, the stdout writer only) shows the
// ticket without the flag. In a CI run it does not.
func TestKubehzRegisterShowsTheClaimTicketOnATerminal(t *testing.T) {
	paths, _ := claimTicketProject(t)
	var out, errOut bytes.Buffer
	err := runKubehzLoTo(paths.Base, ui.Styled(&out, ui.Style{TTY: true}), &errOut, "kubehz", "register", "--domain", "t.dev")
	checkTicketShown(t, "register on a terminal", out.String(), err)

	for _, env := range [][2]string{{"CI", "true"}, {"LOK8S_NONINTERACTIVE", "1"}} {
		paths, _ := claimTicketProject(t)
		t.Setenv(env[0], env[1])
		var out, errOut bytes.Buffer
		err := runKubehzLoTo(paths.Base, ui.Styled(&out, ui.Style{TTY: true}), &errOut, "kubehz", "register", "--domain", "t.dev")
		checkTicketHidden(t, "register on a terminal with "+env[0]+"="+env[1], paths.Base, out.String(), errOut.String(), err)
	}
}

// lo provision and lo up take --print-token to the register hook of their
// dispatcher.
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
			err := disp.Hooks.KubehzRegister(t.Context(), "t.dev", spec)
			if flag {
				checkTicketShown(t, tc.name+" --print-token", out.String(), err)
			} else {
				checkTicketHidden(t, tc.name, paths.Base, out.String(), errOut.String(), err)
			}
		}
	}
}
