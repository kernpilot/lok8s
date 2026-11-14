package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// specMetaFields are the fields of spec.Secret that are not a section of
// generated keys.
var specMetaFields = map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "type": true, "immutable": true, "validate": true}

// sectionNames is the name of each key generator of the keys-only run.
func sectionNames(t *testing.T) []string {
	t.Helper()
	p := New()
	if err := p.Decode(strings.NewReader("apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata: {name: x}\n")); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range p.keySections() {
		names = append(names, g.Name())
	}
	return names
}

// Each section of the spec has a key generator in the keys-only run, so a
// section added to the spec and to Build cannot drop out of keySections
// unseen.
func TestKeysOnly_EverySpecSectionHasKeys(t *testing.T) {
	var sections []string
	typ := reflect.TypeFor[spec.Secret]()
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if !specMetaFields[name] {
			sections = append(sections, name)
		}
	}
	keys := sectionNames(t)
	sort.Strings(sections)
	sort.Strings(keys)
	if !reflect.DeepEqual(sections, keys) {
		t.Errorf("spec sections %v, keys-only sections %v", sections, keys)
	}
}

// realRunKeys runs Decode, Build, the registry and Emit (a real run) in
// fileRoot and returns the data keys of the emitted Secret.
func realRunKeys(t *testing.T, in []byte, env map[string]string, fileRoot string) []string {
	t.Helper()
	p := New()
	if err := p.Decode(bytes.NewReader(in)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, ctx, err := p.Build(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, fileRoot)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	entries, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("real run: %v", err)
	}
	var out bytes.Buffer
	if err := p.Emit(entries, &out); err != nil {
		t.Fatalf("emit: %v", err)
	}
	return dataKeys(decodeSecret(t, out.String()))
}

// The keys-only key set is the key set of a real run of the same spec. The
// spec fills every section, with inputs that let a real run complete in a
// test directory: a fixture store (with the secretRef source), a file, the
// environment variables set, a CAROOT. (An env: entry with optional: true
// whose variable is unset is the one documented difference; this spec has
// none.)
func TestKeysOnly_KeySetMatchesARealRun(t *testing.T) {
	base := t.TempDir()
	store, caroot, root := filepath.Join(base, "store"), filepath.Join(base, "caroot"), filepath.Join(base, "root")
	for _, d := range []string{store, caroot, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("file-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "Secret.other-secret.demo.password"), []byte("ref-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: app
  namespace: demo
type: Opaque
literals: { LIT: literal-value }
env:
  ENV_REQUIRED: { var: KEYS_ONLY_TEST_SET }
  ENV_OPTIONAL: { var: KEYS_ONLY_TEST_SET, optional: true }
  ENV_DEFAULT: { var: KEYS_ONLY_TEST_UNSET, default: from-default }
b64: { B64: dmFsdWU= }
file: { FILE: ./file.txt }
passwd: { PASSWD: 32 }
key: { private.pem: ed25519 }
template:
  TEMPLATE: { pattern: "a={p}", literals: { p: v } }
bash: { BASH: "echo from-bash" }
secretRef: { REF: other-secret/password }
htpasswd:
  auth: { username: user, password: { length: 16 } }
cert:
  hosts: [app.example.test]
  caRef: own-ca/demo
`)
	env := map[string]string{
		PathSecretsEnv:       store,
		"CAROOT":             caroot,
		"KEYS_ONLY_TEST_SET": "set",
		"PATH":               os.Getenv("PATH"),
	}
	real := realRunKeys(t, in, env, root)
	out, err := runEntry(t, in, keysOnlyEnv(nil))
	if err != nil {
		t.Fatalf("keys-only run: %v", err)
	}
	if keys := dataKeys(decodeSecret(t, out)); !reflect.DeepEqual(keys, real) {
		t.Errorf("keys-only keys %v, real run keys %v", keys, real)
	}
	if sections := sectionNames(t); len(real) < len(sections) {
		t.Errorf("the real run emitted %d keys for %d sections: the spec does not fill every section", len(real), len(sections))
	}
}

// `Secret --capabilities` lists the keys-only run: lo build --keys-only
// refuses an exec plugin without it.
func TestCapabilitiesListKeysOnly(t *testing.T) {
	got := strings.Split(Capabilities, "\n")
	if !reflect.DeepEqual(got, []string{"dry-run", "keys-only"}) {
		t.Errorf("Capabilities = %q, want the lines dry-run and keys-only", Capabilities)
	}
}
