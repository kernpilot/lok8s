package cli

import "github.com/kernpilot/lok8s/internal/assets"

// version is stamped at build time via -ldflags (see Makefile). The
// stamped value is handed to internal/assets at init so every package
// reports the same version (assets.Version: stamped, else the embedded
// VERSION file, else "dev" — nothing reads .lok8s/VERSION from disk).
var version = "dev"

func init() { assets.BuildVersion = version }
