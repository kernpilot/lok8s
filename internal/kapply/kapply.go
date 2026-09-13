// Package kapply is the Go port of the kubectl-apply progress layer
// (.lok8s/utils/kapply.sh). This first slice ports kapply::run — the named,
// collapsing progress block the Lo driver wraps its registry/coredns phases
// in; the apply/heal/preflight/wait_ready half ports together with the
// bootstrap engine, which is their real consumer.
//
// Display contract (bash parity):
//   - OFF a terminal (CI/Tilt logs, LOK8S_NONINTERACTIVE, CI, DEBUG set):
//     pure passthrough — the phase function's output goes straight to the
//     caller's writers, untouched.
//   - ON a terminal: the phase's kubectl-style "<resource> <verb>" lines are
//     collapsed into one "✓ <phase> · N resources" summary; on failure the
//     non-progress lines (errors) are surfaced on stderr, deduplicated with
//     an (×N) count; a run that produced no progress lines at all prints its
//     raw output unmodified.
//
// DOCUMENTED SIMPLIFICATION vs bash: the bash tty path streams a live
// spinner + a 3-line scrolling window to /dev/tty while the phase runs; this
// port renders the identical FINAL state (the summary line / surfaced
// errors) after the phase completes, without the in-flight animation. The
// off-tty behavior — the one CI and tests observe — is byte-identical.
package kapply

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// OKRe matches kubectl success verbs — lines ending in one of these are
// "progress" (counted + collapsed); anything else (errors) is surfaced on
// failure (bash: _KAPPLY_OK). SHARED with the future bootstrap scheduler's
// reap-park detection; keep the verb list in this one place.
var OKRe = regexp.MustCompile(` (serverside-applied|created|configured|unchanged|applied|deleted|annotated|labeled|patched|restarted|scaled|rolled back|condition met)$`)

// ttyUI reports whether the collapsed one-line UI may be drawn (bash:
// kapply::_tty). KAPPLY_TTY forces the UI (tests); DEBUG (verbose lo -v)
// prints everything; LOK8S_NONINTERACTIVE/CI never draw.
func ttyUI() bool {
	if os.Getenv("KAPPLY_TTY") != "" {
		return true
	}
	if os.Getenv("DEBUG") != "" {
		return false
	}
	if os.Getenv("LOK8S_NONINTERACTIVE") != "" || os.Getenv("CI") != "" {
		return false
	}
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Aggregate collapses repeated identical lines into a single line with a
// "(×N)" count, preserving first-seen order (bash: kapply::_aggregate).
// Distinct lines (different objects) are kept separate.
func Aggregate(lines []string) []string {
	counts := map[string]int{}
	var order []string
	for _, l := range lines {
		if counts[l] == 0 {
			order = append(order, l)
		}
		counts[l]++
	}
	out := make([]string, 0, len(order))
	for _, l := range order {
		if counts[l] > 1 {
			out = append(out, fmt.Sprintf("%s  \033[2m(×%d)\033[0m", l, counts[l]))
		} else {
			out = append(out, l)
		}
	}
	return out
}

// Run executes fn as one named progress phase (bash: kapply::run). fn
// receives the writers its progress lines and errors should go to; off-tty
// those are the caller's own stdout/stderr (passthrough), on a tty they are
// a capture buffer that is rendered per the display contract above. Returns
// fn's error unchanged.
func Run(phase string, stdout, stderr io.Writer, fn func(out, errOut io.Writer) error) error {
	if !ttyUI() {
		return fn(stdout, stderr)
	}

	// tty: capture the phase's combined output (bash: `"${@}" 2>&1 | tee`).
	var buf strings.Builder
	err := fn(&buf, &buf)

	lines := splitLines(buf.String())
	var okCount int
	var rest []string
	for _, l := range lines {
		if OKRe.MatchString(l) {
			okCount++
		} else if l != "" {
			rest = append(rest, l)
		}
	}

	if okCount == 0 {
		// No progress lines (warnings/notes) — show as-is.
		if buf.Len() > 0 {
			fmt.Fprint(stdout, buf.String())
		}
		return err
	}

	noun := "resources"
	if okCount == 1 {
		noun = "resource"
	}
	fmt.Fprintf(stdout, "\033[32m✓\033[0m %s · %d %s\n", phase, okCount, noun)
	if err != nil {
		// Surface errors (deduped) on failure.
		for _, l := range Aggregate(rest) {
			fmt.Fprintln(stderr, l)
		}
	}
	return err
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
