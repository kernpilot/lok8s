package assets

import (
	"io/fs"
	"strings"
)

// BuildVersion is the release version the Makefile and goreleaser stamp
// with -ldflags -X. Empty or "dev" means "not stamped".
var BuildVersion string

// Version is the lok8s version the binary reports: the stamped build
// version, else the embedded VERSION file (a plain `go build` in the repo),
// else "dev". Nothing reads .lok8s/VERSION from disk any more — the version
// is a property of the binary, not of the project tree it runs in.
func Version() string {
	if BuildVersion != "" && BuildVersion != "dev" {
		return BuildVersion
	}
	if raw, err := fs.ReadFile(FS(), "VERSION"); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v
		}
	}
	return "dev"
}
