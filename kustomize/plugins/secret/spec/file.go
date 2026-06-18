package spec

import (
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// FileEntry is the spec for a value sourced from a local file.
//
// Shorthand forms:
//
//	ca.crt: ./certs/ca.crt                # scalar string → path, mode=raw
//	tls.crt:                              # full mapping
//	  path: ./certs/tls.crt
//	  mode: passthrough                   # file content is already base64
type FileEntry struct {
	// Path is relative to the directory the plugin was invoked from
	// (typically the kustomize build root). Path traversal is rejected.
	Path string `yaml:"path"`
	// Mode controls how the file content is treated:
	//   "raw"          → read bytes verbatim, base64-encode at emit (default)
	//   "passthrough"  → read bytes as already-base64, validate and store
	Mode string `yaml:"mode,omitempty"`
}

// File modes.
const (
	FileModeRaw         = "raw"
	FileModePassthrough = "passthrough"
)

// fileEntryRaw avoids infinite recursion.
type fileEntryRaw FileEntry

// UnmarshalYAML accepts shorthand path string or full mapping.
func (f *FileEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		s := node.Value
		if s == "" {
			return kyaml.NodeErrf(node, "file shorthand must not be empty")
		}
		f.Path = s
		f.Mode = FileModeRaw
		return nil
	case yaml.MappingNode:
		var raw fileEntryRaw
		if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
			return err
		}
		*f = FileEntry(raw)
		if f.Path == "" {
			return kyaml.NodeErrf(node, "file.path is required")
		}
		if f.Mode == "" {
			f.Mode = FileModeRaw
		}
		if f.Mode != FileModeRaw && f.Mode != FileModePassthrough {
			return kyaml.NodeErrf(node, "file.mode must be %q or %q, got %q",
				FileModeRaw, FileModePassthrough, f.Mode)
		}
		return nil
	default:
		return kyaml.NodeErrf(node, "file entry must be string or mapping, got %s", nodeKindString(node.Kind))
	}
}
