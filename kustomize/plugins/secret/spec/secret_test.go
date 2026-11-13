package spec

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
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

func TestSecretRef_NullValueRejectedAtDecode(t *testing.T) {
	// `R: ~` skips SecretRefEntry.UnmarshalYAML (yaml.v3 !!null quirk), leaving
	// Secret+Key empty. Secret.validate() must reject it at decode so it fails
	// with a clear message instead of a confusing runtime "read /default/:" error.
	in := []byte(`apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
secretRef:
  R: ~
`)
	_, err := DecodeBytes(in)
	if err == nil || !strings.Contains(err.Error(), "secret is required") {
		t.Errorf("expected null secretRef rejected at decode, got %v", err)
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

// errorCanary is the start of each malformed string value below, and
// numberCanary the digits of each malformed number. A spec value can be a
// secret, so no decode error may hold it.
const (
	errorCanary  = "Zq7"
	numberCanary = "1773"
)

// specHead is the start of each malformed spec below that has no head of
// its own.
const specHead = "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n"

// malformedSpecs holds one malformed value for each validator of each
// section, and for both forms in which yaml.v3 quotes a value: a type error
// (also with a newline in the value) and a tagged scalar that does not
// resolve. path is the section and key that the error names; "" for an
// error that has no position (the head, and a tagged scalar outside an
// entry: yaml.v3 gives no line for it). full marks a spec with its own head.
var malformedSpecs = map[string]struct {
	body, path, canary string
	full               bool
}{
	"passwd shorthand":           {body: "passwd:\n  PASSWORD: Zq7-actual-password\n", path: "passwd.PASSWORD"},
	"passwd shorthand length":    {body: "passwd:\n  PASSWORD: -1773\n", path: "passwd.PASSWORD", canary: numberCanary},
	"passwd short length":        {body: "passwd:\n  PASSWORD: {length: Zq7pw}\n", path: "passwd.PASSWORD"},
	"passwd negative length":     {body: "passwd:\n  PASSWORD: {length: -1773}\n", path: "passwd.PASSWORD", canary: numberCanary},
	"passwd long length":         {body: "passwd:\n  OTHER: 8\n  PASSWORD:\n    length: Zq7-actual-password\n", path: "passwd.PASSWORD"},
	"passwd length with newline": {body: "passwd:\n  PASSWORD:\n    length: \"Zq7\\nabc\"\n", path: "passwd.PASSWORD"},
	"passwd tagged length":       {body: "passwd:\n  PASSWORD: {length: !!int Zq7-actual-password}\n", path: "passwd.PASSWORD"},
	"passwd require":             {body: "passwd:\n  PASSWORD: {length: 8, require: [Zq7-class]}\n", path: "passwd.PASSWORD"},
	"template bytes shorthand":   {body: "template:\n  DSN:\n    pattern: \"{K}\"\n    bytes:\n      K: Zq7-actual-bytes\n", path: "template.DSN"},
	"template bytes count":       {body: "template:\n  DSN:\n    pattern: \"{K}\"\n    bytes:\n      K: 17730000\n", path: "template.DSN", canary: numberCanary},
	"template bytes encoding":    {body: "template:\n  DSN:\n    pattern: \"{K}\"\n    bytes:\n      K: {bytes: 16, encoding: Zq7-enc}\n", path: "template.DSN"},
	"template field charset":     {body: "template:\n  DSN:\n    pattern: \"{K}\"\n    fields:\n      K: {length: 8, chars: Zq7-name}\n", path: "template.DSN"},
	"template field encoding":    {body: "template:\n  DSN:\n    pattern: \"{K}\"\n    fields:\n      K: {bytes: 16, encoding: Zq7-enc}\n", path: "template.DSN"},
	"template field bytes":       {body: "template:\n  DSN:\n    pattern: \"{K}\"\n    fields:\n      K: {bytes: 17730000}\n", path: "template.DSN", canary: numberCanary},
	"template operator":          {body: "template:\n  DSN:\n    pattern: \"{K:Zq7}\"\n    literals: {K: v}\n", path: "template.DSN"},
	"template replace operator":  {body: "template:\n  DSN:\n    pattern: \"{K/Zq7}\"\n    literals: {K: v}\n", path: "template.DSN"},
	"template secretRef":         {body: "template:\n  DSN:\n    pattern: \"{R}\"\n    secretRef: {R: a/b/c/Zq7}\n", path: "template.DSN"},
	"key shorthand":              {body: "key:\n  PRIVATE: Zq7-actual-password\n", path: "key.PRIVATE"},
	"key encoding":               {body: "key:\n  PRIVATE: {algorithm: rsa, encoding: Zq7-enc}\n", path: "key.PRIVATE"},
	"key rsa bits":               {body: "key:\n  PRIVATE: {algorithm: rsa, bits: 1773}\n", path: "key.PRIVATE", canary: numberCanary},
	"key ed25519 bits":           {body: "key:\n  PRIVATE: {algorithm: ed25519, bits: 1773}\n", path: "key.PRIVATE", canary: numberCanary},
	"secretRef shorthand":        {body: "secretRef:\n  R: Zq7-actual-password\n", path: "secretRef.R"},
	"file mode":                  {body: "file:\n  F: {path: f.txt, mode: Zq7-mode}\n", path: "file.F"},
	"bash output":                {body: "bash:\n  B: {exec: \"true\", output: Zq7-out}\n", path: "bash.B"},
	"bash newline":               {body: "bash:\n  B: {exec: \"true\", newline: Zq7-nl}\n", path: "bash.B"},
	"bash encode":                {body: "bash:\n  B: {exec: \"true\", encode: Zq7-enc}\n", path: "bash.B"},
	"env":                        {body: "env:\n  E: {optional: Zq7-bool}\n", path: "env.E"},
	"htpasswd":                   {body: "htpasswd:\n  H: {username: {length: Zq7pw}, password: x}\n", path: "htpasswd.H"},
	"cert":                       {body: "cert: {ca: Zq7-bool}\n", path: "cert.ca"},
	"validate":                   {body: "validate: Zq7-actual\nliterals: {A: b}\n", path: "validate"},
	"literal tagged":             {body: "literals:\n  A: !!int Zq7-actual-password\n"},
	"apiVersion":                 {body: "apiVersion: Zq7-version\nkind: Secret\nmetadata: {name: app}\n", full: true},
	"kind":                       {body: "apiVersion: secrets.lok8s.dev/v1\nkind: Zq7-kind\nmetadata: {name: app}\n", full: true},
}

// TestDecodeBytes_ErrorNamesThePathNotTheValue: a malformed value gives an
// error with the line, the section and the key, and without the value.
func TestDecodeBytes_ErrorNamesThePathNotTheValue(t *testing.T) {
	for name, tc := range malformedSpecs {
		t.Run(name, func(t *testing.T) {
			in := specHead + tc.body
			if tc.full {
				in = tc.body
			}
			canary := tc.canary
			if canary == "" {
				canary = errorCanary
			}
			_, err := DecodeBytes([]byte(in))
			if err == nil {
				t.Fatal("the malformed spec decoded")
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the error holds the value: %v", err)
			}
			if tc.path == "" {
				return
			}
			var e *errs.Error
			if !errors.As(err, &e) || e.Path != tc.path || e.Line == 0 {
				t.Errorf("error %v (%#v), want line and path %s", err, e, tc.path)
			}
			// The message follows the position once: a custom decoder's
			// error is not read again as a message ("line 6: 13: ...").
			if !regexp.MustCompile(`^line [0-9]+(:[0-9]+)?: ` + regexp.QuoteMeta(tc.path) + `: [^0-9]`).MatchString(err.Error()) {
				t.Errorf("error %q, want \"line N[:M]: %s: <message>\"", err, tc.path)
			}
		})
	}
}
