//go:build inprocess

package render

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// The two exec generators lok8s renders depend on, at the relative paths
// kustomize resolves them under a plugin home:
// <group>/<version>/<lowercase kind>/<Kind>.
const (
	secretPluginRel        = "secrets.lok8s.dev/v1/secret/Secret"
	chartRendererPluginRel = "khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer"
)

var (
	homeOnce sync.Once
	homeDir  string
	homeErr  error
)

// selfExecPluginHome returns the per-process plugin home: a temp directory
// holding the two plugin paths as symlinks to the running executable (a
// copy where symlinks are unavailable). Created once, on first render;
// Cleanup removes it.
//
// The home is what KUSTOMIZE_PLUGIN_HOME is set to for the duration of an
// in-process run. kustomize execs `<home>/…/Secret <cfgfile>` in the
// kustomization directory with KUSTOMIZE_PLUGIN_CONFIG_STRING in the
// environment — so the child is `lo` again, started under the plugin's
// name, and DispatchPlugin routes it to the generator (dispatch.go).
func selfExecPluginHome() (string, error) {
	homeOnce.Do(func() {
		homeDir, homeErr = makeSelfExecPluginHome()
	})
	return homeDir, homeErr
}

func makeSelfExecPluginHome() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("render: locate own executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir, err := os.MkdirTemp("", "lo-plugins-")
	if err != nil {
		return "", fmt.Errorf("render: plugin home: %w", err)
	}
	for _, rel := range []string{secretPluginRel, chartRendererPluginRel} {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("render: plugin home: %w", err)
		}
		if err := os.Symlink(exe, target); err != nil {
			// No symlinks (or a filesystem that refuses them): a copy of the
			// binary behaves identically, argv[0] is what is dispatched on.
			if err := copyExecutable(exe, target); err != nil {
				_ = os.RemoveAll(dir)
				return "", fmt.Errorf("render: plugin home: %w", err)
			}
		}
	}
	return dir, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Cleanup removes the per-process plugin home. main defers it so every
// exit path drops the temp dir; a second call is a no-op.
func Cleanup() {
	if homeDir != "" {
		_ = os.RemoveAll(homeDir)
		homeDir = ""
	}
}
