package ui

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"unicode"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// go test ./internal/ui/ -update
var update = flag.Bool("update", false, "rewrite testdata/unshown.json from Unshown")

const unshownTable = "testdata/unshown.json"

// unshownNote is the first field of testdata/unshown.json (JSON has no
// comments).
const unshownNote = "Generated from ui.Unshown by: go test ./internal/ui/ -update. " +
	"Each run is a range of code points (first to last, both in) that a message does not show. " +
	"Each other code point from 0 to max is shown. ui.Unshown (internal/ui/unshown_test.go) and " +
	"the jq def unshown (tests/unit/verbose_shown_test.bats) must agree with it on every code point."

// unshownRun is a run of code points, First to Last (both in), that
// Unshown reports. Hex gives the same run as U+XXXX for a reader.
type unshownRun struct {
	First rune   `json:"first"`
	Last  rune   `json:"last"`
	Hex   string `json:"hex"`
}

type unshownFile struct {
	Note string       `json:"note"`
	Max  rune         `json:"max"`
	Runs []unshownRun `json:"runs"`
}

// unshownRuns gives the maximal runs of Unshown over every code point from
// 0 to unicode.MaxRune.
func unshownRuns() []unshownRun {
	var runs []unshownRun
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !Unshown(r) {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1].Last == r-1 {
			runs[n-1].Last = r
			continue
		}
		runs = append(runs, unshownRun{First: r, Last: r})
	}
	for i := range runs {
		runs[i].Hex = fmt.Sprintf("U+%04X..U+%04X", runs[i].First, runs[i].Last)
	}
	return runs
}

// renderUnshown is the file for runs: one run on each line.
func renderUnshown(runs []unshownRun) []byte {
	var b bytes.Buffer
	note, _ := json.Marshal(unshownNote)
	fmt.Fprintf(&b, "{\n  \"note\": %s,\n  \"max\": %d,\n  \"runs\": [\n", note, unicode.MaxRune)
	for i, r := range runs {
		sep := ","
		if i == len(runs)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "    {\"first\": %d, \"last\": %d, \"hex\": %q}%s\n", r.First, r.Last, r.Hex, sep)
	}
	b.WriteString("  ]\n}\n")
	return b.Bytes()
}

// Unshown and its bash twin, the jq def unshown (VERBOSE_JQ_UNSHOWN,
// .lok8s/utils/verbose.sh), agree on every code point from 0 to U+10FFFF.
// testdata/unshown.json holds the runs of Unshown. This test checks
// Unshown against the file on each code point, and that the file is the
// one that Unshown gives. tests/unit/verbose_shown_test.bats checks the jq
// def against the same file on each code point. Thus a bound that moves in
// one twin only fails one of the two tests.
func TestUnshownMatchesTheTableOnEveryCodePoint(t *testing.T) {
	want := renderUnshown(unshownRuns())
	if *update {
		if err := os.WriteFile(unshownTable, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(unshownTable)
	if err != nil {
		t.Fatal(err)
	}
	var f unshownFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Max != unicode.MaxRune {
		t.Fatalf("%s: max = %d, want %d (U+10FFFF)", unshownTable, f.Max, unicode.MaxRune)
	}
	if len(f.Runs) < 10 {
		t.Fatalf("%s has %d runs, want 10 or more: the test proves nothing", unshownTable, len(f.Runs))
	}
	// The scan below reads the runs in order: each run must start after
	// the end of the run before it, with a gap.
	for i, r := range f.Runs {
		if r.First < 0 || r.First > r.Last || r.Last > f.Max || (i > 0 && r.First <= f.Runs[i-1].Last+1) {
			t.Fatalf("%s: run %d (%d to %d) is not after the run before it, with a gap, inside 0 to max", unshownTable, i, r.First, r.Last)
		}
	}
	// Each code point against the file as it is read, not against
	// unshownRuns: a fault of the generator does not hide here.
	i, bad := 0, 0
	for r := rune(0); r <= f.Max; r++ {
		for i < len(f.Runs) && f.Runs[i].Last < r {
			i++
		}
		in := i < len(f.Runs) && f.Runs[i].First <= r
		if got := Unshown(r); got != in {
			bad++
			if bad <= 20 {
				t.Errorf("U+%04X: Unshown = %v, %s says %v", r, got, unshownTable, in)
			}
		}
	}
	if bad > 20 {
		t.Errorf("%d code points differ in all", bad)
	}
	if !bytes.Equal(raw, want) {
		t.Errorf("%s is not the file that Unshown gives (run: go test ./internal/ui/ -update)\n%s",
			unshownTable, testutil.FirstDiff(string(want), string(raw)))
	}
}
