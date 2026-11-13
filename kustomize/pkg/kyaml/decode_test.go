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

// TestDecodeStrictBytes_TypeErrorWithoutValue: yaml.v3 quotes the input
// value in a type error (all of it up to 10 characters, else the first 7).
// The decode error names the tag and keeps the line, not the value.
func TestDecodeStrictBytes_TypeErrorWithoutValue(t *testing.T) {
	for in, want := range map[string]string{
		"age: Zq7pw\n":                  "cannot unmarshal !!str value into int",
		"age: Zq7-a-long-value\n":       "cannot unmarshal !!str value into int",
		"age: \"Zq7\\nabc\"\n":          "cannot unmarshal !!str value into int",
		"age: \"Zq7`x\"\n":              "cannot unmarshal !!str value into int",
		"age: Zq7pw\nname: [Zq7a]\n":    "cannot unmarshal !!str value into int",
		"age: !!int Zq7-actual-value\n": "cannot decode !!str value as a !!int",
	} {
		var s sample
		err := DecodeStrictBytes([]byte(in), &s)
		var e *errs.Error
		if !errors.As(err, &e) {
			t.Fatalf("%q: want an *errs.Error, got %v", in, err)
		}
		if strings.Contains(err.Error(), "Zq7") || e.Msg != want {
			t.Errorf("%q: error %q, want %q", in, err, want)
		}
	}
}

// TestDecodeNodeStrict_ErrorWithoutValue: an error of a custom decoder
// (an *errs.Error, also one that wraps a yaml.v3 error) loses the value
// too, and gets the position of the node.
func TestDecodeNodeStrict_ErrorWithoutValue(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("x:\n  age: Zq7pw\n"), &doc); err != nil {
		t.Fatal(err)
	}
	node := doc.Content[0].Content[1]
	var s sample
	err := DecodeNodeStrict(node, &s)
	if err == nil || strings.Contains(err.Error(), "Zq7") {
		t.Errorf("node decode: error %v", err)
	}
	// Each case makes a new error: the wrappers change the error in place.
	inner := "yaml: line 1: cannot unmarshal !!str `Zq7pw` into int"
	for name, custom := range map[string]func() error{
		"message": func() error { return &errs.Error{Msg: inner} },
		"wrapped": func() error { return errs.Wrap("age", errors.New(inner)) },
	} {
		got := wrapNodeErr(node, custom())
		if strings.Contains(got.Error(), "Zq7") || !strings.Contains(got.Error(), "cannot unmarshal !!str value into int") {
			t.Errorf("wrapNodeErr, %s: %q", name, got)
		}
		if ge, ok := errors.AsType[*errs.Error](got); !ok || ge.Line != node.Line || ge.Col != node.Column {
			t.Errorf("wrapNodeErr, %s: error %#v, want the position %d:%d", name, got, node.Line, node.Column)
		}
		if got := wrapDecodeErr(custom()); strings.Contains(got.Error(), "Zq7") || !strings.Contains(got.Error(), "cannot unmarshal !!str value into int") {
			t.Errorf("wrapDecodeErr, %s: %q", name, got)
		}
	}
}

// TestRedactValue: only the quoted values go, in both forms in which
// yaml.v3 quotes one.
func TestRedactValue(t *testing.T) {
	for in, want := range map[string]string{
		"cannot unmarshal !!str `hunter2` into int":                                        "cannot unmarshal !!str value into int",
		"cannot unmarshal !!str `hunter2...` into bool":                                    "cannot unmarshal !!str value into bool",
		"cannot unmarshal !!str `hu\nter2` into int":                                       "cannot unmarshal !!str value into int",
		"cannot unmarshal !!str `hu`ter2` into int":                                        "cannot unmarshal !!str value into int",
		"unmarshal errors:\n  line 1: cannot unmarshal !!str `a\nb` into int\n  line 2: x": "unmarshal errors:\n  line 1: cannot unmarshal !!str value into int\n  line 2: x",
		"cannot decode !!str `hunter2` as a !!int":                                         "cannot decode !!str value as a !!int",
		"cannot decode !!str `a long value with ` and\n a newline` as a !!float":           "cannot decode !!str value as a !!float",
		"cannot unmarshal !!map into string":                                               "cannot unmarshal !!map into string",
		"field email not found in type kyaml.sample":                                       "field email not found in type kyaml.sample",
	} {
		if got := RedactValue(in); got != want {
			t.Errorf("RedactValue(%q) = %q, want %q", in, got, want)
		}
	}
}
