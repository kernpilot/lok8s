package initctx

// form_test.go: how a form under bubbletea ends. A Ctrl-C press and an
// external SIGINT (InterruptMsg) interrupt (ErrAborted, rc 130); Esc
// cancels (ErrCancelled); a plain end is nil; another error is wrapped.

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func testForm() *huh.Form {
	v := ""
	return huh.NewForm(huh.NewGroup(huh.NewInput().Title("x").Value(&v)))
}

func TestFormModelEnds(t *testing.T) {
	for _, c := range []struct {
		name string
		msg  tea.Msg
		want error
	}{
		{"ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, ErrAborted},
		{"sigint", tea.InterruptMsg{}, ErrAborted},
		{"esc", tea.KeyPressMsg{Code: tea.KeyEscape}, ErrCancelled},
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}, ErrCancelled},
	} {
		m := &formModel{form: testForm()}
		m.form.Init()
		m.Update(c.msg)
		if got := m.ended(tea.ErrInterrupted); !errors.Is(got, c.want) {
			t.Errorf("%s: interrupted program ends with %v, want %v", c.name, got, c.want)
		}
		if got := m.ended(tea.ErrInterrupted); (got == ErrAborted) != m.interrupted {
			t.Errorf("%s: interrupted=%v, end %v", c.name, m.interrupted, got)
		}
	}
	// An aborted form (huh's own state) reads the same way.
	m := &formModel{form: testForm()}
	m.form.State = huh.StateAborted
	if got := m.ended(nil); !errors.Is(got, ErrCancelled) {
		t.Errorf("aborted form: %v", got)
	}
	m.interrupted = true
	if got := m.ended(nil); !errors.Is(got, ErrAborted) {
		t.Errorf("aborted form after Ctrl-C: %v", got)
	}
	// A plain end is nil; another error is wrapped.
	m = &formModel{form: testForm()}
	if got := m.ended(nil); got != nil {
		t.Errorf("plain end: %v", got)
	}
	boom := errors.New("boom")
	if got := m.ended(boom); !errors.Is(got, boom) || errors.Is(got, ErrAborted) || errors.Is(got, ErrCancelled) {
		t.Errorf("other error: %v", got)
	}
}
