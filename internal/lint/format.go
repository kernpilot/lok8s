package lint

// format.go: `lo lint --format text|editor|github` (Go-only). The checks
// print their findings as `[error] …`, `[warn] …` and `[note] …` lines
// through the ui helpers. Text is that output unchanged. editor and github
// rewrite each finding line as it is written:
//
//	editor  <file>:<line>: [error] <message>
//	github  ::error file=<file>,line=<line>::<message>
//
// The file is the path the finding names (a clusters/… or .lok8s/… path in
// the message), else the domain's spec file. The line is the one the
// message names (`line N`), else 1: the checks work per file, not per
// line. Any other line passes through unchanged.

import (
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Formats.
const (
	FormatText   = "text"
	FormatEditor = "editor"
	FormatGitHub = "github"
)

var (
	findingRe  = regexp.MustCompile(`^\s*\x1b\[[0-9;]*m\[(error|warn|note)\]\x1b\[0m\s*(.*)$|^\s*\[(error|warn|note)\]\s*(.*)$`)
	pathRe     = regexp.MustCompile(`(?:clusters|\.lok8s|\.targets|\.secrets|services|targets)/[^\s:'"()]+|[^\s:'"()]+\.(?:ya?ml|enc|json|sops\.yaml)\b`)
	lineNumRe  = regexp.MustCompile(`\bline (\d+)\b`)
	ansiPrefix = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// FormatWriter rewrites finding lines for one format and passes every
// other line through. Fallback is the file a finding without a path is
// charged to.
type FormatWriter struct {
	W        io.Writer
	Format   string
	Fallback string
	buf      strings.Builder
}

// NewFormatWriter wraps w. For FormatText it returns w itself.
func NewFormatWriter(w io.Writer, format, fallback string) io.Writer {
	if format == FormatText || format == "" {
		return w
	}
	return &FormatWriter{W: w, Format: format, Fallback: fallback}
}

func (f *FormatWriter) Write(p []byte) (int, error) {
	f.buf.Write(p)
	for {
		line, rest, found := strings.Cut(f.buf.String(), "\n")
		if !found {
			return len(p), nil
		}
		if _, err := io.WriteString(f.W, f.line(line)+"\n"); err != nil {
			return 0, err
		}
		f.buf.Reset()
		f.buf.WriteString(rest)
	}
}

// Flush writes a trailing partial line, if any.
func (f *FormatWriter) Flush() error {
	if f.buf.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(f.W, f.line(f.buf.String())+"\n")
	f.buf.Reset()
	return err
}

// line rewrites one finding line, or returns it unchanged.
func (f *FormatWriter) line(raw string) string {
	m := findingRe.FindStringSubmatch(raw)
	if m == nil {
		return raw
	}
	level, msg := m[1], m[2]
	if level == "" {
		level, msg = m[3], m[4]
	}
	msg = strings.TrimSpace(ansiPrefix.ReplaceAllString(msg, ""))
	file := f.Fallback
	if p := pathRe.FindString(msg); p != "" {
		file = strings.TrimRight(p, ".,;")
	}
	line := 1
	if n := lineNumRe.FindStringSubmatch(msg); n != nil {
		line, _ = strconv.Atoi(n[1])
	}
	switch f.Format {
	case FormatGitHub:
		// The workflow-command escaping rules: a message escapes % \r \n, a
		// property value also : and , so a key or a path in a finding can
		// never end the command or start another one.
		return "::" + githubLevel(level) + " file=" + githubProperty(file) + ",line=" + strconv.Itoa(line) + "::" + githubData(msg)
	default:
		return file + ":" + strconv.Itoa(line) + ": [" + level + "] " + msg
	}
}

// githubData escapes a workflow-command message.
func githubData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// githubProperty escapes a workflow-command property value.
func githubProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

// githubLevel maps the finding level onto a workflow command.
func githubLevel(level string) string {
	switch level {
	case "error":
		return "error"
	case "warn":
		return "warning"
	default:
		return "notice"
	}
}
