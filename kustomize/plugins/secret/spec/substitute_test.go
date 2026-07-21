package spec

import (
	"strings"
	"testing"
)

// sub is a tiny helper: render a one-placeholder pattern against a single value.
func sub(t *testing.T, pattern string, values map[string]string) (string, error) {
	t.Helper()
	return TemplateEntry{Pattern: pattern}.Substitute(values)
}

func TestSubstitute_PlainAndEscapes(t *testing.T) {
	cases := []struct {
		pattern string
		vals    map[string]string
		want    string
	}{
		{"{a}", map[string]string{"a": "x"}, "x"},
		{"pre-{a}-post", map[string]string{"a": "MID"}, "pre-MID-post"},
		{"{{literal}}", nil, "{literal}"},
		{"{{{a}}}", map[string]string{"a": "v"}, "{v}"},
		{"no placeholders", nil, "no placeholders"},
	}
	for _, c := range cases {
		got, err := sub(t, c.pattern, c.vals)
		if err != nil {
			t.Errorf("%q: unexpected err %v", c.pattern, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.pattern, got, c.want)
		}
	}
}

// TestSubstitute_Operators is the operator truth table.
func TestSubstitute_Operators(t *testing.T) {
	vals := map[string]string{
		"s":     "abcDEF",
		"path":  "matrix-hookshot.pem",
		"lead":  "prefix_body",
		"rep":   "a.b.c.d",
		"sub":   "0123456789",
		"upper": "hello world",
	}
	cases := []struct {
		name    string
		pattern string
		want    string
	}{
		{"upper_all", "{upper^^}", "HELLO WORLD"},
		{"lower_all", "{s,,}", "abcdef"},
		{"upper_first", "{upper^}", "Hello world"},
		{"lower_first", "{s,}", "abcDEF"}, // already lower first char → unchanged
		{"upper_first_ofmixed", "{path^}", "Matrix-hookshot.pem"},
		{"strip_suffix_long", "{path%%.pem}", "matrix-hookshot"},
		{"strip_suffix_short", "{path%.pem}", "matrix-hookshot"},
		{"strip_suffix_nomatch", "{path%%.zip}", "matrix-hookshot.pem"},
		{"strip_prefix_long", "{lead##prefix_}", "body"},
		{"strip_prefix_short", "{lead#prefix_}", "body"},
		{"replace_all", "{rep//./_}", "a_b_c_d"},
		{"replace_first", "{rep/./_}", "a_b.c.d"},
		{"replace_all_multichar", "{path//-/_}", "matrix_hookshot.pem"},
		{"substring_off", "{sub:3}", "3456789"},
		{"substring_off_len", "{sub:2:4}", "2345"},
		{"substring_zero_len", "{sub:5:0}", ""},
		{"substring_to_end", "{sub:10}", ""}, // offset == len → empty tail
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sub(t, c.pattern, vals)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Errorf("%q: got %q, want %q", c.pattern, got, c.want)
			}
		})
	}
}

func TestSubstitute_CaseOpEmptyValue(t *testing.T) {
	// First-char ops on an empty value are a no-op (not an index panic).
	for _, op := range []string{"{a^}", "{a,}", "{a^^}", "{a,,}"} {
		got, err := sub(t, op, map[string]string{"a": ""})
		if err != nil {
			t.Errorf("%s on empty: %v", op, err)
		}
		if got != "" {
			t.Errorf("%s on empty: got %q, want empty", op, got)
		}
	}
}

func TestSubstitute_UTF8FirstChar(t *testing.T) {
	// ^ upper-cases the first RUNE, not the first byte (no mojibake).
	got, err := sub(t, "{a^}", map[string]string{"a": "über"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Über" {
		t.Errorf("got %q, want Über", got)
	}
}

// --- malformed operators: rejected at decode (parse) time ---

func decodePatternErr(t *testing.T, pattern string) error {
	t.Helper()
	// A pattern's placeholders are parsed at Secret decode; drive that path.
	body := "template:\n  K:\n    pattern: " + quoteYAML(pattern) + "\n    literals: {x: y}\n"
	return decodeTemplateErr(t, body)
}

func quoteYAML(s string) string {
	// minimal YAML double-quote escaping for the test patterns below
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func TestSubstitute_MalformedOperatorsRejectedAtParse(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantSub string // substring expected in the error
	}{
		{"triple_percent", "{x%%%suf}", "%"},
		{"triple_hash", "{x###pre}", "#"},
		{"replace_missing_sep", "{x/onlyold}", "old/new"},
		{"replace_empty_search", "{x//}", "old/new"},
		{"substring_bad_offset", "{x:abc}", "offset"},
		{"substring_bad_len", "{x:1:xy}", "length"},
		{"substring_negative_offset", "{x:-1}", "offset"},
		{"empty_name_before_op", "{^^}", "field name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := decodePatternErr(t, c.pattern)
			if err == nil {
				t.Fatalf("expected error for %q", c.pattern)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("%q: error %q missing %q", c.pattern, err.Error(), c.wantSub)
			}
		})
	}
}

// --- substring RANGE errors: value-dependent, surfaced at Substitute time ---

func TestSubstitute_SubstringOutOfRange(t *testing.T) {
	cases := []struct {
		pattern string
		val     string
	}{
		{"{a:100}", "short"},   // offset past end
		{"{a:2:100}", "short"}, // length past end
	}
	for _, c := range cases {
		_, err := sub(t, c.pattern, map[string]string{"a": c.val})
		if err == nil {
			t.Errorf("%q on %q: expected out-of-range error", c.pattern, c.val)
		} else if !strings.Contains(err.Error(), "range") {
			t.Errorf("%q: error %q missing 'range'", c.pattern, err.Error())
		}
	}
}

// A range-valid substring at the exact boundary must succeed.
func TestSubstitute_SubstringBoundary(t *testing.T) {
	got, err := sub(t, "{a:2:3}", map[string]string{"a": "abcde"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "cde" {
		t.Errorf("got %q, want cde", got)
	}
}

// A field VALUE containing operator sigils must NOT be reparsed — operators
// come only from the PATTERN, applied once to the resolved value.
func TestSubstitute_ValueWithSigilsNotReparsed(t *testing.T) {
	got, err := sub(t, "{a}", map[string]string{"a": "x%%y#z:1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "x%%y#z:1" {
		t.Errorf("value sigils were interpreted: got %q", got)
	}
}
