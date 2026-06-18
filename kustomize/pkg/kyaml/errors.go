package kyaml

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
)

// NodeErr returns an *errs.Error annotated with the node's source
// position. Used by custom UnmarshalYAML implementations that detect a
// problem after parsing a node.
func NodeErr(node *yaml.Node, msg string) error {
	if node == nil {
		return errs.New(msg)
	}
	return errs.At(node.Line, node.Column, msg)
}

// NodeErrf is the formatted variant of NodeErr.
func NodeErrf(node *yaml.Node, format string, args ...any) error {
	return NodeErr(node, fmt.Sprintf(format, args...))
}
