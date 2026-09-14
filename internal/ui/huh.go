package ui

import (
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// HuhTheme is the one huh theme of the CLI: base16 (the terminal's own
// palette) with the accent on the doctor green: the title, the selector
// and the selected option. The init wizard and the `lo use` select share
// it.
func HuhTheme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeBase16(isDark)
		accent := lipgloss.Color("2")
		t.Focused.Title = t.Focused.Title.Foreground(accent)
		t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(accent)
		t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(accent)
		t.Focused.SelectedPrefix = t.Focused.SelectedPrefix.Foreground(accent)
		t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(accent)
		t.Group.Title = t.Focused.Title
		return t
	})
}
