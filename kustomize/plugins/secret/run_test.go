package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runEntry drives secret.Run with the spec on stdin (argv len 1 → stdin
// source) and a controlled env map, returning stdout + error.
func runEntry(t *testing.T, in []byte, env map[string]string) (string, error) {
	t.Helper()
	envFn := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	var out bytes.Buffer
	err := Run([]string{"plugin"}, bytes.NewReader(in), &out, envFn)
	return out.String(), err
}

// countEntries returns the number of directory entries in dir (0 if the
// dir does not exist). Used to prove whether the cache was touched.
func countEntries(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	return len(ents)
}

const passwdSpec = `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: t
  namespace: default
passwd:
  K: 32
`

// TestRun_Disable_EmptyOutput_NoStore proves the store-free OFF switch:
// with LOK8S_SECRETS_DISABLE=1 the plugin emits nothing and succeeds even
// though the spec has a cache-first passwd generator AND no PATH_SECRETS
// is set — so no store access happened.
func TestRun_Disable_EmptyOutput_NoStore(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "True"} {
		t.Run(v, func(t *testing.T) {
			out, err := runEntry(t, []byte(passwdSpec), map[string]string{DisableEnv: v})
			if err != nil {
				t.Fatalf("disable=%q should not error (no store needed): %v", v, err)
			}
			if out != "" {
				t.Errorf("disable=%q should emit nothing, got:\n%s", v, out)
			}
		})
	}
}

// TestRun_Disable_NonexistentStore_NoCacheFile proves DISABLE never reads
// or mints the cache: even pointed at a nonexistent PATH_SECRETS dir, Run
// succeeds, emits nothing, and creates no cache file (the passwd generator
// never ran).
func TestRun_Disable_NonexistentStore_NoCacheFile(t *testing.T) {
	store := filepath.Join(t.TempDir(), "does-not-exist")
	out, err := runEntry(t, []byte(passwdSpec), map[string]string{
		DisableEnv:     "1",
		PathSecretsEnv: store,
	})
	if err != nil {
		t.Fatalf("disable must not touch the (nonexistent) store: %v", err)
	}
	if out != "" {
		t.Errorf("disable should emit nothing, got:\n%s", out)
	}
	if n := countEntries(t, store); n != 0 {
		t.Errorf("disable must not mint a cache file, found %d entries in %s", n, store)
	}
}

// TestRun_OutputNone_RunsGenerators_SuppressesEmit proves OUTPUT=none runs
// the FULL pipeline (the passwd generator mints into the cache) but writes
// nothing to stdout.
func TestRun_OutputNone_RunsGenerators_SuppressesEmit(t *testing.T) {
	store := t.TempDir()
	out, err := runEntry(t, []byte(passwdSpec), map[string]string{
		OutputEnv:      OutputNone,
		PathSecretsEnv: store,
	})
	if err != nil {
		t.Fatalf("output=none should run cleanly: %v", err)
	}
	if out != "" {
		t.Errorf("output=none should suppress emit, got:\n%s", out)
	}
	if n := countEntries(t, store); n == 0 {
		t.Errorf("output=none must run generators and mint the cache, but %s is empty", store)
	}
}

// TestRun_OutputNone_ValidationStillFires proves the suppressed pipeline
// still validates: a TLS secret missing tls.key must error even though the
// output would be discarded.
func TestRun_OutputNone_ValidationStillFires(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: tls
type: kubernetes.io/tls
literals:
  tls.crt: cert-data
`)
	_, err := runEntry(t, in, map[string]string{OutputEnv: OutputNone})
	if err == nil {
		t.Fatal("output=none must still run validation (expected tls.key error)")
	}
	if !strings.Contains(err.Error(), "tls.key") {
		t.Errorf("error should mention tls.key: %v", err)
	}
}

// TestRun_OutputNone_UnknownValue_FailsClosed proves a typo in
// LOK8S_SECRETS_OUTPUT is rejected rather than silently emitting.
func TestRun_OutputNone_UnknownValue_FailsClosed(t *testing.T) {
	_, err := runEntry(t, []byte(passwdSpec), map[string]string{
		OutputEnv:      "nope",
		PathSecretsEnv: t.TempDir(),
	})
	if err == nil {
		t.Fatal("unknown LOK8S_SECRETS_OUTPUT must fail closed")
	}
	if !strings.Contains(err.Error(), OutputEnv) {
		t.Errorf("error should name %s: %v", OutputEnv, err)
	}
}

// TestRun_OutputNone_WhitespaceValue_FailsClosed proves a PRESENT but
// whitespace-only value (trims to "") is rejected, not silently normal-emitted
// — an explicitly-empty "" is still normal (see TestRun_Normal_EmitsSecret with
// OutputEnv:"").
func TestRun_OutputNone_WhitespaceValue_FailsClosed(t *testing.T) {
	_, err := runEntry(t, []byte(passwdSpec), map[string]string{
		OutputEnv:      "  ",
		PathSecretsEnv: t.TempDir(),
	})
	if err == nil {
		t.Fatal("whitespace-only LOK8S_SECRETS_OUTPUT must fail closed")
	}
	if !strings.Contains(err.Error(), OutputEnv) {
		t.Errorf("error should name %s: %v", OutputEnv, err)
	}
}

// TestRun_OutputEmpty_NormalEmit proves an explicitly-EMPTY value is treated as
// unset (normal emit) — the one present value that does not fail closed.
func TestRun_OutputEmpty_NormalEmit(t *testing.T) {
	out, err := runEntry(t, []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: { name: t, namespace: default }
type: Opaque
literals: { K: v }
`), map[string]string{OutputEnv: ""})
	if err != nil {
		t.Fatalf("empty OUTPUT should emit normally: %v", err)
	}
	if len(out) == 0 {
		t.Error("empty OUTPUT must NOT suppress the emit")
	}
}

// TestRun_Normal_EmitsSecret is the regression: neither knob set → the
// plugin renders the Secret as before.
func TestRun_Normal_EmitsSecret(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: test
  namespace: default
type: Opaque
literals:
  KEY: value
`)
	out, err := runEntry(t, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kind: Secret") || !strings.Contains(out, "name: test") {
		t.Errorf("normal run should emit the Secret, got:\n%s", out)
	}
}

// TestRun_DisableWinsOverOutput proves precedence: DISABLE beats OUTPUT, so
// even OUTPUT=none present alongside DISABLE means no store access at all
// (nonexistent store, no cache file, empty output, no error).
func TestRun_DisableWinsOverOutput(t *testing.T) {
	store := filepath.Join(t.TempDir(), "nope")
	out, err := runEntry(t, []byte(passwdSpec), map[string]string{
		DisableEnv:     "1",
		OutputEnv:      OutputNone,
		PathSecretsEnv: store,
	})
	if err != nil {
		t.Fatalf("disable+output=none: disable wins, no store access: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output, got:\n%s", out)
	}
	if n := countEntries(t, store); n != 0 {
		t.Errorf("disable must not touch the store even with output=none set, found %d entries", n)
	}
}
