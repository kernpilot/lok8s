package ui

import "testing"

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"a", "", 1}, {"", "abc", 3},
		{"kitten", "sitting", 3}, {"alpha.de", "alpha.dev", 1},
		{"boostrap", "bootstrap", 1}, {"Status", "status", 0},
	}
	for _, c := range cases {
		if got := Levenshtein(c.a, c.b); got != c.want {
			t.Errorf("Levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestClosest(t *testing.T) {
	domains := []string{"alpha.dev", "beta.cloud", "gamma.app"}
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"alpha.de", "alpha.dev", true},
		{"beta", "beta.cloud", true},    // prefix
		{"gama.app", "gamma.app", true}, // distance 1
		{"nothing.like", "", false},
		{"", "alpha.dev", true}, // the empty prefix matches the first
	}
	for _, c := range cases {
		got, ok := Closest(c.name, domains)
		if got != c.want || ok != c.ok {
			t.Errorf("Closest(%q) = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
