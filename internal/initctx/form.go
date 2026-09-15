package initctx

// form.go: the huh side of the screens. The form IO and the one runner
// every form goes through, with the CLI's one huh theme (ui.HuhTheme). IO.Accessible selects huh's
// line-driven mode. The tests script that mode over an io.Reader; a
// terminal gets the interactive forms under bubbletea, so the key that
// ended a form is known (Esc cancels, Ctrl-C interrupts).

import (
	"errors"
	"fmt"
	"io"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrAborted means the user pressed Ctrl-C: the run ends with rc 130 and
// nothing is written. ErrCancelled means Esc or the Cancel choice of a
// screen: the project loop returns to its list, the bootstrap ends with
// nothing written.
var (
	ErrAborted   = errors.New("lo init: interrupted")
	ErrCancelled = errors.New("lo init: cancelled")
	// ErrIncomplete means a value the screen needs stayed empty at the
	// end of the input (an accessible prompt at EOF): the screen cannot
	// run. A terminal never ends the input: a required field re-asks.
	ErrIncomplete = errors.New("lo init: a value is missing")
)

// IO is where a form reads and writes.
type IO struct {
	In  io.Reader
	Out io.Writer
	// Accessible runs the fields as line prompts instead of the
	// interactive forms (off a TTY; the tests).
	Accessible bool
}

// keyMap is huh's default key map without the list filter, with Esc as
// a second quit key. Every list of `lo init` is short. The help line then
// names the keys that matter: up, down, enter, esc (and x on a
// multi-select).
func keyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	km.Select.Filter.SetEnabled(false)
	km.MultiSelect.Filter.SetEnabled(false)
	return km
}

// run applies the shared options and maps how the form ended. In
// accessible mode huh runs the line prompts itself (a signal ends the
// process). On a terminal the form runs under bubbletea through
// formModel, which records a Ctrl-C press: Ctrl-C is ErrAborted, Esc is
// ErrCancelled.
func run(form *huh.Form, tio IO) error {
	form = form.WithTheme(ui.HuhTheme()).WithKeyMap(keyMap())
	if tio.Accessible {
		form = form.WithAccessible(true)
		if tio.In != nil {
			form = form.WithInput(tio.In)
		}
		if tio.Out != nil {
			form = form.WithOutput(tio.Out)
		}
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return ErrCancelled
			}
			return err
		}
		return nil
	}
	m := &formModel{form: form}
	return m.run(tio.In, tio.Out)
}

// formModel runs a huh form under bubbletea itself, so the key that
// ended it is known. huh maps Esc and Ctrl-C to the same abort, and the
// two differ here: Esc cancels, Ctrl-C interrupts (rc 130). The wrapper
// records a Ctrl-C press, and an InterruptMsg (an external SIGINT),
// before the form sees them. (The same pattern as the form of `lo use`;
// one copy stays until both land on main.)
type formModel struct {
	form        *huh.Form
	interrupted bool
}

func (m *formModel) Init() tea.Cmd { return m.form.Init() }

func (m *formModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyPressMsg:
		if v.String() == "ctrl+c" {
			m.interrupted = true
		}
	case tea.InterruptMsg:
		m.interrupted = true
	}
	_, cmd := m.form.Update(msg)
	return m, cmd
}

func (m *formModel) View() tea.View { return tea.NewView(m.form.View()) }

// run drives the form the way huh's own Run does (submit quits, cancel
// interrupts) and maps the end: nil, ErrCancelled or ErrAborted.
func (m *formModel) run(in io.Reader, out io.Writer) error {
	m.form.SubmitCmd, m.form.CancelCmd = tea.Quit, tea.Interrupt
	var opts []tea.ProgramOption
	if in != nil {
		opts = append(opts, tea.WithInput(in))
	}
	if out != nil {
		opts = append(opts, tea.WithOutput(out))
	}
	_, err := tea.NewProgram(m, opts...).Run()
	return m.ended(err)
}

// ended maps how the program ended: an interrupt or an aborted form is
// ErrAborted after Ctrl-C or a SIGINT and ErrCancelled after Esc; any
// other error is wrapped; nil stays nil.
func (m *formModel) ended(err error) error {
	if errors.Is(err, tea.ErrInterrupted) || m.form.State == huh.StateAborted {
		if m.interrupted {
			return ErrAborted
		}
		return ErrCancelled
	}
	if err != nil {
		return fmt.Errorf("lo init: %w", err)
	}
	return nil
}
