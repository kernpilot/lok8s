package secret

import (
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// keysOnlyEnv is the env of a keys-only run, plus extra entries.
func keysOnlyEnv(extra map[string]string) map[string]string {
	env := map[string]string{DisableEnv: "1", KeysOnlyEnv: "1"}
	maps.Copy(env, extra)
	return env
}

// storeSnapshot is the name, size, mode and mtime of every file under dir.
func storeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fmt.Sprintf("%v %v %d", info.Mode(), info.ModTime(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// secretDoc decodes one emitted Secret.
type secretDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Immutable *bool             `yaml:"immutable"`
	Type      string            `yaml:"type"`
	Data      map[string]string `yaml:"data"`
}

func decodeSecret(t *testing.T, out string) secretDoc {
	t.Helper()
	var d secretDoc
	if err := yaml.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("decode the emitted Secret: %v\n%s", err, out)
	}
	return d
}

func dataKeys(d secretDoc) []string {
	keys := make([]string, 0, len(d.Data))
	for k := range d.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The canary is a value that only the store holds.
const storeCanary = "store-canary-value-6b1f"

// allSectionsSpec names a key in every section. A real run of it reads the
// store (passwd, key, env, template, bash, secretRef, htpasswd, cert), a
// file (file:), environment variables (env:), runs a script (bash:) and
// reads or makes a CA (cert:).
func allSectionsSpec(marker string) string {
	return `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: app
  namespace: demo
  labels: { team: a }
  annotations: { note: kept }
type: Opaque
immutable: true
literals: { LIT: literal-value }
env:
  ENV_REQUIRED: { var: KEYS_ONLY_TEST_UNSET }
  ENV_OPTIONAL: { var: KEYS_ONLY_TEST_UNSET, optional: true }
  ENV_DEFAULT: { var: KEYS_ONLY_TEST_UNSET, default: from-default }
b64: { B64: dmFsdWU= }
file: { FILE: ./does-not-exist.txt }
passwd: { PASSWD: 32, CACHED: 32 }
key: { private.pem: ed25519 }
template:
  TEMPLATE: { pattern: "a={p}", literals: { p: v } }
bash: { BASH: "touch ` + marker + `; echo from-bash" }
secretRef: { REF: other-secret/password }
htpasswd:
  auth: { username: user, password: { length: 16 } }
cert:
  hosts: [app.example.test]
  caRef: own-ca/demo
`
}

// TestKeysOnly_EmitsEveryKeyEmpty is the keys-only contract: every key that
// the spec names, an empty value for each, the annotation, the metadata
// kept; and nothing read but the spec. The store holds an .enc-only entry
// (a real run refuses it before anything else, B425) and a plaintext
// canary for another key, so a run that opens, reads or mints the store
// fails, emits the canary or adds a file. The bash script would leave a
// marker; CAROOT is an empty directory that a CA load would fill.
func TestKeysOnly_EmitsEveryKeyEmpty(t *testing.T) {
	base := t.TempDir()
	store, caroot := filepath.Join(base, "store"), filepath.Join(base, "caroot")
	for _, d := range []string{store, caroot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"Secret.app.demo.PASSWD.enc": "sops-encrypted",
		"Secret.app.demo.CACHED":     storeCanary,
	} {
		if err := os.WriteFile(filepath.Join(store, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(base, "bash-ran")
	before, cabefore := storeSnapshot(t, store), storeSnapshot(t, caroot)

	out, err := runEntry(t, []byte(allSectionsSpec(marker)), keysOnlyEnv(map[string]string{
		PathSecretsEnv: store,
		"CAROOT":       caroot,
	}))
	if err != nil {
		t.Fatalf("keys-only run: %v", err)
	}
	d := decodeSecret(t, out)
	want := []string{"B64", "BASH", "CACHED", "ENV_DEFAULT", "ENV_OPTIONAL", "ENV_REQUIRED", "FILE", "LIT",
		"PASSWD", "REF", "TEMPLATE", "auth", "private.pem", "tls.crt", "tls.key"}
	if got := dataKeys(d); !reflect.DeepEqual(got, want) {
		t.Errorf("data keys:\n got %v\nwant %v", got, want)
	}
	for k, v := range d.Data {
		if v != "" {
			t.Errorf("data[%s] = %q, want an empty value", k, v)
		}
	}
	if got := d.Metadata.Annotations[KeysOnlyAnnotation]; got != "true" {
		t.Errorf("annotation %s = %q, want \"true\"", KeysOnlyAnnotation, got)
	}
	if d.Metadata.Annotations["note"] != "kept" || d.Metadata.Labels["team"] != "a" {
		t.Errorf("metadata not kept: %+v", d.Metadata)
	}
	if d.Kind != "Secret" || d.APIVersion != "v1" || d.Metadata.Name != "app" || d.Metadata.Namespace != "demo" ||
		d.Type != "Opaque" || d.Immutable == nil || !*d.Immutable {
		t.Errorf("Secret shape not kept: %+v", d)
	}
	if strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(storeCanary))) || strings.Contains(out, storeCanary) {
		t.Errorf("the store canary reached the output:\n%s", out)
	}
	if after := storeSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Errorf("the store changed:\nbefore %v\nafter  %v", before, after)
	}
	if after := storeSnapshot(t, caroot); !reflect.DeepEqual(cabefore, after) {
		t.Errorf("CAROOT changed: %v", after)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the bash: script ran")
	}
}

// TestKeysOnly_ArgvSpec reads the spec from argv[1], the way kustomize
// calls an exec generator.
func TestKeysOnly_ArgvSpec(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(spec, []byte("apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata: {name: a}\nliterals: {K: v}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	env := keysOnlyEnv(nil)
	if err := Run([]string{"Secret", spec}, strings.NewReader(""), &out, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}); err != nil {
		t.Fatal(err)
	}
	if d := decodeSecret(t, out.String()); !reflect.DeepEqual(dataKeys(d), []string{"K"}) || d.Data["K"] != "" {
		t.Errorf("argv spec: %+v", d)
	}
}

// TestKeysOnly_WithoutDisable_Fails: keys-only without DISABLE on is an
// error before anything is read, never a run that reads the store.
func TestKeysOnly_WithoutDisable_Fails(t *testing.T) {
	for _, disable := range []string{"<unset>", "", "0", "false", "yes"} {
		t.Run(disable, func(t *testing.T) {
			store := t.TempDir()
			if err := os.WriteFile(filepath.Join(store, "Secret.t.default.K"), []byte(storeCanary), 0o600); err != nil {
				t.Fatal(err)
			}
			before := storeSnapshot(t, store)
			env := map[string]string{KeysOnlyEnv: "1", PathSecretsEnv: store}
			if disable != "<unset>" {
				env[DisableEnv] = disable
			}
			out, err := runEntry(t, []byte(passwdSpec), env)
			if err == nil || !strings.Contains(err.Error(), KeysOnlyEnv) || !strings.Contains(err.Error(), DisableEnv) {
				t.Fatalf("want an error naming both variables, got %v", err)
			}
			if out != "" {
				t.Errorf("emitted:\n%s", out)
			}
			if after := storeSnapshot(t, store); !reflect.DeepEqual(before, after) {
				t.Errorf("the store changed: %v", after)
			}
		})
	}
}

// TestKeysOnly_UnknownValue_Fails: a value that is neither on nor off is an
// error, with DISABLE on and without it.
func TestKeysOnly_UnknownValue_Fails(t *testing.T) {
	for _, v := range []string{"yes", "2", "on", "keys", "  "} {
		for _, disable := range []string{"1", "0"} {
			t.Run(v+"/disable="+disable, func(t *testing.T) {
				out, err := runEntry(t, []byte(passwdSpec), map[string]string{KeysOnlyEnv: v, DisableEnv: disable})
				if err == nil || !strings.Contains(err.Error(), KeysOnlyEnv+": unknown value") {
					t.Fatalf("want the unknown-value error, got %v", err)
				}
				if out != "" {
					t.Errorf("emitted:\n%s", out)
				}
			})
		}
	}
}

// TestKeysOnly_OffValues: an off value with DISABLE on is the plain
// store-free run: nothing emitted.
func TestKeysOnly_OffValues(t *testing.T) {
	for _, v := range []string{"", "0", "false", "FALSE", " 0 "} {
		t.Run(v, func(t *testing.T) {
			out, err := runEntry(t, []byte(passwdSpec), map[string]string{KeysOnlyEnv: v, DisableEnv: "1"})
			if err != nil || out != "" {
				t.Fatalf("want nothing and no error, got %v:\n%s", err, out)
			}
		})
	}
}

// TestKeysOnly_OnValues: each on value emits the keys.
func TestKeysOnly_OnValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " 1 "} {
		t.Run(v, func(t *testing.T) {
			out, err := runEntry(t, []byte(passwdSpec), map[string]string{KeysOnlyEnv: v, DisableEnv: "1"})
			if err != nil {
				t.Fatal(err)
			}
			if d := decodeSecret(t, out); !reflect.DeepEqual(dataKeys(d), []string{"K"}) || d.Data["K"] != "" {
				t.Errorf("want the key K, empty: %+v", d)
			}
		})
	}
}

// TestKeysOnly_DisableWinsOverOutput: DISABLE wins over OUTPUT, so the
// keys are emitted with OUTPUT=none as well.
func TestKeysOnly_DisableWinsOverOutput(t *testing.T) {
	out, err := runEntry(t, []byte(passwdSpec), keysOnlyEnv(map[string]string{OutputEnv: OutputNone}))
	if err != nil || !strings.Contains(out, "kind: Secret") {
		t.Fatalf("want the keys-only Secret, got %v:\n%s", err, out)
	}
}

// TestKeysOnly_RefusesLikeARealRun: a key from two sections, a Secret type
// without its required keys, a cert spec with conflicting fields and an
// unknown spec field fail the keys-only run with the error of a real run.
func TestKeysOnly_RefusesLikeARealRun(t *testing.T) {
	head := "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata: {name: a, namespace: n}\n"
	for name, body := range map[string]string{
		"collision":         "literals: {K: x}\nb64: {K: eA==}\n",
		"type keys":         "type: kubernetes.io/tls\nliterals: {tls.crt: x}\n",
		"caRoot with hosts": "cert: {caRoot: true, hosts: [a.test]}\n",
		"ca with hosts":     "cert: {ca: true, hosts: [a.test]}\n",
		"ca with caRef":     "cert: {ca: true, caRef: x}\n",
		"leaf includeKey":   "cert: {hosts: [a.test], includeKey: true}\n",
		"leaf without host": "cert: {caRef: x}\n",
		"unknown field":     "literals: {K: x}\nbogus: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, realErr := runEntry(t, []byte(head+body), map[string]string{PathSecretsEnv: t.TempDir()})
			if realErr == nil {
				t.Fatal("the real run passed: the case is wrong")
			}
			out, err := runEntry(t, []byte(head+body), keysOnlyEnv(nil))
			if err == nil || err.Error() != realErr.Error() {
				t.Errorf("keys-only error %v, want the real run's %v", err, realErr)
			}
			if out != "" {
				t.Errorf("emitted:\n%s", out)
			}
		})
	}
	// validate: false lifts the type check in both.
	out, err := runEntry(t, []byte(head+"type: kubernetes.io/tls\nvalidate: false\nliterals: {tls.crt: x}\n"), keysOnlyEnv(nil))
	if err != nil || !strings.Contains(out, "tls.crt") {
		t.Errorf("validate: false: %v\n%s", err, out)
	}
}

