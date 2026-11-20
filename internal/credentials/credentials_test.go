package credentials

import (
	"bytes"
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/ui"
)

// A CR or LF in a credential is refused with the variable named; the
// line-based carriers (curl config, env file) cannot hold it.
func TestNoNewlineRefusesCRAndLF(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"tok\nen", "tok\ren", "token\n", "\r\ntoken"} {
		var stderr bytes.Buffer
		err := NoNewline("KKP_TOKEN", value, &stderr)
		if err == nil {
			t.Fatalf("%q was accepted", value)
		}
		if !errors.Is(err, ui.ErrHandled) {
			t.Errorf("%q: the error must be marked handled: %v", value, err)
		}
		if want := "environment variable KKP_TOKEN must not contain a newline"; !bytes.Contains(stderr.Bytes(), []byte(want)) {
			t.Errorf("%q: stderr = %q, want %q", value, stderr.String(), want)
		}
	}
	var stderr bytes.Buffer
	if err := NoNewline("KKP_TOKEN", "plain token with spaces and \"quotes\"", &stderr); err != nil || stderr.Len() != 0 {
		t.Errorf("a plain value was refused: %v %q", err, stderr.String())
	}
}

// controlRefusal is the refusal of http::credential_ok for name, as the
// bash twin prints it on a stderr that is not a terminal.
func controlRefusal(name string) string {
	return "[error] environment variable " + name + " must not contain a control character\n" +
		"  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.\n"
}

// NoControl refuses exactly the bytes of [[:cntrl:]] under LC_ALL=C
// (0x00-0x1f and 0x7f), as http::credential_ok does. A byte 0x80-0xff alone
// is not valid UTF-8, and it passes. So does a C1 character in UTF-8, and
// an empty or unset value.
func TestNoControlRefusesExactlyTheControlBytes(t *testing.T) {
	t.Parallel()
	check := func(value string, refuse bool) {
		t.Helper()
		var stderr bytes.Buffer
		env := map[string]string{"TEST_CREDENTIAL": value}
		err := NoControl(func(k string) string { return env[k] }, &stderr, "TEST_CREDENTIAL")
		want := ""
		if refuse {
			want = controlRefusal("TEST_CREDENTIAL")
		}
		if (err != nil) != refuse || stderr.String() != want {
			t.Errorf("%q: NoControl = %v, stderr %q", value, err, stderr.String())
		}
		if refuse && !errors.Is(err, ui.ErrHandled) {
			t.Errorf("%q: the error must be marked handled: %v", value, err)
		}
		if refuse && err != nil && err.Error() != "credentials: TEST_CREDENTIAL must not contain a control character" {
			t.Errorf("%q: error %q", value, err.Error())
		}
	}
	for b := 0; b <= 0xff; b++ {
		check("a"+string([]byte{byte(b)})+"b", b <= 0x1f || b == 0x7f)
	}
	for _, v := range []string{"", "a\u0085b", "a\u009bb", "a\xc2b", "\xff\xfe\xfd", "a\xe2\x80b", "\xed\xa0\x80"} {
		check(v, false)
	}
	var stderr bytes.Buffer
	if err := NoControl(func(string) string { return "" }, &stderr, "UNSET_CREDENTIAL"); err != nil || stderr.Len() != 0 {
		t.Errorf("an unset value: %v, stderr %q", err, stderr.String())
	}
}

// Each refused name gets its two lines, in the order of the names, and
// the error names each one. The value is in no line and not in the error.
func TestNoControlNamesEachRefusedVariable(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"FIRST":  "first-value-not-real\r",
		"SECOND": "second-value-not-real",
		"THIRD":  "third-value\tnot-real",
	}
	var stderr bytes.Buffer
	err := NoControl(func(k string) string { return env[k] }, &stderr, "FIRST", "SECOND", "THIRD")
	if !errors.Is(err, ui.ErrHandled) {
		t.Fatalf("error %v, want ErrHandled", err)
	}
	if got, want := stderr.String(), controlRefusal("FIRST")+controlRefusal("THIRD"); got != want {
		t.Errorf("stderr\n%q\nwant\n%q", got, want)
	}
	if got, want := err.Error(), "credentials: FIRST, THIRD must not contain a control character"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
	if strings.Contains(stderr.String()+err.Error(), "not-real") {
		t.Errorf("the output holds a value: %q %v", stderr.String(), err)
	}
}

