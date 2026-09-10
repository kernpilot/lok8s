package kubehz

// fingerprint_test.go covers SSHFingerprint per cluster kind. The
// fingerprint tools run through the fake Runner.

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

func TestFingerprintLoKind(t *testing.T) {
	h := newHarness(t)
	fp, err := h.ctx.SSHFingerprint(t.Context(), loSpec(h))
	mustOK(t, err, h.output())
	if fp != "lo:test.kubehz.dev" {
		t.Fatalf("fp = %q", fp)
	}
}

func TestFingerprintKubeOneReadsKeyFile(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("k.dev", "kind: KubeOne\nspec:\n  hcloud:\n    sshPublicKeyFile: "+filepath.Join(h.base, "test_key.pub")+"\n")
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name != "ssh-keygen" || !strings.Contains(argvLine(c), " -E md5") {
			t.Fatalf("ssh-keygen called without -E md5: %s", argvLine(c))
		}
		io.WriteString(c.Stdout, "256 MD5:ec:ea:8f:11:f3:c6:e8:10:c1:58:40:be:24:87:a8:04 test@host (ED25519)\n")
		return nil
	}
	fp, err := h.ctx.SSHFingerprint(t.Context(), spec)
	mustOK(t, err, h.output())
	if fp != "MD5:ec:ea:8f:11:f3:c6:e8:10:c1:58:40:be:24:87:a8:04" {
		t.Fatalf("fp = %q", fp)
	}
	if !strings.Contains(argvLine(h.runner.calls[0]), "-lf "+filepath.Join(h.base, "test_key.pub")) {
		t.Fatalf("key file not passed: %s", argvLine(h.runner.calls[0]))
	}
}

func TestFingerprintCapiQueriesHcloud(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("c.dev", "kind: Capi\nspec:\n  hcloud:\n    sshKeyName: my-key\n")
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		switch c.Name {
		case "hcloud":
			io.WriteString(c.Stdout, `{"public_key": "ssh-ed25519 AAAA mock-capi-key"}`)
		case "ssh-keygen":
			if stdin != "ssh-ed25519 AAAA mock-capi-key\n" || !strings.Contains(argvLine(c), "-E md5") {
				t.Fatalf("ssh-keygen stdin=%q argv=%s", stdin, argvLine(c))
			}
			io.WriteString(c.Stdout, "256 MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99 test@host (ED25519)\n")
		}
		return nil
	}
	fp, err := h.ctx.SSHFingerprint(t.Context(), spec)
	mustOK(t, err, h.output())
	if fp != "MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99" {
		t.Fatalf("fp = %q", fp)
	}
}

func TestFingerprintUnknownKindFails(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("u.dev", "kind: UnknownKind\n")
	_, err := h.ctx.SSHFingerprint(t.Context(), spec)
	mustErr(t, err)
	mustContain(t, h.output(), "Cannot extract SSH fingerprint for kind=unknownkind")
}
