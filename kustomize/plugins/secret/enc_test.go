package secret

// enc_test.go: the plugin run end to end (secret.Run, the entry point
// cmd/secret and the in-process render call) against a store that holds a
// committed .enc but not its plaintext (B425). It must stop and name the
// entry, never generate a new value over the encrypted one.

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// encStore makes clusters/alpha.dev/secrets with one encrypted twin per name.
func encStore(t *testing.T, names ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n+".enc"), []byte(`{"data":"ENC[AES256_GCM,data:fake]","sops":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func listing(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return strings.Join(names, " ")
}

func TestRun_EncryptedTwinStopsTheGenerators(t *testing.T) {
	const decryptHint = `Run "lo secrets decrypt --domain alpha.dev", then build again`
	cases := []struct {
		name, spec, entry string
		env               map[string]string
	}{
		{"passwd", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: db, namespace: default}
passwd:
  PASSWORD: 32
`, "Secret.db.default.PASSWORD", nil},
		{"env without update", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: api, namespace: default}
env:
  TOKEN: {var: API_TOKEN}
`, "Secret.api.default.TOKEN", map[string]string{"API_TOKEN": "from-env"}},
		{"env passwd fallback", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: app, namespace: default}
env:
  SESSION_KEY: {var: SK, passwd: {length: 64}}
`, "Secret.app.default.SESSION_KEY", nil},
		{"key", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: signer, namespace: default}
key:
  private.pem: {algorithm: ed25519}
`, "Secret.signer.default.private.pem", nil},
		{"secretRef", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: consumer, namespace: default}
secretRef:
  DB_PASSWORD: db/PASSWORD
`, "Secret.db.default.PASSWORD", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := encStore(t, c.entry)
			env := map[string]string{"PATH_SECRETS": store}
			maps.Copy(env, c.env)
			before := listing(t, store)
			out, err := runEntry(t, []byte(c.spec), env)
			if err == nil {
				t.Fatalf("no error; output:\n%s", out)
			}
			msg := err.Error()
			if !strings.Contains(msg, "cannot read "+c.entry+": the store "+store+" has "+c.entry+".enc but no plaintext.") ||
				!strings.HasSuffix(msg, decryptHint) {
				t.Errorf("error = %s", msg)
			}
			if after := listing(t, store); after != before {
				t.Errorf("the store changed:\nbefore %s\nafter  %s", before, after)
			}
			if out != "" {
				t.Errorf("the plugin emitted output: %s", out)
			}
		})
	}
}

// update: true is the deliberate "always take the env value" path. It writes
// over the plaintext slot whatever the store holds, so it keeps working.
func TestRun_EnvUpdateWritesOverAnEncryptedTwin(t *testing.T) {
	store := encStore(t, "Secret.api.default.TOKEN")
	spec := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: api, namespace: default}
env:
  TOKEN: {var: API_TOKEN, update: true}
`)
	out, err := runEntry(t, spec, map[string]string{"PATH_SECRETS": store, "API_TOKEN": "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TOKEN: ZnJlc2g=") {
		t.Errorf("output: %s", out)
	}
	if raw, _ := os.ReadFile(filepath.Join(store, "Secret.api.default.TOKEN")); string(raw) != "fresh" {
		t.Errorf("stored = %q", raw)
	}
}

// With neither the plaintext nor the .enc, the plugin generates (the first
// build, and the rotation path after both files are deleted).
func TestRun_GeneratesWhenNeitherExists(t *testing.T) {
	store := encStore(t)
	out, err := runEntry(t, []byte(passwdSpec), map[string]string{"PATH_SECRETS": store})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "K: ") {
		t.Errorf("output: %s", out)
	}
	if raw, err := os.ReadFile(filepath.Join(store, "Secret.t.default.K")); err != nil || len(raw) != 32 {
		t.Errorf("stored = %d bytes, %v", len(raw), err)
	}
}

// The plugin checks every entry a Secret uses before any generator runs. A
// refusal in the middle of the run left the store half-written: an earlier
// key (no .enc, no plaintext) was generated, then a later key (.enc only)
// stopped the run.
func TestRun_EncryptedTwinStopsBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name, spec, encOnly string
		env                 map[string]string
	}{
		{"passwd: A new, B encrypted-only", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: db, namespace: default}
passwd:
  A: 32
  B: 32
`, "Secret.db.default.B", nil},
		{"htpasswd: username new, password encrypted-only", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: web, namespace: default}
htpasswd:
  auth: {}
`, "Secret.web.default.auth.password", nil},
		{"cert: ca.key new, ca.crt encrypted-only", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: myca, namespace: default}
cert:
  ca: true
`, "Secret.myca.default.ca.crt", nil},
		{"passwd new, secretRef target encrypted-only", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: app, namespace: default}
passwd:
  A: 32
secretRef:
  DB: db/PASSWORD
`, "Secret.db.default.PASSWORD", nil},
		{"leaf key new, caRef CA encrypted-only", `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: leaf, namespace: default}
cert:
  hosts: [a.example.dev]
  caRef: myca
`, "Secret.myca.default.ca.key", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := encStore(t, c.encOnly)
			env := map[string]string{"PATH_SECRETS": store}
			maps.Copy(env, c.env)
			before := listing(t, store)
			if _, err := runEntry(t, []byte(c.spec), env); err == nil ||
				!strings.Contains(err.Error(), "cannot read "+c.encOnly+": ") {
				t.Fatalf("err = %v", err)
			}
			if after := listing(t, store); after != before {
				t.Errorf("the store changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}
