package secret

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
)

// yamlUnmarshal / b64Decode are thin aliases used by decodeSecretDataKey so the
// test's own imports stay readable.
func yamlUnmarshal(data []byte, out any) error { return yaml.Unmarshal(data, out) }
func b64Decode(s string) ([]byte, error)       { return base64.StdEncoding.DecodeString(s) }

// runPlugin is a tiny driver that runs Decode → Build → registry → Emit
// against an in-memory input/output and a controlled env. Used by all
// the plugin-level tests below.
func runPlugin(t *testing.T, in []byte, env map[string]string) (string, error) {
	t.Helper()
	p := New()
	if err := p.Decode(bytes.NewReader(in)); err != nil {
		return "", err
	}
	envFn := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	r, ctx, err := p.Build(envFn, t.TempDir())
	if err != nil {
		return "", err
	}
	entries, err := r.Run(ctx)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := p.Emit(entries, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

func TestPlugin_HappyPath_Literals(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: test
  namespace: default
type: Opaque
literals:
  KEY: value
`)
	out, err := runPlugin(t, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kind: Secret") {
		t.Errorf("output missing kind: Secret\n%s", out)
	}
	if !strings.Contains(out, "name: test") {
		t.Errorf("missing name: %s", out)
	}
	if !strings.Contains(out, "type: Opaque") {
		t.Errorf("missing type: %s", out)
	}
	if !strings.Contains(out, "KEY:") {
		t.Errorf("missing KEY: %s", out)
	}
}

func TestPlugin_TLS_RequiresKeys(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: tls
type: kubernetes.io/tls
literals:
  tls.crt: cert-data
`)
	// Missing tls.key.
	_, err := runPlugin(t, in, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "tls.key") {
		t.Errorf("error should mention tls.key: %v", err)
	}
}

func TestPlugin_TLS_ValidateFalseOptOut(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: tls
type: kubernetes.io/tls
validate: false
literals:
  tls.crt: cert-data
`)
	out, err := runPlugin(t, in, nil)
	if err != nil {
		t.Errorf("validate: false should suppress validation, got: %v", err)
	}
	if !strings.Contains(out, "tls.crt") {
		t.Errorf("output should contain tls.crt: %s", out)
	}
}

func TestPlugin_TLS_AllRequiredKeys(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: tls
type: kubernetes.io/tls
literals:
  tls.crt: cert-data
  tls.key: key-data
`)
	out, err := runPlugin(t, in, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "tls.crt") || !strings.Contains(out, "tls.key") {
		t.Errorf("output missing required keys: %s", out)
	}
}

func TestPlugin_BasicAuth_Validation(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: ba
type: kubernetes.io/basic-auth
literals:
  username: alice
`)
	_, err := runPlugin(t, in, nil)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("expected password missing error, got %v", err)
	}
}

func TestPlugin_DockerConfigJSON_Validation(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: dh
type: kubernetes.io/dockerconfigjson
literals:
  wrong-key: value
`)
	_, err := runPlugin(t, in, nil)
	if err == nil || !strings.Contains(err.Error(), ".dockerconfigjson") {
		t.Errorf("expected .dockerconfigjson missing error, got %v", err)
	}
}

func TestPlugin_PasswdNeedsCache(t *testing.T) {
	// Without PATH_SECRETS in the env, the passwd generator should fail.
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: t
passwd:
  K: 32
`)
	_, err := runPlugin(t, in, nil)
	if err == nil {
		t.Fatal("expected error: passwd requires PATH_SECRETS")
	}
	if !strings.Contains(err.Error(), "PATH_SECRETS") {
		t.Errorf("error should mention PATH_SECRETS: %v", err)
	}
}

func TestPlugin_PasswdWithCache(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: t
passwd:
  K: 32
`)
	env := map[string]string{"PATH_SECRETS": t.TempDir()}
	out, err := runPlugin(t, in, env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "K:") {
		t.Errorf("missing K: %s", out)
	}
}

func TestPlugin_DecodeError(t *testing.T) {
	in := []byte(`not valid yaml: [unclosed`)
	_, err := runPlugin(t, in, nil)
	if err == nil {
		t.Error("expected decode error")
	}
}

func TestPlugin_BuildBeforeDecode(t *testing.T) {
	p := New()
	_, _, err := p.Build(plugin.DefaultEnv, "/tmp")
	if err == nil {
		t.Error("expected error: Build before Decode")
	}
}

func TestPlugin_EmitBeforeDecode(t *testing.T) {
	p := New()
	if err := p.Emit(nil, &bytes.Buffer{}); err == nil {
		t.Error("expected error: Emit before Decode")
	}
}

