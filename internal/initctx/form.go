package initctx

// form.go — the huh side of the screens: the house theme, the form IO
// and the one runner every form goes through. accessible selects huh's
// line-driven mode (the tests script it over an io.Reader; a terminal
// gets the interactive forms).

import (
	"errors"
	"io"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// Theme is the house form theme: huh's base theme with the accent set to
// the doctor's green (ANSI colour 2, the `✓` of `lo doctor`) for the
// selection cursor, the chosen options, the focused button and the input
// prompt; the muted text in the terminal's dim colour; and no border
// (`lo` prints no boxes). Everything else inherits from the base theme.
func Theme(isDark bool) *huh.Styles {
	t := huh.ThemeBase(isDark)
	accent := lipgloss.Color("2")
	muted := lipgloss.Color("8")
	bad := lipgloss.Color("1")
	base := lipgloss.NewStyle().PaddingLeft(2)

	t.Focused.Base = base
	t.Focused.Card = base
	t.Focused.Title = t.Focused.Title.Bold(true)
	t.Focused.NoteTitle = t.Focused.NoteTitle.Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(muted)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(bad)
	t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(bad)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(accent)
	t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(accent)
	t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(accent)
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(accent)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(accent)
	t.Focused.SelectedPrefix = t.Focused.SelectedPrefix.Foreground(accent)
	t.Focused.UnselectedPrefix = t.Focused.UnselectedPrefix.Foreground(muted)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(lipgloss.Color("0")).Background(accent)
	t.Focused.BlurredButton = t.Focused.BlurredButton.Foreground(lipgloss.Color("7")).Background(muted)
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(accent)
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(accent)
	t.Focused.TextInput.Placeholder = t.Focused.TextInput.Placeholder.Foreground(muted)

	t.Blurred = t.Focused
	t.Blurred.Title = t.Blurred.Title.Foreground(muted)
	t.Blurred.NoteTitle = t.Blurred.NoteTitle.Foreground(muted)
	t.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()

	t.Group.Title = t.Focused.Title
	t.Group.Description = t.Focused.Description
	return t
}

// ErrAborted is returned when the user leaves a form (Esc, Ctrl-C):
// the run ends, nothing was written. ErrCancelled is the Cancel choice
// of a screen: the project loop returns to its list.
var (
	ErrAborted   = errors.New("lo init: cancelled")
	ErrCancelled = errors.New("lo init: cancelled, back to the list")
	// ErrIncomplete is a required value left empty (an accessible prompt
	// at EOF): the screen cannot run.
	ErrIncomplete = errors.New("lo init: a required value is empty")
)

// IO is where a form reads and writes.
type IO struct {
	In  io.Reader
	Out io.Writer
	// Accessible runs the fields as line prompts instead of the
	// interactive forms (off a TTY; the tests).
	Accessible bool
}

// keyMap is huh's default key map without the list filter: every list
// of `lo init` is short, and the help line stays to the three keys that
// matter (up, down, enter; x toggles a multi-select).
func keyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Select.Filter.SetEnabled(false)
	km.MultiSelect.Filter.SetEnabled(false)
	return km
}

// run applies the shared options and maps huh's abort.
func run(form *huh.Form, tio IO) error {
	form = form.WithTheme(huh.ThemeFunc(Theme)).WithKeyMap(keyMap()).WithAccessible(tio.Accessible)
	if tio.In != nil {
		form = form.WithInput(tio.In)
	}
	if tio.Out != nil {
		form = form.WithOutput(tio.Out)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrAborted
		}
		return err
	}
	return nil
}
