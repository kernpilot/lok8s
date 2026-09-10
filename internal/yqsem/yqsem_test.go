package yqsem

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

const fixture = `
str: hello
empty: ""
num: 1.30
t: true
f: false
F: False
FF: FALSE
qf: "false"
tilde: ~
nul: null
bare:
map:
  k: v
seq: [a, b, ~, {x: 1}]
anchor: &a anchored
alias: *a
*a : keyed
`

func parse(t *testing.T) *yaml.Node {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(fixture), &root); err != nil {
		t.Fatal(err)
	}
	return &root
}

// The three alternative-operator flavours, pinned side by side: the row a
// call site migrates to is the behaviour it shipped with, so this table is
// the contract every port reads through.
func TestAlternativeFlavoursPinned(t *testing.T) {
	root := parse(t)
	rows := []struct {
		key                        string
		or, orNull, orLiteralFalse string
	}{
		{"str", "hello", "hello", "hello"},
		{"empty", "", "", ""}, // an empty string is truthy under `//`
		{"num", "1.30", "1.30", "1.30"},
		{"t", "true", "true", "true"},
		{"f", "DEF", "false", "DEF"},
		{"F", "DEF", "False", "False"}, // OrLiteralFalse: the port quirk
		{"FF", "DEF", "FALSE", "FALSE"},
		{"qf", "false", "false", "false"}, // a quoted "false" is a string
		{"tilde", "DEF", "DEF", "DEF"},
		{"nul", "DEF", "DEF", "DEF"},
		{"bare", "DEF", "DEF", "DEF"},
		{"missing", "DEF", "DEF", "DEF"},
		{"map", "DEF", "DEF", "DEF"},
		{"seq", "DEF", "DEF", "DEF"},
		{"alias", "anchored", "anchored", "anchored"},
	}
	for _, r := range rows {
		n := Lookup(root, r.key)
		if got := Or(n, "DEF"); got != r.or {
			t.Errorf("Or(%s) = %q, want %q", r.key, got, r.or)
		}
		if got := OrNull(n, "DEF"); got != r.orNull {
			t.Errorf("OrNull(%s) = %q, want %q", r.key, got, r.orNull)
		}
		if got := OrLiteralFalse(n, "DEF"); got != r.orLiteralFalse {
			t.Errorf("OrLiteralFalse(%s) = %q, want %q", r.key, got, r.orLiteralFalse)
		}
	}
}

func TestRawScalarToStringAndPredicates(t *testing.T) {
	root := parse(t)
	rows := []struct {
		key                      string
		raw, scalar, toString    string
		isNull, isFalse, present bool
	}{
		{"str", "hello", "hello", "hello", false, false, true},
		{"f", "false", "false", "false", false, true, false},
		{"F", "False", "False", "False", false, true, false},
		{"qf", "false", "false", "false", false, false, true},
		{"tilde", "null", "~", "null", true, false, false},
		{"bare", "null", "", "null", true, false, false},
		{"missing", "null", "", "null", true, false, false},
		{"map", "", "", "null", false, false, true},
	}
	for _, r := range rows {
		n := Lookup(root, r.key)
		if got := Raw(n); got != r.raw {
			t.Errorf("Raw(%s) = %q, want %q", r.key, got, r.raw)
		}
		if got := Scalar(n); got != r.scalar {
			t.Errorf("Scalar(%s) = %q, want %q", r.key, got, r.scalar)
		}
		if got := ToString(n); got != r.toString {
			t.Errorf("ToString(%s) = %q, want %q", r.key, got, r.toString)
		}
		if got := IsNull(n); got != r.isNull {
			t.Errorf("IsNull(%s) = %v, want %v", r.key, got, r.isNull)
		}
		if got := IsFalse(n); got != r.isFalse {
			t.Errorf("IsFalse(%s) = %v, want %v", r.key, got, r.isFalse)
		}
		if got := Present(n); got != r.present {
			t.Errorf("Present(%s) = %v, want %v", r.key, got, r.present)
		}
	}
}

