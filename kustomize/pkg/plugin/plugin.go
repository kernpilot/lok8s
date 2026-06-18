package plugin

import "io"

// Plugin is the per-plugin contract implemented by each entry in
// plugins/<name>/plugin.go. It binds a CRD type to a registry of
// generators and a result emitter.
//
// A Plugin's lifecycle on each invocation:
//
//  1. Decode reads stdin into the plugin's CRD struct
//  2. Build returns a Registry populated with generators derived from
//     the decoded spec, plus a Context to run them against
//  3. Run executes the registry and returns the merged Entries
//  4. Emit serializes the Entries (typically via a kresource Builder)
//     and writes the result to stdout
//
// Steps 1, 2, and 4 are plugin-specific. Step 3 is shared via Registry.
type Plugin interface {
	// Decode reads the CRD spec from r. The plugin owns its own type
	// definitions and is responsible for strict parsing.
	Decode(r io.Reader) error

	// Build returns a Registry assembled from the decoded spec, plus a
	// Context to run it against. May only be called after Decode.
	Build(env func(string) (string, bool), fileRoot string) (*Registry, *Context, error)

	// Emit serializes the merged entries as the plugin's output
	// resource (e.g. a Kubernetes Secret) and writes the bytes to w.
	Emit(entries []Entry, w io.Writer) error
}
