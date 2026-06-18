package kyaml

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

type sample struct {
	Name string `yaml:"name"`
	Age  int    `yaml:"age"`
}

func TestDecodeStrictBytes_Valid(t *testing.T) {
	in := []byte("name: alice\nage: 30\n")
	var s sample
	if err := DecodeStrictBytes(in, &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Name != "alice" || s.Age != 30 {
		t.Errorf("got %+v", s)
	}
}

func TestDecodeStrictBytes_UnknownField(t *testing.T) {
	in := []byte("name: alice\nage: 30\nemail: a@b.c\n")
	var s sample
	err := DecodeStrictBytes(in, &s)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errs.Error, got %T", err)
	}
	if e.Line == 0 {
		t.Errorf("line should be set, got %+v", e)
	}
	if !strings.Contains(e.Msg, "email") {
		t.Errorf("error should mention 'email': %q", e.Msg)
	}
}

func TestDecodeStrictBytes_BadType(t *testing.T) {
	in := []byte("name: alice\nage: not-a-number\n")
	var s sample
	err := DecodeStrictBytes(in, &s)
	if err == nil {
		t.Fatal("expected type error")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errs.Error, got %T", err)
	}
}

func TestDecodeNodeStrict_RoundTrip(t *testing.T) {
	in := []byte("name: bob\nage: 42\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	var s sample
	if err := DecodeNodeStrict(root, &s); err != nil {
		t.Fatalf("DecodeNodeStrict: %v", err)
	}
	if s.Name != "bob" || s.Age != 42 {
		t.Errorf("got %+v", s)
	}
}

func TestDecodeNodeStrict_NilNode(t *testing.T) {
	var s sample
	if err := DecodeNodeStrict(nil, &s); err == nil {
		t.Error("expected error for nil node")
	}
}

func TestEncode_Deterministic(t *testing.T) {
	v := map[string]string{
		"c": "3",
		"a": "1",
		"b": "2",
	}
	out1, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Errorf("non-deterministic encode:\n%s\nvs\n%s", out1, out2)
	}
	// Map keys should appear in sorted order.
	s := string(out1)
	if strings.Index(s, "a:") > strings.Index(s, "b:") || strings.Index(s, "b:") > strings.Index(s, "c:") {
		t.Errorf("keys not sorted: %s", s)
	}
}

func TestNodeErr_LineExtracted(t *testing.T) {
	in := []byte("name: alice\nage: 30\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	err := NodeErr(root, "boom")
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errs.Error, got %T", err)
	}
	if e.Line == 0 {
		t.Errorf("line should be set, got %+v", e)
	}
}

func TestNodeErr_NilNode(t *testing.T) {
	err := NodeErr(nil, "boom")
	if err.Error() != "boom" {
		t.Errorf("nil-node NodeErr = %q", err.Error())
	}
}

func TestNodeErrf(t *testing.T) {
	err := NodeErrf(nil, "value=%d", 42)
	if !strings.Contains(err.Error(), "value=42") {
		t.Errorf("NodeErrf format failed: %q", err.Error())
	}
}

// fakeStruct has an unmarshal-time error path so we can hit DecodeNodeStrict's
// error wrapping and wrapNodeErr.
type fakeStruct struct {
	Foo int `yaml:"foo"`
}

func TestDecodeNodeStrict_TypeError(t *testing.T) {
	in := []byte("foo: not-a-number\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	var s fakeStruct
	err := DecodeNodeStrict(root, &s)
	if err == nil {
		t.Fatal("expected error")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errs.Error, got %T", err)
	}
}

func TestDecodeNodeStrict_UnknownField(t *testing.T) {
	in := []byte("foo: 1\nbar: 2\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	var s fakeStruct
	err := DecodeNodeStrict(root, &s)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "bar") {
		t.Errorf("error should mention bar: %q", err.Error())
	}
}
