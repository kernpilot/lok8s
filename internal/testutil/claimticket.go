package testutil

// claimticket.go is the claim ticket fixture of the register tests: a valid
// ed25519 public key and the MD5 fingerprint of its key blob, the one-time
// claim ticket that the platform returns with the claim key. The bats twin
// has the same values in tests/unit/kubehz_register_test.bats.
//
// Each form in claimTicketForms is a literal of its own. None comes from
// the code under test or from another form, so a check cannot share a bug
// with the format code of the register.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// ClaimKeyPublic is the public claim key that the platform mints (an
	// ssh-keygen output for these tests; the private key does not exist).
	ClaimKeyPublic = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILb+3WQ5OfPMd1OnV0p1lHo1cEizaxwddsmWOt+c+dpL claim-ticket-fixture"
	// ClaimTicket is the MD5 fingerprint of ClaimKeyPublic, as the
	// platform returns it: lower case, with colons.
	ClaimTicket = "04:35:55:25:a3:3d:b3:9a:bb:d9:10:20:56:b8:92:31"
)

// claimTicketForms are the forms of the ticket that no output may hold
// when the ticket is hidden: the ticket with colons and without, in lower
// and upper case, and the base64 key blob, whose MD5 the ticket is.
var claimTicketForms = []string{
	"04:35:55:25:a3:3d:b3:9a:bb:d9:10:20:56:b8:92:31",
	"04:35:55:25:A3:3D:B3:9A:BB:D9:10:20:56:B8:92:31",
	"04355525a33db39abbd9102056b89231",
	"04355525A33DB39ABBD9102056B89231",
	"AAAAC3NzaC1lZDI1NTE5AAAAILb+3WQ5OfPMd1OnV0p1lHo1cEizaxwddsmWOt+c+dpL",
}

// NoClaimTicket fails t when s holds the claim ticket fixture in any form.
// where names the place of s (stdout, stderr, the error, a file).
func NoClaimTicket(t testing.TB, where, s string) {
	t.Helper()
	for _, f := range claimTicketForms {
		if strings.Contains(s, f) {
			t.Errorf("%s holds the claim ticket (the form %q)", where, f)
		}
	}
}

// NoClaimTicketInFiles runs NoClaimTicket on each regular file under root.
// skip names the files (relative to root) that the test itself writes, for
// example the request log of a stub.
func NoClaimTicketInFiles(t testing.TB, root string, skip ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		for _, s := range skip {
			if rel == s {
				return nil
			}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		NoClaimTicket(t, "the file "+rel, string(b))
		return nil
	})
	if err != nil {
		t.Errorf("walk %s: %v", root, err)
	}
}
