// Package version exposes the build-time version string.
package version

// Version is set via -ldflags by the Makefile. Defaults to "dev" for
// `go build` invocations without ldflags.
var Version = "dev"
