package secrets

// readops_test.go covers the read-only verbs: allow, list, print and env.
// The goldens pin the bash output byte for byte.

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestAllow(t *testing.T) {
	c, out, _ := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	os.MkdirAll(store, 0o755)

	if err := c.Allow(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "No bash: entries found (no .sha files)\n" {
		t.Errorf("stdout: %q", out.String())
	}

	write(t, store+"/Secret.gen.default.X.sha", "deadbeef  \n")
	write(t, store+"/other.sha", "cafe\n")
	write(t, store+"/dup.sha", "cafe\n")
	out.Reset()
	if err := c.Allow(); err != nil {
		t.Fatal(err)
	}
	want := "Approved 3 bash: entry/entries\n  Secret.gen.default.X.sha\n  dup.sha\n  other.sha\n"
	if out.String() != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), want)
	}
	allow, err := os.ReadFile(store + "/.bash-allow")
	if err != nil {
		t.Fatal(err)
	}
	if string(allow) != "cafe\ndeadbeef\n" {
		t.Errorf(".bash-allow: %q", allow)
	}
	info, _ := os.Stat(store + "/.bash-allow")
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
}

func TestListGolden(t *testing.T) {
	c, out, errOut := testEnv(t)
	store := c.Paths.Base + "/.secrets"

	// Missing store → warn, exit 0.
	if err := c.List(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "Secrets directory not found: "+store) {
		t.Errorf("stderr: %s", errOut.String())
	}

	write(t, store+"/Secret.a.default.K", "v")
	write(t, store+"/Secret.a.default.K.enc", "e")
	write(t, store+"/Secret.b.default.K", "v")
	write(t, store+"/Secret.c.default.K.enc", "e")
	write(t, store+"/Secret.s.default.X.sha", "h")
	out.Reset()
	if err := c.List(); err != nil {
		t.Fatal(err)
	}
	want := "Secret.a.default.K (encrypted)\n" +
		"Secret.b.default.K (plaintext)\n" +
		"Secret.s.default.X.sha (plaintext)\n" +
		"Secret.c.default.K.enc (needs decrypt)\n"
	if out.String() != want {
		t.Errorf("list:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestPrintGolden(t *testing.T) {
	c, out, errOut := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.ML", "line1\nline2")
	write(t, store+"/Secret.app.default.ONE", "raw-value")
	write(t, store+"/Secret.app.default.ONE.enc", "x")

	// Single match → raw cat, no decoration.
	if err := c.Print(t.Context(), []string{"ONE"}, false, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "raw-value" {
		t.Errorf("single: %q", out.String())
	}

	// Multiple matches → green basename + content + two newlines each.
	out.Reset()
	if err := c.Print(t.Context(), []string{"app"}, false, false); err != nil {
		t.Fatal(err)
	}
	want := "\033[0;32mSecret.app.default.ML\033[0m\nline1\nline2\n\n" +
		"\033[0;32mSecret.app.default.ONE\033[0m\nraw-value\n\n"
	if out.String() != want {
		t.Errorf("multi:\n%q\nwant:\n%q", out.String(), want)
	}

	// only-one with multiple → error to stderr, green FULL paths to stdout.
	out.Reset()
	errOut.Reset()
	if err := c.Print(t.Context(), []string{"app"}, true, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if !strings.Contains(errOut.String(), "Multiple matches found:") {
		t.Errorf("stderr: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "\033[0;32m"+store+"/Secret.app.default.ML\033[0m\n") {
		t.Errorf("stdout: %q", out.String())
	}

	// Zero matches, only-one → SAME quirk branch (empty list), preserved
	// from bash.
	out.Reset()
	errOut.Reset()
	if err := c.Print(t.Context(), []string{"zzz"}, true, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if !strings.Contains(errOut.String(), "Multiple matches found:") || out.String() != "" {
		t.Errorf("quirk: out=%q err=%s", out.String(), errOut.String())
	}

	// Zero matches → No matches found.
	errOut.Reset()
	if err := c.Print(t.Context(), []string{"zzz"}, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if !strings.Contains(errOut.String(), "No matches found") {
		t.Errorf("stderr: %s", errOut.String())
	}

	// Patterns AND-match case-insensitively.
	out.Reset()
	if err := c.Print(t.Context(), []string{"ml", "APP"}, false, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "line1\nline2" {
		t.Errorf("and-match: %q", out.String())
	}
}

func TestEnvGolden(t *testing.T) {
	c, out, errOut := testEnv(t)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.hetzner.default.HCLOUD_TOKEN", "tok en$1")
	write(t, store+"/Secret.hetzner.default.PLAIN", "simple")
	write(t, store+"/Secret.hetzner.default.PLAIN.enc", "x")
	write(t, store+"/Secret.hetzner.default.SKIP.sha", "h")
	write(t, store+"/Secret.hetzner.other.NOPE", "other-ns")

	if err := c.Env("hetzner", "default"); err != nil {
		t.Fatal(err)
	}
	want := "export HCLOUD_TOKEN=tok\\ en\\$1\nexport PLAIN=simple\n"
	if out.String() != want {
		t.Errorf("env:\n%q\nwant:\n%q", out.String(), want)
	}

	errOut.Reset()
	if err := c.Env("nope", "default"); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if !strings.Contains(errOut.String(), "No cached keys for nope/default in "+store) {
		t.Errorf("stderr: %s", errOut.String())
	}

	errOut.Reset()
	if err := c.Env("", "default"); !errors.Is(err, ErrHandled) || !strings.Contains(errOut.String(), "Secret --name is required") {
		t.Fatalf("name check: %v %s", err, errOut.String())
	}
}