// IsControl is the set of [[:cntrl:]] under LC_ALL=C, over runes.
func TestIsControlIsC0AndDEL(t *testing.T) {
	t.Parallel()
	for r := rune(0); r <= 0x10ffff; r++ {
		if got, want := IsControl(r), r < 0x20 || r == 0x7f; got != want {
			t.Fatalf("IsControl(%U) = %v, want %v", r, got, want)
		}
	}
}

// NoControl is the Go copy of http::credential_ok (utils/http.sh): the
// bash body must be the one that tests [[:cntrl:]] under LC_ALL=C and
// prints the two Go lines (noControlLine, noControlHint), and IsControl
// must be the cntrl class of the C locale (POSIX: 0x00-0x1f and 0x7f).
// The bash file is the embedded tree, as for TestTLSLogVarsMatchBash.
func TestNoControlMatchesBash(t *testing.T) {
	t.Parallel()
	src, err := fs.ReadFile(assets.FS(), "utils/http.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?ms)^http::credential_ok\(\) \{\n(.*?)\n\}$`).FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("utils/http.sh: %d functions http::credential_ok, want 1", len(m))
	}
	want := strings.Join([]string{
		`  local LC_ALL=C name rc=0`,
		`  for name in "${@}"; do`,
		`    if [[ "${!name:-}" == *[[:cntrl:]]* ]]; then`,
		`      error "` + strings.Replace(noControlLine, "%s", "${name}", 1) + `"`,
		`      echo "` + noControlHint + `" >&2`,
		`      rc=1`,
		`    fi`,
		`  done`,
		`  return "${rc}"`,
	}, "\n")
	if got := string(m[0][1]); got != want {
		t.Fatalf("utils/http.sh http::credential_ok body\n%s\nwant (from the Go text)\n%s", got, want)
	}
	for r := rune(0); r <= 0xff; r++ {
		if got, want := IsControl(r), r <= 0x1f || r == 0x7f; got != want {
			t.Errorf("IsControl(%U) = %v, the cntrl class of the C locale: %v", r, got, want)
		}
	}
}

// TLSLogVars is the Go copy of the bash list _HTTP_TLS_LOG_VARS
// (utils/http.sh, the list of http::tls_log_vars_unset): the two must hold
// the same names in the same order. The bash file is the embedded tree.
func TestTLSLogVarsMatchBash(t *testing.T) {
	t.Parallel()
	src, err := fs.ReadFile(assets.FS(), "utils/http.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^_HTTP_TLS_LOG_VARS=\(([^)]*)\)$`).FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("utils/http.sh: %d lines _HTTP_TLS_LOG_VARS=(…), want 1", len(m))
	}
	bash := strings.Fields(string(m[0][1]))
	if got := TLSLogVars(); !slices.Equal(got, bash) {
		t.Fatalf("TLSLogVars() = %q, utils/http.sh _HTTP_TLS_LOG_VARS = %q", got, bash)
	}
}

// The six names as literals, apart from the list itself: the drift tests
// compare the copies with each other, so a name dropped from every copy
// would pass them.
func TestTLSLogVarsAreTheSix(t *testing.T) {
	t.Parallel()
	want := []string{"SSLKEYLOGFILE", "GNUTLS_KEYLOGFILE", "GNUTLS_DEBUG_LEVEL", "SSLTRACE", "SSLDEBUG", "SSLDEBUGFILE"}
	if got := TLSLogVars(); !slices.Equal(got, want) {
		t.Fatalf("TLSLogVars() = %q, want %q", got, want)
	}
}
