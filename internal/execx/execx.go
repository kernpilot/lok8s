// Package execx locates and runs the external tools lo still shells out to.
// Lookup prefers the project's b-managed toolchain (.bin) over PATH, so lo
// works without the caller having prepared any environment.
package execx

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kernpilot/lok8s/internal/config"
)

// Look resolves tool to an executable path: $PATH_BIN/<tool> when present and
// executable, else the first PATH hit. ok is false when the tool is nowhere.
func Look(p *config.Paths, tool string) (string, bool) {
	candidate := filepath.Join(p.Bin, tool)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return candidate, true
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		return "", false
	}
	return path, true
}
