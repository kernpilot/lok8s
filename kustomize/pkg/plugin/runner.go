package plugin

import (
	"fmt"
	"io"
	"os"
)

// Run is the canonical kustomize exec plugin entrypoint. Each
// cmd/<name>/main.go calls this with its own plugin.Plugin
// implementation:
//
//	func main() {
//	    plugin.Run(os.Args, os.Stdin, os.Stdout, secret.New())
//	}
//
// Kustomize exec generator plugins receive their CRD spec as a file
// path in argv[1] (the spec is NOT on stdin — that's the convention
// for transformer plugins). Run handles this contract:
//
//   - If argv has a config-file argument (len(argv) >= 2), open it
//   - Otherwise, fall back to stdin (helpful for tests + manual use)
//
// The fileRoot is the working directory at invocation time, used by
// the file generator to resolve relative paths in the spec.
func Run(argv []string, stdin io.Reader, stdout io.Writer, p Plugin) error {
	src, closer, err := openSpec(argv, stdin)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	if err := p.Decode(src); err != nil {
		return err
	}
	fileRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	registry, ctx, err := p.Build(DefaultEnv, fileRoot)
	if err != nil {
		return err
	}
	entries, err := registry.Run(ctx)
	if err != nil {
		return err
	}
	return p.Emit(entries, stdout)
}

// openSpec selects the spec source: argv[1] file if present, else stdin.
// Returns the reader, an optional cleanup func (nil if reader is stdin),
// and any error.
func openSpec(argv []string, stdin io.Reader) (io.Reader, func(), error) {
	if len(argv) >= 2 && argv[1] != "" {
		f, err := os.Open(argv[1])
		if err != nil {
			return nil, nil, fmt.Errorf("open spec %q: %w", argv[1], err)
		}
		return f, func() { _ = f.Close() }, nil
	}
	return stdin, nil, nil
}

// Fail prints err to stderr and exits with code 1. Use as the
// terminating action of a plugin's main function.
func Fail(err error) {
	fmt.Fprintln(os.Stderr, "secret plugin:", err)
	os.Exit(1)
}
