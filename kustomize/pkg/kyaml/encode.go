package kyaml

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Encode marshals v to YAML with deterministic key ordering and 2-space
// indentation. Used by plugins to emit Kubernetes resources.
//
// yaml.v3 sorts struct fields by declaration order and map fields
// alphabetically by default — both are deterministic. We just wrap that
// behavior with consistent indentation and a trailing newline.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("yaml encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yaml encode close: %w", err)
	}
	return buf.Bytes(), nil
}
