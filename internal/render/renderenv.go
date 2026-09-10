//go:build inprocess

package render

// renderenv.go — the per-render environment file that carries
// Options.Env from an in-process render to the plugin child it starts
// (through kustomize). Written by buildInProcess under the self-exec plugin
// home, read by DispatchPlugin in the child. The process environment is
// never touched, so goroutines that read it or start children while a
// render is in flight (the bootstrap DAG) see nothing of the overlay.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// renderEnvDirName is the directory under the plugin home that holds one
// file per in-flight render.
const renderEnvDirName = "env"

// renderEnvPath names the overlay file of a render of dir: a hash of the
// cleaned directory, so two renders of the same directory share it (they
// are serialized, see lockDir) and renders of different directories never
// collide.
func renderEnvPath(home, dir string) string {
	sum := sha256.Sum256([]byte(cleanDir(dir)))
	return filepath.Join(home, renderEnvDirName, hex.EncodeToString(sum[:8])+".env")
}

// cleanDir is the directory identity the child matches against: absolute,
// cleaned, symlinks resolved when they can be.
func cleanDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir)
}

// writeRenderEnv writes the overlay of a render of dir. The first line is
// the directory, every following line one KEY=VALUE entry; each line is
// Go-quoted so a value with a newline survives. The returned func removes
// the file.
func writeRenderEnv(home, dir string, overlay []string) (remove func(), err error) {
	path := renderEnvPath(home, dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(strconv.Quote(cleanDir(dir)))
	b.WriteByte('\n')
	for _, kv := range overlay {
		b.WriteString(strconv.Quote(kv))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// renderEnv is one in-flight render's overlay, as the child reads it.
type renderEnv struct {
	dir     string
	overlay []string
}

func readRenderEnv(path string) (renderEnv, error) {
	f, err := os.Open(path)
	if err != nil {
		return renderEnv{}, err
	}
	defer func() { _ = f.Close() }()
	var re renderEnv
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line, err := strconv.Unquote(sc.Text())
		if err != nil {
			return renderEnv{}, err
		}
		if re.dir == "" {
			re.dir = line
			continue
		}
		re.overlay = append(re.overlay, line)
	}
	if err := sc.Err(); err != nil {
		return renderEnv{}, err
	}
	if re.dir == "" {
		return renderEnv{}, errors.New("render env file without a directory line")
	}
	return re, nil
}

// pluginRenderEnv finds the overlay of the render that started this plugin
// child. kustomize runs the plugin in the kustomization directory that
// declared the generator (KUSTOMIZE_PLUGIN_CONFIG_ROOT, the cwd), which is
// the render directory or one of its bases, so the in-flight render whose
// directory is the LONGEST prefix of that root is the one. A generator
// loaded from outside every in-flight render directory (LoadRestrictionsNone
// and a base above the root) matches none; it then takes the overlay of
// the only in-flight render, or none at all when several run at once.
func pluginRenderEnv(home, configRoot string) []string {
	entries, err := os.ReadDir(filepath.Join(home, renderEnvDirName))
	if err != nil {
		return nil
	}
	root := cleanDir(configRoot)
	var best *renderEnv
	var all []renderEnv
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		re, err := readRenderEnv(filepath.Join(home, renderEnvDirName, e.Name()))
		if err != nil {
			continue
		}
		all = append(all, re)
		if root == re.dir || strings.HasPrefix(root, re.dir+string(filepath.Separator)) {
			if best == nil || len(re.dir) > len(best.dir) {
				re := re
				best = &re
			}
		}
	}
	if best != nil {
		return best.overlay
	}
	if len(all) == 1 {
		return all[0].overlay
	}
	return nil
}

// pluginHomeOf recovers the plugin home from the plugin's own argv[0]
// (<home>/<group>/<version>/<kind>/<Kind>).
func pluginHomeOf(argv0, rel string) string {
	argv0 = filepath.Clean(argv0)
	suffix := string(filepath.Separator) + filepath.FromSlash(rel)
	if !strings.HasSuffix(argv0, suffix) {
		return ""
	}
	return strings.TrimSuffix(argv0, suffix)
}

// applyPluginEnv installs the overlay of the render that started this
// plugin child in the child's own environment, before the generator runs.
// The child is a fresh process, so this is its environment alone.
func applyPluginEnv(argv0, rel string) {
	home := pluginHomeOf(argv0, rel)
	if home == "" {
		return
	}
	root := os.Getenv("KUSTOMIZE_PLUGIN_CONFIG_ROOT")
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	for _, kv := range pluginRenderEnv(home, root) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			os.Setenv(k, v)
		}
	}
}
