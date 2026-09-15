// Command gen-cli-docs writes docs/reference/cli-commands.md from the cobra
// command tree (internal/clidoc). Run it from the repository root:
//
//	go run ./hack/gen-cli-docs        # or: make docs-cli
//
// `go test ./internal/clidoc/` fails while the committed page is stale.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/cli"
	"github.com/kernpilot/lok8s/internal/clidoc"
	"github.com/kernpilot/lok8s/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-cli-docs: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// An empty project as the base: the tree must not depend on the
	// checkout (no routing, no local assets), so the page is the same on
	// every machine.
	base, err := os.MkdirTemp("", "lo-clidoc-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(base) }()
	page, err := clidoc.Generate(cli.NewRoot(&config.Paths{Base: base}))
	if err != nil {
		return err
	}
	out := filepath.Join("docs", "reference", "cli-commands.md")
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.WriteFile(out, page, 0o644); err != nil { // #nosec G306 -- a committed docs page, not a secret
		return err
	}
	fmt.Printf("wrote %s\n", out)
	return nil
}
