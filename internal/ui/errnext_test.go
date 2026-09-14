package ui

import (
	"bytes"
	"testing"
)

func TestErrorNextOffATerminalIsTheLegacyLine(t *testing.T) {
	t.Cleanup(ForceTTY(false))
	var buf bytes.Buffer
	ErrorNext(&buf, "use kubehz.dev", "the domain exists", "domain not found: %s", "x.dev")
	if want := "[error] domain not found: x.dev\n"; buf.String() != want {
		t.Errorf("ErrorNext piped = %q, want %q", buf.String(), want)
	}
}

func TestErrorNextOnATerminalIsTwoLines(t *testing.T) {
	t.Cleanup(ForceTTY(true))
	t.Cleanup(ForceColor(false))
	var buf bytes.Buffer
	ErrorNext(&buf, "use kubehz.dev", "the domain exists", "domain not found: %s", "x.dev")
	if want := "error: domain not found: x.dev\nnext: lo use kubehz.dev   # the domain exists\n"; buf.String() != want {
		t.Errorf("ErrorNext on a terminal = %q, want %q", buf.String(), want)
	}
	buf.Reset()
	ErrorNext(&buf, "", "", "no next step")
	if want := "error: no next step\n"; buf.String() != want {
		t.Errorf("ErrorNext without a next = %q, want %q", buf.String(), want)
	}
}

func TestNoticeToIsSilentUnderQuiet(t *testing.T) {
	t.Cleanup(func() { SetQuiet(false) })
	var buf bytes.Buffer
	NoticeTo(&buf, "notice: %s", "a")
	SetQuiet(true)
	NoticeTo(&buf, "notice: %s", "b")
	if buf.String() != "notice: a\n" {
		t.Errorf("NoticeTo = %q, want only the line before SetQuiet(true)", buf.String())
	}
}