// TestPlugin_HookshotEndToEnd renders the matrix-hookshot registration.yml +
// passkey.pem in the NEW form: passwd tokens, a top-level key: for the RSA
// passkey, and a template: whose sibling secretRef sub-section pulls the same
// tokens into registration.yml — proving the passwd → key → template pipeline
// and sibling resolution work through the full plugin (Decode→Build→Run→Emit).
func TestPlugin_HookshotEndToEnd(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: hookshot-secrets
  namespace: element
passwd:
  as_token: { length: 64, chars: hex }
  hs_token: { length: 64, chars: hex }
key:
  passkey.pem: rsa
template:
  registration.yml:
    pattern: "id: matrix-hookshot\nas_token: \"{as_token}\"\nhs_token: \"{hs_token}\"\nsender_localpart: hookshot\n"
    secretRef:
      as_token: as_token
      hs_token: hs_token
`)
	env := map[string]string{"PATH_SECRETS": t.TempDir()}
	out, err := runPlugin(t, in, env)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"as_token:", "hs_token:", "passkey.pem:", "registration.yml:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestPlugin_TemplateSiblingRefMatchesTopLevel decodes the emitted Secret and
// verifies the token INSIDE registration.yml is byte-identical to the top-level
// as_token key — the whole point of the sibling secretRef (single source of the
// shared token, so hookshot and Synapse agree).
func TestPlugin_TemplateSiblingRefMatchesTopLevel(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: hookshot-secrets, namespace: element}
passwd:
  as_token: { length: 64, chars: hex }
template:
  registration.yml:
    pattern: "as_token: {as_token}"
    secretRef: { as_token: as_token }
`)
	env := map[string]string{"PATH_SECRETS": t.TempDir()}
	out, err := runPlugin(t, in, env)
	if err != nil {
		t.Fatal(err)
	}
	as := decodeSecretDataKey(t, out, "as_token")
	reg := decodeSecretDataKey(t, out, "registration.yml")
	if want := "as_token: " + as; reg != want {
		t.Errorf("registration.yml = %q, want %q (sibling token must match top-level)", reg, want)
	}
}

// TestPlugin_TemplateAllTypedSubsections drives every typed sub-section through
// the full pipeline in one template entry, plus a substitution operator.
func TestPlugin_TemplateAllTypedSubsections(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: s, namespace: default}
passwd:
  shared: { length: 8, chars: hex }
template:
  out:
    pattern: "tag={tag} up={tag^^} pw={pw} seed={seed} region={region} ref={ref}"
    literals:  { tag: hookshot }
    passwd:    { pw: { length: 6, chars: hex } }
    bytes:     { seed: { bytes: 8, encoding: hex } }
    env:       { region: MY_REGION }
    secretRef: { ref: shared }
`)
	env := map[string]string{"PATH_SECRETS": t.TempDir(), "MY_REGION": "fsn1"}
	out, err := runPlugin(t, in, env)
	if err != nil {
		t.Fatal(err)
	}
	v := decodeSecretDataKey(t, out, "out")
	if !strings.Contains(v, "tag=hookshot ") || !strings.Contains(v, "up=HOOKSHOT ") {
		t.Errorf("literals/operator wrong: %q", v)
	}
	if !strings.Contains(v, "region=fsn1 ") {
		t.Errorf("env sub-section wrong: %q", v)
	}
	shared := decodeSecretDataKey(t, out, "shared")
	if !strings.Contains(v, "ref="+shared) {
		t.Errorf("secretRef sub-section did not resolve sibling: %q (shared=%q)", v, shared)
	}
}

// decodeSecretDataKey pulls out the RAW value of data.<key> from an emitted
// Secret YAML (base64-decoding it), so tests can assert on the real bytes.
func decodeSecretDataKey(t *testing.T, secretYAML, key string) string {
	t.Helper()
	var doc struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yamlUnmarshal([]byte(secretYAML), &doc); err != nil {
		t.Fatalf("parse emitted Secret: %v", err)
	}
	enc, ok := doc.Data[key]
	if !ok {
		t.Fatalf("data.%s not present in:\n%s", key, secretYAML)
	}
	raw, err := b64Decode(enc)
	if err != nil {
		t.Fatalf("data.%s not valid base64: %v", key, err)
	}
	return string(raw)
}

func TestPlugin_LabelsAndAnnotationsPropagate(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: t
  labels:
    app: foo
  annotations:
    note: bar
literals:
  k: v
`)
	out, err := runPlugin(t, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app: foo") {
		t.Errorf("missing label: %s", out)
	}
	if !strings.Contains(out, "note: bar") {
		t.Errorf("missing annotation: %s", out)
	}
}
