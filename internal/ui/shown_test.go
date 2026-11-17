package ui

import (
	"encoding/json"
	"os"
	"testing"
)

// shownCase is one case of testdata/shown.json. The bash twin
// (VERBOSE_JQ_SHOWN_NAME, tests/unit/verbose_shown_test.bats) reads the
// same file, so the two rules cannot drift apart.
type shownCase struct {
	Case string `json:"case"`
	In   string `json:"in"`
	Want string `json:"want"`
}

func TestShownReplacesControlBackslashBidiAndZeroWidth(t *testing.T) {
	raw, err := os.ReadFile("testdata/shown.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []shownCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("testdata/shown.json has %d cases, want 10 or more: the test proves nothing", len(cases))
	}
	for _, c := range cases {
		if got := Shown(c.In); got != c.Want {
			t.Errorf("%s: Shown(%q) = %q, want %q", c.Case, c.In, got, c.Want)
		}
	}
}

// A byte that is not UTF-8 becomes U+FFFD, as jq reads it (JSON cannot
// hold such a byte, so this case is not in testdata/shown.json).
func TestShownReplacesABadByteAsJqDoes(t *testing.T) {
	if got, want := Shown("a\xffb\x1b"), "a\uFFFDb?"; got != want {
		t.Fatalf("Shown = %q, want %q", got, want)
	}
}
