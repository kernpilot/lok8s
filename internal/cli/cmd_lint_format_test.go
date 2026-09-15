package cli

import (
	"bytes"
	"os"
	"strings"
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
	// A terminal on the real stdout keeps text, GITHUB_ACTIONS or not.
	lintStdoutIsTerminal = func() bool { return true }
	if f, _ := lintFormat("", os.Stdout); f != lint.FormatText {
		t.Errorf("real stdout on a terminal under GITHUB_ACTIONS: %q, want text", f)
	}
	lintStdoutIsTerminal = func() bool { return false }
	t.Setenv("GITHUB_ACTIONS", "")
	if f, _ := lintFormat("", os.Stdout); f != lint.FormatText {
		t.Errorf("real stdout without GITHUB_ACTIONS: %q, want text", f)
	}
	if _, err := lintFormat("xml", os.Stdout); err == nil {
		t.Errorf("an unknown format must be an error")
	}
}

// TestLintFormatChargesTheValidatedSpecFile: a finding without a path is
// charged to the file the linter validated, deploy.lok8s.yaml for a
// deploy domain.
func TestLintFormatChargesTheValidatedSpecFile(t *testing.T) {
	paths := completionProject(t)
	t.Setenv("GITHUB_ACTIONS", "")
	_, errOut, _ := runOut(t, paths, "lint", "--domain", "gamma.app", "--format", "editor")
	if !strings.Contains(errOut, "clusters/gamma.app/deploy.lok8s.yaml:1: [error]") || strings.Contains(errOut, "cluster.lok8s.yaml:1:") {
		t.Errorf("deploy domain findings:\n%s", errOut)
	}
	_, errOut, _ = runOut(t, paths, "lint", "--domain", "beta.cloud", "--format", "editor")
	if !strings.Contains(errOut, "clusters/beta.cloud/cluster.lok8s.yaml:1: [error]") {
		t.Errorf("cluster domain findings:\n%s", errOut)
	}
}