func TestWalkers(t *testing.T) {
	root := parse(t)
	if got := Scalar(Lookup(root, "map", "k")); got != "v" {
		t.Errorf("Lookup map.k = %q", got)
	}
	if Lookup(root, "str", "k") != nil {
		t.Error("Lookup through a scalar must be nil")
	}
	if Lookup(root, "map", "missing") != nil {
		t.Error("Lookup of a missing key must be nil")
	}
	if !HasKey(root, "map") || HasKey(root, "nope") || HasKey(Lookup(root, "str"), "x") {
		t.Error("HasKey")
	}
	if got := Scalar(MapGet(root, "anchored")); got != "keyed" {
		t.Errorf("aliased key resolves by its anchored text: got %q", got)
	}
	keys, ok := MapKeys(Lookup(root, "map"))
	if !ok || !reflect.DeepEqual(keys, []string{"k"}) {
		t.Errorf("MapKeys = %v, %v", keys, ok)
	}
	if _, ok := MapKeys(Lookup(root, "seq")); ok {
		t.Error("MapKeys on a sequence must report !ok")
	}
	items := SeqItems(Lookup(root, "seq"))
	if len(items) != 4 || Scalar(items[0]) != "a" || !IsNull(items[2]) || items[3].Kind != yaml.MappingNode {
		t.Errorf("SeqItems = %v", items)
	}
	if SeqItems(Lookup(root, "map")) != nil {
		t.Error("SeqItems on a mapping must be nil")
	}
	// Deref: document → value, alias → target, empty document → nil.
	if Deref(root).Kind != yaml.MappingNode {
		t.Error("Deref(document) must yield the mapping")
	}
	if Deref(&yaml.Node{Kind: yaml.DocumentNode}) != nil {
		t.Error("Deref(empty document) must be nil")
	}
	if Deref(nil) != nil {
		t.Error("Deref(nil)")
	}
}

// Doc keeps the bash `$(yq … file)` split: a file that does not load reads
// "" everywhere (the yq call failed), a missing path reads "null"/default.
func TestDocLoadStates(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("spec:\n  a: x\n  off: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("a: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := Load(good)
	if !d.OK() {
		t.Fatalf("Load(good): %v", d.Err)
	}
	if got := d.Raw("spec", "a"); got != "x" {
		t.Errorf("Raw = %q", got)
	}
	if got := d.Raw("spec", "missing"); got != "null" {
		t.Errorf("Raw(missing) = %q", got)
	}
	if got := d.Or("DEF", "spec", "off"); got != "DEF" {
		t.Errorf("Or(false) = %q", got)
	}
	if got := d.OrChain("DEF", []string{"spec", "off"}, []string{"spec", "a"}); got != "x" {
		t.Errorf("OrChain = %q", got)
	}
	if got := d.OrChain("DEF", []string{"spec", "off"}); got != "DEF" {
		t.Errorf("OrChain(all falsy) = %q", got)
	}
	if !d.Present("spec", "a") || d.Present("spec", "off") || d.Present("nope") {
		t.Error("Present")
	}
	for _, path := range []string{bad, filepath.Join(dir, "missing.yaml")} {
		d := Load(path)
		if d.OK() {
			t.Errorf("Load(%s) must not be ok", path)
		}
		if d.Raw("spec") != "" || d.Or("DEF", "spec") != "" || d.OrChain("DEF", []string{"spec"}) != "" {
			t.Errorf("a failed load reads \"\" everywhere: %s", path)
		}
		if d.Lookup("spec") != nil || d.Present("spec") {
			t.Errorf("a failed load resolves nothing: %s", path)
		}
	}
	if LoadNode(bad) != nil || LoadNode(good) == nil {
		t.Error("LoadNode")
	}
}
