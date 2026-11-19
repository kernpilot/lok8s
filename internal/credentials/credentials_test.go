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
