package kapply

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// csiRe strips CSI sequences (any terminator — Aggregate's ×N styling,
// captured colors, cursor moves) so off-tty output stays plain text (bash:
// the sed in kapply::render_captured; OSC and bare ESC are left alone —
// nothing in an apply stream realistically emits those).
var csiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// writerIsTTY reports whether w is a terminal (bash: [[ -t 1 ]], captured
// once at entry).
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// RenderCaptured renders a CAPTURED apply-output stream as the OFFLINE
// equivalent of the live collapsing block (bash: kapply::render_captured).
// The bootstrap DAG fans non-gate entries out as concurrent background jobs
// which buffer their output; the serialized foreground reap renders it here,
// one de-interleaved block per entry. Routine "…applied/…created/…condition
// met" lines collapse to a single "· N resources" count; every other line
// (errors, CRD/webhook-race retries, wait output) is surfaced and deduped
// via Aggregate. rc picks ✓/✗. Styling is tty-only: off-tty (CI, piped
// logs) the same collapsed block is emitted PLAIN — no ANSI of our own, and
// embedded CSI sequences + carriage returns in the captured lines are
// stripped.
func RenderCaptured(w io.Writer, label string, rc int, r io.Reader) {
	marker, color := "✓", "32"
	if rc != 0 {
		marker, color = "✗", "31"
	}
	isTTY := writerIsTTY(w)
	cOn, cOff, cDim := "", "", ""
	if isTTY {
		cOn, cOff, cDim = "\033["+color+"m", "\033[0m", "\033[2m"
	}

	// Count DISTINCT resources (keyed on the first token), not raw OK lines:
	// the CRD/webhook-race retry re-applies the whole manifest up to 6×, and
	// a raw line count would inflate "· N resources" by the number of passes.
	seenResource := map[string]bool{}
	var extra []string
	// bufio scanner keeps a final line that lacks a trailing newline (a
	// killed job's buffered output can end mid-write; bash: `|| [[ -n … ]]`).
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if OKRe.MatchString(line) {
			// Count ONLY when the first token has kubectl's type/name
			// resource shape. An INDENTED line ending in an OK verb makes
			// the token empty, and prose that merely ENDS in an OK verb
			// ("Warning: … configured") must surface as a message, not
			// vanish into the count.
			key, _, _ := strings.Cut(line, " ")
			if key != "" && strings.Contains(key, "/") {
				seenResource[key] = true
			} else {
				extra = append(extra, line)
			}
		} else if line != "" {
			extra = append(extra, line)
		}
	}
	n := len(seenResource)
	if n > 0 {
		noun := "resources"
		if n == 1 {
			noun = "resource"
		}
		fmt.Fprintf(w, "%s%s%s %s %s· %d %s%s\n", cOn, marker, cOff, label, cDim, n, noun, cOff)
	} else {
		fmt.Fprintf(w, "%s%s%s %s\n", cOn, marker, cOff, label)
	}
	if len(extra) == 0 {
		return
	}
	for _, line := range Aggregate(extra) {
		if !isTTY {
			// off-tty: strip CSI sequences and carriage returns so
			// CI/piped logs stay plain text.
			line = csiRe.ReplaceAllString(line, "")
			line = strings.ReplaceAll(line, "\r", "")
		}
		fmt.Fprintf(w, "      %s%s%s\n", cDim, line, cOff)
	}
}