// TestKeysOnly_CertKeys: each cert shape gives the keys of a real run.
func TestKeysOnly_CertKeys(t *testing.T) {
	for body, want := range map[string][]string{
		"cert: {caRoot: true}\n":                      {"ca.crt"},
		"cert: {caRoot: true, includeKey: true}\n":    {"tls.crt", "tls.key"},
		"cert: {ca: true}\n":                          {"ca.crt"},
		"cert: {ca: true, includeKey: true}\n":        {"tls.crt", "tls.key"},
		"cert: {hosts: [a.test]}\n":                   {"tls.crt", "tls.key"},
		"cert: {hosts: [a.test], caRef: ca/other}\n":  {"tls.crt", "tls.key"},
		"type: kubernetes.io/tls\ncert: {ca: true}\n": nil, // ca.crt only: the tls type refuses it
	} {
		t.Run(body, func(t *testing.T) {
			out, err := runEntry(t, []byte("apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata: {name: c}\n"+body), keysOnlyEnv(nil))
			if want == nil {
				if err == nil {
					t.Fatalf("want the type error, got:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := dataKeys(decodeSecret(t, out)); !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

// TestKeysOnly_SpecAnnotationsUnchanged: the annotation goes on the output
// only; a plain run after a keys-only run of the same Plugin value would
// otherwise carry it.
func TestKeysOnly_SpecAnnotationsUnchanged(t *testing.T) {
	p := New()
	if err := p.Decode(strings.NewReader("apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata: {name: a, annotations: {x: y}}\n")); err != nil {
		t.Fatal(err)
	}
	p.keysOnly = true
	var out strings.Builder
	if err := p.Emit(nil, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.spec.Metadata.Annotations[KeysOnlyAnnotation]; ok {
		t.Error("Emit wrote the keys-only annotation into the spec")
	}
}
