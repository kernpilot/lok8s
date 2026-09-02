package operator

// jsonx.go — the jq idioms the bash hooks read the CR JSON with, over a
// decoded `any` tree:
//
//	jq -r '.a.b'           → jqR(get(obj, "a", "b"))   (missing/null → "null")
//	jq -r '.a // "x"'      → jqR(alt(get(obj, "a"), "x"))
//	jq -r '.a // empty'    → jqEmpty(get(obj, "a"))      (missing/null/false → "")
//	jq -e '.a'             → present(get(obj, "a"))      (exists, not null/false)
//	jq -c '.a // []'       → compact(alt(get(obj, "a"), []any{}))
//
// Numbers decode as json.Number so the literal text survives (jq 1.7
// prints a number back as written; `replicas: 3` → "3").

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// decode parses raw JSON into the `any` tree with number literals kept.
func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// get walks object keys; a missing key or a non-object step yields nil
// (jq: `.a.b` on a missing `a` is null).
func get(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = m[key]
		if !ok {
			return nil
		}
	}
	return v
}

// alt is jq's `//`: the left value unless it is null or false.
func alt(v any, fallback any) any {
	if v == nil {
		return fallback
	}
	if b, ok := v.(bool); ok && !b {
		return fallback
	}
	return v
}

// present is `jq -e`: exists and is neither null nor false.
func present(v any) bool {
	return alt(v, nil) != nil
}

// jqR is `jq -r`: a string prints raw; everything else prints as JSON
// (null → "null", numbers as written, objects/arrays compact — jq -r
// pretty-prints those, but every hook site reading a container with -r only
// tests it for emptiness).
func jqR(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		return compact(v)
	}
}

// jqEmpty is `jq -r '… // empty'`: "" when null or false, else jqR.
func jqEmpty(v any) string {
	if alt(v, nil) == nil {
		return ""
	}
	return jqR(v)
}

// compact is `jq -c`: JSON without whitespace. A value that cannot encode
// (never, for a decoded tree) prints as "null".
func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// contains is `index($f) != null` on a string array; a non-array (a
// malformed finalizers value) contains nothing.
func contains(list any, want string) bool {
	arr, ok := list.([]any)
	if !ok {
		return false
	}
	for _, item := range arr {
		if s, ok := item.(string); ok && s == want {
			return true
		}
	}
	return false
}

// pretty is jq's default output: 2-space indented, `"key": value`.
func pretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "null"
	}
	return string(b)
}

// errorf formats a plain error (no stderr side effect).
func errorf(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}
