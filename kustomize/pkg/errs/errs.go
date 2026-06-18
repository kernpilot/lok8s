// Package errs provides user-facing error formatting for kustomize plugins.
//
// All errors emitted to stderr should go through this package so they
// share a consistent shape: a field path, an optional source line, and a
// short human-readable message. This makes plugin failures debuggable
// without inventing new error formats per generator.
//
// Three error types:
//
//   - Error: a single failure with optional path + line context
//   - LineError: an Error with line/column from a YAML node
//   - Multi: aggregates multiple errors and reports them in a stable order
package errs

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Error is the canonical user-facing error type. It carries an optional
// field path (e.g. "passwd.NUXT_SESSION_PASSWORD") and an optional source
// line/column for YAML-rooted errors.
type Error struct {
	Path string // field path, e.g. "passwd.NUXT_SESSION_PASSWORD"
	Line int    // 1-based source line; 0 if not from YAML
	Col  int    // 1-based source column; 0 if unknown
	Msg  string // short human-readable message
	Err  error  // wrapped underlying error (optional)
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	if e.Line > 0 {
		if e.Col > 0 {
			fmt.Fprintf(&b, "line %d:%d: ", e.Line, e.Col)
		} else {
			fmt.Fprintf(&b, "line %d: ", e.Line)
		}
	}
	if e.Path != "" {
		fmt.Fprintf(&b, "%s: ", e.Path)
	}
	b.WriteString(e.Msg)
	if e.Err != nil && e.Err.Error() != e.Msg {
		fmt.Fprintf(&b, ": %s", e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the wrapped error so errors.Is/As traversal works.
func (e *Error) Unwrap() error { return e.Err }

// New returns a new Error with the given message.
func New(msg string) *Error {
	return &Error{Msg: msg}
}

// Newf returns a new Error with a formatted message.
func Newf(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// At returns a new Error annotated with a YAML source line and column.
// Pass col=0 if only the line is known.
func At(line, col int, msg string) *Error {
	return &Error{Line: line, Col: col, Msg: msg}
}

// Atf is the formatted variant of At.
func Atf(line, col int, format string, args ...any) *Error {
	return &Error{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

// Wrap wraps an existing error in our format. If err is already an *Error,
// the new path is prepended (so wrapping during recursive parsing builds
// "outer.inner.field" naturally). If err is nil, returns nil.
func Wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		// Prepend path if both have one.
		if path != "" && e.Path != "" {
			e.Path = path + "." + e.Path
		} else if path != "" {
			e.Path = path
		}
		return e
	}
	return &Error{Path: path, Msg: err.Error(), Err: err}
}

// WithPath returns a copy of the error with the given path. If the input
// is not an *Error, it is wrapped first.
func WithPath(err error, path string) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		copyE := *e
		copyE.Path = path
		return &copyE
	}
	return &Error{Path: path, Msg: err.Error(), Err: err}
}

// Multi aggregates multiple errors. Used when a generator wants to report
// every problem instead of failing on the first one.
type Multi struct {
	Errs []error
}

// Add appends err to the multi-error. Nil errors are ignored.
func (m *Multi) Add(err error) {
	if err != nil {
		m.Errs = append(m.Errs, err)
	}
}

// Err returns m as an error if it has any entries, otherwise nil.
// Useful as a return-value finalizer:
//
//	var m errs.Multi
//	for _, item := range items { m.Add(process(item)) }
//	return m.Err()
func (m *Multi) Err() error {
	if m == nil || len(m.Errs) == 0 {
		return nil
	}
	return m
}

// Error implements the error interface for Multi. Errors are joined with
// newlines in a stable, sorted order so test output is deterministic.
func (m *Multi) Error() string {
	if len(m.Errs) == 0 {
		return ""
	}
	if len(m.Errs) == 1 {
		return m.Errs[0].Error()
	}
	lines := make([]string, len(m.Errs))
	for i, e := range m.Errs {
		lines[i] = e.Error()
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
