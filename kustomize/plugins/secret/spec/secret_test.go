package spec

import (
	"strings"
	"testing"
)

func TestDecodeBytes_Minimal(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: test
literals:
  FOO: bar
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Metadata.Name != "test" {
		t.Errorf("name = %q", s.Metadata.Name)
	}
	if s.Literals["FOO"] != "bar" {
		t.Errorf("literals = %v", s.Literals)
	}
	if !s.ValidationEnabled() {
		t.Error("default ValidationEnabled should be true")
	}
}

func TestDecodeBytes_AllGenerators(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: test
  namespace: ns
type: Opaque
validate: true
literals:
  FOO: bar
passwd:
  PASS: 32
  COMPLEX:
    length: 64
    chars: alphanum+symbols
env:
  USE_KEY: ~
  EXPLICIT: SOME_VAR
  WITH_UPDATE:
    var: HOT_VAR
    update: true
secretRef:
  REF1: db/password
  REF2: db/other-ns/password
  REF3:
    secret: foo
    namespace: bar
    key: baz
htpasswd:
  smtp.htpasswd:
    username: {length: 16}
    password: {length: 32}
file:
  ca.crt: ./certs/ca.crt
  tls.crt:
    path: ./certs/tls.crt
    mode: passthrough
b64:
  legacy: dGVzdA==
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Passwd["PASS"].Length != 32 {
		t.Errorf("PASS length = %d", s.Passwd["PASS"].Length)
	}
	if s.Passwd["COMPLEX"].Chars != "alphanum+symbols" {
		t.Errorf("COMPLEX chars = %q", s.Passwd["COMPLEX"].Chars)
	}
	if s.Env["USE_KEY"].Var != "" {
		t.Errorf("USE_KEY should have empty Var")
	}
	if s.Env["EXPLICIT"].Var != "SOME_VAR" {
		t.Errorf("EXPLICIT.Var = %q", s.Env["EXPLICIT"].Var)
	}
	if !s.Env["WITH_UPDATE"].Update {
		t.Error("WITH_UPDATE should have Update=true")
	}
	if s.SecretRef["REF1"].Secret != "db" || s.SecretRef["REF1"].Key != "password" {
		t.Errorf("REF1 = %+v", s.SecretRef["REF1"])
	}
	if s.SecretRef["REF2"].Namespace != "other-ns" {
		t.Errorf("REF2 namespace = %q", s.SecretRef["REF2"].Namespace)
	}
	if s.SecretRef["REF3"].Secret != "foo" {
		t.Errorf("REF3 = %+v", s.SecretRef["REF3"])
	}
	htp := s.Htpasswd["smtp.htpasswd"]
	if htp.Username.IsLiteral() || htp.Password.IsLiteral() {
		t.Error("smtp.htpasswd should have generated user/pass")
	}
	if htp.Username.Gen.Length != 16 {
		t.Errorf("smtp username length = %d", htp.Username.Gen.Length)
	}
	if s.File["ca.crt"].Mode != FileModeRaw {
		t.Errorf("ca.crt mode = %q", s.File["ca.crt"].Mode)
	}
	if s.File["tls.crt"].Mode != FileModePassthrough {
		t.Errorf("tls.crt mode = %q", s.File["tls.crt"].Mode)
	}
	if s.B64["legacy"] != "dGVzdA==" {
		t.Errorf("b64.legacy = %q", s.B64["legacy"])
	}
}

func TestDecodeBytes_Validate_Override(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: test
type: kubernetes.io/tls
validate: false
literals:
  some: value
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.ValidationEnabled() {
		t.Error("validate: false should disable validation")
	}
}

func TestDecodeBytes_RejectsWrongAPIVersion(t *testing.T) {
	in := []byte(`apiVersion: secrets.example.com/v2
kind: Secret
metadata:
  name: test
`)
	_, err := DecodeBytes(in)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("error should mention apiVersion: %v", err)
	}
}

func TestDecodeBytes_RejectsWrongKind(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: ConfigMap
metadata:
  name: test
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("expected kind error, got %v", err)
	}
}

func TestDecodeBytes_RejectsMissingName(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  namespace: foo
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("expected name error, got %v", err)
	}
}

