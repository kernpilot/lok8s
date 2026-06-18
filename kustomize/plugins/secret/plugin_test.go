package secret

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
)

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
