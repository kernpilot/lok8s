package ui

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Columns is a table in progress: the header and the measured widths, so
// a caller can interleave its own lines between rows (the assets diff
// prints the per-file rows under each unit). Table is the one-call form.
type Columns struct {
	w      io.Writer
	style  Style
	header []string
	widths []int
}

// NewColumns measures the columns of header over rows. minWidths are the
// minimum column widths (nil, or 0 for a column: none). Piped, a column
// with a minimum is exactly that wide, and a cell that is wider pushes
// only its own row (bash printf `%-20s` semantics), so the bytes match
// the fixed layouts of the ported commands. On a TTY every column is
// measured (never narrower than its minimum), so nothing overflows.
func NewColumns(w io.Writer, header []string, rows [][]string, minWidths []int) *Columns {
	s := For(w)
	widths := make([]int, len(header))
	for i := range widths {
		if i < len(minWidths) {
			widths[i] = minWidths[i]
		}
		if s.TTY || widths[i] == 0 {
			widths[i] = max(widths[i], utf8.RuneCountInString(header[i]))
			for _, r := range rows {
				if i < len(r) {
					widths[i] = max(widths[i], utf8.RuneCountInString(r[i]))
				}
			}
		}
	}
	return &Columns{w: w, style: s, header: header, widths: widths}
}

// Header writes the header row and its underline (one dash per header
// rune, the shape the addons and assets tables print today). On a TTY the
// header is bold and the underline dim.
func (c *Columns) Header() {
	underline := make([]string, len(c.header))
	for i, h := range c.header {
		underline[i] = strings.Repeat("-", utf8.RuneCountInString(h))
	}
	io.WriteString(c.w, c.style.Bold(tableRow(c.header, c.widths))+"\n")
	io.WriteString(c.w, c.style.Dim(tableRow(underline, c.widths))+"\n")
}

// Row writes one row. A missing cell is empty.
func (c *Columns) Row(cells ...string) {
	io.WriteString(c.w, tableRow(cells, c.widths)+"\n")
}

// Table writes header, its underline and rows, columns two spaces apart
// (NewColumns for the width rules).
func Table(w io.Writer, header []string, rows [][]string, minWidths []int) {
	c := NewColumns(w, header, rows, minWidths)
	c.Header()
	for _, r := range rows {
		c.Row(r...)
	}
}

// tableRow pads every cell but the last to its column width (rune count,
// like fmt's %-Ns) and joins them with two spaces.
func tableRow(cells []string, widths []int) string {
	var b strings.Builder
	last := len(widths) - 1
	for i := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(cell)
		if i < last {
			if pad := widths[i] - utf8.RuneCountInString(cell); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
	}
	return b.String()
}
