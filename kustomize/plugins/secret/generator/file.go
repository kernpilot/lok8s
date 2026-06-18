package generator

import (
	"encoding/base64"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/fileio"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// File is the generator for the `file:` field. It reads local files
// (relative to the kustomize invocation directory) with safety checks
// and stores their contents.
//
// Two modes:
//
//   - raw         (default): file content is read verbatim and base64-
//     encoded at emit time
//   - passthrough: file content is treated as already-base64; the
//     generator validates the encoding and stores the decoded bytes
//
// Path traversal is rejected; max file size is 1 MiB by default.
type File struct {
	spec map[string]specpkg.FileEntry
}

// NewFile wraps a file map.
func NewFile(spec map[string]specpkg.FileEntry) *File { return &File{spec: spec} }

// Name returns the generator's spec field name.
func (g *File) Name() string { return "file" }

// Generate reads each file with the configured mode and emits the bytes.
func (g *File) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		data, err := fileio.SafeRead(ctx.FileRoot, entry.Path, 0)
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		if entry.Mode == specpkg.FileModePassthrough {
			// Validate that the file content is valid base64 by
			// round-tripping it. Store the decoded bytes so the
			// builder's base64 emit produces the same string the user
			// pasted.
			decoded, decErr := base64.StdEncoding.DecodeString(string(data))
			if decErr != nil {
				return nil, errs.Wrap(k, errs.Newf("file %q has mode passthrough but content is not valid base64: %v", entry.Path, decErr))
			}
			data = decoded
		}
		out = append(out, plugin.Entry{Key: k, Value: data})
	}
	return out, nil
}
