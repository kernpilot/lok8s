// Package config resolves the lok8s project layout and settings.
//
// This is THE single resolution point for the paths the bash implementation
// derived via `: "${PATH_BASE:=...}"` chains. Precedence for every path:
// explicit env var > derivation from the project base. The base itself is
// env PATH_BASE > nearest ancestor of the working directory that carries a
// project marker (`clusters/`, `lok8s.yaml`, or — during the coexistence
// with the frozen tree — `.lok8s/lo`) > the working directory.
package config

import (
	"os"
	"path/filepath"
)

// Paths is the resolved on-disk layout of a lok8s project.
type Paths struct {
	// Base is the project root (bash: PATH_BASE).
	Base string
	// Bin is the b-managed toolchain directory (bash: PATH_BIN).
	Bin string
	// Lok8s is the framework tree shipped into the project (bash: PATH_LOK8S).
	Lok8s string
	// Clusters holds one directory per domain (bash: PATH_CLUSTERS).
	Clusters string

	// SecretsEnv is the raw PATH_SECRETS value, and SecretsEnvSet whether the
	// user set it. The flat-store default (`$PATH_BASE/.secrets`) is NOT
	// applied here: the per-domain store under clusters/<domain>/secrets is
	// authoritative and auto-detected at build time. Materializing the
	// deprecated default would masquerade as user intent and override that
	// detection — the exact failure mode that re-keyed a live cluster.
	SecretsEnv    string
	SecretsEnvSet bool
}

// ResolvePaths resolves the project layout for the current process.
// It never fails: outside a lok8s project the paths point below the working
// directory and commands that need the project report that themselves.
func ResolvePaths() (*Paths, error) {
	base := os.Getenv("PATH_BASE")
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		base = findBase(cwd)
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}

	p := &Paths{
		Base:     base,
		Bin:      envOr("PATH_BIN", filepath.Join(base, ".bin")),
		Lok8s:    envOr("PATH_LOK8S", filepath.Join(base, ".lok8s")),
		Clusters: envOr("PATH_CLUSTERS", filepath.Join(base, "clusters")),
	}
	p.SecretsEnv, p.SecretsEnvSet = os.LookupEnv("PATH_SECRETS")
	return p, nil
}

// findBase walks up from dir looking for a directory that carries a
// project marker. Falls back to dir itself.
func findBase(dir string) string {
	for d := dir; ; {
		if isProjectRoot(d) {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// isProjectRoot recognizes a lok8s project by `clusters/` (a directory) or
// `lok8s.yaml` (the project file `lo init project` writes) — a project no
// longer needs a synced .lok8s tree since the eject model. `.lok8s/lo`
// stays a marker for the coexistence period (a project that vendors the
// frozen tree but keeps its clusters elsewhere via PATH_CLUSTERS).
func isProjectRoot(d string) bool {
	if info, err := os.Stat(filepath.Join(d, "clusters")); err == nil && info.IsDir() {
		return true
	}
	if info, err := os.Stat(filepath.Join(d, "lok8s.yaml")); err == nil && !info.IsDir() {
		return true
	}
	_, err := os.Stat(filepath.Join(d, ".lok8s", "lo"))
	return err == nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
