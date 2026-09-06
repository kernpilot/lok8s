package secrets

import (
	"os/exec"
	"strings"
	"testing"
)

// TestBashQuote pins the %q quoting table (derived empirically from GNU bash
// 5.3 `printf %q`) that `lo secrets env` depends on for injection-safe eval.
func TestBashQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "plain"},
		{"a b", `a\ b`},
		{"a=b", "a=b"},
		{"a=b c", `a=b\ c`},
		{"a,b", `a\,b`},
		{"a~b", "a~b"},
		{"~ab", `\~ab`},
		{"#ab", `\#ab`},
		{"a#b", "a#b"},
		{"a!b", `a\!b`},
		{"a%b", "a%b"},
		{"a^b", `a\^b`},
		{"a@b", "a@b"},
		{"a:b", "a:b"},
		{"a+b", "a+b"},
		{"a/b.c_d-e", "a/b.c_d-e"},
		{`quo"te`, `quo\"te`},
		{"sing'le", `sing\'le`},
		{`back\slash`, `back\\slash`},
		{"dollar$v", `dollar\$v`},
		{"star*x", `star\*x`},
		{"q?x", `q\?x`},
		{"brk[x]", `brk\[x\]`},
		{"brc{x}", `brc\{x\}`},
		{"par(x)", `par\(x\)`},
		{"lt<gt>", `lt\<gt\>`},
		{"pipe|x", `pipe\|x`},
		{"amp&x", `amp\&x`},
		{"semi;x", `semi\;x`},
		{"tick`x", "tick\\`x"},
		{"ümlaut", "ümlaut"},
		{"€uro", "€uro"},
		// ANSI-C path: any non-printable character switches the whole string.
		{"a\tb", `$'a\tb'`},
		{"nl\nhere", `$'nl\nhere'`},
		{"a\n\"b", `$'a\n"b'`},
		{"a\n'b", `$'a\n\'b'`},
		{"a\n\\b", `$'a\n\\b'`},
		{"a\nüb", `$'a\nüb'`},
		{"\x7fx", `$'\177x'`},
		{"\x01\x02", `$'\001\002'`},
		{"\x1besc", `$'\Eesc'`},
		{"price: 12,50€\nok", `$'price: 12,50€\nok'`},
		{"\xff\xfe", `$'\377\376'`}, // invalid UTF-8 → per-byte octal
		{"bell\a", `$'bell\a'`},
		{"cr\rlf\n", `$'cr\rlf\n'`},
	}
	for _, tc := range cases {
		if got := bashQuote(tc.in); got != tc.want {
			t.Errorf("bashQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestBashQuoteAgainstBash cross-checks against a real bash when one is on
// PATH — the ground truth the table above was derived from.
func TestBashQuoteAgainstBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	inputs := []string{
		"", "plain", "a b", "a=b", "~x", "#x", "x#y", "x~y",
		"pass!word$with`every'sort\"of|char&;<>(){}[]*?,^\\",
		"multi\nline\tvalue", "trailing space ", " leading",
		"ümlaut €uro", "esc\x1b", "del\x7f", "ctl\x01",
	}
	for _, in := range inputs {
		// printf %q via bash, input passed as an argument (no shell parsing
		// of the value itself).
		out, err := exec.Command(bash, "-c", `printf %q "$1"`, "_", in).Output()
		if err != nil {
			t.Fatalf("bash printf %%q failed for %q: %v", in, err)
		}
		if got, want := bashQuote(in), string(out); got != want {
			t.Errorf("bashQuote(%q) = %s, bash says %s", in, got, want)
		}
	}
}

// TestBashQuoteRoundTrip proves the quoting is eval-safe: bash must echo the
// exact original bytes back.
func TestBashQuoteRoundTrip(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	inputs := []string{
		"hunter2", "p@ss w0rd!", "a\nb\tc", "$(rm -rf /)", "`boom`",
		"quote'inside\"both", "back\\slash", "€;|&<>",
	}
	for _, in := range inputs {
		out, err := exec.Command(bash, "-c", "printf %s "+bashQuote(in)).Output()
		if err != nil {
			t.Fatalf("eval failed for %q: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("round-trip %q → %q", in, string(out))
		}
	}
	// And the injection canary: the quoted form must never execute.
	if strings.Contains(bashQuote("$(touch /tmp/pwned)"), "$(") &&
		!strings.Contains(bashQuote("$(touch /tmp/pwned)"), `\$`) {
		t.Error("command substitution not neutralized")
	}
}
