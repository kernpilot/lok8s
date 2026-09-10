//go:build inprocess

package render

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/kernpilot/lok8s/internal/toolchain"
)

// The two exec generators lok8s renders depend on, at the relative paths
// kustomize resolves them under a plugin home
// (<group>/<version>/<lowercase kind>/<Kind>) — the toolchain pins are the
// one source; the same paths b installs the binaries to for lo core.
const (
	secretPluginRel        = toolchain.SecretPluginRel
	chartRendererPluginRel = toolchain.ChartRendererPluginRel
)

var (
	homeOnce sync.Once
	homeDir  string
	homeErr  error
	// priorHome remembers the caller's KUSTOMIZE_PLUGIN_HOME (value, set)
	// so Cleanup can put it back.
	priorHome    string
	priorHomeSet bool
)

// selfExecPluginHome returns the per-process plugin home: a temp directory
// holding the two plugin paths as symlinks to the running executable (a
// copy where symlinks are unavailable) and the env/ directory of the
// in-flight renders' overlay files (renderenv.go). Created once, on first
// render; Cleanup removes it.
//
// KUSTOMIZE_PLUGIN_HOME is set to the home ONCE, here, for the rest of the
// process — not per render. The kustomize API resolves the plugin root
// from that variable only (konfig.DefaultAbsPluginHome; PluginConfig has
// no field for it), and a per-render set/restore would race every other
// goroutine that reads the environment or snapshots it for a child
// (execx). The value is a per-process constant, so setting it once is
// both correct and race-free; it also means every child this process
// starts after the first render (the bash shim, a provider plugin) sees
// the self-exec home — which serves the same two generators.
//
// kustomize execs `<home>/…/Secret <cfgfile>` in the kustomization
// directory with KUSTOMIZE_PLUGIN_CONFIG_STRING in the environment — so
// the child is `lo` again, started under the plugin's name, and
// DispatchPlugin routes it to the generator (dispatch.go).
func selfExecPluginHome() (string, error) {
	homeOnce.Do(func() {
		homeDir, homeErr = makeSelfExecPluginHome()
		if homeErr == nil {
			priorHome, priorHomeSet = os.LookupEnv(kustomizePluginHomeEnv)
			os.Setenv(kustomizePluginHomeEnv, homeDir)
		}
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
	if err := os.MkdirAll(filepath.Join(dir, renderEnvDirName), 0o700); err != nil {
		_ = os.RemoveAll(dir)
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

// Cleanup removes the per-process plugin home and restores the caller's
// KUSTOMIZE_PLUGIN_HOME. main defers it so every exit path drops the temp
// dir; a second call is a no-op.
func Cleanup() {
	if homeDir != "" {
		_ = os.RemoveAll(homeDir)
		homeDir = ""
		if priorHomeSet {
			os.Setenv(kustomizePluginHomeEnv, priorHome)
		} else {
			os.Unsetenv(kustomizePluginHomeEnv)
		}
	}
}
