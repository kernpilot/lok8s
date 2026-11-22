package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yaml11 "go.yaml.in/yaml/v2"
)

// yaml11Case is one line of tests/fixtures/yaml11-quote/cases: a JSON value,
// the yaml scalar that it prints as, and why (set, encoder, plain or value;
// the header of the file says what each means). The bats twin
// (tests/unit/kubehz_agent_yaml11_test.bats) reads the same file.
type yaml11Case struct {
	line            int
	json, want, why string
}

func readYAML11Cases(t *testing.T) []yaml11Case {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "tests", "fixtures", "yaml11-quote", "cases"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cases []yaml11Case
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !json.Valid([]byte(fields[0])) {
			t.Fatalf("cases line %d: want <json value> TAB <yaml scalar> TAB <why>, got %q", n, line)
		}
		isString := strings.HasPrefix(fields[0], `"`)
		switch fields[2] {
		case "set", "encoder", "plain":
			if !isString {
				t.Fatalf("cases line %d: %s is %s, but not a string", n, fields[0], fields[2])
			}
		case "value":
			if isString {
				t.Fatalf("cases line %d: %s is a string, not a value", n, fields[0])
			}
		default:
			t.Fatalf("cases line %d: why %q is not set, encoder, plain or value", n, fields[2])
		}
		cases = append(cases, yaml11Case{line: n, json: fields[0], want: fields[1], why: fields[2]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatalf("the table has %d cases, want at least 100", len(cases))
	}
	return cases
}

// sameYAML11Value reports whether back (what go-yaml v2 read) is the JSON
// value v: a string is the same string, a bool the same bool, null is nil,
// and a number has the same value (v2 reads 7 as an int).
func sameYAML11Value(back, v any) bool {
	switch v := v.(type) {
	case float64:
		switch b := back.(type) {
		case int:
			return float64(b) == v
		case float64:
			return b == v
		}
		return false
	default:
		return back == v
	}
}

// Each case prints its scalar as a value (`v: <scalar>`), and a string case
// prints it as a key too (`<scalar>: 1`). The output reads back with go-yaml
// v2, a YAML 1.1 reader (yes, on and y are bools there), as the JSON value:
// a string stays a string, and a real bool, null or number stays one.
// go-yaml v2 cannot show each row: it has no base 60 (PyYAML has), it reads a
// time into an any as a string, and it has only the three case forms of each
// word (yes, Yes, YES) that YAML 1.1 has. So for 1:20, 2026-11-20 or tRUE the
// exact text is the check. YAML 1.1 reads the other case forms (tRUE) and a
// base 60 int with a first digit 0 (0:30) as strings: the pattern quotes them
// without need, which is harmless.
func TestYAML11QuotesWhatAYAML11ReaderReadsAsABoolANullOrANumber(t *testing.T) {
	for _, c := range readYAML11Cases(t) {
		var v any
		if err := json.Unmarshal([]byte(c.json), &v); err != nil {
			t.Fatalf("line %d: %v", c.line, err)
		}
		var out bytes.Buffer
		if err := writeJSONAsYAML(&out, []byte(`{"v":`+c.json+`}`)); err != nil {
			t.Fatalf("line %d: %v", c.line, err)
		}
		if got, want := out.String(), "v: "+c.want+"\n"; got != want {
			t.Errorf("line %d: %s prints %q, want %q", c.line, c.json, got, want)
		}
		var back map[string]any
		if err := yaml11.Unmarshal(out.Bytes(), &back); err != nil {
			t.Errorf("line %d: %s: YAML 1.1 cannot read %q: %v", c.line, c.json, out.String(), err)
		} else if !sameYAML11Value(back["v"], v) {
			t.Errorf("line %d: %s reads back as %#v (%T) under YAML 1.1", c.line, c.json, back["v"], back["v"])
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out.Reset()
		if err := writeJSONAsYAML(&out, []byte(`{`+c.json+`:1}`)); err != nil {
			t.Fatalf("line %d: %v", c.line, err)
		}
		if got, want := out.String(), c.want+": 1\n"; got != want {
			t.Errorf("line %d: the key %s prints %q, want %q", c.line, c.json, got, want)
		}
		var keys map[any]any
		if err := yaml11.Unmarshal(out.Bytes(), &keys); err != nil {
			t.Errorf("line %d: the key %s: YAML 1.1 cannot read %q: %v", c.line, c.json, out.String(), err)
		} else if len(keys) != 1 || keys[s] != 1 {
			t.Errorf("line %d: the key %s reads back as %#v under YAML 1.1", c.line, c.json, keys)
		}
	}
}

// The pattern holds the set: it matches each string of the table that is in
// the set, and no other string. The output alone cannot show this for a form
// that yaml.v3 or yq quotes itself, so a class could go missing from the
// pattern and the output test would still pass. Both quote these forms
// themselves: true, false and null in the three case forms, ~, the empty
// string, each number form but base 60 and .5_ (7, 0x1F, 0o17, 0777, 08, 1.5,
// 1e3, -_1, 0_x1f, 1e_+3), .inf and .nan in the three case forms, and the
// dates and times that go-yaml reads as a time (2026-11-20,
// 2026-11-20T10:00:00Z, 2026-11-20 10:00:00). yaml.v3 also quotes 0b-1 and
// 0b+1, and yq does not.
func TestYAML11PatternHoldsTheSet(t *testing.T) {
	n := 0
	for _, c := range readYAML11Cases(t) {
		if c.why == "value" {
			continue
		}
		var s string
		if err := json.Unmarshal([]byte(c.json), &s); err != nil {
			t.Fatalf("line %d: %v", c.line, err)
		}
		if got, want := yaml11ScalarRe.MatchString(s), c.why == "set"; got != want {
			t.Errorf("line %d: the pattern matches %s: %v, want %v (%s)", c.line, c.json, got, want, c.why)
		}
		n++
	}
	if n < 100 {
		t.Fatalf("%d strings checked, want at least 100", n)
	}
}

// caseForms returns each form of w in upper and lower case (no: no, nO, No,
// NO).
func caseForms(w string) []string {
	forms := []string{""}
	for _, r := range w {
		lower, upper := strings.ToLower(string(r)), strings.ToUpper(string(r))
		var next []string
		for _, f := range forms {
			next = append(next, f+lower)
			if upper != lower {
				next = append(next, f+upper)
			}
		}
		forms = next
	}
	return forms
}

// yaml11Grammar returns the strings of the property test, each one time, in
// a fixed order: the bool and null words, ~, = and the empty string; .inf
// and .nan with and without a sign; number shapes with and without a sign,
// each with no _, with one _ or two _ at each place, and with _ at each
// place; dates and times; each string of one to three characters over a
// small alphabet; and plain words as controls.
func yaml11Grammar() []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, w := range []string{"y", "n", "yes", "no", "on", "off", "true", "false", "null", "~", "=", ""} {
		for _, f := range caseForms(w) {
			add(f)
		}
	}
	for _, sign := range []string{"", "-", "+"} {
		for _, w := range []string{".inf", ".nan"} {
			for _, f := range caseForms(w) {
				add(sign + f)
			}
		}
	}
	bodies := []string{
		"0", "7", "08", "0777", "1000", "0b1", "0B10", "0b-1", "0b+1", "0o17", "0O7", "0x1f", "0X1F",
		"1.5", "1.", ".5", "1e3", "1E+3", "1e-3", "1.5e3", ".5E-3", "1.e3",
		"1:20", "1:20.5", "0:30", "190:20:30.15",
		"0x", "0b", "0b2", "0o8", "0x1G", "1e", "e3", ".", ".e3", "1.2.3", "1:60",
	}
	for _, sign := range []string{"", "-", "+"} {
		for _, b := range bodies {
			s := sign + b
			add(s)
			for i := 0; i <= len(s); i++ {
				for j := i; j <= len(s); j++ {
					add(s[:i] + "_" + s[i:j] + "_" + s[j:])
				}
				add(s[:i] + "_" + s[i:])
			}
			add(strings.Join(strings.Split(s, ""), "_"))
		}
	}
	for _, date := range []string{"2026-11-20", "2026-1-2"} {
		add(date)
		for _, sep := range []string{"T", "t", " ", "  ", "\t"} {
			for _, clock := range []string{"10:00:00", "1:2:3"} {
				for _, frac := range []string{"", ".5", ",5", ".", ".123456789"} {
					for _, zone := range []string{"", "Z", " Z", "+01:00", "-05:00", " +01:00", "+1", " -5"} {
						add(date + sep + clock + frac + zone)
					}
				}
			}
		}
	}
	const alphabet = "018_-+.eExXbo:"
	for _, a := range alphabet {
		add(string(a))
		for _, b := range alphabet {
			add(string(a) + string(b))
			for _, c := range alphabet {
				add(string(a) + string(b) + string(c))
			}
		}
	}
	for _, w := range []string{"word", "yess", "v1.2", "10.0.0.1", "sp-1a2b3c4d", "inf", "nan", "Infinity", "NaN", "0x1p-2", "1_000_x"} {
		add(w)
	}
	return out
}

// How go-yaml v2 reads a string as a plain value (`v: <s>`).
const (
	yaml11NotPlain = iota // an error, a collection or another string
	yaml11String          // the same string
	yaml11Value           // a bool, a null or a number
	yaml11Time            // a time
)

func yaml11Reads(s string) int {
	doc := []byte("v: " + s)
	var m map[string]any
	if err := yaml11.Unmarshal(doc, &m); err != nil || len(m) != 1 {
		return yaml11NotPlain
	}
	switch v := m["v"].(type) {
	case string:
		if v != s {
			return yaml11NotPlain
		}
		// go-yaml v2 reads a time into an any as its text. A time field
		// shows that it reads a time.
		var tm struct {
			V time.Time `yaml:"v"`
		}
		if yaml11.Unmarshal(doc, &tm) == nil {
			return yaml11Time
		}
		return yaml11String
	case bool, int, int64, uint64, float64, nil:
		return yaml11Value
	}
	return yaml11NotPlain
}

// A property over a grammar of a few thousand strings: each string that
// go-yaml v2 reads as a bool, a null, a number or a time when it is plain is
// in the set of the pattern, and writeJSONAsYAML prints it in double quotes,
// as a value and as a key. Each string of the grammar reads back with
// go-yaml v2 as the same string. The pattern check covers the bash twin too,
// whose yq quotes less than yaml.v3. The table holds the forms that only
// PyYAML reads so (base 60, a time after T without a zone).
func TestYAML11QuotesEachStringThatGoYAMLv2ReadsAsAValue(t *testing.T) {
	strs := yaml11Grammar()
	count := map[int]int{}
	for _, s := range strs {
		reads := yaml11Reads(s)
		count[reads]++
		js, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var val, key bytes.Buffer
		if err := writeJSONAsYAML(&val, []byte(`{"v":`+string(js)+`}`)); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if err := writeJSONAsYAML(&key, []byte(`{`+string(js)+`:1}`)); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		var back map[string]any
		if err := yaml11.Unmarshal(val.Bytes(), &back); err != nil || back["v"] != s {
			t.Errorf("%q prints %q, and go-yaml v2 reads %#v (%v)", s, val.String(), back["v"], err)
		}
		var keys map[any]any
		if err := yaml11.Unmarshal(key.Bytes(), &keys); err != nil || len(keys) != 1 || keys[s] != 1 {
			t.Errorf("the key %q prints %q, and go-yaml v2 reads %#v (%v)", s, key.String(), keys, err)
		}
		if reads != yaml11Value && reads != yaml11Time {
			continue
		}
		if !yaml11ScalarRe.MatchString(s) {
			t.Errorf("go-yaml v2 reads %q as a value, and the pattern does not match it", s)
		}
		if !strings.HasPrefix(val.String(), `v: "`) {
			t.Errorf("go-yaml v2 reads %q as a value, and it prints %q", s, val.String())
		}
		if !strings.HasPrefix(key.String(), `"`) {
			t.Errorf("go-yaml v2 reads the key %q as a value, and it prints %q", s, key.String())
		}
	}
	nonStrings := count[yaml11Value] + count[yaml11Time]
	t.Logf("%d strings, %d non-strings: go-yaml v2 reads %d as a bool, a null or a number and %d as a time when plain; %d cannot be plain",
		len(strs), nonStrings, count[yaml11Value], count[yaml11Time], count[yaml11NotPlain])
	if len(strs) < 2000 || count[yaml11Value] < 500 || count[yaml11Time] < 50 {
		t.Fatalf("%d strings, %d values and %d times, want at least 2000, 500 and 50", len(strs), count[yaml11Value], count[yaml11Time])
	}
}

// writeOutput (every -o yaml of the cli and of the agent tools) goes through
// the same quoting.
func TestYAML11WriteOutputQuotes(t *testing.T) {
	var out bytes.Buffer
	v := struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Ready  bool   `json:"ready"`
		Nodes  int    `json:"nodes"`
	}{ID: "sp-1a2b3c4d", Status: "off", Ready: true, Nodes: 2}
	if err := writeOutput(&out, outputYAML, v); err != nil {
		t.Fatal(err)
	}
	if want := "id: sp-1a2b3c4d\nstatus: \"off\"\nready: true\nnodes: 2\n"; out.String() != want {
		t.Errorf("writeOutput -o yaml:\n%s\nwant:\n%s", out.String(), want)
	}
}

// The bash twin (kubehz::agent_yaml) quotes with KUBEHZ_AGENT_YAML11. It
// must be the pattern of yaml11Scalar, in the tree and in the copy that the
// binary carries.
func TestYAML11PatternIsTheBashPattern(t *testing.T) {
	const prefix = "KUBEHZ_AGENT_YAML11='"
	for _, rel := range []string{".lok8s/libs/kubehz/agent", "internal/assets/lok8s/libs/kubehz/agent"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for line := range strings.SplitSeq(string(raw), "\n") {
			if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, "'") && len(line) > len(prefix) {
				found = append(found, line[len(prefix):len(line)-1])
			}
		}
		if len(found) != 1 {
			t.Errorf("%s: %d KUBEHZ_AGENT_YAML11 lines, want 1", rel, len(found))
			continue
		}
		if found[0] != yaml11Scalar {
			t.Errorf("%s: KUBEHZ_AGENT_YAML11 is\n%s\nthe Go pattern is\n%s", rel, found[0], yaml11Scalar)
		}
	}
}
