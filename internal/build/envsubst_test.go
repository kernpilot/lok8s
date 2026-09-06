package build

import (
	"testing"
)

func TestEnvsubst(t *testing.T) {
	vals := map[string]string{
		"LOK8S_SPEC_FOO":   "foo-val",
		"LOK8S_USER_HOST":  "1.2.3.4",
		"LOK8S_SPEC_EMPTY": "",
	}
	lookup := func(name string) string { return vals[name] }
	names := []string{"LOK8S_SPEC_FOO", "LOK8S_USER_HOST", "LOK8S_SPEC_EMPTY", "LOK8S_SPEC_UNSET"}

	cases := []struct {
		name, in, want string
	}{
		{"braced", "a=${LOK8S_SPEC_FOO}!", "a=foo-val!"},
		{"bare", "a=$LOK8S_SPEC_FOO!", "a=foo-val!"},
		{"bare at eol", "a=$LOK8S_USER_HOST", "a=1.2.3.4"},
		{"boundary: listed name inside longer identifier stays", "$LOK8S_SPEC_FOOBAR", "$LOK8S_SPEC_FOOBAR"},
		{"boundary: braced longer identifier stays", "${LOK8S_SPEC_FOOBAR}", "${LOK8S_SPEC_FOOBAR}"},
		{"listed but unset -> empty", "[${LOK8S_SPEC_UNSET}]", "[]"},
		{"listed and empty -> empty", "[$LOK8S_SPEC_EMPTY]", "[]"},
		{"non-whitelisted braced untouched", "${OTHER_VAR}", "${OTHER_VAR}"},
		{"non-whitelisted bare untouched", "$OTHER_VAR", "$OTHER_VAR"},
		{"shell parameter expansion untouched", "${LOK8S_SPEC_FOO:-x}", "${LOK8S_SPEC_FOO:-x}"},
		{"array subscript untouched", "${arr[0]}", "${arr[0]}"},
		{"unclosed brace untouched", "${LOK8S_SPEC_FOO", "${LOK8S_SPEC_FOO"},
		{"double dollar: second token substitutes", "$$LOK8S_SPEC_FOO", "$foo-val"},
		{"lone dollar at eof", "end$", "end$"},
		{"dollar digit untouched", "$1", "$1"},
		{"mixed line", "x=$LOK8S_SPEC_FOO y=${LOK8S_USER_HOST} z=$KEEP", "x=foo-val y=1.2.3.4 z=$KEEP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(envsubst([]byte(tc.in), names, lookup))
			if got != tc.want {
				t.Errorf("envsubst(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEnvsubstEmptyWhitelistReplacesNothing(t *testing.T) {
	in := "a=${LOK8S_SPEC_FOO} b=$LOK8S_SPEC_FOO"
	got := string(envsubst([]byte(in), nil, func(string) string { return "boom" }))
	if got != in {
		t.Errorf("empty whitelist must pass through untouched, got %q", got)
	}
}

func TestEnvsubstWhitelist(t *testing.T) {
	t.Setenv("LOK8S_SPEC_WLTEST", "1")
	t.Setenv("LOK8S_USER_WLTEST", "2")
	t.Setenv("LOK8S_OTHER_WLTEST", "3")
	t.Setenv("NOT_LOK8S_SPEC_X", "4")
	names := EnvsubstWhitelist()
	has := func(want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	if !has("LOK8S_SPEC_WLTEST") || !has("LOK8S_USER_WLTEST") {
		t.Errorf("whitelist missing LOK8S_(SPEC|USER)_ vars: %v", names)
	}
	if has("LOK8S_OTHER_WLTEST") || has("NOT_LOK8S_SPEC_X") {
		t.Errorf("whitelist must only match ^LOK8S_(SPEC|USER)_: %v", names)
	}
}
