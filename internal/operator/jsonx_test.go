package operator

// jsonx_test.go pins the jq idioms the hook bodies use.

import (
	"testing"
)

func TestJqIdioms(t *testing.T) {
	t.Parallel()
	v, _ := decode([]byte(`{"n":3,"f":1.0,"s":"x","b":false,"z":null,"o":{"k":"v"},"a":["p"]}`))
	cases := map[string]string{
		jqR(get(v, "n")):                   "3",
		jqR(get(v, "f")):                   "1.0",
		jqR(get(v, "s")):                   "x",
		jqR(get(v, "b")):                   "false",
		jqR(get(v, "z")):                   "null",
		jqR(get(v, "missing")):             "null",
		jqR(get(v, "s", "deeper")):         "null",
		jqR(get(v, "o")):                   `{"k":"v"}`,
		jqR(alt(get(v, "b"), "d")):         "d",
		jqR(alt(get(v, "z"), "d")):         "d",
		jqEmpty(get(v, "z")):               "",
		jqEmpty(get(v, "b")):               "",
		jqEmpty(get(v, "n")):               "3",
		compact(alt(get(v, "z"), []any{})): "[]",
		compact(alt(get(v, "a"), []any{})): `["p"]`,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if present(get(v, "z")) || present(get(v, "b")) || !present(get(v, "o")) || !present(get(v, "n")) {
		t.Error("present (jq -e) semantics")
	}
	if !contains(get(v, "a"), "p") || contains(get(v, "o"), "v") || contains(nil, "x") {
		t.Error("contains semantics")
	}
}
