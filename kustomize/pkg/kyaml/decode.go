// Package kyaml wraps yaml.v3 with line-aware errors and a strict
// decoder. All YAML I/O for kustomize plugins should go through this
// package so error messages are consistent across plugins.
package kyaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// DecodeStrict reads YAML from r into out, rejecting unknown fields. Any
// decode error is wrapped in *errs.Error so callers see consistent
// formatting (line numbers, paths, etc).
func DecodeStrict(r io.Reader, out any) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return wrapDecodeErr(err)
	}
	return nil
}

// DecodeStrictBytes is the []byte form of DecodeStrict.
func DecodeStrictBytes(data []byte, out any) error {
	return DecodeStrict(bytes.NewReader(data), out)
}

// DecodeNodeStrict re-decodes a yaml.Node into a typed value with strict
// field checking. Used by custom UnmarshalYAML implementations that need
// to fall back to the default decoder for the long-form case after
// detecting a shorthand form.
//
// Important: this re-marshals the node and re-parses it. yaml.v3 doesn't
// give us a way to feed a Node directly into a strict decoder, so we
// round-trip through bytes. The line/column come from the original Node.
func DecodeNodeStrict(node *yaml.Node, out any) error {
	if node == nil {
		return errs.New("nil node")
	}
	raw, err := yaml.Marshal(node)
	if err != nil {
		return errs.Atf(node.Line, node.Column, "internal: marshal node: %v", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return wrapNodeErr(node, err)
	}
	return nil
}

// wrapDecodeErr converts a yaml.v3 decode error into our *errs.Error
// format, extracting line numbers from the message when possible.
//
// yaml.v3 returns errors in two shapes:
//
//	yaml: line 14: cannot unmarshal !!str into int
//
// or, for KnownFields(true) violations:
//
//	yaml: unmarshal errors:
//	  line 14: field UNKNOWN not found in type pkg.SomeType
//
// We extract the line number from either form and strip the noisy
// "yaml:" / "unmarshal errors" preamble.
func wrapDecodeErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	// Multi-line "unmarshal errors" form: take the first inner line.
	if strings.HasPrefix(msg, "unmarshal errors:\n") {
		lines := strings.SplitN(msg[len("unmarshal errors:\n"):], "\n", 2)
		msg = strings.TrimLeft(lines[0], " \t")
	}
	line := parseLinePrefix(&msg)
	return errs.At(line, 0, msg)
}

// parseLinePrefix detects a leading "line N: " marker in *msg, strips it,
// and returns the parsed line number. Returns 0 if no marker is present.
func parseLinePrefix(msg *string) int {
	const prefix = "line "
	if !strings.HasPrefix(*msg, prefix) {
		return 0
	}
	rest := (*msg)[len(prefix):]
	// Find the ":" that separates the number from the rest.
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(rest[:colon], "%d", &n); err != nil {
		return 0
	}
	// Strip "line N: " from msg, including the space after the colon.
	*msg = strings.TrimPrefix(rest[colon+1:], " ")
	return n
}

// wrapNodeErr converts a decode error encountered while re-decoding a
// node, using the node's own line/column.
func wrapNodeErr(node *yaml.Node, err error) error {
	if err == nil {
		return nil
	}
	var existing *errs.Error
	if errors.As(err, &existing) {
		// Already structured — preserve, but if it has no line, use the node's.
		if existing.Line == 0 {
			existing.Line = node.Line
			existing.Col = node.Column
		}
		return existing
	}
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	return errs.At(node.Line, node.Column, msg)
}
