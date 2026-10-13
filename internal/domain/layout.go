package domain

// layout.go — the document layout of a spec file (bash:
// domain::spec_layout in .lok8s/utils/domain.sh).
//
// The two implementations read a spec file in different ways when it has
// more than one YAML document: the Go readers decode the first document
// (yaml.v3 counts an empty one before it), the bash readers run yq on every
// document (yq drops empty documents before the first one, keeps the ones
// after it, and prints one line per document). The same file then reads as
// two different specs. Only one layout reads the same in both: one
// document, with an optional `---` before it and an optional `...` after
// it. `lo provision`, `lo lint` and the spec.kubernetes.version reader
// refuse every other layout with the line of the marker that breaks it.

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"strings"
)

// layoutMarkerRe is a document marker line: `---` (start) or `...` (end),
// alone or followed by a blank and more text (`--- # a comment`).
var layoutMarkerRe = regexp.MustCompile(`^(---|\.\.\.)([ \t](.*))?$`)

// SpecLayout checks that a spec file holds one YAML document: no content
// before a `---` line, no second `---` line, no content after a `...` line.
// Blank lines, comments and `%` directives are not content. It returns
// ok=false with the line number (1-based) and the marker ("---" or "...")
// that breaks the layout. A file that cannot be read is ok: the readers
// report that themselves.
func SpecLayout(specPath string) (line int, marker string, ok bool) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return 0, "", true
	}
	content, started, endLine := false, false, 0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), len(raw)+1)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSuffix(sc.Text(), "\r")
		if m := layoutMarkerRe.FindStringSubmatch(text); m != nil {
			if m[1] == "..." {
				if endLine == 0 {
					endLine = n
				}
				continue
			}
			if content || started || endLine != 0 {
				return n, "---", false
			}
			started = true
			if rest := strings.TrimLeft(m[3], " \t"); rest != "" && !strings.HasPrefix(rest, "#") {
				content = true
			}
			continue
		}
		rest := strings.TrimLeft(text, " \t")
		if rest == "" || strings.HasPrefix(rest, "#") || strings.HasPrefix(text, "%") {
			continue
		}
		if endLine != 0 {
			return endLine, "...", false
		}
		content = true
	}
	return 0, "", true
}
