package kubehz

// jsonx.go — the jq filters the bash piped every api body through,
// re-expressed over a decoded JSON value. The semantics that matter for
// parity: `//` takes its right side when the left is null OR false; `-r`
// prints a string bare, a number as written, a bool as true/false, null as
// the word "null"; `.data? // .` unwraps the api's {ok, data} envelope and
// tolerates a bare body.

import (
	"bytes"
	"encoding/json"
	"sort"
)

// parseJSON decodes a body (numbers kept as written).
func parseJSON(body []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// jget walks object keys; nil when any hop is missing or not an object.
func jget(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = m[k]
		if !ok {
			return nil
		}
	}
	return v
}

// jfalsy is jq's alternative-operator test: null and false.
func jfalsy(v any) bool {
	if v == nil {
		return true
	}
	b, ok := v.(bool)
	return ok && !b
}

// jalt is `A // B // … // def` over already-resolved values.
func jalt(def any, vals ...any) any {
	for _, v := range vals {
		if !jfalsy(v) {
			return v
		}
	}
	return def
}

// jstr renders a value the way `jq -r` prints a scalar. Objects/arrays
// render as compact JSON (jq prints them too, just never on these paths).
func jstr(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// jstrOr is `<path> // "<def>"` rendered with -r; empty def models
// `// empty`.
func jstrOr(v any, def string, keys ...string) string {
	got := jget(v, keys...)
	if jfalsy(got) {
		return def
	}
	return jstr(got)
}

// envelope is `.data? // .`: the api's {ok, data} wrapper unwrapped, a bare
// body passed through. (`.data?` on a non-object yields empty → `//` picks
// the body itself; a null data does the same.)
func envelope(v any) any {
	if m, ok := v.(map[string]any); ok {
		if d, ok := m["data"]; ok && !jfalsy(d) {
			return d
		}
	}
	return v
}

// rows is `.data? // . | if type == "array" then . else [.] end`.
func rows(v any) []any {
	e := envelope(v)
	if arr, ok := e.([]any); ok {
		return arr
	}
	return []any{e}
}

// domainRows filters rows by .domain == d and sorts them oldest-first by
// .createdAt (jq sort_by: nulls sort before strings; stable).
func domainRows(v any, d string) []any {
	var out []any
	for _, r := range rows(v) {
		if s, ok := jget(r, "domain").(string); ok && s == d {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return jless(jget(out[i], "createdAt"), jget(out[j], "createdAt"))
	})
	return out
}

// jless orders scalars the way jq's sort_by does for the shapes that reach
// it here: null < false < true < numbers < strings, strings bytewise.
func jless(a, b any) bool {
	ra, rb := jrank(a), jrank(b)
	if ra != rb {
		return ra < rb
	}
	switch ta := a.(type) {
	case string:
		return ta < b.(string)
	case json.Number:
		fa, _ := ta.Float64()
		fb, _ := b.(json.Number).Float64()
		return fa < fb
	case bool:
		return !ta && b.(bool)
	}
	return false
}

func jrank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case json.Number:
		return 2
	case string:
		return 3
	}
	return 4
}

// compactJSON marshals a payload the way `jq -n '{…}'` shaped it (key
// order preserved via ordered pairs).
type jsonPair struct {
	Key string
	Val any
}

func compactJSON(pairs ...jsonPair) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(p.Key)
		v, _ := json.Marshal(p.Val)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}
