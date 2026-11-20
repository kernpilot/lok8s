package ui

import "testing"

// The rule of the environment, the same as verbose::noninteractive of the
// bash twin (tests/unit/kubehz_register_test.bats runs the bash rule).
func TestNonInteractive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"CI": "true"}, true},
		{map[string]string{"CI": "1"}, true},
		{map[string]string{"CI": "false"}, false},
		{map[string]string{"CI": ""}, false},
		{map[string]string{"LOK8S_NONINTERACTIVE": "1"}, true},
		{map[string]string{"LOK8S_NONINTERACTIVE": "1", "CI": "false"}, true},
	} {
		if got := NonInteractive(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("NonInteractive(%v) = %v, want %v", tc.env, got, tc.want)
		}
	}
}