func TestDecodeBytes_RejectsUnknownTopLevelField(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: test
typo_field: value
`)
	_, err := DecodeBytes(in)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "typo_field") {
		t.Errorf("error should mention typo_field: %v", err)
	}
}

// --- passwd shorthand tests ---

func TestPasswd_ShorthandInt(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
passwd:
  K: 64
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Passwd["K"].Length != 64 {
		t.Errorf("length = %d", s.Passwd["K"].Length)
	}
}

func TestPasswd_EmptyMapping(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
passwd:
  K: {}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Passwd["K"].EffectiveLength() != 32 {
		t.Errorf("default length = %d", s.Passwd["K"].EffectiveLength())
	}
	if s.Passwd["K"].EffectiveChars() != "alphanum" {
		t.Errorf("default chars = %q", s.Passwd["K"].EffectiveChars())
	}
}

func TestPasswd_ShorthandZero_Rejected(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
passwd:
  K: 0
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("expected length error, got %v", err)
	}
}

func TestPasswd_RejectsUnknownField(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
passwd:
  K:
    length: 32
    nope: bad
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected unknown-field error, got %v", err)
	}
}

func TestPasswd_RejectsBadType(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
passwd:
  K: [1, 2]
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "passwd") {
		t.Errorf("expected passwd type error, got %v", err)
	}
}

// --- env shorthand tests ---

func TestEnv_NullShorthand(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
env:
  MYKEY: ~
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Env["MYKEY"]
	if e.EffectiveVar("MYKEY") != "MYKEY" {
		t.Errorf("EffectiveVar = %q", e.EffectiveVar("MYKEY"))
	}
}

func TestEnv_StringShorthand(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
env:
  MYKEY: REAL_NAME
`)
	s, _ := DecodeBytes(in)
	if s.Env["MYKEY"].Var != "REAL_NAME" {
		t.Errorf("Var = %q", s.Env["MYKEY"].Var)
	}
}

func TestEnv_RejectsUnknownField(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
env:
  K:
    var: V
    typo: x
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Errorf("expected unknown-field error, got %v", err)
	}
}

// --- secretRef tests ---

func TestSecretRef_ShortForms(t *testing.T) {
	cases := []struct {
		yaml   string
		secret string
		ns     string
		key    string
	}{
		{`db/password`, "db", "", "password"},
		{`db/other/key`, "db", "other", "key"},
	}
	for _, c := range cases {
		in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
secretRef:
  K: ` + c.yaml + "\n")
		s, err := DecodeBytes(in)
		if err != nil {
			t.Errorf("%q: %v", c.yaml, err)
			continue
		}
		r := s.SecretRef["K"]
		if r.Secret != c.secret || r.Namespace != c.ns || r.Key != c.key {
			t.Errorf("%q: got %+v", c.yaml, r)
		}
	}
}

func TestSecretRef_BadShorthand(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
secretRef:
  K: just-one-segment
`)
	_, err := DecodeBytes(in)
	if err == nil {
		t.Error("expected error for bad shorthand")
	}
}

func TestSecretRef_MissingKey(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
secretRef:
  K:
    secret: foo
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Errorf("expected key error, got %v", err)
	}
}

// --- htpasswd tests ---

func TestHtpasswd_LiteralUsername_GeneratedPassword(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
htpasswd:
  k:
    username: alice
    password: {length: 32}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Htpasswd["k"]
	if !h.Username.IsLiteral() || h.Username.Literal != "alice" {
		t.Errorf("username = %+v", h.Username)
	}
	if h.Password.IsLiteral() {
		t.Error("password should be generated")
	}
}

func TestHtpasswd_RejectsScalar(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
htpasswd:
  k: just-a-string
`)
	_, err := DecodeBytes(in)
	if err == nil {
		t.Error("expected error for scalar htpasswd entry")
	}
}

func TestHtpasswd_RejectsEmptyUsername(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
htpasswd:
  k:
    username: ""
    password: {length: 32}
`)
	_, err := DecodeBytes(in)
	if err == nil {
		t.Error("expected error for empty username")
	}
}

// --- file tests ---

func TestFile_ShorthandPath(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
file:
  ca.crt: ./certs/ca.crt
`)
	s, _ := DecodeBytes(in)
	if s.File["ca.crt"].Path != "./certs/ca.crt" {
		t.Errorf("path = %q", s.File["ca.crt"].Path)
	}
	if s.File["ca.crt"].Mode != FileModeRaw {
		t.Errorf("mode = %q", s.File["ca.crt"].Mode)
	}
}

func TestFile_RejectsBadMode(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
file:
  k:
    path: x
    mode: weird
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("expected mode error, got %v", err)
	}
}

func TestFile_MissingPath(t *testing.T) {
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
file:
  k:
    mode: raw
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("expected path error, got %v", err)
	}
}
