package ui

import "testing"

// Paint wraps a run in one SGR style and ends it with SGR 0; off (not a
// terminal) the text is unchanged; an empty run stays empty. The card
// paints its marker, key and value as separate runs, so a marker never
// sits inside another run.
func TestPaint(t *testing.T) {
	on, off := Paint(true), Paint(false)
	for _, c := range []struct{ got, want string }{
		{on.Bold("acme"), "\033[1macme\033[0m"},
		{on.Dim("clusters"), "\033[2mclusters\033[0m"},
		{on.Yellow("!"), "\033[33m!\033[0m"},
		{on.Yellow(""), ""},
		{off.Bold("acme"), "acme"},
		{off.Dim("clusters"), "clusters"},
		{off.Yellow("!"), "!"},
	} {
		if c.got != c.want {
			t.Errorf("paint: %q, want %q", c.got, c.want)
		}
	}
	// Two runs side by side: each ends before the next starts.
	if row := on.Yellow("!") + " " + on.Dim("toolchain"); row != "\033[33m!\033[0m \033[2mtoolchain\033[0m" {
		t.Errorf("row: %q", row)
	}
}
