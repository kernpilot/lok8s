package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml11 "go.yaml.in/yaml/v2"
)

// yaml11Case is one line of tests/fixtures/yaml11-quote/cases: a JSON value
// and the yaml scalar that it prints as. The bats twin
// (tests/unit/kubehz_agent_yaml11_test.bats) reads the same file.
type yaml11Case struct {
	line       int
	json, want string
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
		if len(fields) != 2 || !json.Valid([]byte(fields[0])) {
			t.Fatalf("cases line %d: want <json value> TAB <yaml scalar>, got %q", n, line)
		}
		cases = append(cases, yaml11Case{line: n, json: fields[0], want: fields[1]})
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
// go-yaml v2 has no base 60 and no case-insensitive words, so for 1:20 or
// tRUE the exact text is the check.
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
		for _, line := range strings.Split(string(raw), "\n") {
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
