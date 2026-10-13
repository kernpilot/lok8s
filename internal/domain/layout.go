package domain

// layout.go — the document layout of a spec file (bash: domain::spec_layout
// and domain::spec_check in .lok8s/utils/domain.sh).
//
// The two implementations read a spec file in different ways when it has
// more than one YAML document: the Go readers decode the first document
// (yaml.v3 counts an empty one before it), the bash readers run yq on every
// document (yq drops empty documents before the first one, keeps the ones
// after it, and prints one line per document). The same file then reads as
// two different specs. Only one layout reads the same in both: one
// document, with an optional `---` before it and an optional `...` after
// it. Every command that reads a spec refuses every other layout: through
// CheckSpecLayout where it resolves the spec file, and through SpecDriver.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/ui"
)

// ErrSpecLayout is a spec file with more than one YAML document. It wraps
// ErrMalformedDriver: SpecDriver never defaults such a file to a driver.
var ErrSpecLayout = fmt.Errorf("%w: the spec file holds more than one YAML document", ErrMalformedDriver)

// layoutMarkerRe is a document marker line: `---` (start) or `...` (end),
// alone or followed by a blank and more text (`--- # a comment`).
var layoutMarkerRe = regexp.MustCompile(`^(---|\.\.\.)([ \t](.*))?$`)

// SpecLayout checks that a spec file holds one YAML document: one `---`
// line at most before the content, no `---` line after it, no content
// after a `...` line. Blank lines, comments and `%` directives are not
// content. For another layout it returns ok=false and the problem: a text
// that continues the file name ("<file> <problem>") and says which lines
// to remove. A file that cannot be read is ok: the readers report that
// themselves.
func SpecLayout(specPath string) (problem string, ok bool) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return "", true
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), len(raw)+1)
	for sc.Scan() {
		lines = append(lines, strings.TrimSuffix(sc.Text(), "\r"))
	}
	isBlank := func(text string) bool {
		rest := strings.TrimLeft(text, " \t")
		return rest == "" || strings.HasPrefix(rest, "#") || strings.HasPrefix(text, "%")
	}
	last := 0 // the last line that is not blank
	for i, text := range lines {
		if !isBlank(text) {
			last = i + 1
		}
	}
	content, started, endLine := false, false, 0
	var extra []int // `---` lines that start an empty document before the content
	for i, text := range lines {
		n := i + 1
		if m := layoutMarkerRe.FindStringSubmatch(text); m != nil {
			if m[1] == "..." {
				if endLine == 0 {
					endLine = n
				}
				continue
			}
			switch {
			case content || endLine != 0:
				return layoutAfter(n, last), false
			case started:
				extra = append(extra, n)
			default:
				started = true
			}
			if rest := strings.TrimLeft(m[3], " \t"); rest != "" && !strings.HasPrefix(rest, "#") {
				if len(extra) > 0 {
					return layoutLeading(extra), false
				}
				content = true
			}
			continue
		}
		if isBlank(text) {
			continue
		}
		if endLine != 0 {
			return layoutEnd(endLine, n, last), false
		}
		if len(extra) > 0 {
			return layoutLeading(extra), false
		}
		content = true
	}
	if len(extra) > 0 {
		return layoutLeading(extra), false
	}
	return "", true
}

// layoutLeading: `---` lines that start empty documents before the spec.
func layoutLeading(extra []int) string {
	if len(extra) == 1 {
		return fmt.Sprintf("holds an empty YAML document before the spec. Remove line %d (a '---' line).", extra[0])
	}
	return fmt.Sprintf("holds empty YAML documents before the spec. Remove lines %s (the '---' lines).", joinLines(extra))
}

// layoutAfter: a `---` line after the spec, at line n.
func layoutAfter(n, last int) string {
	if last <= n {
		return fmt.Sprintf("holds more than one YAML document. Remove line %d (a '---' line after the spec).", n)
	}
	return fmt.Sprintf("holds more than one YAML document. Remove lines %d to %d (the '---' at line %d and everything after it).", n, last, n)
}

// layoutEnd: content at line n after the `...` at line end.
func layoutEnd(end, n, last int) string {
	if last <= n {
		return fmt.Sprintf("holds text after the document end marker '...' at line %d. Remove line %d.", end, n)
	}
	return fmt.Sprintf("holds text after the document end marker '...' at line %d. Remove lines %d to %d.", end, n, last)
}

// joinLines is "2 and 3" or "2, 3 and 5".
func joinLines(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// CheckSpecLayout prints "[error] <file> <problem>" for a spec file whose
// layout SpecLayout refuses and returns ErrSpecLayout (handled). Every
// command that resolves a spec file calls it before it reads the file.
func CheckSpecLayout(specPath string, stderr io.Writer) error {
	problem, ok := SpecLayout(specPath)
	if ok {
		return nil
	}
	ui.ErrorTo(stderr, "%s %s", specPath, problem)
	return ui.Handled(ErrSpecLayout)
}

// IsSpecLayout reports a SpecDriver or CheckSpecLayout error for a spec
// file with more than one YAML document.
func IsSpecLayout(err error) bool {
	return errors.Is(err, ErrSpecLayout)
}
