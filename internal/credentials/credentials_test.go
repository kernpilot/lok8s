package credentials

import (
	"bytes"
	"errors"
	"testing"

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
