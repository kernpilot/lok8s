package cli

import (
	"bytes"
	"os"
	"testing"

	"github.com/kernpilot/lok8s/internal/lint"
)

// TestLintFormatAutoSelectReadsTheRealStdoutOnly: GITHUB_ACTIONS picks
// github only for the process stdout off a terminal; a supplied writer
// stays text, and the flag always wins.
func TestLintFormatAutoSelectReadsTheRealStdoutOnly(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	prev := lintStdoutIsTerminal
	lintStdoutIsTerminal = func() bool { return false }
	t.Cleanup(func() { lintStdoutIsTerminal = prev })

	if f, _ := lintFormat("", os.Stdout); f != lint.FormatGitHub {
		t.Errorf("real stdout under GITHUB_ACTIONS: %q, want github", f)
	}
	if f, _ := lintFormat("", &bytes.Buffer{}); f != lint.FormatText {
		t.Errorf("a supplied writer under GITHUB_ACTIONS: %q, want text", f)
	}
	if f, _ := lintFormat(lint.FormatText, os.Stdout); f != lint.FormatText {
		t.Errorf("--format text under GITHUB_ACTIONS: %q, want text", f)
	}
	t.Setenv("GITHUB_ACTIONS", "")
	if f, _ := lintFormat("", os.Stdout); f != lint.FormatText {
		t.Errorf("real stdout without GITHUB_ACTIONS: %q, want text", f)
	}
	if _, err := lintFormat("xml", os.Stdout); err == nil {
		t.Errorf("an unknown format must be an error")
	}
}
